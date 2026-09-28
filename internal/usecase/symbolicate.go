package usecase

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
	"github.com/antoniojosev/trapline/internal/sentry"
	"github.com/antoniojosev/trapline/internal/sourcemap"
)

// contextLines is how many lines of original source are kept on either side of
// a resolved frame. Five is what the official SDKs send for a language they can
// read, and matching it means a symbolicated JavaScript frame renders in the
// panel exactly like a Python one instead of needing its own case.
const contextLines = 5

// artifactCheckTTL is how long the answer to "does this project have any
// artifacts at all" is believed.
//
// Ten seconds, the same window the rate limiter caches configuration for, and
// for the same reason: it is short enough that a first upload starts resolving
// frames while somebody is still watching their terminal, and long enough that
// an installation which never uploads a source map pays one query every ten
// seconds instead of one per event (ADR 005).
const artifactCheckTTL = 10 * time.Second

// Symbolicator turns minified stack frames back into the source they came
// from, on the ingest path.
//
// It resolves by two routes and in this order (ADR 018): the debug id an
// event's `debug_meta` names, and — only when that finds nothing — the legacy
// pairing of release, dist and the frame's URL. The order is not a preference,
// it is what the tooling does: `sentry-cli sourcemaps upload` with no arguments
// never mentions a release, so for a modern pipeline the debug id is the only
// thing tying an event to a file (from the recording).
//
// It is deliberately not on the read path. Resolving when somebody opens an
// issue would mean the same frames are resolved again on every view, that a
// deleted artifact silently un-resolves an issue somebody already read, and —
// worst — that grouping saw the minified frame while the panel shows the
// original, so two issues that look identical would be two issues forever.
type Symbolicator struct {
	artifacts ports.ArtifactRepository
	releases  ports.ReleaseIDLookup
	cache     *sourcemap.Cache
	clock     ports.Clock
	logger    *slog.Logger

	// mu guards the memo of which projects have artifacts. It is a mutex and
	// a map rather than anything cleverer because it is read once per event
	// and written once per ten seconds per project.
	mu     sync.Mutex
	checks map[int64]artifactCheck
}

type artifactCheck struct {
	has bool
	at  time.Time
}

// NewSymbolicator wires the use case.
func NewSymbolicator(
	artifacts ports.ArtifactRepository,
	releases ports.ReleaseIDLookup,
	cache *sourcemap.Cache,
	clock ports.Clock,
) *Symbolicator {
	if cache == nil {
		cache = sourcemap.NewCache(sourcemap.DefaultCacheBytes)
	}
	return &Symbolicator{
		artifacts: artifacts,
		releases:  releases,
		cache:     cache,
		clock:     clock,
		checks:    map[int64]artifactCheck{},
	}
}

// WithLogger sends the resolution failures somewhere. Without one they go to
// slog's default, which is where every other use case sends them.
func (s *Symbolicator) WithLogger(logger *slog.Logger) *Symbolicator {
	if logger != nil {
		s.logger = logger
	}
	return s
}

// Apply rewrites the event's frames in place and reports how many it resolved.
//
// Nil-safe on the receiver, like the broadcaster: an assembly without
// symbolication ingests exactly as every build without source maps did.
//
// It never returns an error. A source map that cannot be found, read or parsed
// costs symbolication and nothing else — the event is still an error report,
// and turning a bad build artefact into a dropped event would mean one wrong
// upload silently stops error tracking for a whole front end.
func (s *Symbolicator) Apply(ctx context.Context, projectID int64, event *sentry.Event) int {
	if s == nil || event == nil {
		return 0
	}
	stacktraces := stacktracesOf(event)
	if len(stacktraces) == 0 {
		return 0
	}

	images := imagesByCodeFile(event.DebugMeta)
	if len(images) == 0 && event.Release == "" {
		// Neither route has a key. Not a failure and not worth a query.
		return 0
	}
	if !s.projectHasArtifacts(ctx, projectID) {
		return 0
	}

	resolver := &frameResolver{
		symbolicator: s,
		ctx:          ctx,
		projectID:    projectID,
		images:       images,
		release:      event.Release,
		dist:         event.Dist,
		maps:         map[string]*resolvedMap{},
	}

	resolved := 0
	for _, stacktrace := range stacktraces {
		for index := range stacktrace.Frames {
			if resolver.resolve(&stacktrace.Frames[index]) {
				resolved++
			}
		}
	}
	return resolved
}

