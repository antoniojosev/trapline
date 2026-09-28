package httpapi

import (
	"net/http"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine"
)

// configResponse is a project's ingest profile.
//
// Explicit settings and defaults are returned side by side rather than merged.
// Merging them reads better but destroys information the client needs: a value
// that is unset must stay unset, so that raising a default later reaches the
// projects that never chose otherwise. A form that saves the merged view turns
// every default into a decision nobody made.
type configResponse struct {
	// EnabledCategories is null when nothing is set and [] when the operator
	// turned every category off. The two are opposite states — one accepts
	// errors, the other accepts nothing — so they must not share a shape.
	EnabledCategories  []string       `json:"enabled_categories"`
	RateLimitPerMinute int            `json:"rate_limit_per_minute"`
	RetentionDays      map[string]int `json:"retention_days"`
	// TracesSampleRate is null when nothing is set, exactly like the category
	// list above and for the same reason: zero is a decision here — aggregate
	// everything, keep no waterfalls — and a client that saw 0 could not tell
	// it from "never chosen", so raising the default later would never reach
	// the projects that never chose (ADR 021, ADR 031).
	TracesSampleRate *float64 `json:"traces_sample_rate"`
	// ArtifactsMaxMB is null when nothing is set, for the same reason: zero
	// is a decision — accept no uploads against this project at all — and a
	// client that saw 0 could not tell it from "never chosen" (ADR 018,
	// ADR 031).
	ArtifactsMaxMB *int `json:"artifacts_max_mb"`
	// StatusPage is whether this project publishes a public page, and it is
	// the one field here that is not about ingestion. It lives in the same
	// document because it is stored in the same document (ADR 017), and
	// splitting it into an endpoint of its own would be a second round trip
	// for one boolean on the same screen.
	StatusPage domain.StatusPageProject `json:"status_page"`

	// Defaults are what applies where nothing is set, so a client can show
	// them as placeholders instead of hard-coding this product's numbers.
	Defaults configDefaults `json:"defaults"`
	// AvailableCategories lets a client render the toggles without keeping
	// its own copy of the list.
	AvailableCategories []string `json:"available_categories"`
	// RetentionCategories is what a keep-window can be set for, which is one
	// more thing than what can be ingested: the hourly aggregates have their
	// own, much longer window and nobody can switch them on (ADR 010). A
	// client that reused AvailableCategories here would silently offer no way
	// to change the one window that decides how much history survives.
	RetentionCategories []string `json:"retention_categories"`
}

type configDefaults struct {
	EnabledCategories  []string       `json:"enabled_categories"`
	RateLimitPerMinute int            `json:"rate_limit_per_minute"`
	RetentionDays      map[string]int `json:"retention_days"`
	TracesSampleRate   float64        `json:"traces_sample_rate"`
	ArtifactsMaxMB     int            `json:"artifacts_max_mb"`
}

func newConfigResponse(config domain.ProjectConfig) configResponse {
	available := make([]string, 0, len(engine.Categories()))
	// Retention covers one more thing than ingestion does: the hourly
	// aggregates, which nobody can switch on because they are written as a
	// side effect of storing an event, but which do have a keep-window of
	// their own — a far longer one, so the dashboard outlives the payloads
	// (ADR 010).
	retentionCategories := make([]string, 0, len(engine.RetentionCategories()))
	retentionDefaults := make(map[string]int, len(engine.RetentionCategories()))
	for _, category := range engine.RetentionCategories() {
		retentionCategories = append(retentionCategories, string(category))
		retentionDefaults[string(category)] = int(engine.DefaultRetention()[category].Hours() / 24)
	}
	for _, category := range engine.Categories() {
		available = append(available, string(category))
	}

	response := configResponse{
		EnabledCategories:  config.EnabledCategories,
		RateLimitPerMinute: config.RateLimitPerMinute,
		RetentionDays:      config.RetentionDays,
		TracesSampleRate:   config.TracesSampleRateSetting,
		ArtifactsMaxMB:     config.ArtifactsMaxMBSetting,
		StatusPage:         config.StatusPage,
		Defaults: configDefaults{
			EnabledCategories:  domain.DefaultEnabledCategories(),
			RateLimitPerMinute: domain.DefaultRateLimitPerMinute,
			RetentionDays:      retentionDefaults,
			TracesSampleRate:   domain.DefaultTracesSampleRate,
			ArtifactsMaxMB:     domain.DefaultArtifactsMaxMB,
		},
		AvailableCategories: available,
		RetentionCategories: retentionCategories,
	}
	// EnabledCategories is deliberately left nil — and so serialised as null —
	// when nothing is set. Normalising it to [] read better and destroyed the
	// only thing this endpoint exists to say: a fresh project (accepts errors,
	// by inheritance) and a project switched fully off (accepts nothing) came
	// back byte-for-byte identical, so `config show` announced "this project
	// accepts nothing" about a project that was happily accepting errors.
	//
	// RetentionDays is normalised to an empty object rather than null: an
	// absent map and an empty one both mean "nothing is overridden", and one
	// shape for one meaning saves every client a null check. A key set to
	// zero is not that — it means keep nothing (ADR 031, proposed) — which is
	// exactly why the absence has to be spelled as absence.
	if response.RetentionDays == nil {
		response.RetentionDays = map[string]int{}
	}
	return response
}

// requireProject answers domain.ErrProjectNotFound for an id that does not
// exist, so a configuration endpoint cannot hand back the defaults for a
// project nobody ever created.
//
// It reads the project without its keys: this needs to know the project is
// there, not what its DSNs are, and keys are the one part of a project that is
// a credential.
func (s *Server) requireProject(r *http.Request, projectID int64) error {
	_, err := s.projects.Find(r.Context(), projectID)
	return err
}

