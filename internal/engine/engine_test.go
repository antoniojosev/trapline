package engine

import (
	"errors"
	"testing"
	"time"
)

func TestCategoryValid(t *testing.T) {
	for _, c := range Categories() {
		if !c.Valid() {
			t.Errorf("Categories() lists %q but Valid() rejects it", c)
		}
	}
	for _, c := range []Category{"", "replay", "profile", "ERROR"} {
		if c.Valid() {
			t.Errorf("%q reported as valid", c)
		}
	}
}

func TestEventValidate(t *testing.T) {
	valid := Event{
		Category:  CategoryError,
		ProjectID: 1,
		Timestamp: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
	}

	if err := valid.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Run("an event with no payload is valid", func(t *testing.T) {
		// Uptime results and cron check-ins are facts with no body; the
		// engine must not require one.
		if err := valid.Validate(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	cases := []struct {
		name  string
		event Event
		want  error
	}{
		{"unknown category", Event{Category: "replay", ProjectID: 1, Timestamp: valid.Timestamp}, ErrUnknownCategory},
		{"empty category", Event{ProjectID: 1, Timestamp: valid.Timestamp}, ErrUnknownCategory},
		{"zero project", Event{Category: CategoryError, Timestamp: valid.Timestamp}, ErrInvalidEvent},
		{"negative project", Event{Category: CategoryError, ProjectID: -1, Timestamp: valid.Timestamp}, ErrInvalidEvent},
		{"zero timestamp", Event{Category: CategoryError, ProjectID: 1}, ErrInvalidEvent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.event.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDefaultRetentionCoversEveryCategory(t *testing.T) {
	// A category with no retention entry would accumulate forever, which is
	// the failure mode a single-file store can least afford. The list is
	// RetentionCategories rather than Categories because the aggregates are
	// deletable without being ingestible: they are exactly the kind of thing
	// that grows forever if nobody writes down how long it is kept.
	policy := DefaultRetention()
	for _, c := range RetentionCategories() {
		if policy[c] <= 0 {
			t.Errorf("category %q has no default retention", c)
		}
	}
	if len(policy) != len(RetentionCategories()) {
		t.Errorf("policy has %d entries for %d retention categories",
			len(policy), len(RetentionCategories()))
	}
}

func TestAggregatesAreNotAnIngestCategory(t *testing.T) {
	// Aggregates are written as a side effect of storing an event, so nothing
	// can send them and no project may enable them. If Valid ever said yes,
	// the configuration endpoint would start offering a toggle for a category
	// no SDK can produce.
	if CategoryAggregates.Valid() {
		t.Error("aggregates is listed as an ingest category; nothing can send one")
	}
	var found bool
	for _, c := range RetentionCategories() {
		found = found || c == CategoryAggregates
	}
	if !found {
		t.Error("aggregates has no keep-window, so the buckets would grow forever")
	}
}