// projectHasArtifacts answers, from a short-lived memo, whether there is any
// point in looking.
func (s *Symbolicator) projectHasArtifacts(ctx context.Context, projectID int64) bool {
	now := s.clock.Now()

	s.mu.Lock()
	cached, found := s.checks[projectID]
	s.mu.Unlock()
	if found && now.Sub(cached.at) < artifactCheckTTL {
		return cached.has
	}

	has, err := s.artifacts.HasArtifacts(ctx, projectID)
	if err != nil {
		s.log().Warn("checking whether a project has build artifacts",
			"project_id", projectID, "error", err)
		return false
	}

	s.mu.Lock()
	s.checks[projectID] = artifactCheck{has: has, at: now}
	s.mu.Unlock()
	return has
}

func (s *Symbolicator) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// frameResolver carries the state of symbolicating one event: which maps have
// already been fetched, and which release the legacy route resolved to.
//
// Per event rather than per frame because a stacktrace is a dozen frames from
// two or three files, and the same map answers most of them. The process-wide
// cache would answer them too, but through a lock each time; this is the loop
// that runs per ingested event.
type frameResolver struct {
	symbolicator *Symbolicator
	ctx          context.Context
	projectID    int64
	images       map[string]string
	release      string
	dist         string

	// maps is memoised per code file, holding a nil map for "looked and found
	// nothing" so a second frame from the same file does not look again.
	maps map[string]*resolvedMap

	// releaseID is resolved at most once, and releaseLooked says whether the
	// zero means "no release" or "not asked yet".
	releaseID     int64
	releaseLooked bool
}

// resolvedMap is a parsed map plus what the resolver worked out about the file
// it explains, kept for the whole event.
//
// The URL work is here rather than beside the frame because it is expensive
// and repetitive: a minified stacktrace is a dozen frames from one bundle
// pointing into a handful of sources, and parsing the same two URLs twelve
// times over is most of what symbolicating a frame would otherwise cost.
// Parsed once per file, and each resolved source remembered.
type resolvedMap struct {
	parsed *sourcemap.Map
	// base is the code file as a URL, when it is one. Nil for a frame whose
	// path is not an absolute URL — a bundle loaded from disk, say — and then
	// a relative source stays relative, because there is nothing to resolve
	// it against.
	base *url.URL
	// absPaths memoises the resolution of each source this map names.
	absPaths map[string]string
}

// absPath resolves one of the map's sources against the code file's URL.
func (r *resolvedMap) absPath(source string) string {
	if cached, found := r.absPaths[source]; found {
		return cached
	}
	resolved := resolveAgainst(r.base, source)
	r.absPaths[source] = resolved
	return resolved
}

// resolve rewrites one frame, reporting whether it did.
func (r *frameResolver) resolve(frame *sentry.Frame) bool {
	// A frame with no line number cannot be resolved: the mapping table is
	// indexed by position, and a frame that carries none is a frame the SDK
	// could not locate either. The column may legitimately be missing — some
	// SDKs drop it — and column one is the honest reading of "somewhere on
	// this line".
	if frame.Lineno < 1 {
		return false
	}
	path := frame.Path()
	if path == "" {
		return false
	}

	resolved := r.mapFor(path)
	if resolved == nil {
		return false
	}
	column := frame.Colno
	if column < 1 {
		column = 1
	}
	position, ok := resolved.parsed.Lookup(frame.Lineno, column)
	if !ok {
		return false
	}

	applyPosition(frame, resolved, position)
	return true
}

