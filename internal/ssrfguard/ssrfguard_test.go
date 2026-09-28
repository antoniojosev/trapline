package ssrfguard_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/ssrfguard"
)

// TestClassify is the table this package exists for.
//
// Exhaustive rather than representative: every category, both families, the
// edges of every range, and each of the four ways an IPv4 address can be
// smuggled inside an IPv6 one. A guard is only as good as the addresses it
// recognises, and the ones it misses are exactly the ones an attacker will
// use.
func TestClassify(t *testing.T) {
	t.Parallel()

	cases := []struct {
		addr string
		want ssrfguard.Category
	}{
		// The only answer that lets a connection happen.
		{"1.1.1.1", ssrfguard.CategoryPublic},
		{"8.8.8.8", ssrfguard.CategoryPublic},
		{"93.184.216.34", ssrfguard.CategoryPublic},
		{"2606:4700:4700::1111", ssrfguard.CategoryPublic},
		{"2a00:1450:4003:80f::200e", ssrfguard.CategoryPublic},
		// The edges of the private ranges, from both sides.
		{"9.255.255.255", ssrfguard.CategoryPublic},
		{"10.0.0.0", ssrfguard.CategoryPrivate},
		{"10.255.255.255", ssrfguard.CategoryPrivate},
		{"11.0.0.0", ssrfguard.CategoryPublic},
		{"172.15.255.255", ssrfguard.CategoryPublic},
		{"172.16.0.0", ssrfguard.CategoryPrivate},
		{"172.31.255.255", ssrfguard.CategoryPrivate},
		{"172.32.0.0", ssrfguard.CategoryPublic},
		{"192.167.255.255", ssrfguard.CategoryPublic},
		{"192.168.0.1", ssrfguard.CategoryPrivate},
		{"192.168.255.255", ssrfguard.CategoryPrivate},
		{"192.169.0.0", ssrfguard.CategoryPublic},
		// RFC 4193.
		{"fc00::1", ssrfguard.CategoryPrivate},
		{"fd12:3456:789a::1", ssrfguard.CategoryPrivate},
		{"fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", ssrfguard.CategoryPrivate},
		{"fe00::1", ssrfguard.CategoryPublic},

		{"127.0.0.1", ssrfguard.CategoryLoopback},
		{"127.255.255.254", ssrfguard.CategoryLoopback},
		{"128.0.0.1", ssrfguard.CategoryPublic},
		{"::1", ssrfguard.CategoryLoopback},

		// The prize. An SSRF that reaches this one walks off with the
		// instance's credentials.
		{"169.254.169.254", ssrfguard.CategoryLinkLocal},
		{"169.254.0.0", ssrfguard.CategoryLinkLocal},
		{"169.253.255.255", ssrfguard.CategoryPublic},
		{"fe80::1", ssrfguard.CategoryLinkLocal},
		{"febf:ffff::1", ssrfguard.CategoryLinkLocal},
		{"fec0::1", ssrfguard.CategoryPublic},

		{"224.0.0.1", ssrfguard.CategoryMulticast},
		{"239.255.255.255", ssrfguard.CategoryMulticast},
		{"ff02::1", ssrfguard.CategoryMulticast},

		// On Linux, connecting to 0.0.0.0 connects to localhost.
		{"0.0.0.0", ssrfguard.CategoryUnspecified},
		{"0.1.2.3", ssrfguard.CategoryUnspecified},
		{"::", ssrfguard.CategoryUnspecified},

		{"255.255.255.255", ssrfguard.CategoryBroadcast},
		{"100.64.0.1", ssrfguard.CategoryShared},
		{"100.127.255.255", ssrfguard.CategoryShared},
		{"100.128.0.0", ssrfguard.CategoryPublic},

		{"192.0.0.1", ssrfguard.CategoryReserved},
		{"192.0.2.1", ssrfguard.CategoryReserved},
		{"198.18.0.1", ssrfguard.CategoryReserved},
		{"198.51.100.1", ssrfguard.CategoryReserved},
		{"203.0.113.1", ssrfguard.CategoryReserved},
		{"240.0.0.1", ssrfguard.CategoryReserved},
		{"2001:db8::1", ssrfguard.CategoryReserved},
		{"100::1", ssrfguard.CategoryReserved},

		// The four disguises. Each of these is 127.0.0.1 or 169.254.169.254
		// written so that a check on "is this IPv6 address private" says no.
		{"::ffff:127.0.0.1", ssrfguard.CategoryLoopback},
		{"::ffff:169.254.169.254", ssrfguard.CategoryLinkLocal},
		{"::ffff:10.0.0.1", ssrfguard.CategoryPrivate},
		{"::ffff:8.8.8.8", ssrfguard.CategoryPublic},
		{"::127.0.0.1", ssrfguard.CategoryLoopback},
		{"::169.254.169.254", ssrfguard.CategoryLinkLocal},
		{"64:ff9b::7f00:1", ssrfguard.CategoryLoopback},
		{"64:ff9b::a9fe:a9fe", ssrfguard.CategoryLinkLocal},
		{"64:ff9b::808:808", ssrfguard.CategoryPublic},
		{"2002:7f00:1::", ssrfguard.CategoryLoopback},
		{"2002:a9fe:a9fe::", ssrfguard.CategoryLinkLocal},
		{"2002:808:808::", ssrfguard.CategoryPublic},
	}

	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			addr, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.addr, err)
			}
			if got := ssrfguard.Classify(addr); got != tc.want {
				t.Errorf("Classify(%s) = %s, want %s", tc.addr, got, tc.want)
			}
			if got := ssrfguard.Classify(addr).Public(); got != (tc.want == ssrfguard.CategoryPublic) {
				t.Errorf("Public() disagrees with the category %s", tc.want)
			}
		})
	}
}

