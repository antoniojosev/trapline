package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

func TestParseTriggerAcceptsTheFourKinds(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantErr  bool
		mentions string
	}{
		{name: "new issue", raw: `{"kind":"new_issue"}`},
		{name: "regression", raw: `{"kind":"regression"}`},
		{name: "spike", raw: `{"kind":"issue_spike","window_s":3600,"min_count":10,"factor":3}`},
		{name: "error rate", raw: `{"kind":"error_rate","window_s":300,"min_events_per_min":100}`},

		{name: "an unknown kind names the alternatives", raw: `{"kind":"disk_full"}`,
			wantErr: true, mentions: "new_issue"},
		// A field the trigger does not use means the author expected
		// something it will not do, and silence there is how a rule that
		// never fires becomes a mystery.
		{name: "a parameter on a parameterless trigger", raw: `{"kind":"new_issue","factor":3}`,
			wantErr: true, mentions: "no parameters"},
		{name: "a misspelt field", raw: `{"kind":"issue_spike","windows":60}`,
			wantErr: true, mentions: "unknown field"},
		{name: "spike without a floor", raw: `{"kind":"issue_spike","window_s":3600,"factor":3}`,
			wantErr: true, mentions: "min_count"},
		{name: "spike with a factor under one", raw: `{"kind":"issue_spike","window_s":3600,"min_count":5,"factor":0.5}`,
			wantErr: true, mentions: "factor"},
		{name: "spike with the wrong parameter", raw: `{"kind":"issue_spike","window_s":60,"min_count":1,"factor":2,"min_events_per_min":4}`,
			wantErr: true, mentions: "issue_spike takes"},
		{name: "a window under a minute", raw: `{"kind":"error_rate","window_s":30,"min_events_per_min":10}`,
			wantErr: true, mentions: "window_s"},
		// error_rate is measured from a five-minute in-memory counter, so a
		// rule asking for six hours is asking a question that counter cannot
		// answer — and answering it approximately would look right.
		{name: "an error rate window longer than the counter",
			raw:     `{"kind":"error_rate","window_s":3600,"min_events_per_min":10}`,
			wantErr: true, mentions: "sliding window"},
		{name: "error rate with no threshold", raw: `{"kind":"error_rate","window_s":60}`,
			wantErr: true, mentions: "min_events_per_min"},
		{name: "error rate with a spike parameter", raw: `{"kind":"error_rate","window_s":60,"min_events_per_min":1,"factor":2}`,
			wantErr: true, mentions: "error_rate takes"},
		{name: "not an object", raw: `"new_issue"`, wantErr: true},
		{name: "two objects", raw: `{"kind":"new_issue"} {"kind":"regression"}`, wantErr: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			trigger, err := domain.ParseTrigger([]byte(testCase.raw))
			switch {
			case testCase.wantErr && err == nil:
				t.Fatalf("%s was accepted as %+v", testCase.raw, trigger)
			case !testCase.wantErr && err != nil:
				t.Fatalf("%s was rejected: %v", testCase.raw, err)
			case err == nil:
				return
			}
			if !errors.Is(err, domain.ErrInvalidAlert) {
				t.Errorf("error %v does not wrap ErrInvalidAlert", err)
			}
			if testCase.mentions != "" && !strings.Contains(err.Error(), testCase.mentions) {
				t.Errorf("error %q does not mention %q", err, testCase.mentions)
			}
		})
	}
}

// TestTriggerRoundTrips is what the stored column depends on: a rule written
// by one build has to parse in the next, and the encoding must not carry
// parameters the kind does not take — which would then be rejected on read.
func TestTriggerRoundTrips(t *testing.T) {
	for _, raw := range []string{
		`{"kind":"new_issue"}`,
		`{"kind":"regression"}`,
		`{"kind":"issue_spike","window_s":7200,"min_count":10,"factor":2.5}`,
		`{"kind":"error_rate","window_s":300,"min_events_per_min":100}`,
	} {
		trigger, err := domain.ParseTrigger([]byte(raw))
		if err != nil {
			t.Fatalf("%s did not parse: %v", raw, err)
		}
		encoded, err := trigger.Encode()
		if err != nil {
			t.Fatalf("%s did not encode: %v", raw, err)
		}
		again, err := domain.ParseTrigger(encoded)
		if err != nil {
			t.Fatalf("%s did not parse after a round trip (%s): %v", raw, encoded, err)
		}
		if again != trigger {
			t.Errorf("%s became %+v after a round trip, want %+v", raw, again, trigger)
		}
	}
}

