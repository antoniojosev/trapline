package sqlite

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

func newArtifactRepo(t *testing.T) (*ArtifactRepository, *ReleaseRepository, int64) {
	t.Helper()
	db := openTemp(t)
	project, _ := createProject(t, NewProjectRepository(db), "venekambio")
	return NewArtifactRepository(db), NewReleaseRepository(db), project.ID
}

func scriptArtifact(projectID int64, debugID, name string) domain.Artifact {
	return domain.Artifact{
		ProjectID:    projectID,
		DebugID:      debugID,
		Name:         name,
		Kind:         domain.ArtifactMinifiedSource,
		SourceMapRef: "bundle.min.js.map",
		SHA256:       "0000",
		Size:         14,
		CreatedAt:    testNow,
	}
}

func TestAnArtifactRoundTripsWithItsBytes(t *testing.T) {
	repo, _, projectID := newArtifactRepo(t)
	ctx := context.Background()

	content := []byte("console.log(1)")
	stored, err := repo.Store(ctx, scriptArtifact(projectID, "fc31aec3-520c-531b-836e-0d5e872e8a18",
		"~/bundle.min.js"), content)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if stored.ID == 0 {
		t.Fatal("no id came back")
	}

	found, stored2, err := repo.ByDebugID(ctx, projectID,
		"fc31aec3-520c-531b-836e-0d5e872e8a18", domain.ArtifactMinifiedSource)
	if err != nil {
		t.Fatalf("ByDebugID: %v", err)
	}
	if !bytes.Equal(stored2, content) {
		t.Errorf("content came back as %q", stored2)
	}
	if found.SourceMapRef != "bundle.min.js.map" {
		t.Error("the sourcemap header did not survive; without it there is no way from a " +
			"script to its map when there is no debug id")
	}
	if !found.CreatedAt.Equal(testNow) {
		t.Errorf("created_at = %v, want %v", found.CreatedAt, testNow)
	}
}

// The kind is part of the lookup because a script and its map share a debug
// id. A resolver that asked without one would, half the time, be handed the
// minified script it was trying to resolve (from the recording).
func TestOneDebugIDHoldsBothFiles(t *testing.T) {
	repo, _, projectID := newArtifactRepo(t)
	ctx := context.Background()
	const debugID = "fc31aec3-520c-531b-836e-0d5e872e8a18"

	if _, err := repo.Store(ctx, scriptArtifact(projectID, debugID, "~/bundle.min.js"),
		[]byte("script")); err != nil {
		t.Fatalf("storing the script: %v", err)
	}
	sourceMap := scriptArtifact(projectID, debugID, "~/bundle.min.js.map")
	sourceMap.Kind = domain.ArtifactSourceMap
	sourceMap.SourceMapRef = ""
	if _, err := repo.Store(ctx, sourceMap, []byte(`{"version":3}`)); err != nil {
		t.Fatalf("storing the map: %v", err)
	}

	script, _, err := repo.ByDebugID(ctx, projectID, debugID, domain.ArtifactMinifiedSource)
	if err != nil {
		t.Fatalf("looking for the script: %v", err)
	}
	found, content, err := repo.ByDebugID(ctx, projectID, debugID, domain.ArtifactSourceMap)
	if err != nil {
		t.Fatalf("looking for the map: %v", err)
	}
	if script.ID == found.ID {
		t.Fatal("one row answered for both kinds")
	}
	if string(content) != `{"version":3}` {
		t.Errorf("the map came back as %q", content)
	}
}

func TestReUploadingABuildReplacesItAndTwoBuildsBothSurvive(t *testing.T) {
	repo, _, projectID := newArtifactRepo(t)
	ctx := context.Background()

	first := scriptArtifact(projectID, "aaaaaaaa-0000-5000-8000-000000000000", "~/bundle.min.js")
	if _, err := repo.Store(ctx, first, []byte("v1")); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	// The same build again, which is what a rerun of a pipeline step sends.
	if _, err := repo.Store(ctx, first, []byte("v1 again")); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	// And a different build of the same file, which an event from the old
	// deploy still needs to be able to resolve.
	second := scriptArtifact(projectID, "bbbbbbbb-0000-5000-8000-000000000000", "~/bundle.min.js")
	if _, err := repo.Store(ctx, second, []byte("v2")); err != nil {
		t.Fatalf("second build: %v", err)
	}

	listed, err := repo.List(ctx, projectID, ports.ArtifactFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("%d rows, want one per build", len(listed))
	}

	_, content, err := repo.ByDebugID(ctx, projectID, first.DebugID, domain.ArtifactMinifiedSource)
	if err != nil {
		t.Fatalf("the first build is gone: %v", err)
	}
	if string(content) != "v1 again" {
		t.Errorf("the re-upload did not replace: %q", content)
	}
}

