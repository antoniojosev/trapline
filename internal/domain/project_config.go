package domain

import (
	"encoding/json"
	"fmt"
)

// DesignEventsPerSecond is the sustained volume the storage design targets
// (ADR 001). It lives here, next to the limit derived from it, because the two
// numbers were once chosen independently and quietly contradicted each other.
const DesignEventsPerSecond = 100

// SpikeHeadroom is how far above the design volume a project may burst before
// spike protection cuts in.
//
// Spike protection is not about attackers: the usual cause is someone's retry
// loop reporting an error per iteration, which runs orders of magnitude faster
// than any real traffic. So the ceiling only has to be comfortably above
// legitimate volume, not close to it.
const SpikeHeadroom = 2

// DefaultRateLimitPerMinute is the spike-protection ceiling for a category.
//
// Derived rather than picked, because picking it is how this went wrong: the
// shipped default was 2000 per minute — about 33 events per second — while
// ADR 001 declared a design target of 100 per second. A project running at
// exactly the volume the documentation promised to handle would have been
// refused by this product's own spike protection, long before anything
// strained. The benchmark suite found it; nothing else would have, because
// each number was defensible on its own and only their relationship was wrong.
const DefaultRateLimitPerMinute = DesignEventsPerSecond * 60 * SpikeHeadroom

// ProjectConfig is a project's ingest profile.
//
// The zero value is the minimum profile — errors only — because that is what a
// fresh installation must start with. Everything else is opt-in, and opting in
// is what makes the cost of a subsystem visible to the person choosing it
// (ADR 005).
type ProjectConfig struct {
	// EnabledCategories are the categories this project accepts. Nil means
	// the default: errors only.
	//
	// Deliberately without omitempty, unlike the fields below. An empty list
	// is a decision — the operator turned everything off — and omitempty
	// erases an empty slice on the way out, so a stored "accept nothing" came
	// back as "unset" and quietly re-enabled errors. The domain has always
	// distinguished the two; storage did not, and only a test that went
	// through both noticed.
	EnabledCategories []string `json:"enabled_categories"`
	// RateLimitPerMinute caps each category. Zero means the default.
	RateLimitPerMinute int `json:"rate_limit_per_minute,omitempty"`
	// RetentionDays overrides retention per category, keyed by category name.
	RetentionDays map[string]int `json:"retention_days,omitempty"`
	// TracesSampleRateSetting is the share of traces whose raw spans are
	// stored, between 0 and 1. Nil means the default.
	//
	// The awkward name is deliberate: the useful accessor is
	// TracesSampleRate(), which resolves the default, and a field and a
	// method cannot share one name in Go. Naming the field for what it is —
	// the setting, which may be absent — rather than for what callers want
	// keeps the two apart at every call site.
	//
	// A pointer, unlike RateLimitPerMinute above, because zero is a decision
	// here and not an absence: "aggregate everything, keep no waterfalls" is
	// exactly what somebody with a busy service and a small disk chooses, and
	// a bare float64 could not tell it from "never set". That is the same
	// mistake retention made with a plain int, and the same fix (ADR 031,
	// ADR 021).
	//
	// It decides storage only. The per-minute aggregates — count, failures
	// and the latency sketch — are written for every transaction received
	// whatever this says, so lowering it costs waterfalls and never accuracy.
	TracesSampleRateSetting *float64 `json:"traces_sample_rate,omitempty"`
	// ArtifactsMaxMBSetting is how much this project may hold in uploaded
	// source maps and scripts. Nil means the default.
	//
	// A pointer for the reason TracesSampleRateSetting is one, and the reason
	// ADR 031 exists: zero is a decision here, not an absence. "This project
	// ships no JavaScript and nobody is to upload megabytes against its name"
	// is a real thing to want, and a bare int could not tell it from never
	// having been set — which is how a setting an operator can write, read
	// back and watch do nothing gets shipped.
	ArtifactsMaxMBSetting *int `json:"artifacts_max_mb,omitempty"`
	// StatusPage is whether this project publishes a public status page.
	//
	// Per project and not installation-wide, because publishing is a decision
	// about one thing: an installation usually has a customer-facing service
	// and several internal ones, and "everything gets a public page" is not a
	// setting anybody wants. It rides in this document rather than in a
	// column of its own because that is a migration for one boolean, and this
	// document is already the project's configuration (ADR 017).
	StatusPage StatusPageProject `json:"status_page,omitzero"`
}

