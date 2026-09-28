package domain

import "testing"

// The strings below are real headers, kept verbatim. A User-Agent invented for
// a test is a test of what the author believed browsers send, and every
// interesting property of these strings — that Edge claims to be Chrome, that
// Chrome claims to be Safari, that Safari's own version is under a different
// token — comes from the parts an invented one would tidy away.
func TestBrowserFromUserAgent(t *testing.T) {
	cases := map[string]struct {
		userAgent string
		want      Browser
	}{
		"chrome on macos": {
			userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
				"(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
			want: Browser{Name: "Chrome", Version: "141.0.0.0"},
		},
		// Safari is the reason the order matters twice over: it is the token
		// every other browser borrows, and its own version is not the number
		// after its name. Reading that one would report 605 for every Safari
		// released since 2017.
		"safari on macos": {
			userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 " +
				"(KHTML, like Gecko) Version/17.6 Safari/605.1.15",
			want: Browser{Name: "Safari", Version: "17.6"},
		},
		"firefox on linux": {
			userAgent: "Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0",
			want:      Browser{Name: "Firefox", Version: "131.0"},
		},
		// Edge says Chrome and Safari before it says Edg, so a parser that
		// took the first match it found would report Chrome for every Edge
		// user in the product.
		"edge on windows": {
			userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
				"(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.3537.57",
			want: Browser{Name: "Edge", Version: "141.0.3537.57"},
		},
		"opera": {
			userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
				"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 OPR/125.0.0.0",
			want: Browser{Name: "Opera", Version: "125.0.0.0"},
		},
		// Every browser on iOS is Safari's engine under another name, and the
		// name is the only way to tell a Chrome user from a Safari one.
		"chrome on ios": {
			userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 " +
				"(KHTML, like Gecko) CriOS/141.0.7390.65 Mobile/15E148 Safari/604.1",
			want: Browser{Name: "Chrome", Version: "141.0.7390.65"},
		},
		// The browser suite in the compatibility matrix runs in this one, and
		// so does most continuous integration. It is not Chrome — the product
		// token says so — and it is not Safari either, however the string
		// ends.
		"headless chrome": {
			userAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) " +
				"HeadlessChrome/141.0.0.0 Safari/537.36",
			want: Browser{Name: "Chrome Headless", Version: "141.0.0.0"},
		},
		"samsung internet": {
			userAgent: "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) " +
				"SamsungBrowser/27.0 Chrome/125.0.0.0 Mobile Safari/537.36",
			want: Browser{Name: "Samsung Internet", Version: "27.0"},
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			got, found := BrowserFromUserAgent(testCase.userAgent)
			if !found {
				t.Fatalf("nothing recognised in %q", testCase.userAgent)
			}
			if got != testCase.want {
				t.Errorf("got %+v, want %+v", got, testCase.want)
			}
		})
	}
}

// A wrong browser name on an issue is worse than none: it sends someone
// looking in a browser the bug never happened in.
func TestUnrecognisedUserAgentsReportNothing(t *testing.T) {
	for _, userAgent := range []string{
		"",
		"   ",
		"curl/8.7.1",
		"python-requests/2.32.3",
		"sentry.python/2.68.1",
		"Dart/3.13 (dart:io)",
		"Mozilla/5.0",
		// A crawler wearing WebKit's clothes. It ends in "Safari/537.36" like
		// almost everything WebKit ever touched, but Safari always sends its
		// own Version/ token and this does not. Tagging an issue with a
		// browser the bug never happened in sends someone looking in the wrong
		// place, so the last entry in the table refuses rather than guesses.
		"Mozilla/5.0 (compatible; SomeBot/1.0; +http://example.com/bot) " +
			"AppleWebKit/537.36 (KHTML, like Gecko) Safari/537.36",
	} {
		if browser, found := BrowserFromUserAgent(userAgent); found {
			t.Errorf("%q was reported as %+v; it is not a browser", userAgent, browser)
		}
	}
}
