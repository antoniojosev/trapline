package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

// These tests replay the recording.
//
// compat/sentry-cli/fixtures/<version>/ holds every request the real, pinned
// sentry-cli made while running a release through a deploy pipeline, captured
// by compat/sentry-cli/record.sh. Nothing here is hand-written traffic: a
// hand-written request is this project's idea of what the tool sends, which is
// the one thing in question (ADR 013, and the same argument as ADR 002).
//
// The gate scripts/sentry-cli.sh runs these *and* the real tool against a real
// server. Both are needed. These are fast, run without Docker and pin the wire
// shape byte for byte; the tool is the only thing that can prove it accepts
// what this server answers.

// fixtureRoot is reached by relative path because go:embed cannot leave the
// package directory and the fixtures belong next to the script that records
// them, not next to the handler.
const fixtureRoot = "../../../compat/sentry-cli/fixtures"

// recordedRequest mirrors the recorder's output format.
type recordedRequest struct {
	Seq      int                 `json:"seq"`
	Method   string              `json:"method"`
	Path     string              `json:"path"`
	Query    map[string][]string `json:"query"`
	Header   map[string]string   `json:"headers"`
	BodyJSON json.RawMessage     `json:"body_json"`
	Body     string              `json:"body"`
	Status   int                 `json:"response_status"`
}

func loadFixtures(t *testing.T) []recordedRequest {
	t.Helper()

	versions, err := os.ReadDir(fixtureRoot)
	if err != nil {
		t.Fatalf("reading %s: %v", fixtureRoot, err)
	}
	var recorded []recordedRequest
	for _, version := range versions {
		if !version.IsDir() {
			continue
		}
		pattern := filepath.Join(fixtureRoot, version.Name(), "*.json")
		files, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("globbing %s: %v", pattern, err)
		}
		for _, file := range files {
			raw, err := os.ReadFile(file) //nolint:gosec // a committed fixture path
			if err != nil {
				t.Fatalf("reading %s: %v", file, err)
			}
			var entry recordedRequest
			if err := json.Unmarshal(raw, &entry); err != nil {
				t.Fatalf("decoding %s: %v", file, err)
			}
			recorded = append(recorded, entry)
		}
	}
	if len(recorded) == 0 {
		t.Fatalf("no fixtures under %s: run scripts/sentry-cli.sh to record them", fixtureRoot)
	}
	sort.Slice(recorded, func(a, b int) bool { return recorded[a].Seq < recorded[b].Seq })
	return recorded
}

// compatStack is a server with one project whose slug is the one the recording
// used, plus a token with every scope — which is what SENTRY_AUTH_TOKEN is.
type compatStack struct {
	baseURL string
	token   string
}

