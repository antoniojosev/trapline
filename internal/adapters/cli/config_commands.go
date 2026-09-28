package cli

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// runConfig dispatches the project-configuration subcommands.
//
// These exist alongside the panel's settings screen rather than after it. A
// subsystem that can only be switched on by clicking is a subsystem an agent
// cannot switch on, and the toggles of ADR 005 are the product's central
// claim — being able to change them from a script is part of the claim, not a
// convenience on top of it.
func runConfig(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "config: expected show or set")
		return ExitUsage
	}
	switch args[0] {
	case "show":
		return runConfigShow(c, args[1:])
	case "set":
		return runConfigSet(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "config: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

type configPayload struct {
	EnabledCategories  []string       `json:"enabled_categories"`
	RateLimitPerMinute int            `json:"rate_limit_per_minute"`
	RetentionDays      map[string]int `json:"retention_days"`
	TracesSampleRate   *float64       `json:"traces_sample_rate"`
	ArtifactsMaxMB     *int           `json:"artifacts_max_mb"`
	Defaults           struct {
		EnabledCategories  []string       `json:"enabled_categories"`
		RateLimitPerMinute int            `json:"rate_limit_per_minute"`
		RetentionDays      map[string]int `json:"retention_days"`
		TracesSampleRate   float64        `json:"traces_sample_rate"`
		ArtifactsMaxMB     int            `json:"artifacts_max_mb"`
	} `json:"defaults"`
	AvailableCategories []string `json:"available_categories"`
	RetentionCategories []string `json:"retention_categories"`
	StatusPage          struct {
		Enabled bool `json:"enabled"`
	} `json:"status_page"`
}

func runConfigShow(c *context_, args []string) int {
	flags := newFlagSet(c, "config show")
	projectID := flags.Int64("project", 0, "project id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "config show: -project is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var config configPayload
	path := "/projects/" + strconv.FormatInt(*projectID, 10) + "/config"
	if err := client.do(c.ctx, http.MethodGet, path, nil, &config); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, config, renderConfig(&config))
}

// renderConfig shows what applies and marks what is inherited, because "the
// default" and "someone chose this" are different facts and an operator
// deciding whether to change something needs to know which they are looking at.
func renderConfig(config *configPayload) string {
	var text strings.Builder

	enabled := config.EnabledCategories
	source := "set"
	if enabled == nil {
		enabled, source = config.Defaults.EnabledCategories, "default"
	}
	fmt.Fprintf(&text, "categories   %s  (%s)\n", strings.Join(enabled, ", "), source)
	if len(enabled) == 0 {
		text.WriteString("             this project accepts nothing\n")
	}

	limit, limitSource := config.RateLimitPerMinute, "set"
	if limit == 0 {
		limit, limitSource = config.Defaults.RateLimitPerMinute, "default"
	}
	fmt.Fprintf(&text, "rate limit   %d/minute (%d/second)  (%s)\n", limit, limit/60, limitSource)

	text.WriteString("retention\n")
	// The retention list is longer than the ingest list: the hourly aggregates
	// keep their own, much longer window and cannot be switched on at all
	// (ADR 010). Falling back keeps this readable against a server from before
	// that distinction existed.
	retentionCategories := config.RetentionCategories
	if len(retentionCategories) == 0 {
		retentionCategories = config.AvailableCategories
	}
	for _, category := range retentionCategories {
		days, configured := config.RetentionDays[category]
		retentionSource := "set"
		if !configured {
			days, retentionSource = config.Defaults.RetentionDays[category], "default"
		}
		if configured && days == 0 {
			// Zero is a decision, not an absence: it means keep nothing, and
			// printing it as "0 days (set)" beside the others would read like
			// a value that had not been reached yet.
			fmt.Fprintf(&text, "  %-12s keeps nothing  (set)\n", category)
			continue
		}
		fmt.Fprintf(&text, "  %-12s %d days  (%s)\n", category, days, retentionSource)
	}

	off := make([]string, 0, len(config.AvailableCategories))
	for _, category := range config.AvailableCategories {
		if !contains(enabled, category) {
			off = append(off, category)
		}
	}
	// Printed with its source like everything else, and with the sentence
	// that stops the number being read as a loss: the aggregates do not
	// depend on it. Somebody lowering this to save disk needs to know they
	// are giving up examples and not accuracy (ADR 021).
	rate, rateSource := config.Defaults.TracesSampleRate, "default"
	if config.TracesSampleRate != nil {
		rate, rateSource = *config.TracesSampleRate, "set"
	}
	fmt.Fprintf(&text, "traces       %.2f of traces keep their spans  (%s)\n", rate, rateSource)
	text.WriteString("             latency and failure rates are computed over every transaction\n")

	budget, budgetSource := config.Defaults.ArtifactsMaxMB, "default"
	if config.ArtifactsMaxMB != nil {
		budget, budgetSource = *config.ArtifactsMaxMB, "set"
	}
	fmt.Fprintf(&text, "artifacts    %d MB of source maps and scripts  (%s)\n", budget, budgetSource)

	statusPage := "off"
	if config.StatusPage.Enabled {
		statusPage = "on"
	}
	fmt.Fprintf(&text, "status page  %s\n", statusPage)

	if len(off) > 0 {
		// Said plainly, because a switched-off category answering an SDK with
		// backpressure looks like an error to whoever is reading SDK logs.
		fmt.Fprintf(&text, "\n%s are refused with a rate-limit response; SDKs stop sending them.\n",
			strings.Join(off, ", "))
	}
	return strings.TrimRight(text.String(), "\n")
}

func runConfigSet(c *context_, args []string) int {
	flags := newFlagSet(c, "config set")
	projectID := flags.Int64("project", 0, "project id (required)")
	categories := flags.String("categories", "",
		`comma-separated categories to accept, "none" for nothing, or "default" to inherit; omit to leave unchanged`)
	rateLimit := flags.Int("rate-limit", -1, "events per minute per category; 0 restores the default")
	retention := flags.String("retention", "",
		`comma-separated category=days, e.g. error=30,aggregates=400; 0 keeps nothing, `+
			`"default" restores every window`)
	// A string and not a bool: a bool flag that is absent and a bool flag set
	// to false are the same value, and this command's whole contract is that
	// an omitted flag changes nothing.
	statusPage := flags.String("status-page", "",
		`"on" or "off" — whether this project publishes a public status page at /status/<slug>`)
	// A string and not a float, for the same reason -status-page is not a
	// bool: an absent numeric flag and a flag set to zero are the same value,
	// and zero is a decision here — keep no raw traces. "default" is the way
	// back to inheriting, as it is for -categories and -retention.
	tracesRate := flags.String("traces-sample-rate", "",
		`share of traces whose spans are stored, 0 to 1, or "default"; the aggregates always cover 100%`)
	// A string for the third time and for the third identical reason: zero is
	// a decision here — refuse every upload against this project — so it has
	// to be distinguishable from the flag being absent, and "default" is the
	// way back to inheriting.
	artifactsBudget := flags.String("artifacts-max-mb", "",
		`megabytes of uploaded source maps this project may hold, or "default"; 0 refuses every upload`)
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "config set: -project is required and must be positive")
		return ExitUsage
	}

	body := map[string]any{}
	if *categories != "" {
		switch *categories {
		// "none" rather than an empty string, because an empty flag value is
		// indistinguishable from not passing the flag, and "accept nothing"
		// has to be expressible.
		case "none":
			body["enabled_categories"] = []string{}
		// And "default" because the way back is a third state, not the absence
		// of the other two: writing today's default profile in by hand would
		// look identical and quietly opt the project out of ever receiving a
		// changed one. -rate-limit and -retention already have this in the
		// shape of 0; the category list needs a word because [] is taken.
		case "default":
			body["enabled_categories"] = nil
		default:
			body["enabled_categories"] = splitAndTrim(*categories)
		}
	}
	if *rateLimit >= 0 {
		body["rate_limit_per_minute"] = *rateLimit
	}
	if *retention != "" {
		// "default" for the same reason -categories has one: the way back to
		// inheriting is a third state, and with 0 now meaning "keep nothing"
		// there is no number that spells it.
		if *retention == "default" {
			body["retention_days"] = nil
		} else {
			parsed, err := parseRetention(*retention)
			if err != nil {
				fmt.Fprintf(c.stderr, "config set: %v\n", err)
				return ExitUsage
			}
			body["retention_days"] = parsed
		}
	}
	if *tracesRate != "" {
		if *tracesRate == "default" {
			body["traces_sample_rate"] = nil
		} else {
			parsed, err := strconv.ParseFloat(*tracesRate, 64)
			if err != nil || parsed < 0 || parsed > 1 {
				fmt.Fprintf(c.stderr,
					"config set: -traces-sample-rate takes a share between 0 and 1, or \"default\", got %q\n",
					*tracesRate)
				return ExitUsage
			}
			body["traces_sample_rate"] = parsed
		}
	}
	if *artifactsBudget != "" {
		if *artifactsBudget == "default" {
			body["artifacts_max_mb"] = nil
		} else {
			parsed, err := strconv.Atoi(*artifactsBudget)
			if err != nil || parsed < 0 {
				fmt.Fprintf(c.stderr,
					"config set: -artifacts-max-mb takes a number of megabytes, or \"default\", got %q\n",
					*artifactsBudget)
				return ExitUsage
			}
			body["artifacts_max_mb"] = parsed
		}
	}
	if *statusPage != "" {
		switch *statusPage {
		case "on":
			body["status_page"] = map[string]any{"enabled": true}
		case "off":
			body["status_page"] = map[string]any{"enabled": false}
		default:
			fmt.Fprintf(c.stderr, "config set: -status-page takes \"on\" or \"off\", got %q\n", *statusPage)
			return ExitUsage
		}
	}
	if len(body) == 0 {
		fmt.Fprintln(c.stderr,
			"config set: nothing to change; pass -categories, -rate-limit, -retention, "+
				"-traces-sample-rate, -artifacts-max-mb or -status-page")
		return ExitUsage
	}

	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	var config configPayload
	path := "/projects/" + strconv.FormatInt(*projectID, 10) + "/config"
	if err := client.do(c.ctx, http.MethodPut, path, body, &config); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON, config, renderConfig(&config))
}

func parseRetention(raw string) (map[string]int, error) {
	parsed := map[string]int{}
	for _, pair := range splitAndTrim(raw) {
		category, value, found := strings.Cut(pair, "=")
		if !found {
			return nil, fmt.Errorf("%q is not category=days", pair)
		}
		days, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("%q is not a number of days", value)
		}
		parsed[strings.TrimSpace(category)] = days
	}
	return parsed, nil
}

func splitAndTrim(raw string) []string {
	parts := strings.Split(raw, ",")
	trimmed := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			trimmed = append(trimmed, value)
		}
	}
	return trimmed
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