func mustTrigger(t *testing.T, raw string) domain.Trigger {
	t.Helper()
	trigger, err := domain.ParseTrigger([]byte(raw))
	if err != nil {
		t.Fatalf("the fixture %s does not parse: %v", raw, err)
	}
	return trigger
}

func TestNewAlertRuleValidates(t *testing.T) {
	newIssue := mustTrigger(t, `{"kind":"new_issue"}`)
	project := int64(3)
	notAProject := int64(0)

	if _, err := domain.NewAlertRule(nil, "", newIssue, []int64{1}, 0, true); err == nil {
		t.Error("a rule with no name was accepted")
	}
	if _, err := domain.NewAlertRule(nil, strings.Repeat("x", domain.MaxRuleName+1),
		newIssue, []int64{1}, 0, true); err == nil {
		t.Error("an over-long name was accepted")
	}
	// A rule with no channel is configuration that looks like alerting and
	// notifies nobody, which is the worst state this subsystem has.
	if _, err := domain.NewAlertRule(nil, "n", newIssue, nil, 0, true); err == nil {
		t.Error("a rule with no channel was accepted")
	}
	tooMany := make([]int64, domain.MaxRuleChannels+1)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	if _, err := domain.NewAlertRule(nil, "n", newIssue, tooMany, 0, true); err == nil {
		t.Error("a rule fanning out past the ceiling was accepted")
	}
	if _, err := domain.NewAlertRule(nil, "n", newIssue, []int64{0}, 0, true); err == nil {
		t.Error("channel id 0 was accepted")
	}
	if _, err := domain.NewAlertRule(&notAProject, "n", newIssue, []int64{1}, 0, true); err == nil {
		t.Error("project id 0 was accepted; a global rule is expressed as no project, not as zero")
	}
	if _, err := domain.NewAlertRule(nil, "n", newIssue, []int64{1}, domain.MaxSilence+time.Hour, true); err == nil {
		t.Error("a silence longer than the ceiling was accepted")
	}
	if _, err := domain.NewAlertRule(nil, "n", domain.Trigger{Kind: "nope"}, []int64{1}, 0, true); err == nil {
		t.Error("an invalid trigger was accepted")
	}

	rule, err := domain.NewAlertRule(&project, " deploys ", newIssue, []int64{3, 1, 3}, 0, true)
	if err != nil {
		t.Fatalf("a valid rule was rejected: %v", err)
	}
	if rule.Name != "deploys" {
		t.Errorf("the name is %q, want it trimmed", rule.Name)
	}
	if len(rule.ChannelIDs) != 2 || rule.ChannelIDs[0] != 1 || rule.ChannelIDs[1] != 3 {
		t.Errorf("channel ids are %v, want them deduplicated and sorted", rule.ChannelIDs)
	}
	if rule.Silence() != domain.DefaultSilence {
		t.Errorf("silence is %s, want the default %s applied", rule.Silence(), domain.DefaultSilence)
	}
	// The project id is copied, so a caller mutating theirs afterwards cannot
	// change what the rule covers.
	project = 99
	if *rule.ProjectID != 3 {
		t.Error("the rule aliases the caller's project id")
	}
}

func TestAppliesTo(t *testing.T) {
	project := int64(7)
	scoped := domain.AlertRule{ProjectID: &project}
	global := domain.AlertRule{}

	if !scoped.AppliesTo(7) || scoped.AppliesTo(8) {
		t.Error("a scoped rule does not cover exactly its own project")
	}
	if !global.AppliesTo(7) || !global.AppliesTo(8) {
		t.Error("a rule with no project should cover every project")
	}
}

