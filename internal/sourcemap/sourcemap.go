// Package sourcemap reads Source Map v3 documents and answers the one question
// symbolication asks of them: given a position in generated code, where did it
// come from.
//
// It is a pure package for the same reason internal/envelope is one, and the
// reason is not tidiness. These bytes are a file a user uploaded — a build
// artefact produced by a bundler this product does not control, on a machine
// it does not own, and posted to an authenticated endpoint by whoever holds a
// token. A parser for that has to be fuzzable in isolation, so it depends on
// nothing but the standard library and reaches no clock, no store and no
// network (ADR 018, and the same argument as ADR 002).
//
// What is supported is version 3 and nothing else: the `sections` form of an
// index map is flattened at parse time, and every unknown key — `x_facebook_*`
// and the rest of the vendor namespace included — is ignored rather than
// guessed at. Ignoring is a decision, not an omission: `x_facebook_offsets`
// changes what a line number means in Metro's world, and a parser that read
// the key without implementing its semantics would resolve every frame of a
// React Native bundle to a confidently wrong line.
package sourcemap

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// The failure modes a caller can tell apart. Everything else is wrapped in
// ErrInvalid, because a caller cannot act on the difference between a
// truncated VLQ and a bad JSON escape: both mean "this file is not usable".
var (
	// ErrInvalid means the bytes are not a usable source map.
	ErrInvalid = errors.New("invalid source map")
	// ErrUnsupportedVersion means the document announced a version this
	// parser does not implement. Kept apart from ErrInvalid because it is the
	// one failure worth telling an operator about verbatim: the file is
	// fine, this build just does not read it.
	ErrUnsupportedVersion = errors.New("unsupported source map version")
	// ErrTooLarge means the document is past one of the ceilings below.
	ErrTooLarge = errors.New("source map too large")
)

// The ceilings. Every one of them exists because this parser turns a
// user-supplied file into memory, and the ratio between the two is
// attacker-chosen: a few kilobytes of `;` produce a million empty lines, and a
// few hundred kilobytes of segments produce tens of megabytes of structs.
// A cache with a byte budget (Cache) is not enough on its own — the budget is
// checked after a map is parsed, so a single document has to be unable to
// exhaust memory before it is ever offered to the cache.
const (
	// MaxDocumentBytes is the largest document Parse will look at.
	MaxDocumentBytes = 64 << 20
	// maxSegments bounds the mapping table. Two million segments is about 48
	// MB of mappings, already larger than the default cache budget, and far
	// past any real bundle: a 10 MB minified application maps in the low
	// hundreds of thousands.
	maxSegments = 2 << 20
	// maxSections bounds an index map's section list.
	maxSections = 4096
	// maxSectionDepth stops a hand-written index map that nests into itself.
	// The specification forbids nesting outright; this parser tolerates a
	// little of it rather than rejecting, and refuses to recurse forever.
	maxSectionDepth = 3
	// maxGeneratedLine bounds how far a `;` run can push the line counter.
	// Sixteen million lines is past anything a bundler emits and far short of
	// what an int32 would wrap at, which is the failure this prevents: a
	// wrapped line number sorts before everything and makes the binary search
	// answer with a segment from the wrong end of the file.
	maxGeneratedLine = 1 << 24
	// maxTableEntries bounds the sources and names tables, which sections keep
	// appending to. It is also what keeps the index arithmetic inside an
	// int32, which is what the mapping table stores.
	maxTableEntries = 1 << 20
)

// Position is one place in an original source, as a source map names it.
//
// Line and Column are one-based, because that is what a stack frame carries
// and what a human reads. Inside the mapping table they are zero-based, as the
// format stores them; the conversion happens at this boundary exactly once, so
// no caller has to remember which convention it is holding.
type Position struct {
	// Source is the path the map gives, with sourceRoot already applied. It
	// is whatever the bundler wrote — `../src/checkout.js`,
	// `webpack:///./src/checkout.js`, an absolute URL — and is deliberately
	// not normalised here: what it means depends on the bundler, and this
	// package does not know which one produced the file.
	Source string
	// Name is the identifier the mapping named, when it named one. Most
	// segments do not.
	Name string
	// Line and Column are one-based.
	Line   int
	Column int

	sourceIndex int32
}