// mapFor finds the source map that explains a generated file, by whichever
// route has a key for it.
func (r *frameResolver) mapFor(path string) *resolvedMap {
	if cached, memoised := r.maps[path]; memoised {
		return cached
	}

	parsed := r.byDebugID(path)
	if parsed == nil {
		parsed = r.byReleaseURL(path)
	}

	var resolved *resolvedMap
	if parsed != nil {
		resolved = &resolvedMap{
			parsed:   parsed,
			base:     absoluteURL(path),
			absPaths: make(map[string]string, len(parsed.Sources())),
		}
	}
	// Memoised even when nothing was found: a nil entry is what stops the
	// second frame from the same file repeating a lookup that missed.
	r.maps[path] = resolved
	return resolved
}

// byDebugID is route one: the id the SDK put in `debug_meta`.
func (r *frameResolver) byDebugID(path string) *sourcemap.Map {
	debugID, named := r.images[path]
	if !named {
		// The image's code_file is the URL the browser loaded, which can
		// carry a cache-busting query the frame does not — or the other way
		// round. Comparing the bare paths is what makes a build that appends
		// `?v=hash` resolvable at all.
		debugID, named = r.images[stripQuery(path)]
	}
	if !named || debugID == "" {
		return nil
	}

	key := "debug:" + strconv.FormatInt(r.projectID, 10) + ":" + debugID
	return r.symbolicator.load(key, func() (domain.Artifact, []byte, error) {
		return r.symbolicator.artifacts.ByDebugID(
			r.ctx, r.projectID, debugID, domain.ArtifactSourceMap)
	})
}

// byReleaseURL is route two: release, dist and the `~/path` form of the
// frame's URL.
//
// Two lookups, not one. The URL in a stack frame names the *script*, and what
// is wanted is the map beside it, which the manifest names in the script's
// `sourcemap` header — by file name, relative to the script's own URL, and not
// as a URL (from the recording). Guessing `name + ".map"` instead is right often enough
// to look like it works.
func (r *frameResolver) byReleaseURL(path string) *sourcemap.Map {
	releaseID := r.resolveRelease()
	if releaseID == 0 {
		return nil
	}

	candidates := urlCandidates(path)
	if len(candidates) == 0 {
		return nil
	}
	script, content, err := r.findByNames(releaseID, candidates)
	if err != nil {
		r.symbolicator.logLookup("finding the script of a frame", path, err)
		return nil
	}

	if script.Kind == domain.ArtifactSourceMap {
		// Somebody uploaded the map under the script's own URL. Unusual, but
		// harmless and unambiguous: it is already what was wanted.
		return r.symbolicator.load(artifactKey(script), func() (domain.Artifact, []byte, error) {
			return script, content, nil
		})
	}

	mapNames := mapCandidates(script.Name, script.SourceMapRef)
	key := "name:" + strconv.FormatInt(r.projectID, 10) + ":" +
		strconv.FormatInt(releaseID, 10) + ":" + r.dist + ":" + strings.Join(mapNames, "|")
	return r.symbolicator.load(key, func() (domain.Artifact, []byte, error) {
		return r.findByNames(releaseID, mapNames)
	})
}

// findByNames tries each spelling in order and returns the first that exists.
//
// The repository looks a file up by one normalised name, which is the right
// shape for a lookup: `domain.ArtifactURL` is what makes the uploader's
// `~/bundle.min.js` and an event's `https://host/static/bundle.min.js?v=8f3a`
// the same string, and putting that rule anywhere else would give the two
// sides their own idea of what the same file is. What it cannot know is that
// a pipeline may have flattened the path on the way up, so the list of
// spellings stays here, on the side that generated it from a stack frame.
func (r *frameResolver) findByNames(
	releaseID int64, names []string,
) (domain.Artifact, []byte, error) {
	for _, name := range names {
		artifact, content, err := r.symbolicator.artifacts.ByReleaseURL(
			r.ctx, r.projectID, releaseID, r.dist, name)
		if err == nil {
			return artifact, content, nil
		}
		if !errors.Is(err, domain.ErrArtifactNotFound) {
			return domain.Artifact{}, nil, err
		}
	}
	return domain.Artifact{}, nil, domain.ErrArtifactNotFound
}

