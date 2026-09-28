package cli

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/artifactbundle"
)

// runArtifacts dispatches the source-map subcommands.
//
// They exist so that uploading source maps does not require installing
// somebody else's tool (ADR 006). The emulated /api/0/ surface is there for
// the pipelines that already run sentry-cli; this is for the ones that do not,
// and it builds the same archive and posts it to this product's own endpoint,
// so both paths converge on one reader and one set of rules.
func runArtifacts(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "artifacts: expected upload, list or delete")
		return ExitUsage
	}
	switch args[0] {
	case "upload":
		return runArtifactsUpload(c, args[1:])
	case "list":
		return runArtifactsList(c, args[1:])
	case "delete":
		return runArtifactsDelete(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "artifacts: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

type artifactPayload struct {
	ID            int64     `json:"id"`
	ReleaseID     *int64    `json:"release_id"`
	Dist          string    `json:"dist"`
	DebugID       string    `json:"debug_id"`
	BundleDebugID string    `json:"bundle_debug_id"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	SourceMapRef  string    `json:"sourcemap_ref"`
	SHA256        string    `json:"sha256"`
	Size          int64     `json:"size"`
	CreatedAt     time.Time `json:"created_at"`
}

type artifactListPayload struct {
	Artifacts []artifactPayload `json:"artifacts"`
	Used      int64             `json:"used_bytes"`
	Budget    int64             `json:"budget_bytes"`
}

func artifactsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/artifacts"
}

// runArtifactsUpload builds a bundle from a directory and sends it.
//
// It does NOT inject debug ids. Injection rewrites the files a build produced
// — it appends a `//# debugId=` comment to every script and shifts its map to
// match — and a command called "upload" must not modify the thing it is
// uploading. What it does instead is read the ids a bundler plugin or
// `sentry-cli sourcemaps inject` already wrote, and honour them.
//
// So a build with debug ids uploads with them, and needs no release: the debug
// id is the whole join (ADR 018). A build without them needs -release, because
// then there is nothing else to find the map by, and the command says so
// rather than uploading files nothing will ever look up.
func runArtifactsUpload(c *context_, args []string) int {
	flags := newFlagSet(c, "artifacts upload")
	projectID := flags.Int64("project", 0, "project id (required)")
	release := flags.String("release", "", "release version these files belong to, e.g. myapp@1.4.0")
	dist := flags.String("dist", "", "which build of that release")
	prefix := flags.String("url-prefix", "~/",
		"the url these files are served under; artefacts are stored as <prefix><file>")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "artifacts upload: -project is required and must be positive")
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(c.stderr, "artifacts upload: expected exactly one directory to upload")
		return ExitUsage
	}

	bundle, err := collectBundle(flags.Arg(0), *prefix, *release, *dist)
	if err != nil {
		return c.fail(err)
	}
	if len(bundle.Files) == 0 {
		return c.fail(fmt.Errorf("no scripts or source maps under %s", flags.Arg(0)))
	}
	if *release == "" && !anyDebugID(bundle) {
		// Refused rather than uploaded. Without a debug id and without a
		// release there is no key: the files would be stored, the command
		// would report success, and nothing would ever resolve a frame with
		// them. Saying so here costs one message; finding out costs somebody
		// an afternoon staring at a minified stack trace (ADR 018).
		return c.fail(fmt.Errorf(
			"none of these files carries a debug id and no -release was given, so nothing " +
				"would ever find them again: pass -release, or inject debug ids at build time"))
	}

	encoded, err := artifactbundle.Write(bundle)
	if err != nil {
		return c.fail(err)
	}

	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	path := artifactsPath(*projectID)
	query := url.Values{}
	if *release != "" {
		query.Set("release", *release)
	}
	if *dist != "" {
		query.Set("dist", *dist)
	}
	if encodedQuery := query.Encode(); encodedQuery != "" {
		path += "?" + encodedQuery
	}

	var stored artifactListPayload
	if err := client.upload(c.ctx, path, "application/zip", encoded, &stored); err != nil {
		return c.fail(err)
	}

	var text strings.Builder
	for index := range stored.Artifacts {
		artifact := &stored.Artifacts[index]
		fmt.Fprintf(&text, "%s\t%s\t%s\n", artifact.Name, artifact.Kind, shortDebugID(artifact.DebugID))
	}
	fmt.Fprintf(&text, "%d files, %s of %s used",
		len(stored.Artifacts), megabytes(stored.Used), megabytes(stored.Budget))
	return c.emit(*remote.asJSON, stored, text.String())
}

// collectBundle reads a directory into the archive the server expects.
//
// Scripts and maps are paired by the map's file name, taken from the script's
// `sourceMappingURL` comment when it has one and from `<script>.map` when it
// does not — which is what every bundler produces. The pairing matters twice
// over: it is what puts the `sourcemap` header on the script, and it is what
// lets a map with no debug id of its own inherit the one its script carries.
func collectBundle(directory, prefix, release, dist string) (*artifactbundle.Bundle, error) {
	info, err := os.Stat(directory)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", directory, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", directory)
	}

	type candidate struct {
		name    string
		content []byte
	}
	var scripts, maps []candidate

	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking %s: %w", directory, err)
		}
		if entry.IsDir() {
			return nil
		}
		lower := strings.ToLower(entry.Name())
		isMap := strings.HasSuffix(lower, ".map")
		isScript := strings.HasSuffix(lower, ".js") || strings.HasSuffix(lower, ".mjs") ||
			strings.HasSuffix(lower, ".cjs")
		if !isMap && !isScript {
			return nil
		}
		content, err := os.ReadFile(path) //nolint:gosec // the path comes from the operator's own directory walk.
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			relative = entry.Name()
		}
		relative = filepath.ToSlash(relative)
		if isMap {
			maps = append(maps, candidate{name: relative, content: content})
		} else {
			scripts = append(scripts, candidate{name: relative, content: content})
		}
		return nil
	})
	if err != nil {
		// Every error the walk can produce was already wrapped by the
		// callback, which names the file it was reading; wrapping again here
		// would only prefix the same sentence twice.
		return nil, err //nolint:wrapcheck // wrapped at the point it happened, inside the callback.
	}

	// Sorted so two runs over one directory produce the same archive, and
	// therefore the same chunks: a pipeline that changed nothing should not
	// re-upload everything.
	sort.Slice(scripts, func(i, j int) bool { return scripts[i].name < scripts[j].name })
	sort.Slice(maps, func(i, j int) bool { return maps[i].name < maps[j].name })

	debugIDByMap := map[string]string{}
	mapNames := map[string]bool{}
	for _, entry := range maps {
		mapNames[entry.name] = true
		if id := artifactbundle.DebugIDInSourceMap(entry.content); id != "" {
			debugIDByMap[entry.name] = id
		}
	}

	bundle := &artifactbundle.Bundle{Release: release, Dist: dist}
	for _, entry := range scripts {
		debugID := artifactbundle.DebugIDInScript(entry.content)
		reference := sourceMapNameFor(entry.name, entry.content, mapNames)
		if debugID == "" && reference != "" {
			// The map is where `sourcemaps inject` writes the id in its
			// underscore spelling, and some bundler plugins write only there.
			debugID = debugIDByMap[reference]
		}
		if debugID != "" && reference != "" {
			// And back the other way: a script's id belongs to its map too,
			// because that is what the manifest says and what makes one
			// lookup return both (from the recording).
			debugIDByMap[reference] = debugID
		}
		// Only when there is one. filepath.Base("") is ".", and a manifest
		// carrying `"sourcemap": "."` would send the server looking for a
		// file by that name — a header that is worse than no header, because
		// it turns "this script has no map here" into "this script's map is
		// missing".
		header := ""
		if reference != "" {
			header = filepath.Base(reference)
		}
		bundle.Files = append(bundle.Files, artifactbundle.File{
			Path:      artifactbundle.EntryPath(entry.name),
			Kind:      artifactbundle.KindMinifiedSource,
			URL:       prefix + entry.name,
			DebugID:   debugID,
			SourceMap: header,
			Content:   entry.content,
		})
	}
	for _, entry := range maps {
		bundle.Files = append(bundle.Files, artifactbundle.File{
			Path:    artifactbundle.EntryPath(entry.name),
			Kind:    artifactbundle.KindSourceMap,
			URL:     prefix + entry.name,
			DebugID: debugIDByMap[entry.name],
			Content: entry.content,
		})
	}
	return bundle, nil
}

