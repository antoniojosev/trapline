package domain

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// ArtifactKind is what an uploaded file is.
//
// The two names are the manifest's own, not this product's. ADR 018 wrote them
// as `source` and `sourcemap`; the recording says `minified_source` and
// `source_map` (from the recording), and a schema written from the first pair would
// have had to be migrated the first time a real upload arrived.
type ArtifactKind string

const (
	// ArtifactMinifiedSource is a shipped script.
	ArtifactMinifiedSource ArtifactKind = "minified_source"
	// ArtifactSourceMap is the map that resolves one.
	ArtifactSourceMap ArtifactKind = "source_map"
)

// Valid reports whether a kind is one this product stores.
func (k ArtifactKind) Valid() bool {
	return k == ArtifactMinifiedSource || k == ArtifactSourceMap
}

// MaxArtifactNameLength bounds the `~/…` url an artefact is addressed by. A
// name is a path somebody's bundler produced, and the longest real ones are
// deeply nested chunk names; four kilobytes is far past any of them and still
// bounds a column somebody could otherwise fill with a megabyte.
const MaxArtifactNameLength = 4096

// DefaultArtifactsMaxMB is how much a project may hold in artefacts before
// further uploads are refused.
//
// Two hundred megabytes is several full builds of a large single-page
// application with their maps, and it is a ceiling rather than an allocation:
// nothing is reserved, and a project that uploads one bundle uses what one
// bundle costs. It exists because source maps are the one thing a user uploads
// by the megabyte on a schedule, and an error tracker whose disk fills up has
// failed at the only moment that matters (ADR 018).
const DefaultArtifactsMaxMB = 200

// OrphanArtifactRetention is how long an artefact with no release is kept.
//
// An artefact uploaded with a release dies with it, which is the honest
// window: the maps for a build stop being useful when nothing reports errors
// from that build any more. A bundle uploaded by debug id alone names no
// release — that is the whole point of debug ids — so there is nothing for it
// to die with, and thirty days is the fallback (ADR 018).
const OrphanArtifactRetention = 30 * 24 * time.Hour

// ChunkRetention is how long an uploaded chunk waits for the assemble call
// that names it.
//
// The chunks of one upload arrive across several requests and the assembly
// comes after the last of them, so they have to outlive a request; they must
// not outlive an interrupted upload by much, because nothing will ever ask for
// them again and they are the same bytes as the bundle they would have made.
// A day is far longer than any upload and short enough that an abandoned one
// is not a leak.
const ChunkRetention = 24 * time.Hour

// Artifact is one stored file: a script or its map, addressable by debug id,
// by release and url, or both.
type Artifact struct {
	ID        int64
	ProjectID int64
	// ReleaseID is nil for a bundle uploaded by debug id alone, which is the
	// default path today (from the recording).
	ReleaseID *int64
	// Dist distinguishes two builds of one release — a debug build and a
	// production one, say. Empty when the upload named none.
	Dist string
	// DebugID is the id shared by a script and its map. Empty when the build
	// was never injected.
	DebugID string
	// BundleDebugID is the identity of the archive this came out of, which is
	// a different value from DebugID and belongs to no file (from the recording).
	// Nothing looks artefacts up by it; it is stored so an operator can tell
	// which upload a file arrived in.
	BundleDebugID string
	// Name is the `~/path` url the artefact is served under, which is what
	// the release+url lookup matches an event's abs_path against.
	Name string
	Kind ArtifactKind
	// SourceMapRef is the `sourcemap` header of a script: the file name of
	// its map. It is what joins the two without a debug id, and ADR 018's
	// schema had nowhere to put it (from the recording).
	SourceMapRef string
	// SHA256 is the digest of the stored bytes. Not sha1: the protocol's sha1
	// names chunks on the wire and this names content at rest, and the two
	// have nothing to do with each other.
	SHA256    string
	Size      int64
	CreatedAt time.Time
}

// Validate checks an artefact against the rules that do not depend on storage.
func (a Artifact) Validate() error {
	if a.ProjectID <= 0 {
		return fmt.Errorf("%w: an artifact belongs to a project", ErrInvalidArtifact)
	}
	if a.Name == "" {
		return fmt.Errorf("%w: an artifact needs a url", ErrInvalidArtifact)
	}
	if len(a.Name) > MaxArtifactNameLength {
		return fmt.Errorf("%w: the url is %d bytes and the limit is %d",
			ErrInvalidArtifact, len(a.Name), MaxArtifactNameLength)
	}
	if !a.Kind.Valid() {
		return fmt.Errorf("%w: %q is not a kind this server stores, expected %s or %s",
			ErrInvalidArtifact, a.Kind, ArtifactMinifiedSource, ArtifactSourceMap)
	}
	if a.Size < 0 {
		return fmt.Errorf("%w: a negative size", ErrInvalidArtifact)
	}
	return nil
}

// ArtifactURL normalises an address into the `~/path` form artefacts are
// stored under.
//
// It is the one place the two halves of ADR 018's second lookup meet: the
// uploader sends `~/bundle.min.js` and the event carries
// `https://app.example.com/static/bundle.min.js`, and they have to reduce to
// the same string or the lookup silently finds nothing. Reducing here — a pure
// function, in the domain — is what keeps the ingest side and the upload side
// from each growing their own idea of what "the same file" means.
//
// A full url loses its scheme, host and query and keeps its path. Anything
// already in `~/` form is left alone. A bare path gains the prefix.
func ArtifactURL(address string) string {
	address = strings.TrimSpace(address)
	if address == "" {
		return ""
	}
	if strings.HasPrefix(address, "~/") {
		return address
	}

	// Cut a query and a fragment before anything else: a cache-busting
	// `?v=8f3a` is not part of the file's identity, and leaving it on would
	// make every deploy's artefacts unmatchable by the next one's events.
	if index := strings.IndexAny(address, "?#"); index >= 0 {
		address = address[:index]
	}

	if index := strings.Index(address, "://"); index >= 0 {
		rest := address[index+3:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return "~/"
		}
		address = rest[slash:]
	}
	if !strings.HasPrefix(address, "/") {
		address = "/" + address
	}
	return "~" + path.Clean(address)
}
