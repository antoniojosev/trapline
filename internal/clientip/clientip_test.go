package clientip

import (
	"net/netip"
	"strings"
	"testing"
)

// The header this package exists to distrust. Named once so the tests read
// like the deployment they describe.
const xff = "X-Forwarded-For"

func mustResolver(t *testing.T, trusted ...string) *Resolver {
	t.Helper()
	resolver, err := New(trusted)
	if err != nil {
		t.Fatalf("New(%v): %v", trusted, err)
	}
	return resolver
}

func TestResolveIgnoresForwardedForFromAnUntrustedPeer(t *testing.T) {
	t.Parallel()

	// The whole point of the design, stated as a test: a peer that is not on
	// the list may claim whatever it likes and is not believed. If this ever
	// goes green with "203.0.113.9", the limiter's keys are attacker-chosen.
	cases := []struct {
		name    string
		trusted []string
	}{
		{name: "no proxies configured at all"},
		{name: "proxies configured, but not this peer", trusted: []string{"10.0.0.1"}},
		{name: "a range that does not contain the peer", trusted: []string{"10.0.0.0/8"}},
		{name: "a v6 range against a v4 peer", trusted: []string{"2001:db8::/32"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			resolver := mustResolver(t, testCase.trusted...)
			got, ok := resolver.Resolve("198.51.100.7:44321", []string{"203.0.113.9"})
			if !ok {
				t.Fatal("the direct peer was parseable; want a decision")
			}
			if got.String() != "198.51.100.7" {
				t.Errorf("Resolve = %s, want the direct peer 198.51.100.7 with %s ignored", got, xff)
			}
		})
	}
}

func TestResolveWalksAChainOfTrustedProxiesFromTheRight(t *testing.T) {
	t.Parallel()

	resolver := mustResolver(t, "10.0.0.0/8", "192.168.1.5")

	cases := []struct {
		name      string
		peer      string
		forwarded []string
		want      string
	}{
		{
			name:      "one proxy",
			peer:      "10.0.0.1:9000",
			forwarded: []string{"203.0.113.9"},
			want:      "203.0.113.9",
		},
		{
			name:      "two proxies, one header",
			peer:      "10.0.0.1:9000",
			forwarded: []string{"203.0.113.9, 10.0.0.2, 192.168.1.5"},
			want:      "203.0.113.9",
		},
		{
			name: "two proxies, one header each — Go reports repeated headers separately",
			peer: "10.0.0.1:9000",
			// The rightmost value is the most recent hop, so the walk starts
			// at the end of the last header and moves left across all of them.
			forwarded: []string{"203.0.113.9", "10.0.0.2, 192.168.1.5"},
			want:      "203.0.113.9",
		},
		{
			name: "the client spoofed a prefix, and it does not help them",
			peer: "10.0.0.1:9000",
			// A client that sends its own X-Forwarded-For has it appended to,
			// not replaced. Its invention ends up to the LEFT of the address
			// the proxy observed, which is why the walk goes right to left.
			forwarded: []string{"1.1.1.1, 203.0.113.9, 10.0.0.2"},
			want:      "203.0.113.9",
		},
		{
			name:      "the client claims to be a trusted proxy, and it does not help them either",
			peer:      "10.0.0.1:9000",
			forwarded: []string{"10.0.0.99, 203.0.113.9, 10.0.0.2"},
			want:      "203.0.113.9",
		},
		{
			name:      "every hop is trusted: the leftmost is the origin",
			peer:      "10.0.0.1:9000",
			forwarded: []string{"10.1.2.3, 10.0.0.2"},
			want:      "10.1.2.3",
		},
		{
			name:      "trusted peer with no header at all",
			peer:      "10.0.0.1:9000",
			forwarded: nil,
			want:      "10.0.0.1",
		},
		{
			name:      "a single trusted address, not a range",
			peer:      "192.168.1.5:443",
			forwarded: []string{"203.0.113.9"},
			want:      "203.0.113.9",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, ok := resolver.Resolve(testCase.peer, testCase.forwarded)
			if !ok {
				t.Fatal("want a decision")
			}
			if got.String() != testCase.want {
				t.Errorf("Resolve(%q, %q) = %s, want %s",
					testCase.peer, testCase.forwarded, got, testCase.want)
			}
		})
	}
}