// Map is a parsed source map: the mapping table, the sources it points into,
// and their contents when the bundler embedded them.
type Map struct {
	// File is the `file` field: the generated file this map describes.
	File string
	// DebugID is the identifier `sentry-cli sourcemaps inject` writes into
	// the document, which is what ties this map to the frames of a bundle
	// that carries the same id (ADR 018). Empty for a map nobody injected.
	DebugID string

	sources []string
	// contents[i] is sources[i]'s text, or "" when the bundler did not embed
	// it. hasContent says which of the two an empty string means, because a
	// genuinely empty source file and an absent one produce different
	// answers: one has no context line, the other has none *yet* and might
	// have one if the file is uploaded later.
	hasContent []bool
	contents   []string
	// lineStarts[i] holds the byte offset of every line of contents[i], so a
	// context line is a slice and not a scan. Built once at parse time rather
	// than lazily: a lazy index needs a lock on the read path, and the read
	// path is per stack frame of every ingested event.
	lineStarts [][]int32

	names    []string
	mappings []mapping

	// size is the approximate resident cost of this map, in bytes. It is
	// what the cache charges against its budget, and it is computed here
	// because this is the only place that knows what was allocated.
	size int64
}

// mapping is one segment of the table: a position in the generated file and
// the position in an original source it came from.
//
// Fixed-width and flat on purpose. This is the single largest allocation a
// parsed map makes — one of these per segment, hundreds of thousands for a
// real bundle — so it is a value in a slice rather than anything with a
// pointer in it, and the whole table is one contiguous block the binary search
// walks without chasing anything.
type mapping struct {
	genLine int32
	genCol  int32
	// srcIndex is -1 for a segment that only marks generated code with no
	// original. Such segments exist and are meaningful: they are how a
	// bundler says "everything from here on came from nowhere", and treating
	// them as a match would resolve a frame to whatever the previous real
	// segment pointed at.
	srcIndex int32
	srcLine  int32
	srcCol   int32
	// nameIndex is -1 when the segment named no identifier.
	nameIndex int32
}

// Size is the approximate number of bytes this map holds.
//
// Approximate is honest: it counts the mapping table exactly and the strings
// by their contents, and does not chase Go's per-allocation overhead. The
// number exists to give a cache a budget it can enforce, and a budget enforced
// against a figure that is 10% low is a budget, while one enforced against no
// figure at all is a memory leak with a nice name (ADR 018).
func (m *Map) Size() int64 { return m.size }

// Sources is the list of original files this map points into.
func (m *Map) Sources() []string { return m.sources }

// Lookup resolves a one-based position in the generated file.
//
// It returns the last segment that starts at or before the column, on that
// exact line, and reports false when there is none. Never a segment from
// another line: a minified bundle is a handful of enormous lines, and falling
// back to the previous line's last segment would answer every unmapped column
// with a confident, wrong location — which is worse than saying nothing,
// because nobody checks an answer that looks plausible.
func (m *Map) Lookup(line, column int) (Position, bool) {
	// The upper bounds are not paranoia about callers: a frame's line and
	// column come off the wire, and `lineno: 1e12` is a legal JSON number. A
	// conversion that wrapped would land the binary search somewhere real and
	// wrong rather than off the end.
	if line < 1 || column < 1 || line > math.MaxInt32 || column > math.MaxInt32 {
		return Position{}, false
	}
	if len(m.mappings) == 0 {
		return Position{}, false
	}
	// #nosec G115 -- both are refused above unless they are in [1, MaxInt32].
	genLine, genCol := int32(line-1), int32(column-1)

	// The first index whose position is strictly greater than the target;
	// the segment we want is the one before it.
	index := sort.Search(len(m.mappings), func(i int) bool {
		candidate := &m.mappings[i]
		if candidate.genLine != genLine {
			return candidate.genLine > genLine
		}
		return candidate.genCol > genCol
	})
	if index == 0 {
		return Position{}, false
	}
	found := &m.mappings[index-1]
	if found.genLine != genLine || found.srcIndex < 0 {
		return Position{}, false
	}

	position := Position{
		Line:        int(found.srcLine) + 1,
		Column:      int(found.srcCol) + 1,
		sourceIndex: found.srcIndex,
	}
	if int(found.srcIndex) < len(m.sources) {
		position.Source = m.sources[found.srcIndex]
	}
	if found.nameIndex >= 0 && int(found.nameIndex) < len(m.names) {
		position.Name = m.names[found.nameIndex]
	}
	return position, true
}

// SourceLine returns one line of an original source, when the bundler embedded
// it. The line number is one-based, as Position carries it.
func (m *Map) SourceLine(position Position, line int) (string, bool) {
	index := int(position.sourceIndex)
	if index < 0 || index >= len(m.contents) || !m.hasContent[index] {
		return "", false
	}
	starts := m.lineStarts[index]
	if line < 1 || line > len(starts) {
		return "", false
	}
	content := m.contents[index]
	start := int(starts[line-1])
	end := len(content)
	if line < len(starts) {
		end = int(starts[line])
	}
	return strings.TrimRight(content[start:end], "\r\n"), true
}

