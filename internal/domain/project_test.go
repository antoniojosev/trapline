package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 22, 15, 4, 5, 0, time.UTC)

func TestNewProject(t *testing.T) {
	t.Run("trims the name", func(t *testing.T) {
		got, err := NewProject("  venekambio  ", testNow)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Name != "venekambio" {
			t.Errorf("Name = %q, want %q", got.Name, "venekambio")
		}
	})

	t.Run("stores the timestamp in UTC", func(t *testing.T) {
		caracas := time.FixedZone("-04", -4*60*60)
		got, err := NewProject("despacha", testNow.In(caracas))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.CreatedAt.Location() != time.UTC {
			t.Errorf("CreatedAt location = %v, want UTC", got.CreatedAt.Location())
		}
		if !got.CreatedAt.Equal(testNow) {
			t.Errorf("CreatedAt = %v, want the same instant as %v", got.CreatedAt, testNow)
		}
	})

	t.Run("leaves the id to the store", func(t *testing.T) {
		got, err := NewProject("repuestos", testNow)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != 0 {
			t.Errorf("ID = %d, want 0 until saved", got.ID)
		}
	})

	t.Run("rejects invalid names", func(t *testing.T) {
		cases := map[string]string{
			"empty":      "",
			"whitespace": "   \t\n ",
			"too long":   strings.Repeat("a", MaxProjectNameLen+1),
		}
		for name, input := range cases {
			t.Run(name, func(t *testing.T) {
				if _, err := NewProject(input, testNow); !errors.Is(err, ErrInvalidProject) {
					t.Errorf("error = %v, want ErrInvalidProject", err)
				}
			})
		}
	})

	t.Run("accepts a name at the limit", func(t *testing.T) {
		if _, err := NewProject(strings.Repeat("a", MaxProjectNameLen), testNow); err != nil {
			t.Errorf("unexpected error at the boundary: %v", err)
		}
	})
}

func TestSlugifyDerivesWhatAPipelineWillType(t *testing.T) {
	// The slug goes into SENTRY_PROJECT and from there straight into a URL
	// path, so what matters is that a name a person would give a project
	// becomes something a person would type.
	for _, testCase := range []struct {
		name string
		want string
		why  string
	}{
		{"venekambio", "venekambio", "already a slug"},
		{"Venekambio", "venekambio", "case does not survive a URL"},
		{"Mi App", "mi-app", "a space is one separator"},
		{"Mi   App", "mi-app", "a run of separators is still one"},
		{"  Mi App  ", "mi-app", "the ends are trimmed"},
		{"api_v2.checkout", "api-v2-checkout", "punctuation separates"},
		{"Diseño", "diseno", "the Latin accents fold rather than break the word"},
		{"Añejo", "anejo", "so does the tilde"},
		{"Café Ñu", "cafe-nu", "an accent at the end of a word too"},
		{"Ürgüp Çay", "urgup-cay", "the diaeresis and the cedilla, upper and lower"},
		{"Àéîõü", "aeiou", "every vowel the fold knows"},
		{"---", "project", "a name that derives nothing falls back"},
		{"2026", "project", "a slug of only digits would be shadowed by the id"},
		{"2026 facturas", "2026-facturas", "digits are fine as long as something else is there"},
	} {
		if got := Slugify(testCase.name); got != testCase.want {
			t.Errorf("Slugify(%q) = %q, want %q — %s", testCase.name, got, testCase.want, testCase.why)
		}
	}
}

func TestASlugFitsTheColumnEvenWithASuffix(t *testing.T) {
	// A long name plus a disambiguating suffix still has to fit, or the
	// hundredth candidate would be a value the store cannot hold.
	long := strings.Repeat("nombre-larguisimo-", 20)
	for _, candidate := range SlugCandidates(long) {
		if len(candidate) > MaxProjectSlugLen {
			t.Fatalf("candidate %q is %d characters, over the %d the column promises",
				candidate, len(candidate), MaxProjectSlugLen)
		}
	}
}

func TestSlugCandidatesAreDistinctAndStartWithTheBestOne(t *testing.T) {
	candidates := SlugCandidates("Mi App")
	if candidates[0] != "mi-app" {
		t.Fatalf("the first candidate is %q, want the plain derivation", candidates[0])
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate] {
			t.Fatalf("%q appears twice: the store would try the same taken slug again", candidate)
		}
		seen[candidate] = true
	}
}

func TestANewProjectCarriesItsSlug(t *testing.T) {
	project, err := NewProject("Mi App", time.Now())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if project.Slug != "mi-app" {
		t.Errorf("slug = %q, want mi-app", project.Slug)
	}
}