// sourceMapNameFor finds the map that belongs to a script.
//
// The `sourceMappingURL` comment first, because it is what the script itself
// says; the conventional `<script>.map` second, because a build that strips
// the comment still ships the map beside it. A data: URI is ignored — the map
// is already inside the script and there is nothing separate to upload.
func sourceMapNameFor(scriptName string, content []byte, known map[string]bool) string {
	directory := filepath.ToSlash(filepath.Dir(scriptName))
	if directory == "." {
		directory = ""
	} else {
		directory += "/"
	}

	const marker = "//# sourceMappingURL="
	if index := strings.LastIndex(string(content), marker); index >= 0 {
		rest := string(content[index+len(marker):])
		if end := strings.IndexAny(rest, "\r\n"); end >= 0 {
			rest = rest[:end]
		}
		rest = strings.TrimSpace(rest)
		if rest != "" && !strings.HasPrefix(rest, "data:") {
			if candidate := directory + rest; known[candidate] {
				return candidate
			}
			if known[rest] {
				return rest
			}
		}
	}
	if candidate := scriptName + ".map"; known[candidate] {
		return candidate
	}
	return ""
}

func anyDebugID(bundle *artifactbundle.Bundle) bool {
	for index := range bundle.Files {
		if bundle.Files[index].DebugID != "" {
			return true
		}
	}
	return false
}