// Context returns the line a position points at, plus the `around` lines on
// either side of it.
//
// The three are returned apart because that is how the event protocol carries
// them — pre_context, context_line, post_context — and folding them into one
// slice would only move the splitting somewhere less obvious.
func (m *Map) Context(position Position, around int) (pre []string, line string, post []string, ok bool) {
	line, ok = m.SourceLine(position, position.Line)
	if !ok {
		return nil, "", nil, false
	}
	// Capacity up front. This runs once per resolved frame of every ingested
	// event, and growing two slices five times each is measurable work for a
	// length that is known before the loop starts.
	pre = make([]string, 0, around)
	post = make([]string, 0, around)
	for offset := around; offset >= 1; offset-- {
		if before, found := m.SourceLine(position, position.Line-offset); found {
			pre = append(pre, before)
		}
	}
	for offset := 1; offset <= around; offset++ {
		after, found := m.SourceLine(position, position.Line+offset)
		if !found {
			break
		}
		post = append(post, after)
	}
	if len(pre) == 0 {
		pre = nil
	}
	if len(post) == 0 {
		post = nil
	}
	return pre, line, post, true
}

// rawMap is the wire shape. Every field that a hostile document could put in
// an unexpected shape is raw JSON, decoded by its own tolerant reader — the
// same discipline internal/sentry applies to events, for the same reason: a
// decoder that assumes one shape does not fail loudly, it fails on one
// bundler's output and nowhere else.
type rawMap struct {
	Version        json.RawMessage   `json:"version"`
	File           string            `json:"file"`
	SourceRoot     string            `json:"sourceRoot"`
	Sources        []json.RawMessage `json:"sources"`
	SourcesContent []json.RawMessage `json:"sourcesContent"`
	Names          []json.RawMessage `json:"names"`
	Mappings       *string           `json:"mappings"`
	Sections       []rawSection      `json:"sections"`
	// Both spellings, because both exist in the wild for the same fact:
	// `sentry-cli sourcemaps inject` writes `debug_id` (from the recording) and several
	// bundler plugins write `debugId`. Reading only one of them means half
	// the toolchains upload maps this product cannot tie to a frame, and the
	// symptom is silence.
	DebugID      string `json:"debug_id"`
	DebugIDCamel string `json:"debugId"`
}

type rawSection struct {
	Offset struct {
		Line   int32 `json:"line"`
		Column int32 `json:"column"`
	} `json:"offset"`
	Map *json.RawMessage `json:"map"`
	// URL is the deprecated form: a section whose map lives somewhere else.
	// Recorded so the parser can skip such a section deliberately instead of
	// treating it as an empty one — this product does not fetch URLs a build
	// artefact names (SECURITY.md).
	URL string `json:"url"`
}

// Parse reads a source map document.
func Parse(data []byte) (*Map, error) {
	if len(data) > MaxDocumentBytes {
		return nil, fmt.Errorf("%w: %d bytes, ceiling is %d", ErrTooLarge, len(data), MaxDocumentBytes)
	}

	parsed := &Map{}
	if err := parsed.absorb(data, 0, 0, 0); err != nil {
		return nil, err
	}
	parsed.finish()
	return parsed, nil
}

// absorb decodes one document into the map, offsetting every generated
// position it produces. Sections recurse through here, which is what makes an
// index map and a plain map the same table once parsed: nothing downstream of
// Parse can tell which form the file was in, and nothing downstream should
// have to.
func (m *Map) absorb(data []byte, lineOffset, columnOffset int32, depth int) error {
	var raw rawMap
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	if err := checkVersion(raw.Version); err != nil {
		return err
	}
	if depth == 0 {
		m.File = raw.File
		m.DebugID = firstNonEmpty(raw.DebugID, raw.DebugIDCamel)
	}

	if len(raw.Sections) > 0 {
		return m.absorbSections(&raw, lineOffset, columnOffset, depth)
	}

	sourceCount, nameCount := len(m.sources), len(m.names)
	if sourceCount > maxTableEntries || nameCount > maxTableEntries {
		return fmt.Errorf("%w: more than %d sources or names", ErrTooLarge, maxTableEntries)
	}
	if sourceCount > math.MaxInt32 || nameCount > math.MaxInt32 {
		return fmt.Errorf("%w: source or name table does not fit in 32 bits", ErrTooLarge)
	}
	sourceBase := int32(sourceCount)
	nameBase := int32(nameCount)
	m.appendSources(&raw)
	m.appendNames(&raw)

	if raw.Mappings == nil {
		// A map with sources and no mappings is legal and useless: it
		// resolves nothing. Not an error — an empty table answers every
		// lookup with false, which is the correct answer.
		return nil
	}
	return m.appendMappings(*raw.Mappings, lineOffset, columnOffset, sourceBase, nameBase)
}

