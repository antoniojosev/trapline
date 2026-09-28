// Package artifactbundle reads and writes the archive `sentry-cli sourcemaps
// upload` sends: a ZIP with a `manifest.json` at its root and the files it
// describes under `files/_/_/`.
//
// It is a pure package — standard library only, no clock, no store, no
// transport — for the same reason the envelope parser is (ADR 002): these are
// bytes chosen by whoever holds an upload token, arriving as a compressed
// archive, and a reader of attacker-supplied archives is exactly the surface
// that should be exhaustively testable and fuzzable in isolation.
//
// Everything here was measured, not read: the four recorded flows under
// compat/sentry-cli/fixtures/ carry the manifest of a real upload, the same
// archive four times over. Three
// details from that recording are load-bearing and would not have been guessed:
//
//   - The type names are `minified_source` and `source_map`. ADR 018 wrote
//     them as `source` and `sourcemap`, which is a rename the schema would
//     have had to live with (from the recording).
//   - The script and its map carry the *same* `debug-id` header, so a lookup
//     by debug id returns two rows and the caller has to pick the map. The
//     bundle also has a `debug_id` of its own, which is the identity of the
//     archive and of nothing inside it (from the recording).
//   - The `sourcemap` header on a script names the map by *file name*, not by
//     the `~/…` url that everything else is addressed by.
package artifactbundle

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// ManifestName is the entry every bundle is read from.
const ManifestName = "manifest.json"

// filePrefix is where the tool puts the files themselves. The two `_` segments
// are placeholders the uploader fills in when it knows a dist and a release,
// and it does not: every recorded bundle uses `_` for both, release and dist
// or not. They are not parsed here — the manifest is the authority on what a
// file is, and deriving anything from the path would be reading the same fact
// from the less reliable of two places.
const filePrefix = "files/"

// Kind is what a file inside the bundle is, in the manifest's own vocabulary.
type Kind string

const (
	// KindMinifiedSource is the shipped script.
	KindMinifiedSource Kind = "minified_source"
	// KindSourceMap is the map that resolves it.
	KindSourceMap Kind = "source_map"
)

// Known reports whether a kind is one this product stores. Anything else is
// carried through the reader and dropped by the caller, rather than failing
// the whole upload: the manifest's vocabulary belongs to somebody else's tool
// and gains members between its releases (ADR 013).
func (k Kind) Known() bool { return k == KindMinifiedSource || k == KindSourceMap }

// File is one entry of a bundle: what the manifest says about it, and its
// bytes.
type File struct {
	// Path is the entry's name inside the archive, e.g.
	// "files/_/_/bundle.min.js".
	Path string
	// Kind is the manifest's `type`.
	Kind Kind
	// URL is the manifest's `url`, in the `~/path` form the legacy lookup
	// addresses artefacts by. It is present even when there is no release.
	URL string
	// DebugID is the `debug-id` header. A script and its map share one.
	//
	// Note the spelling: the header inside the manifest is `debug-id` with a
	// hyphen, while the same value inside the source map itself is the
	// top-level key `debug_id` with an underscore. They are not the same name
	// and confusing them is how a reader ends up finding neither.
	DebugID string
	// SourceMap is the `sourcemap` header of a script: the *file name* of its
	// map, not its url.
	SourceMap string
	// Content is the file's bytes, as stored.
	Content []byte
}

// Bundle is a read archive.
type Bundle struct {
	// DebugID is the bundle's own identity, from the manifest's top level. It
	// is not the debug id of any file in it.
	DebugID string
	// Org is ignored by this product (ADR 013) and kept only so a reader can
	// see what the uploader thought it was talking to.
	Org string
	// Project is what the manifest says. The `projects` field of the assemble
	// call is the authority; this is the second opinion.
	Project string
	// Release and Dist are present only when the upload named them.
	Release string
	Dist    string
	// Files are the entries, sorted by path so a bundle read twice produces
	// the same order.
	Files []File
}

// ErrNotABundle means the bytes are not a readable artifact bundle.
//
// One sentinel for every way of not being one, because the caller does exactly
// one thing with all of them: tell the client the archive could not be
// processed. The wrapped message says which way it was.
var ErrNotABundle = errors.New("not an artifact bundle")

// Limits bound what a reader will accept out of an archive somebody uploaded.
//
// A ZIP is a format with a compression ratio, which means a small upload can
// name a very large expansion. Every limit here exists because the alternative
// is letting the sender choose how much memory this server allocates.
type Limits struct {
	// MaxFiles is how many entries may be read.
	MaxFiles int
	// MaxFileBytes bounds one decompressed entry.
	MaxFileBytes int64
	// MaxTotalBytes bounds the decompressed archive.
	MaxTotalBytes int64
}

// DefaultLimits are what the server reads an upload with.
//
// Sized against what a real front end ships: a source map for a large single
// page application is a few megabytes, a bundle carries a handful of them, and
// nothing legitimate approaches the totals here. They are a ceiling on damage,
// not a budget anyone should be planning against — the budget is the project's
// (domain.ProjectConfig.ArtifactsMaxMB).
var DefaultLimits = Limits{
	MaxFiles:      2000,
	MaxFileBytes:  64 << 20,
	MaxTotalBytes: 256 << 20,
}

