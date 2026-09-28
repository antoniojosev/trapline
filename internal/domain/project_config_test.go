package domain

import "testing"

func TestTheZeroConfigIsTheMinimumProfile(t *testing.T) {
	// A fresh installation must not quietly pay for subsystems nobody asked
	// for — that is the whole complaint against the incumbent.
	config := ProjectConfig{}

	if !config.CategoryEnabled("error") {
		t.Error("error tracking is off by default")
	}
	for _, category := range []string{"transaction", "session", "uptime", "check_in"} {
		if config.CategoryEnabled(category) {
			t.Errorf("%q is on by default", category)
		}
	}
	if config.Limit() != DefaultRateLimitPerMinute {
		t.Errorf("Limit() = %d", config.Limit())
	}
}

func TestOptingIn(t *testing.T) {
	config := ProjectConfig{EnabledCategories: []string{"error", "transaction"}}

	for _, category := range []string{"error", "transaction"} {
		if !config.CategoryEnabled(category) {
			t.Errorf("%q was not enabled", category)
		}
	}
	if config.CategoryEnabled("session") {
		t.Error("a category nobody enabled is on")
	}
}

func TestAnExplicitlyEmptyListDisablesEverything(t *testing.T) {
	// Distinct from nil: nil means "unset, use defaults", empty means
	// "the operator turned everything off". Collapsing the two would make it
	// impossible to run an installation that ingests nothing.
	config := ProjectConfig{EnabledCategories: []string{}}

	if config.CategoryEnabled("error") {
		t.Error("an explicitly empty list still accepted errors")
	}
}

func TestAnEmptyListSurvivesStorage(t *testing.T) {
	// The domain distinguishes "unset" from "explicitly empty"; encoding has
	// to as well, or turning everything off silently reverts to the default
	// the next time the config is read.
	encoded, err := EncodeProjectConfig(ProjectConfig{EnabledCategories: []string{}})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	decoded := DecodeProjectConfig(encoded)
	if decoded.EnabledCategories == nil {
		t.Fatalf("an explicitly empty list came back as unset, from %q", encoded)
	}
	if decoded.CategoryEnabled("error") {
		t.Error("a project configured to accept nothing accepts errors again after a round trip")
	}

	// And unset must still round-trip as unset.
	unset := DecodeProjectConfig(mustEncode(t, ProjectConfig{}))
	if unset.EnabledCategories != nil {
		t.Errorf("unset came back as %v", unset.EnabledCategories)
	}
	if !unset.CategoryEnabled("error") {
		t.Error("an unset config stopped accepting errors")
	}
}

func mustEncode(t *testing.T, config ProjectConfig) string {
	t.Helper()
	encoded, err := EncodeProjectConfig(config)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return encoded
}

func TestConfigRoundTrip(t *testing.T) {
	original := ProjectConfig{
		EnabledCategories:  []string{"error", "transaction"},
		RateLimitPerMinute: 500,
		RetentionDays:      map[string]int{"error": 30},
	}

	encoded, err := EncodeProjectConfig(original)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	decoded := DecodeProjectConfig(encoded)

	if decoded.Limit() != 500 {
		t.Errorf("Limit() = %d", decoded.Limit())
	}
	if !decoded.CategoryEnabled("transaction") || decoded.CategoryEnabled("session") {
		t.Errorf("categories = %v", decoded.EnabledCategories)
	}
	if decoded.RetentionDays["error"] != 30 {
		t.Errorf("RetentionDays = %v", decoded.RetentionDays)
	}
}

func TestUnreadableConfigFallsBackToDefaults(t *testing.T) {
	// This is on the ingest path. Refusing every event because a config
	// column got corrupted would turn a cosmetic problem into an outage of
	// the thing people installed this for.
	for _, raw := range []string{"", "no soy json", "[1,2,3]", "null"} {
		config := DecodeProjectConfig(raw)
		if !config.CategoryEnabled("error") {
			t.Errorf("%q left the installation unable to accept errors", raw)
		}
	}
}

func TestTheDefaultLimitDoesNotContradictTheDesignVolume(t *testing.T) {
	// The two numbers were once chosen independently: the default allowed
	// about 33 events per second while the documented design target was 100.
	// A project running at exactly the promised volume would have been refused
	// by this product's own spike protection. The relationship is asserted so
	// they cannot drift apart again.
	perSecond := DefaultRateLimitPerMinute / 60

	if perSecond < DesignEventsPerSecond {
		t.Errorf("the default limit allows %d events/s, below the %d events/s the storage design targets",
			perSecond, DesignEventsPerSecond)
	}
	// And not so high that a runaway loop fills a disk before anyone notices.
	if perSecond > DesignEventsPerSecond*10 {
		t.Errorf("the default limit allows %d events/s, so far above the design volume that it protects nothing",
			perSecond)
	}
}
