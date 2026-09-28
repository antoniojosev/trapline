package domain

import "strings"

// Browser is which browser a request came from.
//
// It exists because a browser error report is only actionable once you know
// which browser produced it — "only on Safari" is the answer to a large share
// of them — and the browser SDK does not put that in the payload. It arrives
// in the one field the page cannot forget to set, because the browser sets it:
// the User-Agent header.
type Browser struct {
	Name    string
	Version string
}

// browserTokens maps a User-Agent product token to a browser name, in the
// order they must be tried.
//
// The order is the whole algorithm. Every browser in this list claims to be
// several of the others: Edge says Chrome and Safari, Chrome says Safari,
// Opera says Chrome and Safari. Matching the first token that appears would
// report Chrome for all of them, so the most specific claim is checked first
// and the most widely borrowed one — Safari — last.
var browserTokens = []struct {
	token string
	name  string
}{
	{"Edg", "Edge"}, // Edg/, EdgA/ on Android, EdgiOS/ on iOS
	{"OPR", "Opera"},
	{"SamsungBrowser", "Samsung Internet"},
	{"FxiOS", "Firefox"},
	{"Firefox", "Firefox"},
	{"CriOS", "Chrome"},
	// Its own name, not "Chrome". A headless browser is what continuous
	// integration and most scrapers run, and "this only ever happens in the
	// test browser" is a useful thing for an issue to be able to say — while
	// filing it under Chrome would put a browser nobody uses in the same
	// bucket as the one everybody does.
	{"HeadlessChrome", "Chrome Headless"},
	{"Chrome", "Chrome"},
	{"Safari", "Safari"},
}

// BrowserFromUserAgent reads the browser out of a User-Agent header.
//
// Deliberately small. A full User-Agent database is a dependency that needs
// updating forever and it would be carried by a binary whose whole promise is
// to be one small file; what an issue page needs is the name and the major
// version, and those are the parts of the string that have stayed stable for
// twenty years. Anything it does not recognise reports nothing rather than
// guessing, because a wrong browser name on an issue is worse than none.
func BrowserFromUserAgent(userAgent string) (Browser, bool) {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		return Browser{}, false
	}

	for _, candidate := range browserTokens {
		version, found := productVersion(userAgent, candidate.token)
		if !found {
			continue
		}
		// Safari reports its own version under a separate token and puts a
		// WebKit build number after "Safari/", so reading the one that follows
		// its name would report 605 for every Safari ever released.
		//
		// And a string that reached this last entry without carrying a
		// "Version/" token is not Safari at all. It is one of the many things
		// that end in "Safari/537.36" — a headless browser, an embedded
		// WebView, a scraper — and calling it Safari would put a browser name
		// on an issue that never happened in that browser, which sends someone
		// looking in the wrong place. Reporting nothing is the honest answer.
		if candidate.name == "Safari" {
			own, ok := productVersion(userAgent, "Version")
			if !ok {
				return Browser{}, false
			}
			version = own
		}
		return Browser{Name: candidate.name, Version: version}, true
	}
	return Browser{}, false
}

// productVersion finds "token/version" in a User-Agent string.
//
// The token must start a product, so it is preceded by the start of the string
// or by a space or an open parenthesis. Without that check "Chrome" would also
// match inside a token like "HeadlessChrome" — which is a different claim, and
// one this function would then report as the ordinary browser.
func productVersion(userAgent, token string) (string, bool) {
	for offset := 0; ; {
		index := strings.Index(userAgent[offset:], token+"/")
		if index < 0 {
			return "", false
		}
		index += offset
		offset = index + len(token) + 1

		if index > 0 {
			previous := userAgent[index-1]
			if previous != ' ' && previous != '(' && previous != ';' {
				continue
			}
		}
		return version(userAgent[offset:]), true
	}
}

// version reads the version number that follows a product token, stopping at
// the first character that cannot be part of one.
func version(rest string) string {
	end := 0
	for end < len(rest) {
		character := rest[end]
		if (character < '0' || character > '9') && character != '.' {
			break
		}
		end++
	}
	return strings.TrimSuffix(rest[:end], ".")
}