// manifest is the document at the root of the archive.
//
// Unknown fields are ignored, which is the rule on everything that comes from
// this tool (ADR 013): the manifest gained `dist` and `release` between two
// recorded flows of the *same* version, and a reader that rejected the first
// field it did not recognise would break on an upgrade nobody here took part
// in.
type manifest struct {
	Files   map[string]manifestFile `json:"files"`
	DebugID string                  `json:"debug_id"`
	Org     string                  `json:"org"`
	Project string                  `json:"project"`
	Release string                  `json:"release"`
	Dist    string                  `json:"dist"`
}

type manifestFile struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// Read parses an artifact bundle.
//
// The manifest is the authority: an entry in the archive that the manifest
// does not describe is not a file of this bundle and is skipped, because the
// only thing that could be done with it is to guess what it was.
func Read(data []byte, limits Limits) (*Bundle, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotABundle, err)
	}

	entries := map[string]*zip.File{}
	for _, entry := range reader.File {
		entries[entry.Name] = entry
	}

	raw, found := entries[ManifestName]
	if !found {
		return nil, fmt.Errorf("%w: no %s at the root of the archive", ErrNotABundle, ManifestName)
	}
	document, err := readEntry(raw, limits.MaxFileBytes)
	if err != nil {
		return nil, err
	}

	var parsed manifest
	if err := json.Unmarshal(document, &parsed); err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrNotABundle, ManifestName, err)
	}
	if len(parsed.Files) == 0 {
		return nil, fmt.Errorf("%w: the manifest describes no files", ErrNotABundle)
	}
	if len(parsed.Files) > limits.MaxFiles {
		return nil, fmt.Errorf("%w: the manifest describes %d files, and the limit is %d",
			ErrNotABundle, len(parsed.Files), limits.MaxFiles)
	}

	bundle := &Bundle{
		DebugID: parsed.DebugID,
		Org:     parsed.Org,
		Project: parsed.Project,
		Release: parsed.Release,
		Dist:    parsed.Dist,
		Files:   make([]File, 0, len(parsed.Files)),
	}

	var total int64
	for name, described := range parsed.Files {
		entry, present := entries[name]
		if !present {
			return nil, fmt.Errorf("%w: the manifest names %q, which is not in the archive",
				ErrNotABundle, name)
		}
		content, err := readEntry(entry, limits.MaxFileBytes)
		if err != nil {
			return nil, err
		}
		total += int64(len(content))
		if total > limits.MaxTotalBytes {
			return nil, fmt.Errorf("%w: the archive expands past %d bytes",
				ErrNotABundle, limits.MaxTotalBytes)
		}
		bundle.Files = append(bundle.Files, File{
			Path:      name,
			Kind:      Kind(described.Type),
			URL:       described.URL,
			DebugID:   described.Headers["debug-id"],
			SourceMap: described.Headers["sourcemap"],
			Content:   content,
		})
	}

	// Sorted so two reads of one archive agree on order. A map iteration is
	// the only source of nondeterminism in this function, and leaving it in
	// would make every golden test flaky in a way that only shows up
	// occasionally.
	sort.Slice(bundle.Files, func(i, j int) bool { return bundle.Files[i].Path < bundle.Files[j].Path })
	return bundle, nil
}

// readEntry decompresses one archive entry under a ceiling.
//
// The ceiling is enforced on the *decompressed* stream rather than trusting
// the entry's declared uncompressed size, which is a number the sender writes
// and can lie about. Reading one byte past the limit is what tells the two
// apart.
func readEntry(entry *zip.File, limit int64) ([]byte, error) {
	if strings.Contains(entry.Name, "..") || path.IsAbs(entry.Name) {
		// Nothing here writes an entry to disk, so this is not a traversal
		// fix; it is a refusal to store a name under which nothing
		// legitimate is ever uploaded, before some later caller does write
		// one out.
		return nil, fmt.Errorf("%w: %q is not a name a bundle entry may have", ErrNotABundle, entry.Name)
	}
	opened, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: opening %q: %w", ErrNotABundle, entry.Name, err)
	}
	defer func() { _ = opened.Close() }()

	content, err := io.ReadAll(io.LimitReader(opened, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %q: %w", ErrNotABundle, entry.Name, err)
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("%w: %q is larger than the %d byte limit", ErrNotABundle, entry.Name, limit)
	}
	return content, nil
}

// SourceMapFor returns the map that resolves a script, following the script's
// `sourcemap` header.
//
// The header names a file, not a url, so the match is against the base name of
// the other entries' urls. Returns nil when the bundle does not carry it,
// which is a real case: a bundle can be uploaded without its maps.
func (b *Bundle) SourceMapFor(script *File) *File {
	if script == nil || script.SourceMap == "" {
		return nil
	}
	want := path.Base(script.SourceMap)
	for index := range b.Files {
		candidate := &b.Files[index]
		if candidate.Kind != KindSourceMap {
			continue
		}
		if path.Base(candidate.URL) == want || path.Base(candidate.Path) == want {
			return candidate
		}
	}
	return nil
}