// TestClassifyIgnoresZones fixes the bypass a zone would otherwise open:
// "fe80::1%eth0" is the same address as "fe80::1" and must not read as
// something else because it carries an interface name.
func TestClassifyIgnoresZones(t *testing.T) {
	t.Parallel()

	addr, err := netip.ParseAddr("fe80::1%eth0")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if got := ssrfguard.Classify(addr); got != ssrfguard.CategoryLinkLocal {
		t.Errorf("Classify(fe80::1%%eth0) = %s, want %s", got, ssrfguard.CategoryLinkLocal)
	}
}

// TestClassifyInvalidIsNotPublic: an address that could not be parsed is not
// something to connect to. The safe answer for "I do not know what this is" is
// never "go ahead".
func TestClassifyInvalidIsNotPublic(t *testing.T) {
	t.Parallel()

	if got := ssrfguard.Classify(netip.Addr{}); got.Public() {
		t.Errorf("the zero address classified as %s, which would be dialled", got)
	}
}

func TestParseTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		wantErr bool
		// contains is a fragment the message must carry, so the rejection is
		// actionable rather than merely correct.
		contains string
	}{
		{name: "http", raw: "http://example.com/health"},
		{name: "https with port", raw: "https://example.com:8443/health"},
		{name: "bare host", raw: "http://example.com"},
		{name: "ipv6 literal", raw: "http://[2606:4700::1111]:8080/"},
		{name: "empty", raw: "   ", wantErr: true, contains: "needs a url"},
		{name: "file scheme", raw: "file:///etc/passwd", wantErr: true, contains: "http or https"},
		{name: "gopher scheme", raw: "gopher://example.com/", wantErr: true, contains: "http or https"},
		{name: "no scheme", raw: "example.com/health", wantErr: true, contains: "http or https"},
		{name: "credentials", raw: "http://user:pass@example.com/", wantErr: true, contains: "credentials"},
		{name: "no host", raw: "http:///health", wantErr: true, contains: "no host"},
		{name: "bad port", raw: "http://example.com:notaport/", wantErr: true},
		{name: "unparseable", raw: "http://exa mple.com/\x7f", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target, err := ssrfguard.ParseTarget(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTarget(%q) succeeded, want a refusal", tc.raw)
				}
				if !errors.Is(err, ssrfguard.ErrInvalidURL) {
					t.Errorf("error is not ErrInvalidURL: %v", err)
				}
				if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
					t.Errorf("message %q does not carry %q", err, tc.contains)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", tc.raw, err)
			}
			if target.Hostname() == "" {
				t.Errorf("no host survived parsing %q", tc.raw)
			}
		})
	}
}

// TestParseTargetDropsFragment: a fragment never leaves a client, so keeping
// it would only make two identical monitors look different.
func TestParseTargetDropsFragment(t *testing.T) {
	t.Parallel()

	target, err := ssrfguard.ParseTarget("https://example.com/health#top")
	if err != nil {
		t.Fatalf("ParseTarget: %v", err)
	}
	if target.Fragment != "" || strings.Contains(target.String(), "#") {
		t.Errorf("the fragment survived: %s", target)
	}
}

// stubResolver answers from a table, and can answer the same question
// differently each time — which is how rebinding is tested without a DNS
// server.
type stubResolver struct {
	answers map[string][][]string
	err     error
	calls   map[string]int
}