func (m *Map) absorbSections(raw *rawMap, lineOffset, columnOffset int32, depth int) error {
	if depth >= maxSectionDepth {
		return fmt.Errorf("%w: sections nested more than %d deep", ErrInvalid, maxSectionDepth)
	}
	if len(raw.Sections) > maxSections {
		return fmt.Errorf("%w: %d sections, ceiling is %d", ErrTooLarge, len(raw.Sections), maxSections)
	}

	for index := range raw.Sections {
		section := &raw.Sections[index]
		if section.Map == nil {
			// Either the deprecated `url` form, which this parser will not
			// go and fetch, or a section with nothing in it. Skipped, and
			// the rest of the document still resolves: half a map is worth
			// more than no map when the half covers the frame somebody is
			// looking at.
			continue
		}
		if section.Offset.Line < 0 || section.Offset.Column < 0 {
			return fmt.Errorf("%w: a section offset is negative", ErrInvalid)
		}
		// The column offset applies only to the section's first generated
		// line: a section starts partway along one line and then owns whole
		// lines after it. Applying it to every line is the classic way to get
		// an index map subtly wrong — every frame past the first line lands a
		// few hundred columns to the right, which usually still finds *a*
		// segment, just not the right one.
		if err := m.absorb(*section.Map,
			lineOffset+section.Offset.Line, columnOffset+section.Offset.Column, depth+1); err != nil {
			return err
		}
		// Only the first section inherits the caller's column offset; the
		// ones after it start from their own.
		columnOffset = 0
	}
	return nil
}

// checkVersion enforces "v3 and nothing else".
//
// The field arrives as a number from every real tool and as a string from
// enough of them to be worth accepting. Anything else — absent, 2, 4, an
// object — is refused rather than assumed, because a version this parser does
// not implement is a document whose mappings mean something else.
func checkVersion(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("%w: no version field", ErrUnsupportedVersion)
	}
	var asNumber float64
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		if asNumber != 3 {
			return fmt.Errorf("%w: version %v, this build reads 3", ErrUnsupportedVersion, asNumber)
		}
		return nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil && strings.TrimSpace(asString) == "3" {
		return nil
	}
	return fmt.Errorf("%w: version %s, this build reads 3", ErrUnsupportedVersion, string(raw))
}

// appendSources adds a document's sources, with sourceRoot applied and their
// embedded contents alongside.
func (m *Map) appendSources(raw *rawMap) {
	for index, encoded := range raw.Sources {
		source := joinSourceRoot(raw.SourceRoot, decodeString(encoded))
		m.sources = append(m.sources, source)

		content, present := "", false
		if index < len(raw.SourcesContent) {
			content, present = decodeOptionalString(raw.SourcesContent[index])
		}
		m.contents = append(m.contents, content)
		m.hasContent = append(m.hasContent, present)
		m.lineStarts = append(m.lineStarts, lineStarts(content, present))
	}
}

func (m *Map) appendNames(raw *rawMap) {
	for _, encoded := range raw.Names {
		m.names = append(m.names, decodeString(encoded))
	}
}