// StatusPageProject is a project's half of the status page decision.
//
// A struct with one field rather than a bare boolean, because the field that
// comes next — a custom domain, a per-project title — belongs beside it, and
// widening a bare `status_page: true` later would break every stored document.
type StatusPageProject struct {
	// Enabled is whether GET /status/{slug} answers for this project. Off by
	// default: a page that appears the moment somebody adds a monitor is a
	// disclosure nobody chose.
	Enabled bool `json:"enabled,omitempty"`
}

// DefaultEnabledCategories is the minimum profile: error tracking and nothing
// else. A new installation must not quietly pay for subsystems nobody asked
// for, which is the whole complaint against the incumbent.
func DefaultEnabledCategories() []string { return []string{"error"} }

// CategoryEnabled reports whether a category may be ingested.
func (c ProjectConfig) CategoryEnabled(category string) bool {
	enabled := c.EnabledCategories
	if enabled == nil {
		enabled = DefaultEnabledCategories()
	}
	for _, candidate := range enabled {
		if candidate == category {
			return true
		}
	}
	return false
}

// Limit returns the per-minute ceiling for a category.
func (c ProjectConfig) Limit() int {
	if c.RateLimitPerMinute > 0 {
		return c.RateLimitPerMinute
	}
	return DefaultRateLimitPerMinute
}

// TracesSampleRate resolves the share of traces whose raw spans are kept.
//
// An out-of-range stored value falls back to the default rather than being
// clamped. A rate of 7 is not "keep everything", it is a configuration
// document somebody edited by hand or a migration that went wrong, and
// silently honouring it as 1.0 would fill a disk on the strength of a typo.
func (c ProjectConfig) TracesSampleRate() float64 {
	if c.TracesSampleRateSetting == nil || !ValidTracesSampleRate(*c.TracesSampleRateSetting) {
		return DefaultTracesSampleRate
	}
	return *c.TracesSampleRateSetting
}

// ArtifactsMaxMB resolves how much this project may hold in artefacts.
//
// A negative stored value falls back to the default rather than being clamped,
// for the same reason an out-of-range sample rate does: it is not a choice,
// it is a document somebody edited by hand or a migration that went wrong.
func (c ProjectConfig) ArtifactsMaxMB() int {
	if c.ArtifactsMaxMBSetting == nil || *c.ArtifactsMaxMBSetting < 0 {
		return DefaultArtifactsMaxMB
	}
	return *c.ArtifactsMaxMBSetting
}

// ArtifactsBudgetBytes is the same ceiling in the unit the store counts in.
func (c ProjectConfig) ArtifactsBudgetBytes() int64 {
	return int64(c.ArtifactsMaxMB()) * 1024 * 1024
}

// DecodeProjectConfig reads stored configuration.
//
// Unreadable configuration falls back to the defaults rather than failing:
// this is on the ingest path, and refusing every event because a config column
// got corrupted would turn a cosmetic problem into an outage of the thing
// people installed this for.
func DecodeProjectConfig(raw string) ProjectConfig {
	if raw == "" {
		return ProjectConfig{}
	}
	var config ProjectConfig
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return ProjectConfig{}
	}
	return config
}

// EncodeProjectConfig renders configuration for storage.
func EncodeProjectConfig(config ProjectConfig) (string, error) {
	encoded, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("encoding project config: %w", err)
	}
	return string(encoded), nil
}