func newStubResolver() *stubResolver {
	return &stubResolver{answers: map[string][][]string{}, calls: map[string]int{}}
}

// answer registers the replies for a host, in order. The last one repeats.
func (s *stubResolver) answer(host string, rounds ...[]string) *stubResolver {
	s.answers[host] = rounds
	return s
}

func (s *stubResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if s.err != nil {
		return nil, s.err
	}
	rounds, ok := s.answers[host]
	if !ok {
		return nil, fmt.Errorf("no such host: %s", host)
	}
	index := s.calls[host]
	s.calls[host]++
	if index >= len(rounds) {
		index = len(rounds) - 1
	}
	addrs := make([]netip.Addr, 0, len(rounds[index]))
	for _, raw := range rounds[index] {
		addrs = append(addrs, netip.MustParseAddr(raw))
	}
	return addrs, nil
}

func mustTarget(t *testing.T, raw string) *url.URL {
	t.Helper()
	target, err := ssrfguard.ParseTarget(raw)
	if err != nil {
		t.Fatalf("ParseTarget(%q): %v", raw, err)
	}
	return target
}

func TestCheck(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		url           string
		resolves      []string
		serverAllows  bool
		monitorAllows bool
		wantErr       bool
		contains      string
	}{
		{
			name: "a public host is reachable",
			url:  "https://example.com/health", resolves: []string{"93.184.216.34"},
		},
		{
			name: "a loopback literal is refused",
			url:  "http://127.0.0.1:1/", wantErr: true, contains: "loopback",
		},
		{
			name: "a private literal is refused",
			url:  "http://10.1.2.3/", wantErr: true, contains: "private",
		},
		{
			name: "the metadata endpoint is refused",
			url:  "http://169.254.169.254/latest/meta-data/", wantErr: true, contains: "link-local",
		},
		{
			name: "a host that resolves to loopback is refused",
			url:  "http://localtest.me/", resolves: []string{"127.0.0.1"},
			wantErr: true, contains: "loopback",
		},
		{
			name: "one bad address among good ones refuses the whole host",
			url:  "http://mixed.example/", resolves: []string{"93.184.216.34", "127.0.0.1"},
			wantErr: true, contains: "loopback",
		},
		{
			name: "the server switch alone is not enough",
			url:  "http://10.1.2.3/", serverAllows: true,
			wantErr: true, contains: "this monitor does not",
		},
		{
			name: "the monitor switch alone is not enough",
			url:  "http://10.1.2.3/", monitorAllows: true,
			wantErr: true, contains: "-uptime-allow-private",
		},
		{
			name: "both switches let it through",
			url:  "http://10.1.2.3/", serverAllows: true, monitorAllows: true,
		},
		{
			name: "both switches let a resolved private host through",
			url:  "http://internal.example/", resolves: []string{"192.168.1.10"},
			serverAllows: true, monitorAllows: true,
		},
		{
			name: "an ipv6 literal in brackets is unwrapped",
			url:  "http://[::1]:8080/", wantErr: true, contains: "loopback",
		},
		{
			name: "a mapped loopback is refused",
			url:  "http://disguised.example/", resolves: []string{"::ffff:127.0.0.1"},
			wantErr: true, contains: "loopback",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resolver := newStubResolver()
			target := mustTarget(t, tc.url)
			if len(tc.resolves) > 0 {
				resolver.answer(target.Hostname(), tc.resolves)
			}
			guard := ssrfguard.New(resolver, tc.serverAllows)

			err := guard.Check(context.Background(), target, tc.monitorAllows)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Check(%s) allowed it", tc.url)
				}
				if !errors.Is(err, ssrfguard.ErrBlocked) {
					t.Errorf("error is not ErrBlocked: %v", err)
				}
				if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
					t.Errorf("message %q does not carry %q", err, tc.contains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check(%s): %v", tc.url, err)
			}
		})
	}
}

func TestCheckReportsResolutionFailure(t *testing.T) {
	t.Parallel()

	guard := ssrfguard.New(newStubResolver(), false)
	err := guard.Check(context.Background(), mustTarget(t, "http://nowhere.invalid/"), false)
	if err == nil || !errors.Is(err, ssrfguard.ErrBlocked) {
		t.Fatalf("a host that does not resolve was not blocked: %v", err)
	}
	if !strings.Contains(err.Error(), "nowhere.invalid") {
		t.Errorf("the message does not name the host: %v", err)
	}
}

