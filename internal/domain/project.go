package domain

import (
	"fmt"
	"strings"
	"time"
)

// MaxProjectNameLen bounds a project name so it stays renderable in a table
// and cannot be used to bloat rows.
const MaxProjectNameLen = 100

// MaxProjectSlugLen bounds the derived slug. It is shorter than the name
// because it goes in a URL path that a deploy tool builds, and a hundred
// characters of it would be unreadable in the one place it is meant to be
// read: a pipeline configuration file.
const MaxProjectSlugLen = 50

// slugFallback is the base used when a name derives nothing usable, and the
// prefix the migration reserves so a backfilled 'project-<id>' can never
// collide with a slug derived from somebody's name.
const slugFallback = "project"

// Project is a tenant: one entry per application that sends events.
//
// ID is an int64 rather than a UUID for protocol compatibility (ADR 002):
// the ingest path is /api/{project_id}/envelope/, official DSNs carry a
// numeric project id there, and SDKs are only known to be exercised against
// that shape. A UUID would read as a needless compatibility risk on the one
// surface that must not surprise an SDK.
type Project struct {
	ID   int64
	Name string
	// Slug is the name a deploy tool puts in a URL. It is derived from Name
	// and unique across the installation (ADR 013). The id keeps working
	// everywhere the slug does — it is what the ingest path carries and it
	// cannot be taken away — but SENTRY_PROJECT=venekambio is what somebody
	// copies out of a documentation page, and a server that only accepted an
	// integer there would make the compatibility claim true only for a reader
	// who first went looking for one.
	Slug      string
	CreatedAt time.Time
}

// NewProject validates and builds a project. The zero ID is intentional:
// the store assigns it, so a project is only fully formed after it is saved.
func NewProject(name string, now time.Time) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Project{}, fmt.Errorf("%w: name is required", ErrInvalidProject)
	}
	if len(name) > MaxProjectNameLen {
		return Project{}, fmt.Errorf("%w: name exceeds %d characters", ErrInvalidProject, MaxProjectNameLen)
	}
	return Project{Name: name, Slug: Slugify(name), CreatedAt: now.UTC()}, nil
}

// Slugify derives a URL-safe name from a project name.
//
// The rules, in order: fold the Latin accents, lowercase, turn every run of
// anything that is not a letter or a digit into one hyphen, trim the hyphens
// off the ends, cut to MaxProjectSlugLen.
//
// Two results are handed back to the caller to disambiguate rather than
// returned as they are. A name that derives nothing — punctuation, or a script
// this function does not know — becomes the fallback base, and so does a name
// that derives only digits: the compatibility surface reads a numeric path
// segment as a project id first, so a slug made of digits could never be
// resolved as a slug and would be a name that silently addresses a different
// project. The store is what turns either into something unique, because
// uniqueness is a fact about the store and not about a string.
func Slugify(name string) string {
	var builder strings.Builder
	builder.Grow(len(name))

	pendingSeparator := false
	for _, symbol := range strings.TrimSpace(name) {
		symbol = foldAccent(symbol)
		switch {
		case symbol >= 'A' && symbol <= 'Z':
			symbol += 'a' - 'A'
			fallthrough
		case symbol >= 'a' && symbol <= 'z', symbol >= '0' && symbol <= '9':
			// A separator is only written once something follows it, which is
			// what trims the leading and trailing runs without a second pass.
			if pendingSeparator && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			pendingSeparator = false
			builder.WriteRune(symbol)
		default:
			pendingSeparator = true
		}
	}

	slug := builder.String()
	if len(slug) > MaxProjectSlugLen {
		slug = strings.TrimRight(slug[:MaxProjectSlugLen], "-")
	}
	if slug == "" || isAllDigits(slug) {
		return slugFallback
	}
	return slug
}

// SlugCandidates lists the slugs to try for a name, best first.
//
// The store walks this list and takes the first one nobody holds. It is a pure
// function so that "what would this project be called" is answerable without a
// database, and so the numbering is the same on every installation rather than
// whatever an autoincrement happened to hand out.
func SlugCandidates(name string) []string {
	base := Slugify(name)
	candidates := make([]string, 0, maxSlugCandidates)
	candidates = append(candidates, base)
	for suffix := 2; len(candidates) < maxSlugCandidates; suffix++ {
		candidates = append(candidates, fmt.Sprintf("%s-%d", trimForSuffix(base, suffix), suffix))
	}
	return candidates
}

// maxSlugCandidates bounds the walk. A hundred projects whose names all derive
// the same slug is not a situation to keep serving quietly; it is one to
// refuse and let somebody name the next one properly.
const maxSlugCandidates = 100

// trimForSuffix makes room for "-<n>" so a long name plus a suffix still fits
// the column's promise.
func trimForSuffix(base string, suffix int) string {
	room := MaxProjectSlugLen - len(fmt.Sprintf("-%d", suffix))
	if len(base) <= room {
		return base
	}
	return strings.TrimRight(base[:room], "-")
}

// foldAccent maps the Latin accents a Spanish or Portuguese project name
// actually contains onto ASCII. Deliberately a short list and not a
// transliteration library: everything it does not know becomes a separator,
// which is a slug that reads badly rather than a wrong one.
func foldAccent(symbol rune) rune {
	switch symbol {
	case 'á', 'à', 'ä', 'â', 'ã', 'Á', 'À', 'Ä', 'Â', 'Ã':
		return 'a'
	case 'é', 'è', 'ë', 'ê', 'É', 'È', 'Ë', 'Ê':
		return 'e'
	case 'í', 'ì', 'ï', 'î', 'Í', 'Ì', 'Ï', 'Î':
		return 'i'
	case 'ó', 'ò', 'ö', 'ô', 'õ', 'Ó', 'Ò', 'Ö', 'Ô', 'Õ':
		return 'o'
	case 'ú', 'ù', 'ü', 'û', 'Ú', 'Ù', 'Ü', 'Û':
		return 'u'
	case 'ñ', 'Ñ':
		return 'n'
	case 'ç', 'Ç':
		return 'c'
	default:
		return symbol
	}
}