func runArtifactsList(c *context_, args []string) int {
	flags := newFlagSet(c, "artifacts list")
	projectID := flags.Int64("project", 0, "project id (required)")
	release := flags.String("release", "", "only the artefacts of this release")
	dist := flags.String("dist", "", "only this build of it")
	debugID := flags.String("debug-id", "", "only the script and map of this build")
	limit := flags.Int("limit", 0, "how many to return")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 {
		fmt.Fprintln(c.stderr, "artifacts list: -project is required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	query := url.Values{}
	for name, value := range map[string]string{
		"release": *release, "dist": *dist, "debug_id": *debugID,
	} {
		if value != "" {
			query.Set(name, value)
		}
	}
	if *limit > 0 {
		query.Set("limit", strconv.Itoa(*limit))
	}
	path := artifactsPath(*projectID)
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var page artifactListPayload
	if err := client.do(c.ctx, http.MethodGet, path, nil, &page); err != nil {
		return c.fail(err)
	}
	if len(page.Artifacts) == 0 {
		return c.emit(*remote.asJSON, page, "no artifacts")
	}

	var text strings.Builder
	for index := range page.Artifacts {
		artifact := &page.Artifacts[index]
		fmt.Fprintf(&text, "%d\t%s\t%s\t%s\t%s\n", artifact.ID, artifact.Name, artifact.Kind,
			shortDebugID(artifact.DebugID), megabytes(artifact.Size))
	}
	fmt.Fprintf(&text, "%s of %s used", megabytes(page.Used), megabytes(page.Budget))
	return c.emit(*remote.asJSON, page, text.String())
}

func runArtifactsDelete(c *context_, args []string) int {
	flags := newFlagSet(c, "artifacts delete")
	projectID := flags.Int64("project", 0, "project id (required)")
	artifactID := flags.Int64("id", 0, "artifact id (required)")
	remote := addRemoteFlags(flags)
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *projectID <= 0 || *artifactID <= 0 {
		fmt.Fprintln(c.stderr, "artifacts delete: -project and -id are required and must be positive")
		return ExitUsage
	}
	client, err := remote.client(c)
	if err != nil {
		return c.fail(err)
	}

	path := artifactsPath(*projectID) + "/" + strconv.FormatInt(*artifactID, 10)
	if err := client.do(c.ctx, http.MethodDelete, path, nil, nil); err != nil {
		return c.fail(err)
	}
	return c.emit(*remote.asJSON,
		map[string]any{"deleted": *artifactID}, "deleted")
}

// shortDebugID keeps a listing readable. The whole uuid is in --json; a person
// scanning a column needs enough to tell two builds apart and no more.
func shortDebugID(debugID string) string {
	if debugID == "" {
		return "-"
	}
	if len(debugID) > 8 {
		return debugID[:8]
	}
	return debugID
}

// megabytes renders a size the way the budget is expressed, so "used" and
// "budget" can be compared by eye.
func megabytes(bytes int64) string {
	const mb = 1024 * 1024
	if bytes < mb {
		return strconv.FormatInt(bytes, 10) + " B"
	}
	return strconv.FormatFloat(float64(bytes)/mb, 'f', 1, 64) + " MB"
}