func (s *Server) handleGetProjectConfig(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireProject(r, projectID); err != nil {
		writeError(w, err)
		return
	}

	config, err := s.projects.Config(r.Context(), projectID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newConfigResponse(config))
}

// configRequest is what a client may set. It deliberately does not accept the
// defaults block back: those are this server's, not a client's to overwrite.
type configRequest struct {
	EnabledCategories  optional[[]string]          `json:"enabled_categories"`
	RateLimitPerMinute optional[int]               `json:"rate_limit_per_minute"`
	RetentionDays      optional[map[string]int]    `json:"retention_days"`
	TracesSampleRate   optional[float64]           `json:"traces_sample_rate"`
	ArtifactsMaxMB     optional[int]               `json:"artifacts_max_mb"`
	StatusPage         optional[statusPageRequest] `json:"status_page"`
}

// statusPageRequest is the per-project half of ADR 017.
type statusPageRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSetProjectConfig writes a project's ingest profile.
//
// Fields are optional so that omitting one leaves it alone while sending null
// clears it back to the default. Without that distinction a client could never
// un-set a value, and "revert to the default" would mean "look up what the
// default is and write it in", which freezes it: raising a default later would
// never reach the projects that never chose otherwise.
//
// For the two numeric settings zero already means "inherited", so null and 0
// agree. For the category list they cannot: [] is a decision to accept
// nothing, so null is the only way back to the inherited profile.
func (s *Server) handleSetProjectConfig(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	// The same existence check the read side does, and for a stronger reason:
	// a write that answers 200 for a project id somebody mistyped reports
	// success for something that was never stored anywhere. The read path
	// has answered 404 since it was written; this one did not, and the
	// difference was invisible because both hand back a plausible body.
	if err := s.requireProject(r, projectID); err != nil {
		writeError(w, err)
		return
	}

	var request configRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}

	current, err := s.projects.Config(r.Context(), projectID)
	if err != nil {
		writeError(w, err)
		return
	}

	if request.EnabledCategories.Present {
		categories := request.EnabledCategories.Value
		if categories == nil {
			// null: back to inheriting, which is not the same as [].
			current.EnabledCategories = nil
		} else {
			for _, category := range *categories {
				if !engine.Category(category).Valid() {
					writeError(w, invalidCategory(category))
					return
				}
			}
			current.EnabledCategories = *categories
		}
	}
	if request.RateLimitPerMinute.Present {
		limit := 0
		if request.RateLimitPerMinute.Value != nil {
			limit = *request.RateLimitPerMinute.Value
		}
		if limit < 0 {
			writeError(w, negativeLimit())
			return
		}
		current.RateLimitPerMinute = limit
	}
	if request.RetentionDays.Present {
		days := map[string]int{}
		if request.RetentionDays.Value != nil {
			days = *request.RetentionDays.Value
		}
		for category, value := range days {
			if !validRetentionCategory(category) {
				writeError(w, invalidRetentionCategory(category))
				return
			}
			if value < 0 {
				writeError(w, negativeRetention(category))
				return
			}
		}
		current.RetentionDays = days
	}
	if request.TracesSampleRate.Present {
		// null clears it back to inheriting, a number sets it. Zero is a
		// number and means it: keep no raw traces at all while still
		// aggregating every transaction received.
		current.TracesSampleRateSetting = nil
		if request.TracesSampleRate.Value != nil {
			rate := *request.TracesSampleRate.Value
			if !domain.ValidTracesSampleRate(rate) {
				writeError(w, invalidSampleRate(rate))
				return
			}
			current.TracesSampleRateSetting = &rate
		}
	}
	if request.ArtifactsMaxMB.Present {
		// null clears it back to inheriting; a number sets it, and zero is a
		// number that means it — no artefact may be stored against this
		// project (ADR 018, ADR 031).
		current.ArtifactsMaxMBSetting = nil
		if request.ArtifactsMaxMB.Value != nil {
			budget := *request.ArtifactsMaxMB.Value
			if budget < 0 {
				writeError(w, negativeArtifactBudget())
				return
			}
			current.ArtifactsMaxMBSetting = &budget
		}
	}
	if request.StatusPage.Present {
		// null switches it off, which is the same as sending {"enabled":false}
		// — there is no third state to inherit here, unlike the fields above.
		current.StatusPage = domain.StatusPageProject{}
		if request.StatusPage.Value != nil {
			current.StatusPage.Enabled = request.StatusPage.Value.Enabled
		}
	}

	if err := s.projects.SetConfig(r.Context(), projectID, current); err != nil {
		writeError(w, err)
		return
	}
	// The public page is cached for thirty seconds, and an operator who has
	// just switched it on will reload it within one. Dropping the entry here
	// is what stops that reload from showing the page they were trying to
	// change — or a 404 for the page they just published.
	if s.status != nil {
		if project, err := s.projects.Find(r.Context(), projectID); err == nil {
			s.status.Invalidate(project.Slug)
		}
	}
	writeJSON(w, http.StatusOK, newConfigResponse(current))
}

// validRetentionCategory reports whether a keep-window may be set for this
// name. Wider than engine.Category.Valid on purpose: aggregates are not
// ingestible but are deletable, so they belong in exactly one of the two
// lists.
func validRetentionCategory(category string) bool {
	for _, known := range engine.RetentionCategories() {
		if string(known) == category {
			return true
		}
	}
	return false
}