func TestResolveHandlesRubbishInTheHeader(t *testing.T) {
	t.Parallel()

	resolver := mustResolver(t, "10.0.0.0/8")

	cases := []struct {
		name      string
		forwarded []string
		want      string
	}{
		{name: "empty header", forwarded: []string{""}, want: "10.0.0.1"},
		{name: "only whitespace", forwarded: []string{"   "}, want: "10.0.0.1"},
		{name: "only commas", forwarded: []string{",,,"}, want: "10.0.0.1"},
		{name: "generous whitespace around real hops", forwarded: []string{"  203.0.113.9  ,  10.0.0.2  "}, want: "203.0.113.9"},
		{name: "stray commas around real hops", forwarded: []string{",203.0.113.9,,10.0.0.2,"}, want: "203.0.113.9"},
		{
			name: "an unparseable hop stops the walk",
			// "unknown" is what some proxies emit for a client they could not
			// determine. Nothing to its left can be vouched for, so the answer
			// is the peer rather than the invented address behind it.
			forwarded: []string{"203.0.113.9, unknown, 10.0.0.2"},
			want:      "10.0.0.1",
		},
		{
			name:      "a hostname is not an address",
			forwarded: []string{"proxy.example.com, 10.0.0.2"},
			want:      "10.0.0.1",
		},
		{
			name:      "an obfuscated identifier, as RFC 7239 allows for Forwarded",
			forwarded: []string{"_hidden, 10.0.0.2"},
			want:      "10.0.0.1",
		},
		{
			name:      "the rightmost hop itself is rubbish",
			forwarded: []string{"203.0.113.9, ???"},
			want:      "10.0.0.1",
		},
		{
			name:      "an enormous header does not change the rule",
			forwarded: []string{strings.Repeat("203.0.113.9, ", 500) + "10.0.0.2"},
			want:      "203.0.113.9",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, ok := resolver.Resolve("10.0.0.1:9000", testCase.forwarded)
			if !ok {
				t.Fatal("want a decision")
			}
			if got.String() != testCase.want {
				t.Errorf("Resolve(%q) = %s, want %s", testCase.forwarded, got, testCase.want)
			}
		})
	}
}

func TestResolveWithIPv6(t *testing.T) {
	t.Parallel()

	resolver := mustResolver(t, "2001:db8:1::/48", "::1")

	cases := []struct {
		name      string
		peer      string
		forwarded []string
		want      string
	}{
		{name: "bracketed peer with a port", peer: "[2001:db8:1::5]:9000", forwarded: []string{"2001:db8:99::7"}, want: "2001:db8:99::7"},
		{name: "loopback peer, bracketed", peer: "[::1]:9000", forwarded: []string{"2001:db8:99::7"}, want: "2001:db8:99::7"},
		{name: "hop bracketed with a port", peer: "[::1]:9000", forwarded: []string{"[2001:db8:99::7]:443"}, want: "2001:db8:99::7"},
		{name: "hop bracketed without a port", peer: "[::1]:9000", forwarded: []string{"[2001:db8:99::7]"}, want: "2001:db8:99::7"},
		{name: "untrusted v6 peer keeps its own address", peer: "[2001:db8:99::7]:443", forwarded: []string{"::1"}, want: "2001:db8:99::7"},
		{
			name: "an IPv4-mapped hop is the IPv4 address",
			peer: "[::1]:9000",
			// Otherwise one machine occupies two buckets by changing how it
			// spells itself, which is a free doubling of any per-IP ceiling.
			forwarded: []string{"::ffff:203.0.113.9"},
			want:      "203.0.113.9",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, ok := resolver.Resolve(testCase.peer, testCase.forwarded)
			if !ok {
				t.Fatal("want a decision")
			}
			if got.String() != testCase.want {
				t.Errorf("Resolve(%q, %q) = %s, want %s", testCase.peer, testCase.forwarded, got, testCase.want)
			}
		})
	}
}

func TestResolveRejectsAnUnparseablePeer(t *testing.T) {
	t.Parallel()

	resolver := mustResolver(t, "10.0.0.0/8")
	for _, peer := range []string{"", "   ", "not-an-address", "[::1", "[::1]junk", "1.2.3.4:", "1.2.3.4:notaport"} {
		if _, ok := resolver.Resolve(peer, []string{"203.0.113.9"}); ok {
			t.Errorf("Resolve(%q) reported a decision; want none", peer)
		}
	}
}

func TestParseAddrShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want string
	}{
		{raw: "1.2.3.4", want: "1.2.3.4"},
		{raw: "1.2.3.4:5678", want: "1.2.3.4"},
		{raw: "  1.2.3.4:5678  ", want: "1.2.3.4"},
		{raw: "::1", want: "::1"},
		{raw: "[::1]", want: "::1"},
		{raw: "[::1]:443", want: "::1"},
		{raw: "2001:db8::1", want: "2001:db8::1"},
		{raw: "[2001:db8::1]:443", want: "2001:db8::1"},
		// A zone is a property of the local interface, not of the peer's
		// identity, and letting it through would split one address into as
		// many buckets as there are interface names.
		{raw: "fe80::1%eth0", want: "fe80::1"},
		{raw: "[fe80::1%eth0]:443", want: "fe80::1"},
		{raw: "::ffff:1.2.3.4", want: "1.2.3.4"},
	}

	for _, testCase := range cases {
		t.Run(testCase.raw, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseAddr(testCase.raw)
			if !ok {
				t.Fatalf("ParseAddr(%q) failed; want %s", testCase.raw, testCase.want)
			}
			if got.String() != testCase.want {
				t.Errorf("ParseAddr(%q) = %s, want %s", testCase.raw, got, testCase.want)
			}
		})
	}
}

func TestParseAddrRejects(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"",
		" ",
		"localhost",
		"1.2.3.4.5",
		"999.1.1.1",
		"[::1",          // unterminated bracket
		"[::1]x",        // trailing junk instead of a port
		"[::1]:",        // a colon with no port
		"[::1]:http",    // a named port, which nothing produces here
		"1.2.3.4:99:88", // two colons, so it is read as IPv6 and is not one
		"unknown",
		"_obfuscated",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if got, ok := ParseAddr(raw); ok {
				t.Errorf("ParseAddr(%q) = %s, want a rejection", raw, got)
			}
		})
	}
}

func TestNewRejectsRubbishAndNamesIt(t *testing.T) {
	t.Parallel()

	for _, entry := range []string{"not-an-address", "10.0.0.0/99", "10.0.0.0/", "proxy.example.com"} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			_, err := New([]string{entry})
			if err == nil {
				t.Fatalf("New(%q) succeeded; a typo here silently disables the header handling "+
					"an operator believes they configured", entry)
			}
			if !strings.Contains(err.Error(), entry) {
				t.Errorf("error %q does not name the offending entry %q", err, entry)
			}
			if Validate([]string{entry}) == nil {
				t.Errorf("Validate(%q) accepted what New rejected", entry)
			}
		})
	}
}

func TestNewAcceptsTheUsualDeploymentShapes(t *testing.T) {
	t.Parallel()

	resolver, err := New([]string{" 10.0.0.1 ", "", "172.16.0.0/12", "::1", "2001:db8::/32"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !resolver.TrustsAnyone() {
		t.Error("TrustsAnyone = false with five entries configured")
	}
	if empty := mustResolver(t); empty.TrustsAnyone() {
		t.Error("TrustsAnyone = true with nothing configured")
	}
	if blanks := mustResolver(t, "", "  "); blanks.TrustsAnyone() {
		t.Error("blank entries counted as configured proxies")
	}

	// An unmasked CIDR is a common way to write one, and it has to behave the
	// same as its masked form rather than matching nothing.
	unmasked, err := New([]string{"10.1.2.3/8"})
	if err != nil {
		t.Fatalf("New with an unmasked CIDR: %v", err)
	}
	got, ok := unmasked.Resolve("10.9.9.9:80", []string{"203.0.113.9"})
	if !ok || got.String() != "203.0.113.9" {
		t.Errorf("Resolve behind an unmasked CIDR = %s (%t), want 203.0.113.9", got, ok)
	}
}

func TestBucketGroupsIPv6BySubnetAndIPv4Exactly(t *testing.T) {
	t.Parallel()

	// Two addresses out of the same /64 must land in one bucket: rotating
	// inside a /64 is free, so a limiter that does not do this counts nothing.
	first := netip.MustParseAddr("2001:db8:1:2::1")
	second := netip.MustParseAddr("2001:db8:1:2:ffff:ffff:ffff:ffff")
	if Bucket(first) != Bucket(second) {
		t.Errorf("Bucket(%s) = %s and Bucket(%s) = %s; want one bucket per /64",
			first, Bucket(first), second, Bucket(second))
	}

	other := netip.MustParseAddr("2001:db8:1:3::1")
	if Bucket(first) == Bucket(other) {
		t.Errorf("addresses in different /64s share bucket %s", Bucket(first))
	}

	// IPv4 is counted exactly: addresses are scarce there and grouping them
	// would punish unrelated customers behind one carrier-grade NAT.
	v4 := netip.MustParseAddr("203.0.113.9")
	if got := Bucket(v4); got.Bits() != 32 || got.Addr() != v4 {
		t.Errorf("Bucket(%s) = %s, want the exact address", v4, got)
	}
	if Bucket(v4) == Bucket(netip.MustParseAddr("203.0.113.10")) {
		t.Error("two IPv4 addresses share a bucket")
	}
}