// TestCheckEmptyAnswer: a resolver that succeeds with no addresses is not
// permission to connect to anything.
func TestCheckEmptyAnswer(t *testing.T) {
	t.Parallel()

	resolver := newStubResolver().answer("empty.example", []string{})
	guard := ssrfguard.New(resolver, false)
	err := guard.Check(context.Background(), mustTarget(t, "http://empty.example/"), false)
	if err == nil || !strings.Contains(err.Error(), "no address") {
		t.Fatalf("an empty answer was not refused: %v", err)
	}
}

// TestAllowsPrivate covers the accessor the startup log reads.
func TestAllowsPrivate(t *testing.T) {
	t.Parallel()

	if ssrfguard.New(nil, false).AllowsPrivate() {
		t.Error("a guard built without the flag reports it as on")
	}
	if !ssrfguard.New(nil, true).AllowsPrivate() {
		t.Error("a guard built with the flag reports it as off")
	}
}

// TestDialContextConnectsToTheValidatedAddress is the test the package is for.
//
// The resolver answers a public address the first time — which is what the
// check sees — and loopback every time after, which is what an ordinary dialer
// would connect to. The connection must not happen: the dialer uses the
// address it validated and never asks again.
func TestDialContextRefusesRebinding(t *testing.T) {
	t.Parallel()

	var sockets net.ListenConfig
	listener, err := sockets.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer func() { _ = listener.Close() }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())

	// Round one: a public address. Round two onwards: the loopback listener
	// above, which is the "internal service" a rebinding attack goes for.
	resolver := newStubResolver().answer("rebind.example",
		[]string{"93.184.216.34"},
		[]string{"127.0.0.1"},
	)
	guard := ssrfguard.New(resolver, false)

	target := mustTarget(t, "http://rebind.example:"+port+"/")
	if err := guard.Check(context.Background(), target, false); err != nil {
		t.Fatalf("the first answer should have passed the check: %v", err)
	}

	conn, err := guard.DialContext(false)(context.Background(), "tcp", "rebind.example:"+port)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the dialer connected to the address the second lookup returned")
	}
	if !errors.Is(err, ssrfguard.ErrBlocked) {
		t.Errorf("the refusal is not ErrBlocked: %v", err)
	}
}

// TestDialContextConnects proves the guard is not simply refusing everything:
// an approved address is dialled, and the connection reaches the listener.
func TestDialContextConnects(t *testing.T) {
	t.Parallel()

	var sockets net.ListenConfig
	listener, err := sockets.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer func() { _ = listener.Close() }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())

	resolver := newStubResolver().answer("allowed.example", []string{"127.0.0.1"})
	// Both switches on: this is the opt-in path, and it has to work or the
	// escape hatch is decorative.
	guard := ssrfguard.New(resolver, true)

	conn, err := guard.DialContext(true)(context.Background(), "tcp", "allowed.example:"+port)
	if err != nil {
		t.Fatalf("dialling an approved address: %v", err)
	}
	_ = conn.Close()
}

// TestDialContextTriesEveryApprovedAddress: a host with two addresses where
// the first is dead must still connect through the second.
func TestDialContextTriesEveryApprovedAddress(t *testing.T) {
	t.Parallel()

	var sockets net.ListenConfig
	listener, err := sockets.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer func() { _ = listener.Close() }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())

	// ::1 on this port refuses immediately — the listener is bound to IPv4
	// only — and the second address is the live one. Both are loopback, so
	// the opt-in is on.
	resolver := newStubResolver().answer("two.example", []string{"::1", "127.0.0.1"})
	guard := ssrfguard.New(resolver, true)

	ctx, cancel := context.WithTimeout(context.Background(), ssrfguard.DialTimeout)
	defer cancel()
	conn, err := guard.DialContext(true)(ctx, "tcp", "two.example:"+port)
	if err != nil {
		t.Fatalf("the second address was never tried: %v", err)
	}
	_ = conn.Close()
}

func TestDialContextRejectsMalformedAddress(t *testing.T) {
	t.Parallel()

	guard := ssrfguard.New(newStubResolver(), false)
	_, err := guard.DialContext(false)(context.Background(), "tcp", "no-port-here")
	if err == nil || !errors.Is(err, ssrfguard.ErrInvalidURL) {
		t.Fatalf("a malformed address was not refused: %v", err)
	}
}

