package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// The tool table, as data.
//
// One list, walked twice: once by `Server` to register the tools on whichever
// transport is running, and once by the gate, which asserts that `tools/list`
// is byte-identical over stdio and over HTTP. Two tables would drift, and the
// shape of that bug is an agent that can resolve an issue when it runs beside
// the server and cannot when it runs in CI — which is the case the HTTP
// transport exists for.
//
// Every entry is a REST call. That is the whole design (ADR 022): a tool
// names a method and a path under `/api/v1/`, and something else decides
// whether that call goes over a socket or into this process's own router. It
// means an MCP tool cannot do anything the API cannot, cannot skip a scope
// check, and cannot answer differently from the endpoint it wraps.
//
// The input schemas are written out rather than inferred from Go types. They
// are the documentation the agent actually reads — the only documentation it
// reads — so the description of `in_next_release` matters more here than
// anywhere else in this repository, and a schema generated from a struct tag
// would say `bool`.

// tool is one entry of the table.
type tool struct {
	// Name is what the agent calls. Snake case, like every other MCP server,
	// and a verb first so a list of them reads as a list of things you can do.
	Name string
	// Description is the prompt. It says what the tool answers and, where it
	// matters, what it does not: an agent told only "resolves an issue" will
	// resolve one it has not fixed.
	Description string
	// Schema is the JSON Schema of the arguments, as written.
	Schema json.RawMessage
	// ReadOnly marks a tool that changes nothing. It is advertised to the
	// client so an agent operating under a confirmation policy knows which
	// calls need one, and it is not a permission: the permission is the scope
	// the underlying route demands, checked by the server.
	ReadOnly bool
	// Build turns arguments into the API call. It takes the environment
	// because a tool may need a call of its own first — resolving a project
	// slug to the id every path is built from.
	Build func(ctx context.Context, env *toolEnv, args arguments) (Request, error)
}

// toolEnv is what a tool may use while it builds its request.
type toolEnv struct {
	caller     Caller
	credential string
}

// projectID resolves what somebody put in the `project` argument.
//
// A number is used as it stands; anything else is looked up among the
// project slugs and then the names, case-insensitively. Accepting both costs
// one extra call in the uncommon case and removes the step an agent would
// otherwise have to take every single time: `list_projects`, read an id out
// of the JSON, then the call it actually wanted. The lookup is a real API
// call under the caller's own credential, so it cannot become a way to learn
// that a project exists without being allowed to read it.
func (e *toolEnv) projectID(ctx context.Context, raw string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("%w: project is required; it is the numeric id or the slug "+
			"from list_projects", errBadArgument)
	}
	if id, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		if id <= 0 {
			return 0, fmt.Errorf("%w: project id %d is not positive", errBadArgument, id)
		}
		return id, nil
	}

	response, err := e.caller.Call(ctx, e.credential, Request{Method: "GET", Path: "/projects"})
	if err != nil {
		return 0, err
	}
	if !response.OK() {
		return 0, fmt.Errorf("looking up project %q: %s", trimmed, response.Message())
	}
	var projects []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(response.Body, &projects); err != nil {
		return 0, fmt.Errorf("reading the project list: %w", err)
	}
	for _, project := range projects {
		if strings.EqualFold(project.Slug, trimmed) || strings.EqualFold(project.Name, trimmed) {
			return project.ID, nil
		}
	}
	// The known names are listed rather than withheld. The caller is already
	// allowed to read them — the call above just did, with their credential —
	// and an agent told only "not found" will guess again.
	known := make([]string, 0, len(projects))
	for _, project := range projects {
		known = append(known, project.Slug)
	}
	return 0, fmt.Errorf("%w: no project called %q; this installation has %s",
		errBadArgument, trimmed, strings.Join(known, ", "))
}