// SQLite treats two NULLs in a unique index as distinct, so without the
// IFNULL in the index an artefact with no release would be stored again on
// every deploy, forever.
func TestAnArtefactWithNoReleaseIsStoredOnceNotOncePerUpload(t *testing.T) {
	repo, _, projectID := newArtifactRepo(t)
	ctx := context.Background()

	legacy := scriptArtifact(projectID, "", "~/bundle.min.js")
	for range 3 {
		if _, err := repo.Store(ctx, legacy, []byte("same bytes")); err != nil {
			t.Fatalf("Store: %v", err)
		}
	}
	listed, err := repo.List(ctx, projectID, ports.ArtifactFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("%d rows after three uploads of one file", len(listed))
	}
}

func TestAReleaseTakesItsArtefactsWithIt(t *testing.T) {
	repo, releases, projectID := newArtifactRepo(t)
	ctx := context.Background()

	release, _, err := releases.Create(ctx, mustRelease(t, projectID, "app@1.0.0"))
	if err != nil {
		t.Fatalf("creating the release: %v", err)
	}

	attached := scriptArtifact(projectID, "", "~/bundle.min.js")
	attached.ReleaseID = &release.ID
	if _, err := repo.Store(ctx, attached, []byte("x")); err != nil {
		t.Fatalf("Store: %v", err)
	}

	found, _, err := repo.ByReleaseURL(ctx, projectID, release.ID, "", "~/bundle.min.js")
	if err != nil {
		t.Fatalf("ByReleaseURL: %v", err)
	}
	if found.Name != "~/bundle.min.js" {
		t.Errorf("found %q", found.Name)
	}

	// The retention rule of ADR 018, stated as schema rather than as a sweep:
	// an artefact uploaded with a release dies with it.
	if _, err := repo.db.ExecContext(ctx, "DELETE FROM releases WHERE id = ?", release.ID); err != nil {
		t.Fatalf("deleting the release: %v", err)
	}
	if _, _, err := repo.ByReleaseURL(ctx, projectID, release.ID, "", "~/bundle.min.js"); !errors.Is(err, domain.ErrArtifactNotFound) {
		t.Fatalf("the artefact outlived its release: %v", err)
	}
}

// The url the event carries and the url the uploader sent have to reduce to
// one string, or the second lookup of ADR 018 silently finds nothing.
func TestTheLookupNormalisesTheURL(t *testing.T) {
	repo, releases, projectID := newArtifactRepo(t)
	ctx := context.Background()

	release, _, err := releases.Create(ctx, mustRelease(t, projectID, "app@1.0.0"))
	if err != nil {
		t.Fatalf("creating the release: %v", err)
	}
	stored := scriptArtifact(projectID, "", "~/static/bundle.min.js")
	stored.ReleaseID = &release.ID
	if _, err := repo.Store(ctx, stored, []byte("x")); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// What an event's abs_path looks like.
	if _, _, err := repo.ByReleaseURL(ctx, projectID, release.ID, "",
		"https://app.example.test/static/bundle.min.js?v=8f3a"); err != nil {
		t.Fatalf("an absolute url with a cache-buster did not resolve: %v", err)
	}
}

func TestListFiltersAndItsBudgetArithmetic(t *testing.T) {
	repo, releases, projectID := newArtifactRepo(t)
	ctx := context.Background()

	release, _, err := releases.Create(ctx, mustRelease(t, projectID, "app@1.0.0"))
	if err != nil {
		t.Fatalf("creating the release: %v", err)
	}

	withRelease := scriptArtifact(projectID, "", "~/a.js")
	withRelease.ReleaseID = &release.ID
	withRelease.Dist = "prod"
	withRelease.Size = 10
	if _, err := repo.Store(ctx, withRelease, []byte("a")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	orphan := scriptArtifact(projectID, "cccccccc-0000-5000-8000-000000000000", "~/b.js")
	orphan.Size = 32
	if _, err := repo.Store(ctx, orphan, []byte("b")); err != nil {
		t.Fatalf("Store: %v", err)
	}

	cases := map[string]struct {
		filter ports.ArtifactFilter
		want   int
	}{
		"everything":         {ports.ArtifactFilter{}, 2},
		"by release":         {ports.ArtifactFilter{Release: "app@1.0.0"}, 1},
		"by dist":            {ports.ArtifactFilter{Dist: "prod"}, 1},
		"by debug id":        {ports.ArtifactFilter{DebugID: orphan.DebugID}, 1},
		"by url":             {ports.ArtifactFilter{Name: "~/b.js"}, 1},
		"by an absolute url": {ports.ArtifactFilter{Name: "https://app.example.test/b.js"}, 1},
		// A version nothing was uploaded against returns nothing rather than
		// everything, which is what a JOIN would have done.
		"by a release nobody uploaded to": {ports.ArtifactFilter{Release: "app@9.9.9"}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			listed, err := repo.List(ctx, projectID, tc.filter)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(listed) != tc.want {
				t.Fatalf("%d rows, want %d", len(listed), tc.want)
			}
		})
	}

	used, err := repo.UsedBytes(ctx, projectID)
	if err != nil {
		t.Fatalf("UsedBytes: %v", err)
	}
	if used != 42 {
		t.Fatalf("used = %d, want the sum of the uncompressed sizes", used)
	}
}