// TestDialContextHonoursAFamilyPin: asked for tcp4, the dialer must not fall
// back to an IPv6 address that the caller excluded.
func TestDialContextHonoursAFamilyPin(t *testing.T) {
	t.Parallel()

	resolver := newStubResolver().answer("v6only.example", []string{"2606:4700::1111"})
	guard := ssrfguard.New(resolver, false)

	_, err := guard.DialContext(false)(context.Background(), "tcp4", "v6only.example:80")
	if err == nil {
		t.Fatal("an IPv6 address was dialled for a tcp4 request")
	}
	if !strings.Contains(err.Error(), "family") {
		t.Errorf("the message does not explain the family mismatch: %v", err)
	}
}

// TestDialContextBlocksBeforeConnecting: the refusal must happen without a
// packet leaving the machine, which is what "the check is not advisory" means.
func TestDialContextBlocksALiteral(t *testing.T) {
	t.Parallel()

	guard := ssrfguard.New(newStubResolver(), false)
	_, err := guard.DialContext(false)(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil || !errors.Is(err, ssrfguard.ErrBlocked) {
		t.Fatalf("the metadata endpoint was dialled: %v", err)
	}
}

// TestDefaultResolver: New(nil, …) must produce a usable guard rather than one
// that panics the first time something is checked. The assertion is on the
// refusal of a literal, which needs no network.
func TestDefaultResolver(t *testing.T) {
	t.Parallel()

	guard := ssrfguard.New(nil, false)
	if err := guard.Check(context.Background(), mustTarget(t, "http://127.0.0.1/"), false); err == nil {
		t.Fatal("a guard with the default resolver allowed loopback")
	}
}

// TestResolverErrorsAreBlocks: a resolver that fails is not an excuse to
// connect. It is reported as a block, with the reason attached.
func TestResolverErrorsAreBlocks(t *testing.T) {
	t.Parallel()

	resolver := newStubResolver()
	resolver.err = errors.New("dns is down")
	guard := ssrfguard.New(resolver, false)

	err := guard.Check(context.Background(), mustTarget(t, "http://example.com/"), false)
	if err == nil || !errors.Is(err, ssrfguard.ErrBlocked) {
		t.Fatalf("a resolver failure was not a block: %v", err)
	}
	if !strings.Contains(err.Error(), "dns is down") {
		t.Errorf("the reason was swallowed: %v", err)
	}
}

// TestParseTargetRejectsUnparseable covers the branch where net/url itself
// refuses the string, which is a different failure from a scheme this package
// does not accept.
func TestParseTargetRejectsUnparseable(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"http://%zz/", "://example.com", "http://[::1/"} {
		if _, err := ssrfguard.ParseTarget(raw); err == nil {
			t.Errorf("ParseTarget(%q) succeeded", raw)
		} else if !errors.Is(err, ssrfguard.ErrInvalidURL) {
			t.Errorf("ParseTarget(%q) failed with %v, want ErrInvalidURL", raw, err)
		}
	}
}

// TestDialContextStopsWhenTheContextEnds: a cancelled context must end the
// walk over the remaining addresses rather than spending a timeout on each.
func TestDialContextStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()

	resolver := newStubResolver().answer("dead.example", []string{"127.0.0.1", "::1"})
	guard := ssrfguard.New(resolver, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Port 1 is not something a test can bind, so nothing answers there.
	if _, err := guard.DialContext(true)(ctx, "tcp", "dead.example:1"); err == nil {
		t.Fatal("a dial on a cancelled context succeeded")
	}
}

// TestParseTargetRejectsPortsOutOfRange covers the numeric bounds, which is
// the half of the port check that a parse error does not reach.
func TestParseTargetRejectsPortsOutOfRange(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"http://example.com:0/", "http://example.com:70000/"} {
		if _, err := ssrfguard.ParseTarget(raw); err == nil {
			t.Errorf("ParseTarget(%q) accepted the port", raw)
		}
	}
	if _, err := ssrfguard.ParseTarget("http://example.com:65535/"); err != nil {
		t.Errorf("the highest valid port was rejected: %v", err)
	}
}

// TestGuardParseTargetIsTheFunction covers the method that exists so a *Guard
// satisfies the port the use case declares.
func TestGuardParseTargetIsTheFunction(t *testing.T) {
	t.Parallel()

	guard := ssrfguard.New(newStubResolver(), false)
	target, err := guard.ParseTarget("https://example.com/health")
	if err != nil {
		t.Fatalf("ParseTarget: %v", err)
	}
	if target.Hostname() != "example.com" {
		t.Errorf("host = %q", target.Hostname())
	}
	if _, err := guard.ParseTarget("file:///etc/passwd"); err == nil {
		t.Error("the method accepted a scheme the function refuses")
	}
}