// tools is the whole set, in the order an agent would use them.
//
// A function rather than a package variable, so the table cannot be mutated
// by whoever imports it and so the schemas are built once per server rather
// than once per process — this is read at startup, not in a loop.
func tools() []tool {
	list := []tool{
		{
			Name: "list_projects",
			Description: "List the projects in this installation, with the id and slug " +
				"every other tool takes. Start here when you do not already know which " +
				"project an error belongs to.",
			Schema:   object(nil, nil),
			ReadOnly: true,
			Build: func(context.Context, *toolEnv, arguments) (Request, error) {
				return Request{Method: "GET", Path: "/projects"}, nil
			},
		},
		{
			Name: "list_issues",
			Description: "List a project's issues, newest activity first. An issue is a " +
				"group of events that share a fingerprint, not a single crash: `times` " +
				"is how often it has happened. Use `status` to see what is still open " +
				"and `q` to search titles and culprits.",
			Schema: object(map[string]any{
				"project": stringProperty("Project id or slug, from list_projects."),
				"status": enumProperty("Only issues in this state. Omit for all of them.",
					"unresolved", "resolved", "ignored"),
				"q": stringProperty("Free text matched against the issue title and culprit. " +
					"Words are ANDed; there are no search operators."),
				"environment": stringProperty("Only issues seen in this environment."),
				"release":     stringProperty("Only issues seen in this release."),
				"limit":       integerProperty("How many to return. The server's default is a page."),
			}, []string{"project"}),
			ReadOnly: true,
			Build: func(ctx context.Context, env *toolEnv, args arguments) (Request, error) {
				projectID, err := env.projectID(ctx, args.String("project"))
				if err != nil {
					return Request{}, err
				}
				query := url.Values{}
				for _, name := range []string{"status", "q", "environment", "release"} {
					if value := args.String(name); value != "" {
						query.Set(name, value)
					}
				}
				if limit, ok, err := args.Int("limit"); err != nil {
					return Request{}, err
				} else if ok {
					query.Set("limit", strconv.FormatInt(limit, 10))
				}
				return Request{Method: "GET", Path: issuesPath(projectID), Query: query}, nil
			},
		},
		{
			Name: "get_issue",
			Description: "One issue in full, as JSON: the stored events with their whole " +
				"payloads, the aggregated tags and the release lifecycle. Prefer " +
				"get_issue_bundle when you are about to fix the bug — it is the same " +
				"facts, already read.",
			Schema: object(map[string]any{
				"project": stringProperty("Project id or slug."),
				"issue":   integerProperty("Issue id, from list_issues."),
			}, []string{"project", "issue"}),
			ReadOnly: true,
			Build: func(ctx context.Context, env *toolEnv, args arguments) (Request, error) {
				path, err := issuePath(ctx, env, args)
				if err != nil {
					return Request{}, err
				}
				return Request{Method: "GET", Path: path}, nil
			},
		},
		{
			Name: "get_issue_bundle",
			Description: "Everything needed to fix one issue, as a markdown document: the " +
				"symbolicated stacktrace with source context, the breadcrumbs, the tags, " +
				"how often it has happened in the last day and fortnight, the release " +
				"lifecycle, and the commits that probably caused it. This is the one call " +
				"to make before changing code.",
			Schema: object(map[string]any{
				"project": stringProperty("Project id or slug."),
				"issue":   integerProperty("Issue id, from list_issues."),
			}, []string{"project", "issue"}),
			ReadOnly: true,
			Build: func(ctx context.Context, env *toolEnv, args arguments) (Request, error) {
				path, err := issuePath(ctx, env, args)
				if err != nil {
					return Request{}, err
				}
				return Request{Method: "GET", Path: path + "/bundle", Accept: bundleMediaType}, nil
			},
		},
		{
			Name: "resolve_issue",
			Description: "Mark an issue fixed. Set in_next_release when the fix is not " +
				"deployed yet: without it, the machines still running the broken build " +
				"reopen the issue within seconds of you resolving it, and the status " +
				"becomes one nobody believes. Requires a token with projects:write.",
			Schema: object(map[string]any{
				"project": stringProperty("Project id or slug."),
				"issue":   integerProperty("Issue id."),
				"in_next_release": booleanProperty("Fixed, and the fix ships next. Events " +
					"from the release it is currently being seen in, or anything older, " +
					"stop counting as regressions."),
			}, []string{"project", "issue"}),
			Build: func(ctx context.Context, env *toolEnv, args arguments) (Request, error) {
				body := map[string]any{"status": "resolved"}
				if args.Bool("in_next_release") {
					body["in_next_release"] = true
				}
				return statusRequest(ctx, env, args, body)
			},
		},
		{
			Name: "ignore_issue",
			Description: "Mute an issue without claiming it is fixed. Its events are still " +
				"counted; it simply stops being listed as open and stops alerting. " +
				"Requires a token with projects:write.",
			Schema: object(map[string]any{
				"project": stringProperty("Project id or slug."),
				"issue":   integerProperty("Issue id."),
			}, []string{"project", "issue"}),
			Build: func(ctx context.Context, env *toolEnv, args arguments) (Request, error) {
				return statusRequest(ctx, env, args, map[string]any{"status": "ignored"})
			},
		},
		{
			Name: "reopen_issue",
			Description: "Put an issue back in the unresolved state by hand, for instance " +
				"after finding that a fix did not work. Requires a token with " +
				"projects:write.",
			Schema: object(map[string]any{
				"project": stringProperty("Project id or slug."),
				"issue":   integerProperty("Issue id."),
			}, []string{"project", "issue"}),
			Build: func(ctx context.Context, env *toolEnv, args arguments) (Request, error) {
				return statusRequest(ctx, env, args, map[string]any{"status": "unresolved"})
			},
		},
		{
			Name: "get_release_health",
			Description: "Crash-free rate per release. Without a version, every release " +
				"that reported sessions in the range, ranked — which is how you find the " +
				"bad one. The newest hour can move after a server restart, and the answer " +
				"says so rather than hiding it.",
			Schema: object(map[string]any{
				"project": stringProperty("Project id or slug."),
				"version": stringProperty("One release, e.g. `shop@1.4.2`. Omit to rank them all."),
				"from":    stringProperty(rangeEndDescription),
				"to":      stringProperty(rangeEndDescription),
			}, []string{"project"}),
			ReadOnly: true,
			Build: func(ctx context.Context, env *toolEnv, args arguments) (Request, error) {
				projectID, err := env.projectID(ctx, args.String("project"))
				if err != nil {
					return Request{}, err
				}
				path := projectPath(projectID) + "/health"
				if version := args.String("version"); version != "" {
					path = projectPath(projectID) + "/releases/" + url.PathEscape(version) + "/health"
				}
				return Request{Method: "GET", Path: path, Query: rangeQuery(args)}, nil
			},
		},
		{
			Name: "query_stats",
			Description: "How much is breaking and when. Without `by`, one point per hour " +
				"over the range. With `by`, the counts grouped by release or environment " +
				"— which is what answers \"was it the deploy\" and \"is it only staging\". " +
				"Read from hourly aggregates, so it still answers after the events " +
				"themselves have been deleted.",
			Schema: object(map[string]any{
				"project": stringProperty("Project id or slug."),
				"from":    stringProperty(rangeEndDescription),
				"to":      stringProperty(rangeEndDescription),
				"by": enumProperty("Group the counts by one of the two facets every event "+
					"carries. Omit for the hourly series.", "release", "environment"),
				"limit": integerProperty("How many values to return, when grouping."),
			}, []string{"project"}),
			ReadOnly: true,
			Build: func(ctx context.Context, env *toolEnv, args arguments) (Request, error) {
				projectID, err := env.projectID(ctx, args.String("project"))
				if err != nil {
					return Request{}, err
				}
				query := rangeQuery(args)
				path := projectPath(projectID) + "/stats"
				if by := args.String("by"); by != "" {
					path += "/breakdown"
					query.Set("by", by)
				}
				if limit, ok, err := args.Int("limit"); err != nil {
					return Request{}, err
				} else if ok {
					query.Set("limit", strconv.FormatInt(limit, 10))
				}
				return Request{Method: "GET", Path: path, Query: query}, nil
			},
		},
		{
			Name: "list_transactions",
			Description: "What is slow, what is busy and what is failing, with p50/p95/p99 " +
				"computed over the range. Sort by `fail` to find what a latency chart " +
				"hides: a request that fails fast looks fast. The percentiles cover every " +
				"transaction received, whatever the trace sampling rate is.",
			Schema: object(map[string]any{
				"project": stringProperty("Project id or slug."),
				"from":    stringProperty(rangeEndDescription),
				"to":      stringProperty(rangeEndDescription),
				"sort": enumProperty("What to rank by: slowest, busiest, or most failing.",
					"p95", "count", "fail"),
				"limit": integerProperty("How many rows to return."),
			}, []string{"project"}),
			ReadOnly: true,
			Build: func(ctx context.Context, env *toolEnv, args arguments) (Request, error) {
				projectID, err := env.projectID(ctx, args.String("project"))
				if err != nil {
					return Request{}, err
				}
				query := rangeQuery(args)
				if sort := args.String("sort"); sort != "" {
					query.Set("sort", sort)
				}
				if limit, ok, err := args.Int("limit"); err != nil {
					return Request{}, err
				} else if ok {
					query.Set("limit", strconv.FormatInt(limit, 10))
				}
				return Request{Method: "GET", Path: projectPath(projectID) + "/transactions", Query: query}, nil
			},
		},
	}
	return list
}

