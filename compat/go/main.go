// Command compat-go sends events through the official Go SDK and checks what
// arrived.
//
// It uses the real SDK, unmodified, configured with nothing but a DSN — which
// is the exact claim being tested. Anything this program has to work around is
// an incompatibility, and it says so rather than adapting.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/getsentry/sentry-go"
)

func main() {
	dsn := flag.String("dsn", "", "DSN of a project on the server under test")
	apiURL := flag.String("api", "", "base URL of the server's API")
	token := flag.String("token", "", "API token for reading back the issues")
	projectID := flag.Int64("project", 1, "project id")
	flag.Parse()

	if *dsn == "" || *apiURL == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "usage: compat-go -dsn ... -api ... -token ... [-project N]")
		os.Exit(2)
	}

	if err := run(*dsn, *apiURL, *token, *projectID); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL %v\n", err)
		os.Exit(1)
	}
	fmt.Println("ok   the official Go SDK works against this server, DSN only")
}

func run(dsn, apiURL, token string, projectID int64) error {
	// The whole configuration. If anything else were needed here, the
	// compatibility claim would be false.
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:         dsn,
		Release:     "compat@1.0.0",
		Environment: "compat-test",
	}); err != nil {
		return fmt.Errorf("initialising the SDK: %w", err)
	}
	defer sentry.Flush(10 * time.Second)

	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetTag("suite", "compat-go")
	})

	// The same error three times with a value that differs each time, so this
	// exercises grouping rather than counting.
	for attempt := range 3 {
		sentry.CaptureException(fmt.Errorf("payment declined for order %d", 4800+attempt))
	}
	// A genuinely different error, which must land in its own issue.
	sentry.CaptureException(errors.New("upstream timed out"))
	// And a message with no exception at all, which takes a different path
	// through grouping.
	sentry.CaptureMessage("cache warm-up skipped")

	if !sentry.Flush(10 * time.Second) {
		return errors.New("the SDK could not flush its events within ten seconds")
	}

	issues, err := readIssues(apiURL, token, projectID)
	if err != nil {
		return err
	}
	return check(issues)
}

type issue struct {
	Title       string `json:"title"`
	Culprit     string `json:"culprit"`
	Level       string `json:"level"`
	Times       int64  `json:"times"`
	LastRelease string `json:"last_release"`
}

func readIssues(apiURL, token string, projectID int64) ([]issue, error) {
	url := fmt.Sprintf("%s/api/v1/projects/%d/issues", apiURL, projectID)

	request, err := http.NewRequest(http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Trapline-Request", "1")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("reading issues: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("reading issues: status %d: %s", response.StatusCode, body)
	}

	// The listing is a page, not a bare array: it carries the status counts
	// and a cursor alongside the issues.
	var page struct {
		Issues []issue `json:"issues"`
	}
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("decoding issues: %w", err)
	}
	return page.Issues, nil
}

func check(issues []issue) error {
	// Three distinct issues: the repeated exception, the other exception, and
	// the message.
	if len(issues) != 3 {
		return fmt.Errorf("got %d issues, want 3:\n%s", len(issues), render(issues))
	}

	var grouped *issue
	for index := range issues {
		if issues[index].Times == 3 {
			grouped = &issues[index]
		}
	}
	if grouped == nil {
		return fmt.Errorf("no issue collected the three occurrences of one error:\n%s", render(issues))
	}
	if grouped.LastRelease != "compat@1.0.0" {
		return fmt.Errorf("the release did not survive: %q", grouped.LastRelease)
	}
	if grouped.Culprit == "" {
		return fmt.Errorf("no culprit was derived from the SDK's stacktrace:\n%s", render(issues))
	}
	return nil
}

func render(issues []issue) string {
	rendered, err := json.MarshalIndent(issues, "  ", "  ")
	if err != nil {
		return fmt.Sprintf("%+v", issues)
	}
	return "  " + string(rendered)
}
