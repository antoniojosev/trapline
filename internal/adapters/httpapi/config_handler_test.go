package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func configPath(projectID int64) string {
	return api("/projects/") + strconv.FormatInt(projectID, 10) + "/config"
}

func TestAFreshProjectIsTheMinimumProfile(t *testing.T) {
	client := newClient(t, newTestServer(t))
	dsn := createProjectForIngest(t, client)

	var config configResponse
	client.decode(client.do(http.MethodGet, configPath(dsn.ProjectID), nil), &config)

	// Errors only. A new installation must not quietly pay for subsystems
	// nobody asked for, which is the whole complaint against the incumbent.
	if len(config.EnabledCategories) != 0 {
		t.Errorf("EnabledCategories = %v, want nothing explicitly set", config.EnabledCategories)
	}
	if got := config.Defaults.EnabledCategories; len(got) != 1 || got[0] != "error" {
		t.Errorf("the default profile is %v, want errors only", got)
	}

	// The defaults travel with the response so a client can show them as
	// placeholders instead of hard-coding this product's numbers.
	if config.Defaults.RateLimitPerMinute <= 0 {
		t.Error("no default rate limit was reported")
	}
	if len(config.AvailableCategories) < 5 {
		t.Errorf("AvailableCategories = %v, want every category the engine knows", config.AvailableCategories)
	}
	if config.Defaults.RetentionDays["error"] <= 0 {
		t.Errorf("no default retention for errors: %v", config.Defaults.RetentionDays)
	}
}

func TestEnablingACategoryTakesEffectOnIngestImmediately(t *testing.T) {
	// The limiter caches configuration, so this also proves the cache is
	// dropped on write. Without that, switching a category on appears not to
	// work for as long as the TTL, which is the kind of bug someone debugs by
	// restarting the server and never reports.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	transaction := fmt.Sprintf("{\"dsn\":%q}\n{\"type\":\"transaction\"}\n{\"a\":1}\n", dsn.String())

	if got := sendEnvelope(t, server.URL, dsn, transaction, "").StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 before enabling", got)
	}

	response := client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{
		"enabled_categories": []string{"error", "transaction"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d", response.StatusCode)
	}

	if got := sendEnvelope(t, server.URL, dsn, transaction, "").StatusCode; got != http.StatusOK {
		t.Errorf("status = %d, want 200 immediately after enabling", got)
	}
}

func TestOmittingAFieldLeavesItAlone(t *testing.T) {
	// Fields are optional so a client can change one thing without having to
	// send back everything else it never looked at.
	client := newClient(t, newTestServer(t))
	dsn := createProjectForIngest(t, client)

	client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{
		"enabled_categories":    []string{"error", "session"},
		"rate_limit_per_minute": 500,
	})

	var config configResponse
	client.decode(client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{
		"rate_limit_per_minute": 900,
	}), &config)

	if config.RateLimitPerMinute != 900 {
		t.Errorf("RateLimitPerMinute = %d, want 900", config.RateLimitPerMinute)
	}
	if len(config.EnabledCategories) != 2 {
		t.Errorf("EnabledCategories = %v, want the untouched value kept", config.EnabledCategories)
	}
}

func TestZeroRevertsToTheDefault(t *testing.T) {
	// Without a way to un-set a value, "revert to the default" would mean
	// "look up the default and write it in", which freezes it: raising the
	// default later would never reach the project.
	client := newClient(t, newTestServer(t))
	dsn := createProjectForIngest(t, client)

	client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{"rate_limit_per_minute": 500})

	var config configResponse
	client.decode(client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{
		"rate_limit_per_minute": 0,
	}), &config)

	if config.RateLimitPerMinute != 0 {
		t.Errorf("RateLimitPerMinute = %d, want 0 meaning unset", config.RateLimitPerMinute)
	}
	if config.Defaults.RateLimitPerMinute <= 0 {
		t.Error("the default is no longer reported, so a client cannot show what now applies")
	}
}

func TestConfigRejectsNonsense(t *testing.T) {
	client := newClient(t, newTestServer(t))
	dsn := createProjectForIngest(t, client)

	cases := map[string]map[string]any{
		"unknown category":                  {"enabled_categories": []string{"error", "teleport"}},
		"negative limit":                    {"rate_limit_per_minute": -1},
		"negative retention":                {"retention_days": map[string]int{"error": -5}},
		"retention for an unknown category": {"retention_days": map[string]int{"teleport": 5}},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			response := client.do(http.MethodPut, configPath(dsn.ProjectID), body)
			if response.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", response.StatusCode)
			}
			var payload errorBody
			client.decode(response, &payload)
			// The message has to name what is wrong and what was expected; a
			// bare "bad request" sends someone reading source.
			if !strings.Contains(payload.Error, "categor") && !strings.Contains(payload.Error, "negative") {
				t.Errorf("error = %q, want it to explain the problem", payload.Error)
			}
		})
	}
}

