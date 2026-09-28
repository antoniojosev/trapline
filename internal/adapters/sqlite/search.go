package sqlite

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/antoniojosev/trapline/internal/domain"
)

// MinSearchTermRunes is the shortest term the index can serve.
//
// It is three because the index is built from three-character windows: a term
// of one or two characters has no window to look up, and no amount of care
// above this layer changes that. It is exported so a client can tell somebody
// before they type rather than after (ADR 011).
const MinSearchTermRunes = 3

// ftsMatch turns what a person typed into an FTS5 MATCH expression.
//
// Two rules, and they are both about not surprising anyone.
//
// The first is that FTS5's query language is never exposed. Someone searching
// for `NOT NULL` means the two words, not a boolean operator, and someone
// searching for `*` means an asterisk. So every term is wrapped as a phrase,
// which makes the whole of it literal, and the phrases are listed one after
// another — FTS5's implicit conjunction, so all of them have to match.
//
// The second is that a phrase against a trigram index is a substring search.
// `Error` finds `ValueError`, which is what the `LIKE '%x%'` this replaced did
// and what people expect from a search box, especially here: exception names
// are compound words and the part somebody remembers is usually in the middle.
// There is no trailing `*`, because with this tokenizer a prefix is just a
// substring that happens to start at the beginning — the wildcard would be
// syntax with nothing left to do.
//
// Terms shorter than MinSearchTermRunes are dropped rather than searched for:
// the index cannot serve them, and a two-letter term narrows almost nothing
// anyway. If that leaves no terms at all, the caller gets an error saying so —
// the one thing this must never do is answer "no results" to a query it never
// ran.
func ftsMatch(query string) (string, error) {
	var (
		phrases []string
		dropped []string
	)
	for _, field := range strings.Fields(query) {
		if utf8.RuneCountInString(field) < MinSearchTermRunes {
			dropped = append(dropped, field)
			continue
		}
		// Doubling the quote is FTS5's own escape, the same convention as SQL
		// string literals. It is what keeps a quote in the search box from
		// ending the phrase and turning the rest of the input into syntax.
		phrases = append(phrases, `"`+strings.ReplaceAll(field, `"`, `""`)+`"`)
	}
	if len(phrases) == 0 {
		return "", fmt.Errorf(
			"%w: %s is shorter than %d characters, which is the shortest the search index can look up",
			domain.ErrInvalidSearch, quoteTerms(dropped), MinSearchTermRunes)
	}
	return strings.Join(phrases, " "), nil
}

// quoteTerms renders the offending terms back to the person who typed them,
// because "your search was too short" without saying which part is a message
// that makes somebody guess.
func quoteTerms(terms []string) string {
	if len(terms) == 0 {
		return "the search"
	}
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, `"`+term+`"`)
	}
	return strings.Join(quoted, ", ")
}