func TestDeletingAnArtefact(t *testing.T) {
	repo, _, projectID := newArtifactRepo(t)
	ctx := context.Background()

	stored, err := repo.Store(ctx, scriptArtifact(projectID, "", "~/a.js"), []byte("a"))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := repo.Delete(ctx, projectID, stored.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := repo.Delete(ctx, projectID, stored.ID); !errors.Is(err, domain.ErrArtifactNotFound) {
		t.Fatalf("deleting twice reported %v", err)
	}
	// And another project's id cannot reach it, which is the only thing
	// standing between two tenants of one installation.
	if err := repo.Delete(ctx, projectID+99, stored.ID); !errors.Is(err, domain.ErrArtifactNotFound) {
		t.Fatalf("a foreign project id reported %v", err)
	}
}

func TestOnlyTheOrphansAreSwept(t *testing.T) {
	repo, releases, projectID := newArtifactRepo(t)
	ctx := context.Background()

	release, _, err := releases.Create(ctx, mustRelease(t, projectID, "app@1.0.0"))
	if err != nil {
		t.Fatalf("creating the release: %v", err)
	}
	attached := scriptArtifact(projectID, "", "~/a.js")
	attached.ReleaseID = &release.ID
	attached.CreatedAt = testNow.Add(-365 * 24 * time.Hour)
	if _, err := repo.Store(ctx, attached, []byte("a")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	orphan := scriptArtifact(projectID, "dddddddd-0000-5000-8000-000000000000", "~/b.js")
	orphan.CreatedAt = testNow.Add(-365 * 24 * time.Hour)
	if _, err := repo.Store(ctx, orphan, []byte("b")); err != nil {
		t.Fatalf("Store: %v", err)
	}

	deleted, err := repo.PruneOrphansBefore(ctx, testNow, 100)
	if err != nil {
		t.Fatalf("PruneOrphansBefore: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("swept %d rows, want only the one with no release to die with", deleted)
	}
	listed, err := repo.List(ctx, projectID, ports.ArtifactFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].ReleaseID == nil {
		t.Fatalf("what survived: %+v", listed)
	}
}

func TestTheChunkStagingArea(t *testing.T) {
	repo, _, _ := newArtifactRepo(t)
	ctx := context.Background()

	if err := repo.PutChunk(ctx, "aaa", []byte("first "), testNow); err != nil {
		t.Fatalf("PutChunk: %v", err)
	}
	if err := repo.PutChunk(ctx, "bbb", []byte("second"), testNow); err != nil {
		t.Fatalf("PutChunk: %v", err)
	}
	// The same chunk twice is one row: two projects uploading builds that
	// share a vendor bundle should cost one copy between them.
	if err := repo.PutChunk(ctx, "aaa", []byte("first "), testNow); err != nil {
		t.Fatalf("PutChunk again: %v", err)
	}

	staged, err := repo.StagedBytes(ctx)
	if err != nil {
		t.Fatalf("StagedBytes: %v", err)
	}
	if staged != 12 {
		t.Fatalf("staged = %d, want the two chunks counted once each", staged)
	}

	missing, err := repo.MissingChunks(ctx, []string{"aaa", "ccc", "bbb"})
	if err != nil {
		t.Fatalf("MissingChunks: %v", err)
	}
	if len(missing) != 1 || missing[0] != "ccc" {
		t.Fatalf("missing = %v", missing)
	}

	// The order is the client's, and it is what the whole bundle's checksum
	// was computed over.
	whole, err := repo.Assemble(ctx, []string{"aaa", "bbb"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if string(whole) != "first second" {
		t.Fatalf("assembled %q", whole)
	}
	if _, err := repo.Assemble(ctx, []string{"aaa", "ccc"}); !errors.Is(err, domain.ErrArtifactNotFound) {
		t.Fatalf("assembling with a chunk that is not there reported %v", err)
	}

	swept, err := repo.PruneChunksBefore(ctx, testNow.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("PruneChunksBefore: %v", err)
	}
	if swept != 2 {
		t.Fatalf("swept %d chunks, want 2", swept)
	}
}

func TestAnArtefactThatBreaksADomainRuleIsNotStored(t *testing.T) {
	repo, _, projectID := newArtifactRepo(t)
	ctx := context.Background()

	broken := scriptArtifact(projectID, "", "~/a.js")
	broken.Kind = "indexed_ram_bundle"
	if _, err := repo.Store(ctx, broken, []byte("a")); !errors.Is(err, domain.ErrInvalidArtifact) {
		t.Fatalf("a kind the schema forbids reported %v", err)
	}

	// And the lookup refuses to treat the empty debug id as an identity:
	// every legacy upload carries one, so matching on it would hand back an
	// arbitrary artefact.
	if _, _, err := repo.ByDebugID(ctx, projectID, "", domain.ArtifactMinifiedSource); !errors.Is(err, domain.ErrArtifactNotFound) {
		t.Fatalf("an empty debug id reported %v", err)
	}
}
