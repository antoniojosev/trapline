package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// countingUptimeRepo is fakeUptimeRepo plus a tally of the reads the page
// makes, which is the only way to tell a cache hit from a cache miss without
// looking inside the use case.
type countingUptimeRepo struct {
	*fakeUptimeRepo
	lists   int
	dailies int
	days    []domain.UptimeDay
}

func (c *countingUptimeRepo) ListMonitors(
	ctx context.Context, projectID *int64,
) ([]domain.UptimeMonitor, error) {
	c.lists++
	return c.fakeUptimeRepo.ListMonitors(ctx, projectID)
}

func (c *countingUptimeRepo) DailyUptime(
	_ context.Context, _ int64, _, _ time.Time,
) ([]domain.UptimeDay, error) {
	c.dailies++
	return c.days, nil
}

func statusPageFixture(t *testing.T) (
	*StatusPage, *fakeRepo, *countingUptimeRepo, *fakeSettings, *movableClock,
) {
	t.Helper()

	projects := newFakeRepo()
	project, err := domain.NewProject("venekambio", time.Now().UTC())
	if err != nil {
		t.Fatalf("building a project: %v", err)
	}
	key, err := domain.NewKey(time.Now().UTC())
	if err != nil {
		t.Fatalf("building a key: %v", err)
	}
	stored, _, err := projects.Create(context.Background(), project, key)
	if err != nil {
		t.Fatalf("storing the project: %v", err)
	}

	uptime := &countingUptimeRepo{fakeUptimeRepo: newFakeUptimeRepo()}
	settings := newFakeSettings()
	clock := &movableClock{now: time.Date(2026, 8, 29, 15, 0, 0, 0, time.UTC)}

	page := NewStatusPage(projects, projects, uptime, settings, clock)
	if stored.Slug != "venekambio" {
		t.Fatalf("slug = %q", stored.Slug)
	}
	return page, projects, uptime, settings, clock
}

func publish(t *testing.T, projects *fakeRepo, enabled bool) {
	t.Helper()
	if err := projects.SetProjectConfig(context.Background(), 1, domain.ProjectConfig{
		StatusPage: domain.StatusPageProject{Enabled: enabled},
	}); err != nil {
		t.Fatalf("configuring: %v", err)
	}
}

func TestAPageIsRefusedUntilItIsPublished(t *testing.T) {
	page, _, _, _, _ := statusPageFixture(t)

	_, err := page.Page(context.Background(), "venekambio")
	if !errors.Is(err, ErrStatusPageDisabled) {
		t.Errorf("an unpublished project = %v, want ErrStatusPageDisabled", err)
	}

	_, err = page.Page(context.Background(), "nope")
	if !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("an unknown slug = %v, want ErrProjectNotFound", err)
	}
}

func TestOnlyPublicMonitorsReachThePage(t *testing.T) {
	page, projects, uptime, _, clock := statusPageFixture(t)
	publish(t, projects, true)

	ctx := context.Background()
	for _, monitor := range []domain.UptimeMonitor{
		{ProjectID: 1, Name: "checkout", URL: "http://93.184.216.34/", Method: "GET", Public: true},
		{ProjectID: 1, Name: "billing", URL: "http://93.184.216.34/", Method: "GET"},
	} {
		validated, err := domain.NewUptimeMonitor(&monitor, clock.now)
		if err != nil {
			t.Fatalf("building %s: %v", monitor.Name, err)
		}
		if _, err := uptime.CreateMonitor(ctx, &validated); err != nil {
			t.Fatalf("storing %s: %v", monitor.Name, err)
		}
	}

	view, err := page.Page(ctx, "venekambio")
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(view.Monitors) != 1 || view.Monitors[0].Name != "checkout" {
		t.Fatalf("the page shows %+v, want only the public monitor", view.Monitors)
	}
	// One daily read for the public monitor and none for the private one: the
	// filter has to happen before the query, or a page with fifty internal
	// monitors pays for all of them on every miss.
	if uptime.dailies != 1 {
		t.Errorf("%d daily reads, want one per public monitor", uptime.dailies)
	}
}