// bundleMediaType is what get_issue_bundle asks for and what it must receive.
const bundleMediaType = "text/markdown"

// rangeEndDescription is the one explanation of a time bound, written once
// because three tools take the same pair and an agent that learned the format
// from one of them should not have to relearn it.
const rangeEndDescription = "A bound of the range: `2026-09-20`, `2026-09-20T14` or an " +
	"RFC 3339 instant. Omit both ends for the server's default window."

func projectPath(id int64) string {
	return "/projects/" + strconv.FormatInt(id, 10)
}

func issuesPath(id int64) string {
	return projectPath(id) + "/issues"
}

// issuePath resolves the project and issue a tool was given.
func issuePath(ctx context.Context, env *toolEnv, args arguments) (string, error) {
	projectID, err := env.projectID(ctx, args.String("project"))
	if err != nil {
		return "", err
	}
	issueID, found, err := args.Int("issue")
	if err != nil {
		return "", err
	}
	if !found || issueID <= 0 {
		return "", fmt.Errorf("%w: issue is required and is the numeric id from list_issues",
			errBadArgument)
	}
	return issuesPath(projectID) + "/" + strconv.FormatInt(issueID, 10), nil
}

// statusRequest is the one call the three triage tools share.
//
// Three tools over one endpoint, because the endpoint takes a status and the
// three are the same operation with different arguments — but three *tools*,
// because a tool called `set_issue_status` would make an agent guess the
// vocabulary, and a guessed status is a 400 it then has to recover from.
func statusRequest(ctx context.Context, env *toolEnv, args arguments, body map[string]any) (Request, error) {
	path, err := issuePath(ctx, env, args)
	if err != nil {
		return Request{}, err
	}
	return Request{Method: "POST", Path: path + "/status", Body: body}, nil
}

// rangeQuery carries the two range ends through untouched.
//
// Unparsed on purpose: the server owns what a range means, including what an
// omitted end defaults to (usecase.Stats.Range), and a client that pre-parsed
// them would be a second opinion on a question with one answer (ADR 006).
func rangeQuery(args arguments) url.Values {
	query := url.Values{}
	for _, name := range []string{"from", "to"} {
		if value := args.String(name); value != "" {
			query.Set(name, value)
		}
	}
	return query
}