func newCompatStack(t *testing.T) compatStack {
	t.Helper()

	server, tokens := newTestServerWithTokens(t)
	_, plaintext, err := tokens.Create(context.Background(), "deploy-pipeline", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	// The recording ran with SENTRY_PROJECT=venekambio, so the project has to
	// derive that slug — which it does, from its name, with nothing else set.
	var project projectResponse
	response := tokenRequest(t, server.URL, plaintext, http.MethodPost, api("/projects"),
		createProjectRequest{Name: "venekambio"})
	decodeInto(t, response, &project)
	if project.Slug != "venekambio" {
		t.Fatalf("project slug = %q, want venekambio: the fixtures address the project by that name", project.Slug)
	}

	return compatStack{baseURL: server.URL, token: plaintext}
}

func decodeInto(t *testing.T, response *http.Response, target any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
}

// replay sends one recorded request at this server, with the recorded
// credential replaced by a real one. Everything else — method, path, query,
// body, headers — is byte for byte what the tool sent.
func (s compatStack) replay(t *testing.T, entry recordedRequest) *http.Response {
	t.Helper()

	target := s.baseURL + entry.Path
	if len(entry.Query) > 0 {
		var pairs []string
		for name, values := range entry.Query {
			for _, value := range values {
				pairs = append(pairs, name+"="+value)
			}
		}
		sort.Strings(pairs)
		target += "?" + strings.Join(pairs, "&")
	}

	body := []byte(entry.Body)
	if len(entry.BodyJSON) > 0 {
		body = entry.BodyJSON
	}

	request, err := http.NewRequestWithContext(context.Background(), entry.Method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building %s %s: %v", entry.Method, entry.Path, err)
	}
	for name, value := range entry.Header {
		if strings.EqualFold(name, "authorization") {
			// The recorder redacts the credential, which is the point: the
			// fixtures are committed. What it kept is the scheme, and the
			// scheme is the finding.
			if !strings.HasPrefix(value, "Bearer ") {
				t.Fatalf("recorded authorization is %q, want a Bearer scheme", value)
			}
			request.Header.Set("Authorization", "Bearer "+s.token)
			continue
		}
		if strings.EqualFold(name, "content-encoding") {
			// The recorder stores the body decompressed, so replaying the
			// header would describe bytes that are no longer there.
			continue
		}
		request.Header.Set(name, value)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", entry.Method, entry.Path, err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// TestTheRecordedPipelineRuns replays the whole recording in order and then
// asks the product's own API what happened.
//
// This is the test the surface exists for: the release, its commits and its
// deploy have to end up where the native API can see them, having been put
// there by traffic nobody in this repository wrote.
func TestTheRecordedPipelineRuns(t *testing.T) {
	stack := newCompatStack(t)

	for _, entry := range loadFixtures(t) {
		response := stack.replay(t, entry)
		// Class, not the exact code. The recorder is stateless — it answered
		// 201 to both creates — while a real server answers 200 to the second,
		// because creating a release twice is not a conflict (ADR 012). What
		// the recording proves is which class the client accepted: it walked
		// on after every 2xx here and took the "no previous release" branch on
		// the one 404. Pinning the digit would pin the stub instead of the
		// protocol.
		if response.StatusCode/100 != entry.Status/100 {
			t.Fatalf("seq %d: %s %s answered %d, and the tool walked on from a %d",
				entry.Seq, entry.Method, entry.Path, response.StatusCode, entry.Status)
		}
	}

	// Everything below reads through the product's own API. The compatibility
	// surface is a second face on the same use cases; if the state it produced
	// were only visible to itself, it would have produced nothing.
	var detail releaseDetailResponse
	decodeInto(t, tokenRequest(t, stack.baseURL, stack.token, http.MethodGet,
		api("/projects/1/releases/app@1.0.0"), nil), &detail)

	if detail.Version != "app@1.0.0" {
		t.Fatalf("version = %q, want app@1.0.0", detail.Version)
	}
	if detail.DateReleased == nil {
		t.Error("the release is not finalised: `releases finalize` did not land")
	}
	if len(detail.Commits) != 3 {
		t.Fatalf("commits = %d, want the three the fixture repository has", len(detail.Commits))
	}
	if len(detail.Deploys) != 1 {
		t.Fatalf("deploys = %d, want one", len(detail.Deploys))
	}
	if detail.Deploys[0].Environment != "production" || detail.Deploys[0].Name != "pipeline-42" {
		t.Errorf("deploy = %+v, want production/pipeline-42", detail.Deploys[0])
	}

	// The changed paths are the whole input to suspect commits later (ADR 019
	// and 012), and they arrive only inside the commit set. A round trip that
	// kept the shas and dropped the paths would look like a success here
	// without one.
	// A path appears once per commit that touched it, with a different change
	// type each time — README.md is added in the first commit and deleted in
	// the third — so what has to survive is every (path, type) pair, not the
	// last one seen. The three types exist in this recording because the
	// fixture repository was built to produce them; a repository where every
	// file is added would have proved nothing about M or D.
	changes := map[string]bool{}
	for _, commit := range detail.Commits {
		for _, file := range commit.PatchSet {
			changes[file.Path+" "+file.Type] = true
		}
	}
	for _, want := range []string{
		"app/views.py A", "app/views.py M",
		"app/total.py A",
		"README.md A", "README.md D",
	} {
		if !changes[want] {
			t.Errorf("%q is not in the stored patch sets: the change types did not survive", want)
		}
	}
}

// TestTheRecordingUsesOnlyABearerToken pins the authentication finding.
//
// If a future version of the tool started sending something else, this fails
// on the fixtures rather than in production — which is the failure mode ADR 002
// was written about: @sentry/node authenticates in a way no documentation
// mentions, and a server that assumed otherwise passed all its own tests.
func TestTheRecordingUsesOnlyABearerToken(t *testing.T) {
	for _, entry := range loadFixtures(t) {
		value, present := entry.Header["authorization"]
		if !present {
			t.Errorf("seq %d (%s %s) carries no authorization header", entry.Seq, entry.Method, entry.Path)
			continue
		}
		if !strings.HasPrefix(value, "Bearer ") {
			t.Errorf("seq %d authenticates with %q, not a bearer token", entry.Seq, value)
		}
	}
}

// TestTheCompatSurfaceRefusesAnAnonymousCaller is the other half of leaving
// this surface outside the CSRF check: it is safe only because nothing here
// accepts an ambient credential.
func TestTheCompatSurfaceRefusesAnAnonymousCaller(t *testing.T) {
	stack := newCompatStack(t)

	response := tokenRequest(t, stack.baseURL, "", http.MethodPost,
		"/api/0/projects/acme/venekambio/releases/", map[string]string{"version": "app@2.0.0"})
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a caller with no token", response.StatusCode)
	}

	var body compatErrorBody
	decodeInto(t, response, &body)
	if body.Detail == "" {
		t.Error("the error came back without a `detail`, which is the only field the client prints")
	}
}

// TestTheCompatSurfaceNeedsNoCSRFHeader is the claim that the routing decision
// rests on. sentry-cli has never heard of this project's header, so a route
// that required it would reject the only client the surface exists for — and
// every test in this package would still pass.
func TestTheCompatSurfaceNeedsNoCSRFHeader(t *testing.T) {
	stack := newCompatStack(t)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		stack.baseURL+"/api/0/projects/whatever/venekambio/releases/",
		strings.NewReader(`{"version":"app@3.0.0","projects":["venekambio"]}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+stack.token)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("posting: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 without %s", response.StatusCode, CSRFHeader)
	}
}

// TestTheOrgSlugIsIgnored pins the decision, because it is the kind of thing a
// later change would quietly tighten. This is a single-organisation
// installation: there is nothing for the segment to select, and rejecting a
// value would mean every user has to discover a name this product never asked
// them for.
func TestTheOrgSlugIsIgnored(t *testing.T) {
	stack := newCompatStack(t)

	for _, org := range []string{"acme", "sentry", "whatever-they-configured"} {
		response := tokenRequest(t, stack.baseURL, stack.token, http.MethodPost,
			"/api/0/projects/"+org+"/venekambio/releases/",
			map[string]any{"version": "app@1.0.0", "projects": []string{"venekambio"}})
		// The second and third are the same release again, which is a 200:
		// creating a release twice is not a conflict (ADR 012).
		if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
			t.Fatalf("org %q answered %d, want the release to be created regardless", org, response.StatusCode)
		}
	}
}

// TestAProjectIsAddressableByIdAndBySlug pins the other half of ADR 013's
// resolution rule. The id is what the ingest path already forces on every
// installation, so it cannot stop working; the slug is what a pipeline
// configuration reads.
func TestAProjectIsAddressableByIdAndBySlug(t *testing.T) {
	stack := newCompatStack(t)

	for _, reference := range []string{"venekambio", "1"} {
		response := tokenRequest(t, stack.baseURL, stack.token, http.MethodPost,
			"/api/0/projects/acme/"+reference+"/releases/",
			map[string]any{"version": "by-" + reference})
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("project reference %q answered %d, want 201", reference, response.StatusCode)
		}
	}

	response := tokenRequest(t, stack.baseURL, stack.token, http.MethodPost,
		"/api/0/projects/acme/no-such-project/releases/",
		map[string]any{"version": "app@1.0.0"})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown project answered %d, want 404", response.StatusCode)
	}
}

// TestAnUnknownCompatPathAnswersInTheClientsErrorShape keeps the promise
// narrow and legible: this server emulates the release endpoints and says so,
// rather than returning a page or this project's own error shape to a client
// that only parses `detail`.
func TestAnUnknownCompatPathAnswersInTheClientsErrorShape(t *testing.T) {
	stack := newCompatStack(t)

	response := tokenRequest(t, stack.baseURL, stack.token, http.MethodGet,
		"/api/0/organizations/acme/projects/", nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
	var body compatErrorBody
	decodeInto(t, response, &body)
	if !strings.Contains(body.Detail, "sentry-cli") {
		t.Errorf("detail = %q, want it to say what this surface covers", body.Detail)
	}
}

// TestUnknownFieldsAreIgnoredOnTheCompatSurface is the opposite of what the
// product's own API does, and deliberately so: the fields on this wire belong
// to somebody else's tool and gain members between its releases.
func TestUnknownFieldsAreIgnoredOnTheCompatSurface(t *testing.T) {
	stack := newCompatStack(t)

	response := tokenRequest(t, stack.baseURL, stack.token, http.MethodPost,
		"/api/0/projects/acme/venekambio/releases/", map[string]any{
			"version":           "app@4.0.0",
			"projects":          []string{"venekambio"},
			"dateStarted":       "2026-08-29T10:00:00Z",
			"somethingBrandNew": "from a version of the tool that does not exist yet",
			"refs":              []any{},
		})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: an unknown field must not break a deploy", response.StatusCode)
	}
}

// TestACommitSetReachesEveryProjectHoldingTheVersion is why the
// organisation-scoped lookup returns a list rather than the first match.
//
// `set-commits` and `deploys new` name no project — only the version — and in
// the API being emulated a release belongs to the organisation and spans
// projects. A handler that picked one would drop the write for the others in
// silence, which is the worst way for a deploy annotation to fail: the command
// prints success and half the installation never hears about it.
func TestACommitSetReachesEveryProjectHoldingTheVersion(t *testing.T) {
	stack := newCompatStack(t)

	// A second project, with the same release in it.
	var second projectResponse
	decodeInto(t, tokenRequest(t, stack.baseURL, stack.token, http.MethodPost, api("/projects"),
		createProjectRequest{Name: "otra app"}), &second)

	for _, reference := range []string{"venekambio", second.Slug} {
		response := tokenRequest(t, stack.baseURL, stack.token, http.MethodPost,
			"/api/0/projects/acme/"+reference+"/releases/",
			map[string]any{"version": "app@2.0.0"})
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("creating the release in %q answered %d", reference, response.StatusCode)
		}
	}

	commits := tokenRequest(t, stack.baseURL, stack.token, http.MethodPut,
		"/api/0/organizations/acme/releases/app@2.0.0/", map[string]any{
			"commits": []map[string]any{{
				"id":      "a3f9c1e",
				"message": "fix the checkout total",
				"patch_set": []map[string]string{
					{"path": "app/total.py", "type": "A"},
				},
			}},
		})
	if commits.StatusCode != http.StatusOK {
		t.Fatalf("setting commits answered %d", commits.StatusCode)
	}

	deploy := tokenRequest(t, stack.baseURL, stack.token, http.MethodPost,
		"/api/0/organizations/acme/releases/app@2.0.0/deploys/",
		map[string]any{"environment": "production", "name": "pipeline-9"})
	if deploy.StatusCode != http.StatusCreated {
		t.Fatalf("recording the deploy answered %d", deploy.StatusCode)
	}

	for _, projectID := range []int64{1, second.ID} {
		var detail releaseDetailResponse
		decodeInto(t, tokenRequest(t, stack.baseURL, stack.token, http.MethodGet,
			api("/projects/")+strconv.FormatInt(projectID, 10)+"/releases/app@2.0.0", nil), &detail)
		if len(detail.Commits) != 1 {
			t.Errorf("project %d has %d commits, want the one that was set org-wide", projectID, len(detail.Commits))
		}
		if len(detail.Deploys) != 1 {
			t.Errorf("project %d has %d deploys, want the one that was recorded org-wide", projectID, len(detail.Deploys))
		}
	}
}

// TestAnOrganisationScopedCallForAVersionNobodyHasIsNotFound keeps the failure
// legible: a typo in a pipeline should say so rather than create something.
func TestAnOrganisationScopedCallForAVersionNobodyHasIsNotFound(t *testing.T) {
	stack := newCompatStack(t)

	for _, call := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPut, "/api/0/organizations/acme/releases/app@9.9.9/", map[string]any{"commits": []any{}}},
		{http.MethodPost, "/api/0/organizations/acme/releases/app@9.9.9/deploys/",
			map[string]any{"environment": "production"}},
	} {
		response := tokenRequest(t, stack.baseURL, stack.token, call.method, call.path, call.body)
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s answered %d, want 404", call.method, call.path, response.StatusCode)
		}
	}
}

// TestFinalisingAReleaseNobodyCreatedIsNotFound covers the other error path a
// pipeline can reach: `finalize` before `new`.
func TestFinalisingAReleaseNobodyCreatedIsNotFound(t *testing.T) {
	stack := newCompatStack(t)

	response := tokenRequest(t, stack.baseURL, stack.token, http.MethodPut,
		"/api/0/projects/acme/venekambio/releases/app@9.9.9/",
		map[string]any{"dateReleased": "2026-08-29T10:00:00Z"})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
}

// TestAMalformedBodyOnTheCompatSurfaceIsRefused: tolerance is about unknown
// fields, not about bytes that are not JSON at all.
func TestAMalformedBodyOnTheCompatSurfaceIsRefused(t *testing.T) {
	stack := newCompatStack(t)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		stack.baseURL+"/api/0/projects/acme/venekambio/releases/", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+stack.token)
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("posting: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}