func TestMatches(t *testing.T) {
	spike := domain.AlertRule{
		Enabled: true,
		Trigger: mustTrigger(t, `{"kind":"issue_spike","window_s":3600,"min_count":10,"factor":3}`),
	}
	rate := domain.AlertRule{
		Enabled: true,
		Trigger: mustTrigger(t, `{"kind":"error_rate","window_s":300,"min_events_per_min":100}`),
	}
	newIssue := domain.AlertRule{Enabled: true, Trigger: mustTrigger(t, `{"kind":"new_issue"}`)}

	cases := []struct {
		name  string
		rule  domain.AlertRule
		event domain.AlertEvent
		want  bool
	}{
		{"a new issue matches", newIssue, domain.AlertEvent{Kind: domain.TriggerNewIssue}, true},
		{"another kind does not", newIssue, domain.AlertEvent{Kind: domain.TriggerRegression}, false},
		{"a disabled rule never matches",
			domain.AlertRule{Trigger: newIssue.Trigger}, domain.AlertEvent{Kind: domain.TriggerNewIssue}, false},

		{"a spike under the floor does not match", spike,
			domain.AlertEvent{Kind: domain.TriggerIssueSpike, Count: 9, Baseline: 1}, false},
		{"a spike over the floor but under the factor does not", spike,
			domain.AlertEvent{Kind: domain.TriggerIssueSpike, Count: 20, Baseline: 10}, false},
		{"a spike over both matches", spike,
			domain.AlertEvent{Kind: domain.TriggerIssueSpike, Count: 30, Baseline: 10}, true},
		// An issue with no history has a baseline of zero, and every ratio
		// against zero is infinite. Clearing the floor is the whole test, or a
		// burst on a brand-new issue would be invisible.
		{"a spike with no history clears on the floor alone", spike,
			domain.AlertEvent{Kind: domain.TriggerIssueSpike, Count: 10, Baseline: 0}, true},

		{"a rate under the threshold does not match", rate,
			domain.AlertEvent{Kind: domain.TriggerErrorRate, Count: 99}, false},
		{"a rate at the threshold matches", rate,
			domain.AlertEvent{Kind: domain.TriggerErrorRate, Count: 100}, true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.rule.Matches(&testCase.event); got != testCase.want {
				t.Errorf("Matches = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestMatchesRejectsAnUnknownKind(t *testing.T) {
	// A kind no build has ever had. It used to be "uptime_down", which stopped
	// being a stand-in for the future the moment uptime implemented it — and a
	// test whose premise has quietly become false asserts nothing.
	const fromANewerBuild = domain.TriggerKind("cost_ceiling")
	rule := domain.AlertRule{Enabled: true, Trigger: domain.Trigger{Kind: fromANewerBuild}}
	if rule.Matches(&domain.AlertEvent{Kind: fromANewerBuild}) {
		t.Error("a rule this build cannot evaluate matched; a trigger from a newer build must not fire here")
	}
}

// TestSubjectKeyIsPerSubject is the reason a deploy that breaks two endpoints
// produces two notifications: a rule-wide silence would let the first issue
// mask the second.
func TestSubjectKeyIsPerSubject(t *testing.T) {
	first := domain.AlertEvent{ProjectID: 1, IssueID: 4}
	second := domain.AlertEvent{ProjectID: 1, IssueID: 5}
	wide := domain.AlertEvent{ProjectID: 1}

	if first.SubjectKey() == second.SubjectKey() {
		t.Error("two issues share a subject key, so one would silence the other")
	}
	if first.SubjectKey() != "issue:4" {
		t.Errorf("subject key is %q, want issue:4", first.SubjectKey())
	}
	if wide.SubjectKey() != "project:1" {
		t.Errorf("a project-wide event has subject key %q, want project:1", wide.SubjectKey())
	}
}

func TestSilenced(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	if domain.Silenced(time.Time{}, time.Hour, now) {
		t.Error("a rule that has never fired was reported as silenced")
	}
	if domain.Silenced(now.Add(-time.Minute), 0, now) {
		t.Error("a silence of zero silenced something; zero means every time")
	}
	if !domain.Silenced(now.Add(-time.Minute), time.Hour, now) {
		t.Error("a rule that fired a minute ago is not silenced by an hour of quiet")
	}
	// The boundary is the moment it becomes audible again, not one tick later.
	if domain.Silenced(now.Add(-time.Hour), time.Hour, now) {
		t.Error("the window is still closed exactly when it expires")
	}
}