// The cache is the whole defence of an unauthenticated page (ADR 017).
func TestAPageIsBuiltOncePerWindow(t *testing.T) {
	page, projects, uptime, _, clock := statusPageFixture(t)
	publish(t, projects, true)
	ctx := context.Background()

	for range 5 {
		if _, err := page.Page(ctx, "venekambio"); err != nil {
			t.Fatalf("Page: %v", err)
		}
	}
	if uptime.lists != 1 {
		t.Errorf("%d listings for five reads inside the window", uptime.lists)
	}

	clock.now = clock.now.Add(domain.StatusPageTTL + time.Second)
	if _, err := page.Page(ctx, "venekambio"); err != nil {
		t.Fatalf("Page: %v", err)
	}
	if uptime.lists != 2 {
		t.Errorf("%d listings after the window expired, want a rebuild", uptime.lists)
	}
}

// A miss must not be remembered: the slug comes from whoever typed the URL, so
// caching misses is a way to grow a map in this server's memory from outside.
func TestAMissIsNotCached(t *testing.T) {
	page, projects, uptime, _, _ := statusPageFixture(t)
	ctx := context.Background()

	for range 3 {
		if _, err := page.Page(ctx, "venekambio"); err == nil {
			t.Fatal("an unpublished page answered")
		}
	}
	publish(t, projects, true)
	if _, err := page.Page(ctx, "venekambio"); err != nil {
		t.Fatalf("publishing did not take effect: %v", err)
	}
	if uptime.lists != 1 {
		t.Errorf("%d listings, want the one after publishing", uptime.lists)
	}
}

func TestInvalidateDropsOnePage(t *testing.T) {
	page, projects, uptime, _, _ := statusPageFixture(t)
	publish(t, projects, true)
	ctx := context.Background()

	if _, err := page.Page(ctx, "venekambio"); err != nil {
		t.Fatal(err)
	}
	page.Invalidate("venekambio")
	if _, err := page.Page(ctx, "venekambio"); err != nil {
		t.Fatal(err)
	}
	if uptime.lists != 2 {
		t.Errorf("%d listings, want the cache to have been dropped", uptime.lists)
	}
}

func TestSettingsRoundTripAndDropEveryCachedPage(t *testing.T) {
	page, projects, uptime, settings, _ := statusPageFixture(t)
	publish(t, projects, true)
	ctx := context.Background()

	if _, err := page.Page(ctx, "venekambio"); err != nil {
		t.Fatal(err)
	}
	written, err := page.SetSettings(ctx, domain.StatusPageSettings{
		Title: "  Acme  ", Description: "status",
	})
	if err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if written.Title != "Acme" {
		t.Errorf("the title was not trimmed: %q", written.Title)
	}
	stored, err := settings.Setting(ctx, domain.StatusPageSettingsKey)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	var decoded domain.StatusPageSettings
	if err := json.Unmarshal(stored, &decoded); err != nil {
		t.Fatalf("stored value is not the settings: %v", err)
	}

	view, err := page.Page(ctx, "venekambio")
	if err != nil {
		t.Fatal(err)
	}
	if view.Title != "Acme" {
		t.Errorf("the cached page survived a settings change: %q", view.Title)
	}
	if uptime.lists != 2 {
		t.Errorf("%d listings, want the cache to have been cleared", uptime.lists)
	}
}

func TestOverLongSettingsAreRefused(t *testing.T) {
	page, _, _, _, _ := statusPageFixture(t)
	_, err := page.SetSettings(context.Background(), domain.StatusPageSettings{
		Description: string(make([]byte, domain.MaxStatusPageDescription+1)),
	})
	if !errors.Is(err, domain.ErrInvalidSetting) {
		t.Errorf("an over-long description = %v, want ErrInvalidSetting", err)
	}
}