// resolveRelease turns the event's release string into a release id, once.
func (r *frameResolver) resolveRelease() int64 {
	if r.releaseLooked {
		return r.releaseID
	}
	r.releaseLooked = true
	if r.release == "" || r.symbolicator.releases == nil {
		return 0
	}
	release, err := r.symbolicator.releases.Find(r.ctx, r.projectID, r.release)
	if err != nil {
		if !errors.Is(err, domain.ErrReleaseNotFound) {
			r.symbolicator.logLookup("finding the release of an event", r.release, err)
		}
		return 0
	}
	r.releaseID = release.ID
	return r.releaseID
}

// load returns a parsed map for a cache key, fetching and parsing it once.
//
// Misses are cached too. An event naming a debug id nobody uploaded is the
// ordinary case for any front end whose build pipeline does not upload maps,
// and without remembering the miss every one of its events would go to the
// database to be told the same thing again.
func (s *Symbolicator) load(
	key string, find func() (domain.Artifact, []byte, error),
) *sourcemap.Map {
	if cached, known := s.cache.Get(key); known {
		return cached
	}

	artifact, content, err := find()
	if err != nil {
		if !errors.Is(err, domain.ErrArtifactNotFound) {
			s.logLookup("finding a source map", key, err)
		}
		s.cache.Put(key, nil)
		return nil
	}

	parsed, err := sourcemap.Parse(content)
	if err != nil {
		// Worth a warning rather than a debug line: somebody uploaded a file
		// this build cannot read, and the only symptom they will otherwise
		// see is that symbolication silently does nothing.
		s.log().Warn("parsing an uploaded source map",
			"artifact_id", artifact.ID, "name", artifact.Name, "error", err)
		s.cache.Put(key, nil)
		return nil
	}
	s.cache.Put(key, parsed)
	return parsed
}

func (s *Symbolicator) logLookup(what, subject string, err error) {
	s.log().Debug(what, "subject", subject, "error", err)
}

// artifactKey names an artifact in the cache by its identity in the database,
// which is what a caller that already has the row should use.
func artifactKey(artifact domain.Artifact) string {
	return "artifact:" + strconv.FormatInt(artifact.ID, 10)
}

// applyPosition rewrites a frame to the original position, keeping what it
// replaced.
func applyPosition(frame *sentry.Frame, resolved *resolvedMap, position sourcemap.Position) {
	frame.Raw = &sentry.RawFrame{
		Filename: frame.Filename,
		AbsPath:  frame.AbsPath,
		Function: frame.Function,
		Module:   frame.Module,
		Lineno:   frame.Lineno,
		Colno:    frame.Colno,
	}

	frame.Filename = position.Source
	frame.AbsPath = resolved.absPath(position.Source)
	frame.Lineno = position.Line
	frame.Colno = position.Column
	if position.Name != "" {
		// Only when the mapping named something. A source map names the token
		// at the position, not the function containing it, so most segments
		// carry no name at all — and inventing one from a neighbouring
		// segment produces a frame that says `encoding` where the function is
		// called `decode`. Keeping the minified name is a smaller lie than a
		// confident wrong one; recovering the real one needs the minified
		// source scanned backwards for the enclosing function, which is left
		// as debt and is recorded as such.
		frame.Function = position.Name
	}

	if pre, line, post, ok := resolved.parsed.Context(position, contextLines); ok {
		frame.PreContext = pre
		frame.ContextLine = line
		frame.PostContext = post
	}
}

// stacktracesOf collects every stacktrace an event carries.
//
// Every one, not just the one grouping reads: a chained exception has a
// stacktrace per link, and resolving only the primary would leave the cause of
// the error — which is the interesting half of a chain — minified.
func stacktracesOf(event *sentry.Event) []*sentry.Stacktrace {
	stacktraces := make([]*sentry.Stacktrace, 0, len(event.Exceptions)+1)
	for index := range event.Exceptions {
		if trace := event.Exceptions[index].Stacktrace; trace != nil && len(trace.Frames) > 0 {
			stacktraces = append(stacktraces, trace)
		}
	}
	if event.Stacktrace != nil && len(event.Stacktrace.Frames) > 0 {
		stacktraces = append(stacktraces, event.Stacktrace)
	}
	return stacktraces
}