// appendMappings decodes the VLQ table.
//
// The deltas are cumulative across the whole document — source index, source
// line, source column and name index all carry from one segment to the next,
// and only the generated column resets per line. Getting that wrong produces a
// map that resolves the first frame correctly and drifts from there.
func (m *Map) appendMappings(encoded string, lineOffset, columnOffset, sourceBase, nameBase int32) error {
	var (
		genLine  = lineOffset
		genCol   int32
		srcIndex int32
		srcLine  int32
		srcCol   int32
		nameIdx  int32
		fields   [5]int32
	)
	// An index map's section starts partway along one generated line and owns
	// whole lines after it, so its column offset applies to the first line
	// and is dropped at the first `;`.
	lineColumn := columnOffset

	start := 0
	for index := 0; index <= len(encoded); index++ {
		if index < len(encoded) && encoded[index] != ';' && encoded[index] != ',' {
			continue
		}

		if segment := encoded[start:index]; segment != "" {
			count, err := decodeSegment(segment, &fields)
			if err != nil {
				return err
			}
			if len(m.mappings) >= maxSegments {
				return fmt.Errorf("%w: more than %d mapping segments", ErrTooLarge, maxSegments)
			}

			genCol += fields[0]
			if genCol < 0 {
				return fmt.Errorf("%w: a generated column went negative", ErrInvalid)
			}
			entry := mapping{genLine: genLine, genCol: genCol + lineColumn, srcIndex: -1, nameIndex: -1}

			switch count {
			case 1:
				// A segment that marks generated code with no original.
			case 4, 5:
				srcIndex += fields[1]
				srcLine += fields[2]
				srcCol += fields[3]
				if srcIndex < 0 || srcLine < 0 || srcCol < 0 {
					return fmt.Errorf("%w: a source position went negative", ErrInvalid)
				}
				entry.srcIndex = srcIndex + sourceBase
				entry.srcLine = srcLine
				entry.srcCol = srcCol
				if count == 5 {
					nameIdx += fields[4]
					if nameIdx < 0 {
						return fmt.Errorf("%w: a name index went negative", ErrInvalid)
					}
					entry.nameIndex = nameIdx + nameBase
				}
			default:
				return fmt.Errorf("%w: a segment has %d fields, the format allows 1, 4 or 5",
					ErrInvalid, count)
			}
			m.mappings = append(m.mappings, entry)
		}

		if index < len(encoded) && encoded[index] == ';' {
			genLine++
			if genLine > maxGeneratedLine {
				return fmt.Errorf("%w: more than %d generated lines", ErrTooLarge, maxGeneratedLine)
			}
			genCol = 0
			lineColumn = 0
		}
		start = index + 1
	}
	return nil
}

// lineStarts is the byte offset of every line of content.
func lineStarts(content string, present bool) []int32 {
	if !present {
		return nil
	}
	starts := make([]int32, 1, strings.Count(content, "\n")+1)
	for index := 0; index < len(content); index++ {
		if content[index] == '\n' && index+1 <= len(content) {
			// #nosec G115 -- an offset into a sourcesContent entry, and the
			// whole document is refused above MaxDocumentBytes (64 MiB), so
			// it cannot come near MaxInt32.
			starts = append(starts, int32(index+1))
		}
	}
	return starts
}

// finish puts the table in order and prices the result.
func (m *Map) finish() {
	if !sortedByPosition(m.mappings) {
		// Only when it is needed. A well-formed document arrives sorted —
		// the format guarantees it — so this is the tolerance path for a file
		// whose sections were out of order, not a cost every upload pays.
		sort.SliceStable(m.mappings, func(a, b int) bool {
			left, right := &m.mappings[a], &m.mappings[b]
			if left.genLine != right.genLine {
				return left.genLine < right.genLine
			}
			return left.genCol < right.genCol
		})
	}
	m.size = m.measure()
}

func sortedByPosition(mappings []mapping) bool {
	for index := 1; index < len(mappings); index++ {
		previous, current := &mappings[index-1], &mappings[index]
		if current.genLine < previous.genLine {
			return false
		}
		if current.genLine == previous.genLine && current.genCol < previous.genCol {
			return false
		}
	}
	return true
}

// measure prices the parsed map for the cache's budget.
func (m *Map) measure() int64 {
	const mappingBytes = 24
	total := int64(len(m.mappings)) * mappingBytes
	for _, source := range m.sources {
		total += int64(len(source)) + 16
	}
	for _, name := range m.names {
		total += int64(len(name)) + 16
	}
	for _, content := range m.contents {
		total += int64(len(content)) + 16
	}
	for _, starts := range m.lineStarts {
		total += int64(len(starts)) * 4
	}
	return total
}

// decodeString reads a JSON value that ought to be a string, and renders what
// it finds when it is not. Bundlers have shipped numeric entries in `names`;
// dropping them would shift every later index by one and silently rename
// every symbol in the file.
func decodeString(raw json.RawMessage) string {
	value, _ := decodeOptionalString(raw)
	return value
}

func decodeOptionalString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, true
	}
	var asNumber float64
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		return strconv.FormatFloat(asNumber, 'f', -1, 64), true
	}
	return "", false
}

// joinSourceRoot applies the document's sourceRoot, which is a prefix and not
// a path: the specification says to concatenate, and bundlers rely on that —
// a "webpack:///" root joined with "./src/a.js" has to come out as
// "webpack:///./src/a.js" and not as anything a path cleaner would produce.
func joinSourceRoot(root, source string) string {
	if root == "" || source == "" {
		return source
	}
	if strings.Contains(source, "://") || strings.HasPrefix(source, "/") {
		return source
	}
	if strings.HasSuffix(root, "/") {
		return root + source
	}
	return root + "/" + source
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
