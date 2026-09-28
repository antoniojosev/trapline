package usecase

import (
	"context"
	"crypto/sha1" //nolint:gosec // the protocol's hash.
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/artifactbundle"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// fakeArtifacts is an in-memory stand-in for both the artefact store and the
// chunk staging area, so the decisions this use case makes — when `ok` is
// allowed, what the budget counts, what a re-upload replaces — are tested
// without a database making half of them.
type fakeArtifacts struct {
	stored []domain.Artifact
	bytes  map[int64][]byte
	nextID int64
	chunks map[string][]byte
	order  []string
}

func newFakeArtifacts() *fakeArtifacts {
	return &fakeArtifacts{bytes: map[int64][]byte{}, chunks: map[string][]byte{}}
}

func (f *fakeArtifacts) Store(
	_ context.Context, artifact domain.Artifact, content []byte,
) (domain.Artifact, error) {
	if err := artifact.Validate(); err != nil {
		return domain.Artifact{}, err
	}
	for index := range f.stored {
		existing := &f.stored[index]
		replaces := artifact.DebugID != "" &&
			existing.DebugID == artifact.DebugID && existing.Kind == artifact.Kind
		// The schema's second unique index, mirrored: a file with no debug id
		// is identified by release, dist and url, so two releases of one url
		// are two rows.
		replaces = replaces || (artifact.DebugID == "" && existing.DebugID == "" &&
			existing.Name == artifact.Name && existing.Dist == artifact.Dist &&
			sameRelease(existing.ReleaseID, artifact.ReleaseID))
		if replaces {
			artifact.ID = existing.ID
			f.stored[index] = artifact
			f.bytes[artifact.ID] = content
			return artifact, nil
		}
	}
	f.nextID++
	artifact.ID = f.nextID
	f.stored = append(f.stored, artifact)
	f.bytes[artifact.ID] = content
	return artifact, nil
}

func (f *fakeArtifacts) List(
	_ context.Context, projectID int64, filter ports.ArtifactFilter,
) ([]domain.Artifact, error) {
	var found []domain.Artifact
	for _, artifact := range f.stored {
		switch {
		case artifact.ProjectID != projectID:
		case filter.DebugID != "" && artifact.DebugID != filter.DebugID:
		case filter.Name != "" && artifact.Name != domain.ArtifactURL(filter.Name):
		case filter.Dist != "" && artifact.Dist != filter.Dist:
		default:
			found = append(found, artifact)
		}
	}
	return found, nil
}

func (f *fakeArtifacts) ByDebugID(
	_ context.Context, projectID int64, debugID string, kind domain.ArtifactKind,
) (domain.Artifact, []byte, error) {
	for _, artifact := range f.stored {
		if artifact.ProjectID == projectID && artifact.DebugID == debugID && artifact.Kind == kind {
			return artifact, f.bytes[artifact.ID], nil
		}
	}
	return domain.Artifact{}, nil, domain.ErrArtifactNotFound
}

func (f *fakeArtifacts) ByReleaseURL(
	context.Context, int64, int64, string, string,
) (domain.Artifact, []byte, error) {
	return domain.Artifact{}, nil, domain.ErrArtifactNotFound
}

func (f *fakeArtifacts) Delete(_ context.Context, projectID, artifactID int64) error {
	for index, artifact := range f.stored {
		if artifact.ProjectID == projectID && artifact.ID == artifactID {
			f.stored = append(f.stored[:index], f.stored[index+1:]...)
			return nil
		}
	}
	return domain.ErrArtifactNotFound
}

func (f *fakeArtifacts) HasArtifacts(_ context.Context, projectID int64) (bool, error) {
	for _, artifact := range f.stored {
		if artifact.ProjectID == projectID {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeArtifacts) UsedBytes(_ context.Context, projectID int64) (int64, error) {
	var used int64
	for _, artifact := range f.stored {
		if artifact.ProjectID == projectID {
			used += artifact.Size
		}
	}
	return used, nil
}

func (f *fakeArtifacts) PruneOrphansBefore(
	_ context.Context, cutoff time.Time, _ int,
) (int64, error) {
	var deleted int64
	kept := f.stored[:0]
	for _, artifact := range f.stored {
		if artifact.ReleaseID == nil && artifact.CreatedAt.Before(cutoff) {
			deleted++
			continue
		}
		kept = append(kept, artifact)
	}
	f.stored = kept
	return deleted, nil
}

func (f *fakeArtifacts) PutChunk(_ context.Context, checksum string, content []byte, _ time.Time) error {
	if _, present := f.chunks[checksum]; !present {
		f.order = append(f.order, checksum)
	}
	f.chunks[checksum] = content
	return nil
}

func (f *fakeArtifacts) MissingChunks(_ context.Context, checksums []string) ([]string, error) {
	missing := []string{}
	for _, checksum := range checksums {
		if _, present := f.chunks[checksum]; !present {
			missing = append(missing, checksum)
		}
	}
	return missing, nil
}

func (f *fakeArtifacts) Assemble(_ context.Context, checksums []string) ([]byte, error) {
	var whole []byte
	for _, checksum := range checksums {
		content, present := f.chunks[checksum]
		if !present {
			return nil, domain.ErrArtifactNotFound
		}
		whole = append(whole, content...)
	}
	return whole, nil
}

func (f *fakeArtifacts) PruneChunksBefore(context.Context, time.Time, int) (int64, error) {
	swept := int64(len(f.chunks))
	f.chunks = map[string][]byte{}
	return swept, nil
}

func (f *fakeArtifacts) StagedBytes(context.Context) (int64, error) {
	var staged int64
	for _, content := range f.chunks {
		staged += int64(len(content))
	}
	return staged, nil
}

func newArtifactsUseCase(t *testing.T) (*Artifacts, *fakeArtifacts, *fakeRepo) {
	t.Helper()
	store := newFakeArtifacts()
	projectRepo := newFakeRepo()
	clock := fixedClock{now: testNow}
	project, err := domain.NewProject("venekambio", testNow)
	if err != nil {
		t.Fatalf("building the project: %v", err)
	}
	key, err := domain.NewKey(testNow)
	if err != nil {
		t.Fatalf("minting a key: %v", err)
	}
	if _, _, err := projectRepo.Create(context.Background(), project, key); err != nil {
		t.Fatalf("creating the project: %v", err)
	}
	releases := NewReleases(newFakeReleases(), &fakeResolution{}, projectRepo, clock)
	projects := NewProjects(projectRepo, projectRepo, clock, testOrigin)
	return NewArtifacts(store, store, releases, projects, projectRepo, clock), store, projectRepo
}

func sampleBundle(t *testing.T, release, dist string) []byte {
	t.Helper()
	encoded, err := artifactbundle.Write(&artifactbundle.Bundle{
		DebugID: "9676ae7d-7fec-50bc-a1b9-085910f75007",
		Project: "venekambio",
		Release: release,
		Dist:    dist,
		Files: []artifactbundle.File{
			{
				Path:      artifactbundle.EntryPath("bundle.min.js"),
				Kind:      artifactbundle.KindMinifiedSource,
				URL:       "~/bundle.min.js",
				DebugID:   "fc31aec3-520c-531b-836e-0d5e872e8a18",
				SourceMap: "bundle.min.js.map",
				Content:   []byte("console.log(1)"),
			},
			{
				Path:    artifactbundle.EntryPath("bundle.min.js.map"),
				Kind:    artifactbundle.KindSourceMap,
				URL:     "~/bundle.min.js.map",
				DebugID: "fc31aec3-520c-531b-836e-0d5e872e8a18",
				Content: []byte(`{"version":3,"mappings":""}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("building a bundle: %v", err)
	}
	return encoded
}

func sum(data []byte) string {
	digest := sha1.Sum(data) //nolint:gosec // the protocol's hash.
	return hex.EncodeToString(digest[:])
}

// TestTheAcceptListIsTheProtocolDecision guards the one field that changes
// what the client does rather than describing what the server is.
func TestTheAcceptListIsTheProtocolDecision(t *testing.T) {
	accept := AcceptedUploadKinds()
	var hasBundles, hasFiles, hasV2 bool
	for _, kind := range accept {
		switch kind {
		case "artifact_bundles":
			hasBundles = true
		case "release_files":
			hasFiles = true
		case "artifact_bundles_v2":
			hasV2 = true
		}
	}
	if !hasBundles {
		t.Error("without artifact_bundles there is no debug-id upload at all")
	}
	if !hasFiles {
		t.Error("without release_files sentry-cli falls back and stops using debug ids, " +
			"which is the opposite of what announcing only the modern path looks like it does")
	}
	if hasV2 {
		t.Error("artifact_bundles_v2 puts assemble before the upload: measured against an " +
			"optimistic server it uploaded no chunks at all and reported success")
	}
}

// TestChunksAreVerifiedBeforeTheyAreStored: a chunk stored under a name that
// is not its content produces an assembly whose checksum does not match and no
// way at all to tell whose fault that was.
func TestChunksAreVerifiedBeforeTheyAreStored(t *testing.T) {
	artifacts, store, _ := newArtifactsUseCase(t)
	ctx := context.Background()

	err := artifacts.UploadChunks(ctx, []Chunk{{Checksum: strings.Repeat("0", 40), Content: []byte("x")}})
	if !errors.Is(err, domain.ErrInvalidArtifact) {
		t.Fatalf("a mis-named chunk was accepted: %v", err)
	}
	if len(store.chunks) != 0 {
		t.Fatal("it was stored anyway")
	}

	if err := artifacts.UploadChunks(ctx, []Chunk{{Checksum: sum([]byte("x")), Content: []byte("x")}}); err != nil {
		t.Fatalf("a correctly named chunk was refused: %v", err)
	}
}

func TestAssemblyIsOnlyOKAfterTheWrite(t *testing.T) {
	artifacts, store, _ := newArtifactsUseCase(t)
	ctx := context.Background()
	bundle := sampleBundle(t, "", "")

	// No chunks at all.
	result, err := artifacts.AssembleBundle(ctx, AssembleRequest{
		Checksum: sum(bundle), Chunks: []string{sum(bundle)}, ProjectRef: "venekambio"})
	if err != nil {
		t.Fatalf("AssembleBundle: %v", err)
	}
	if result.State != AssembleCreated {
		t.Fatalf("state = %q with nothing uploaded", result.State)
	}

	// An empty chunk list is not "assemble nothing successfully".
	result, err = artifacts.AssembleBundle(ctx, AssembleRequest{ProjectRef: "venekambio"})
	if err != nil {
		t.Fatalf("AssembleBundle: %v", err)
	}
	if result.State != AssembleError {
		t.Fatalf("state = %q for an assembly with no chunks", result.State)
	}

	// And with them.
	if err := artifacts.UploadChunks(ctx, []Chunk{{Checksum: sum(bundle), Content: bundle}}); err != nil {
		t.Fatalf("UploadChunks: %v", err)
	}
	result, err = artifacts.AssembleBundle(ctx, AssembleRequest{
		Checksum: sum(bundle), Chunks: []string{sum(bundle)}, ProjectRef: "venekambio"})
	if err != nil {
		t.Fatalf("AssembleBundle: %v", err)
	}
	if result.State != AssembleOK {
		t.Fatalf("state = %q detail = %q", result.State, result.Detail)
	}
	if len(store.stored) != 2 {
		t.Fatalf("%d artefacts stored, want the script and its map", len(store.stored))
	}

	// A retried assembly after success answers ok again rather than asking
	// for the chunks back: a pipeline that retries a step must not be told
	// its upload has gone.
	result, err = artifacts.AssembleBundle(ctx, AssembleRequest{
		Checksum: sum(bundle), Chunks: []string{sum(bundle)}, ProjectRef: "venekambio"})
	if err != nil || result.State != AssembleOK {
		t.Fatalf("a retried assembly answered %q (%v)", result.State, err)
	}
}

func TestTheRequestOutranksTheManifestAndTheManifestFillsTheGaps(t *testing.T) {
	artifacts, store, _ := newArtifactsUseCase(t)
	ctx := context.Background()

	// The manifest says one release; the caller says another. The caller is
	// what somebody typed on a command line; the manifest is a file that
	// could claim to belong anywhere.
	if _, err := artifacts.StoreBundle(ctx, 1, "app@2.0.0", "canary",
		sampleBundle(t, "app@1.0.0", "prod")); err != nil {
		t.Fatalf("StoreBundle: %v", err)
	}
	if store.stored[0].Dist != "canary" {
		t.Errorf("dist = %q, want the request's", store.stored[0].Dist)
	}

	// And with the request silent, the manifest fills in — which is what the
	// fallback endpoint needs, because its body has no such fields at all.
	if _, err := artifacts.StoreBundle(ctx, 1, "", "", sampleBundle(t, "app@1.0.0", "prod")); err != nil {
		t.Fatalf("StoreBundle: %v", err)
	}
	var found bool
	for _, artifact := range store.stored {
		if artifact.Dist == "prod" {
			found = true
		}
	}
	if !found {
		t.Error("the manifest's dist was dropped when the request named none")
	}
}

func TestTheBudgetDiscountsWhatARerunReplaces(t *testing.T) {
	artifacts, store, projects := newArtifactsUseCase(t)
	ctx := context.Background()

	// A budget just big enough for one copy of this bundle.
	bundle := sampleBundle(t, "", "")
	if _, err := artifacts.StoreBundle(ctx, 1, "", "", bundle); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	used, err := store.UsedBytes(ctx, 1)
	if err != nil {
		t.Fatalf("UsedBytes: %v", err)
	}

	budget := int(used/(1024*1024)) + 1
	config := domain.ProjectConfig{ArtifactsMaxMBSetting: &budget}
	if err := projects.SetProjectConfig(ctx, 1, config); err != nil {
		t.Fatalf("setting the budget: %v", err)
	}

	// The same build again. Without the discount this would count twice and a
	// project would drift into its own ceiling without ever growing.
	for range 5 {
		if _, err := artifacts.StoreBundle(ctx, 1, "", "", bundle); err != nil {
			t.Fatalf("re-upload: %v", err)
		}
	}
	if len(store.stored) != 2 {
		t.Fatalf("%d rows after six uploads of one build", len(store.stored))
	}
}

func TestABudgetOfZeroRefusesEverything(t *testing.T) {
	artifacts, _, projects := newArtifactsUseCase(t)
	ctx := context.Background()

	zero := 0
	if err := projects.SetProjectConfig(ctx, 1,
		domain.ProjectConfig{ArtifactsMaxMBSetting: &zero}); err != nil {
		t.Fatalf("setting the budget: %v", err)
	}

	// Zero is a decision, not an absence: "this project ships no JavaScript
	// and nobody is to upload megabytes against its name" (ADR 031's lesson).
	_, err := artifacts.StoreBundle(ctx, 1, "", "", sampleBundle(t, "", ""))
	if !errors.Is(err, domain.ErrArtifactBudgetExceeded) {
		t.Fatalf("a zero budget accepted an upload: %v", err)
	}
}

func TestTheLegacyPathInfersWhatItWasSent(t *testing.T) {
	artifacts, _, _ := newArtifactsUseCase(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		url     string
		content []byte
		want    domain.ArtifactKind
	}{
		{"by extension", "~/bundle.min.js.map", []byte("{}"), domain.ArtifactSourceMap},
		{"a script", "~/bundle.min.js", []byte("console.log(1)"), domain.ArtifactMinifiedSource},
		// A map served under a name that does not end in .map is a real thing
		// people do, and storing it as a script would make it unfindable as a
		// map.
		{"by content", "~/maps/bundle", []byte(`{"version":3,"sources":[],"mappings":""}`),
			domain.ArtifactSourceMap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored, err := artifacts.StoreReleaseFile(ctx, 1, "app@1.0.0", tc.url, tc.content)
			if err != nil {
				t.Fatalf("StoreReleaseFile: %v", err)
			}
			if stored.Kind != tc.want {
				t.Fatalf("kind = %q, want %q", stored.Kind, tc.want)
			}
			if stored.ReleaseID == nil {
				t.Fatal("a legacy upload with no release could never be found again")
			}
		})
	}
}

func TestABundleWithNothingThisServerStores(t *testing.T) {
	artifacts, _, _ := newArtifactsUseCase(t)
	ctx := context.Background()

	encoded, err := artifactbundle.Write(&artifactbundle.Bundle{
		Files: []artifactbundle.File{{
			Path:    artifactbundle.EntryPath("x.wasm"),
			Kind:    "indexed_ram_bundle",
			URL:     "~/x.wasm",
			Content: []byte("x"),
		}},
	})
	if err != nil {
		t.Fatalf("building the bundle: %v", err)
	}
	// The unknown type is skipped rather than refused (ADR 013), so what
	// fails is the bundle carrying nothing at all — and it fails as a domain
	// error, which is what makes the assemble state `error` with a sentence
	// rather than a 500.
	if _, err := artifacts.StoreBundle(ctx, 1, "", "", encoded); !errors.Is(err, domain.ErrInvalidArtifact) {
		t.Fatalf("StoreBundle reported %v", err)
	}
}

func TestListDeleteAndBudgetRefuseAProjectThatIsNotThere(t *testing.T) {
	artifacts, _, _ := newArtifactsUseCase(t)
	ctx := context.Background()

	if _, err := artifacts.List(ctx, 99, ports.ArtifactFilter{}); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("List reported %v", err)
	}
	if err := artifacts.Delete(ctx, 99, 1); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("Delete reported %v", err)
	}
	if _, _, err := artifacts.Budget(ctx, 99); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("Budget reported %v", err)
	}
}

func TestSweepRemovesWhatNothingWillAskForAgain(t *testing.T) {
	artifacts, store, _ := newArtifactsUseCase(t)
	ctx := context.Background()

	if err := artifacts.UploadChunks(ctx,
		[]Chunk{{Checksum: sum([]byte("x")), Content: []byte("x")}}); err != nil {
		t.Fatalf("UploadChunks: %v", err)
	}
	if _, err := artifacts.StoreBundle(ctx, 1, "", "", sampleBundle(t, "", "")); err != nil {
		t.Fatalf("StoreBundle: %v", err)
	}
	// Old enough that nothing will ever look them up.
	for index := range store.stored {
		store.stored[index].CreatedAt = testNow.Add(-2 * domain.OrphanArtifactRetention)
	}

	swept, err := artifacts.Sweep(ctx, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if swept != 3 {
		t.Fatalf("swept %d, want the chunk and the two orphaned artefacts", swept)
	}
}

// The assembly endpoint that carries no project at all resolves it from the
// release version, exactly as a commit set does (ADR 013 §6, from the recording).
func TestAnAssemblyWithNoProjectResolvesByVersion(t *testing.T) {
	artifacts, _, _ := newArtifactsUseCase(t)
	ctx := context.Background()

	if _, err := artifacts.releases.EnsureVersion(ctx, 1, "app@1.0.0"); err != nil {
		t.Fatalf("creating the release: %v", err)
	}
	bundle := sampleBundle(t, "", "")
	if err := artifacts.UploadChunks(ctx, []Chunk{{Checksum: sum(bundle), Content: bundle}}); err != nil {
		t.Fatalf("UploadChunks: %v", err)
	}

	result, err := artifacts.AssembleBundle(ctx, AssembleRequest{
		Checksum: sum(bundle), Chunks: []string{sum(bundle)}, Version: "app@1.0.0"})
	if err != nil {
		t.Fatalf("AssembleBundle: %v", err)
	}
	if result.State != AssembleOK {
		t.Fatalf("state = %q detail = %q", result.State, result.Detail)
	}

	// And one that names neither says so, rather than picking a project.
	result, err = artifacts.AssembleBundle(ctx, AssembleRequest{
		Checksum: sum(bundle), Chunks: []string{sum(bundle)}})
	if err != nil {
		t.Fatalf("AssembleBundle: %v", err)
	}
	if result.State != AssembleError {
		t.Fatalf("state = %q for an upload that names nothing", result.State)
	}
}

// The client aborts with "missing field X" on any one of them and does nothing
// else, so a policy with a gap is a server no pipeline can upload to (from
// the recording).
func TestThePolicyNamesEverySixMandatoryField(t *testing.T) {
	artifacts, _, _ := newArtifactsUseCase(t)
	policy := artifacts.Policy("https://errors.example.com/api/0/organizations/acme/chunk-upload/")

	if policy.URL == "" || !strings.HasPrefix(policy.URL, "https://") {
		t.Errorf("url = %q; the client posts chunks to this field and not to the path it asked on", policy.URL)
	}
	if policy.ChunkSize <= 0 || policy.ChunksPerRequest <= 0 || policy.MaxRequestSize <= 0 ||
		policy.Concurrency <= 0 {
		t.Errorf("a size or a count is zero: %+v", policy)
	}
	if policy.HashAlgorithm != "sha1" {
		t.Errorf("hashAlgorithm = %q; it is an enum with one value", policy.HashAlgorithm)
	}
	// The number the client ignores has to be true anyway, or it is a lie in
	// both directions.
	if policy.MaxFileSize <= policy.ChunkSize {
		t.Errorf("maxFileSize %d is not above one chunk", policy.MaxFileSize)
	}
	if int64(policy.ChunksPerRequest)*policy.ChunkSize > policy.MaxRequestSize {
		t.Errorf("chunksPerRequest × chunkSize (%d) is past maxRequestSize (%d): the client "+
			"would build a request the server refuses",
			int64(policy.ChunksPerRequest)*policy.ChunkSize, policy.MaxRequestSize)
	}
}

func TestListingAndDeletingWhatWasUploaded(t *testing.T) {
	artifacts, _, _ := newArtifactsUseCase(t)
	ctx := context.Background()

	stored, err := artifacts.StoreBundle(ctx, 1, "", "", sampleBundle(t, "", ""))
	if err != nil {
		t.Fatalf("StoreBundle: %v", err)
	}

	listed, err := artifacts.List(ctx, 1, ports.ArtifactFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("%d artefacts listed", len(listed))
	}

	used, budget, err := artifacts.Budget(ctx, 1)
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if used == 0 {
		t.Error("nothing is counted as used after an upload")
	}
	if budget != int64(domain.DefaultArtifactsMaxMB)*1024*1024 {
		t.Errorf("budget = %d, want the default", budget)
	}

	if err := artifacts.Delete(ctx, 1, stored[0].ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := artifacts.Delete(ctx, 1, stored[0].ID); !errors.Is(err, domain.ErrArtifactNotFound) {
		t.Fatalf("deleting twice reported %v", err)
	}
}

// A file with no debug id is replaced only within the release it belongs to.
// Two releases of one url are two artefacts, and an event from the older one
// still needs its own.
func TestOneURLInTwoReleasesIsTwoArtefacts(t *testing.T) {
	artifacts, store, _ := newArtifactsUseCase(t)
	ctx := context.Background()

	for _, version := range []string{"app@1.0.0", "app@2.0.0"} {
		if _, err := artifacts.StoreReleaseFile(ctx, 1, version, "~/bundle.min.js",
			[]byte("console.log('"+version+"')")); err != nil {
			t.Fatalf("StoreReleaseFile %s: %v", version, err)
		}
	}
	if len(store.stored) != 2 {
		t.Fatalf("%d rows, want one per release", len(store.stored))
	}
	if store.stored[0].ReleaseID == nil || store.stored[1].ReleaseID == nil ||
		*store.stored[0].ReleaseID == *store.stored[1].ReleaseID {
		t.Fatalf("the two rows share a release: %+v", store.stored)
	}
}
