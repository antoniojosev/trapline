package httpapi

import (
	"net/http"
	"testing"
)

func TestJobsNeedAuthentication(t *testing.T) {
	// What runs on this server and when is operational detail, and an
	// unauthenticated caller has no business reading it.
	client := newClient(t, newTestServer(t))
	if got := client.do(http.MethodGet, api("/system/jobs"), nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
}

func TestJobsReportsWhatIsRunning(t *testing.T) {
	// Retention is the one job that runs unconditionally, so it is what an
	// operator should see on a server that was only ever asked to track
	// errors (ADR 014).
	stack, _ := newStack(t)
	if err := stack.Scheduler.Start(t.Context()); err != nil {
		t.Fatalf("starting the jobs: %v", err)
	}
	t.Cleanup(stack.Scheduler.Stop)

	client := newClient(t, serve(t, stack, false))
	client.setUpAndLogIn()

	var response jobsResponse
	client.decode(client.do(http.MethodGet, api("/system/jobs"), nil), &response)

	if len(response.Jobs) != 1 {
		t.Fatalf("Jobs = %v, want retention and nothing else", response.Jobs)
	}
	job := response.Jobs[0]
	switch {
	case job.Name != "retention":
		t.Errorf("Name = %q", job.Name)
	case job.IntervalSeconds <= 0:
		t.Errorf("IntervalSeconds = %d", job.IntervalSeconds)
	case job.StartedAt.IsZero():
		t.Error("StartedAt is unset")
	case job.LastError != nil:
		t.Errorf("LastError = %q on a healthy server", *job.LastError)
	}
}

func TestJobsIsEmptyWhenNothingRuns(t *testing.T) {
	// A server whose scheduler was never started reports an empty list rather
	// than a 404 or a null: "nothing is running" is a true answer, and a
	// client should not have to special-case it.
	client := newClient(t, newTestServer(t))
	client.setUpAndLogIn()

	var response jobsResponse
	client.decode(client.do(http.MethodGet, api("/system/jobs"), nil), &response)
	if response.Jobs == nil {
		t.Fatal("Jobs is null, want an empty array")
	}
	if len(response.Jobs) != 0 {
		t.Errorf("Jobs = %v, want nothing", response.Jobs)
	}
}
