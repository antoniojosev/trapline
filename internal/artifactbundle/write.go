package artifactbundle

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Write renders a bundle as the archive the upload surface reads back.
//
// It exists so this product can build one without sentry-cli — that is what
// `trapline artifacts upload` does — and so the tests can construct a bundle
// that is not a committed fixture. The two together are what make Read's
// contract checkable: a round trip through Write and Read is the only way to
// assert on a manifest that nobody recorded.
//
// The output is deterministic. Entry order is sorted, and every entry's
// modification time is left at the zero value the archive writer produces, so
// building the same directory twice gives the same bytes and therefore the
// same chunks and the same checksum. A bundle stamped with the clock would
// re-upload in full on every run of a pipeline that changed nothing.
func Write(bundle *Bundle) ([]byte, error) {
	document := manifest{
		Files:   make(map[string]manifestFile, len(bundle.Files)),
		DebugID: bundle.DebugID,
		Org:     bundle.Org,
		Project: bundle.Project,
		Release: bundle.Release,
		Dist:    bundle.Dist,
	}

	files := append([]File(nil), bundle.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	for index := range files {
		file := &files[index]
		headers := map[string]string{}
		if file.DebugID != "" {
			headers["debug-id"] = file.DebugID
		}
		if file.SourceMap != "" {
			headers["sourcemap"] = file.SourceMap
		}
		document.Files[file.Path] = manifestFile{
			Type:    string(file.Kind),
			URL:     file.URL,
			Headers: headers,
		}
	}

	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encoding the manifest: %w", err)
	}

	var out bytes.Buffer
	archive := zip.NewWriter(&out)
	for index := range files {
		if err := writeEntry(archive, files[index].Path, files[index].Content); err != nil {
			return nil, err
		}
	}
	if err := writeEntry(archive, ManifestName, encoded); err != nil {
		return nil, err
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("closing the archive: %w", err)
	}
	return out.Bytes(), nil
}

func writeEntry(archive *zip.Writer, name string, content []byte) error {
	entry, err := archive.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	if err != nil {
		return fmt.Errorf("creating %q in the archive: %w", name, err)
	}
	if _, err := entry.Write(content); err != nil {
		return fmt.Errorf("writing %q into the archive: %w", name, err)
	}
	return nil
}

// EntryPath is where a file with this name lives inside a bundle.
//
// The two `_` segments are what every recorded bundle uses, release and dist
// or not, so this product writes what it reads rather than inventing a layout
// of its own.
func EntryPath(name string) string { return filePrefix + "_/_/" + path.Base(name) }

// ArtifactURL is the `~/name` form a file is addressed by when there is no
// debug id to find it with.
func ArtifactURL(name string) string { return "~/" + strings.TrimPrefix(path.Base(name), "/") }

// DeriveDebugID turns a sha1 sum into a debug id, the way sentry-cli does.
//
// Measured on the recorded bundle: the id is the first sixteen bytes of the sha1 of
// the *uninjected* source map, with the UUID version nibble forced to 5 and
// the variant bits forced to RFC 4122. That is what makes the whole recording
// reproducible byte for byte, and it is why this product can derive an id for
// a file a bundler never injected one into: two uploads of identical bytes
// produce one id rather than two, so re-uploading an unchanged build does not
// orphan the artefacts an event already refers to.
func DeriveDebugID(sum []byte) string {
	if len(sum) < 16 {
		return ""
	}
	var id [16]byte
	copy(id[:], sum[:16])
	id[6] = (id[6] & 0x0f) | 0x50
	id[8] = (id[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(id[:])
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

// DebugIDInSourceMap reads the `debug_id` a bundler or `sourcemaps inject`
// wrote into a source map.
//
// Underscore here and hyphen in the manifest: the same value travels under two
// spellings and this is the one that lives inside the map itself (from the recording).
func DebugIDInSourceMap(content []byte) string {
	var document struct {
		DebugID string `json:"debug_id"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		return ""
	}
	return document.DebugID
}

// DebugIDInScript reads the `//# debugId=<uuid>` comment injection leaves at
// the end of a script.
//
// Scanned from the end because that is where it is written and because a
// minified bundle is one very long line: searching forwards would walk the
// whole file to find something that is always in its last few bytes.
func DebugIDInScript(content []byte) string {
	const marker = "//# debugId="
	index := bytes.LastIndex(content, []byte(marker))
	if index < 0 {
		return ""
	}
	rest := content[index+len(marker):]
	if end := bytes.IndexAny(rest, "\r\n"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(string(rest))
}