func TestConfigForAMissingProject(t *testing.T) {
	// Answering with the defaults for an id somebody mistyped would look like
	// success and be a lie.
	client := newClient(t, newTestServer(t))
	client.setUpAndLogIn()

	if got := client.do(http.MethodGet, configPath(9999), nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("status = %d, want 404", got)
	}
}

func TestDisablingEverythingIsPossible(t *testing.T) {
	// An explicitly empty list is distinct from unset: it means the operator
	// turned everything off, and an installation that ingests nothing has to
	// be expressible.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{
		"enabled_categories": []string{},
	})

	if got := sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "x"), "").StatusCode; got != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 with every category off", got)
	}
}

// readBody returns a response body as text, for the assertions that are about
// the bytes on the wire rather than about what Go decodes them into.
//
// That distinction is the whole point here: decoding JSON null and JSON [] into
// a []string produces the same empty slice, so a test that only ever looks at
// the decoded value cannot see the difference between "nothing is set" and
// "the operator turned everything off" — which is exactly the difference that
// was lost.
func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return string(body)
}

func TestUnsetCategoriesAreNullAndEmptyCategoriesAreEmpty(t *testing.T) {
	// These are opposite states: a fresh project accepts errors by inheriting
	// the default profile, a switched-off one accepts nothing. They were
	// serialised identically as [], so `config show` told an operator their
	// fresh project accepted nothing and listed error among the categories
	// being refused, while it was accepting errors perfectly well.
	client := newClient(t, newTestServer(t))
	dsn := createProjectForIngest(t, client)

	fresh := readBody(t, client.do(http.MethodGet, configPath(dsn.ProjectID), nil))
	if !strings.Contains(fresh, `"enabled_categories":null`) {
		t.Errorf("a fresh project reports %s, want enabled_categories null so it reads as inherited", fresh)
	}

	client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{
		"enabled_categories": []string{},
	})

	off := readBody(t, client.do(http.MethodGet, configPath(dsn.ProjectID), nil))
	if !strings.Contains(off, `"enabled_categories":[]`) {
		t.Errorf("a switched-off project reports %s, want an explicit empty list", off)
	}
}

func TestNullRevertsCategoriesToTheInheritedProfile(t *testing.T) {
	// The way back has to be a state of its own. Writing today's default
	// profile in by hand looks identical and silently opts the project out of
	// ever receiving a changed one — the freezing this endpoint exists to
	// avoid, arrived at from the other direction.
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{
		"enabled_categories": []string{},
	})
	if got := sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "x"), "").StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 with every category off", got)
	}

	response := client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{
		"enabled_categories": nil,
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d", response.StatusCode)
	}
	if body := readBody(t, response); !strings.Contains(body, `"enabled_categories":null`) {
		t.Errorf("after clearing, the response is %s, want enabled_categories null", body)
	}
	if got := sendEnvelope(t, server.URL, dsn, pythonEnvelope(dsn, "y"), "").StatusCode; got != http.StatusOK {
		t.Errorf("status = %d, want 200: clearing the list should restore the inherited profile", got)
	}
}

func TestNullRevertsTheNumbersToo(t *testing.T) {
	// Zero already means "inherited" for these, so null is only required to
	// agree with it. A client that has one way of saying "unset" for three
	// fields is a client with fewer chances to say it wrong.
	client := newClient(t, newTestServer(t))
	dsn := createProjectForIngest(t, client)

	client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{
		"rate_limit_per_minute": 500,
		"retention_days":        map[string]int{"error": 30},
	})

	var config configResponse
	client.decode(client.do(http.MethodPut, configPath(dsn.ProjectID), map[string]any{
		"rate_limit_per_minute": nil,
		"retention_days":        nil,
	}), &config)

	if config.RateLimitPerMinute != 0 {
		t.Errorf("RateLimitPerMinute = %d, want 0 meaning unset", config.RateLimitPerMinute)
	}
	if len(config.RetentionDays) != 0 {
		t.Errorf("RetentionDays = %v, want nothing set", config.RetentionDays)
	}
}

func TestConfigForAProjectThatDoesNotExistIs404(t *testing.T) {
	// Both directions. Reading answered 404 from the day it was written;
	// writing answered 200 and echoed back a body that looked exactly like a
	// successful save of something that was never stored anywhere.
	client := newClient(t, newTestServer(t))
	client.setUpAndLogIn()

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			var body any
			if method == http.MethodPut {
				body = map[string]any{"rate_limit_per_minute": 500}
			}
			response := client.do(method, configPath(4242), body)
			if response.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404 for a project id nobody created", response.StatusCode)
			}
		})
	}
}