// imagesByCodeFile indexes the debug images by the file they describe.
func imagesByCodeFile(meta *sentry.DebugMeta) map[string]string {
	if meta == nil {
		return nil
	}
	images := make(map[string]string, len(meta.Images))
	for _, image := range meta.Images {
		// Only `sourcemap`. The other image types are for native platforms
		// and name debug files this product does not read; treating one as a
		// source map would send a lookup after an artifact that cannot exist.
		if image.Type != "sourcemap" || image.CodeFile == "" || image.DebugID == "" {
			continue
		}
		images[image.CodeFile] = image.DebugID
		if stripped := stripQuery(image.CodeFile); stripped != image.CodeFile {
			images[stripped] = image.DebugID
		}
	}
	return images
}

// urlCandidates are the spellings an artifact of a frame's URL may have been
// uploaded under, most specific first.
//
// Several because the name is chosen by whoever ran the upload. `sentry-cli`
// writes the `~/path` form, which means "any host"; a pipeline that passed the
// full URL by hand wrote that; and a monorepo that uploaded from a
// subdirectory wrote just the file name. Trying them in one query costs
// nothing over trying one.
func urlCandidates(path string) []string {
	bare := stripQuery(path)
	candidates := make([]string, 0, 5)
	add := func(candidate string) {
		if candidate == "" {
			return
		}
		for _, existing := range candidates {
			if existing == candidate {
				return
			}
		}
		candidates = append(candidates, candidate)
	}

	if parsed, err := url.Parse(bare); err == nil && parsed.Host != "" && parsed.Path != "" {
		add("~" + parsed.Path)
		add(parsed.Path)
	}
	add(bare)
	if slash := strings.LastIndexByte(bare, '/'); slash >= 0 && slash+1 < len(bare) {
		add("~/" + bare[slash+1:])
	}
	return candidates
}

// mapCandidates are the names the map beside a script may carry.
func mapCandidates(scriptName, reference string) []string {
	candidates := make([]string, 0, 3)
	if reference != "" {
		// The reference is a file name relative to the script's URL, so it is
		// resolved against the script's directory rather than used as-is.
		if slash := strings.LastIndexByte(scriptName, '/'); slash >= 0 {
			candidates = append(candidates, scriptName[:slash+1]+reference)
		} else {
			candidates = append(candidates, reference)
		}
		if !strings.Contains(reference, "/") {
			candidates = append(candidates, "~/"+reference)
		}
	}
	return append(candidates, scriptName+".map")
}

// absoluteURL parses a frame's path as a URL, or reports nil when it is not
// one. Parsed once per generated file and kept, because url.Parse allocates
// and this would otherwise run once per frame of every ingested event.
func absoluteURL(path string) *url.URL {
	parsed, err := url.Parse(path)
	if err != nil || parsed.Scheme == "" {
		return nil
	}
	return parsed
}

// resolveAgainst turns the map's source path into something absolute, when
// there is a base to resolve it against.
//
// The map says `../src/checkout.js`, which is relative to the map's own
// location, and the only location known here is the script's URL. Resolving
// gives a path somebody can compare against a repository; leaving it relative
// gives a string whose meaning depends on where the file was built. A source
// that is already absolute — a `webpack:///` scheme, an absolute path — is
// left alone, because it is already saying where it is.
func resolveAgainst(base *url.URL, source string) string {
	if source == "" || base == nil {
		return source
	}
	if strings.Contains(source, "://") || strings.HasPrefix(source, "/") {
		return source
	}
	reference, err := url.Parse(source)
	if err != nil {
		return source
	}
	return base.ResolveReference(reference).String()
}

// stripQuery removes a query string and fragment from a URL-shaped path.
func stripQuery(path string) string {
	if cut := strings.IndexAny(path, "?#"); cut >= 0 {
		return path[:cut]
	}
	return path
}
