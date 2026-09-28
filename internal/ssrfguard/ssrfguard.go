// Package ssrfguard decides whether this server may open a connection to an
// address somebody else chose.
//
// It exists for one feature — uptime monitoring — and for one reason: that
// feature takes a URL typed by a user and makes *the server* fetch it. The
// server is inside the user's network. It can reach the cloud provider's
// metadata endpoint, the admin panel nobody exposed, the database that trusts
// anything on the subnet. A monitor pointed at any of those turns an error
// tracker into a proxy for the operator's own network, and the request comes
// from an address every firewall already trusts (ADR 016).
//
// So this is a security boundary, and it is built like one:
//
//   - It is a package of its own, importing only the standard library, which
//     is what lets it be read in one sitting and tested exhaustively rather
//     than through whatever the HTTP client happens to do.
//   - Classification is a pure function of an address. Every rule about what
//     counts as reachable is a table in one file, with a name attached to each
//     entry, so a review is a review of that table and not of five call sites.
//   - The dialer is here too, and that is the part that makes the rest worth
//     anything. Checking a host and then handing the *name* to net.Dial asks
//     DNS the same question twice and connects to the second answer: a
//     resolver under an attacker's control returns a public address to the
//     check and 169.254.169.254 to the connection. That is DNS rebinding, and
//     it is not exotic — it is the standard way this exact defence is
//     defeated. DialContext resolves once, validates what it got, and connects
//     to the very address it validated.
package ssrfguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrBlocked means an address is one this server refuses to visit.
//
// A distinct sentinel rather than a generic validation error, because the
// adapters answer it differently: the API returns 422 for it, which says "the
// request was understood and this particular target is not allowed" rather
// than "you sent malformed JSON".
var ErrBlocked = errors.New("address not allowed")

// ErrInvalidURL means a URL cannot be used as a check target at all.
var ErrInvalidURL = errors.New("invalid url")

// Category is what kind of address something is.
//
// The names are the ones an operator would use when reading a rejection, and
// they are the whole of the vocabulary: everything not named here is public.
type Category string

// The categories this guard tells apart. Everything except CategoryPublic is
// refused unless both opt-ins are present.
const (
	// CategoryPublic is a globally routable unicast address, which is the
	// only kind a monitor may reach by default.
	CategoryPublic Category = "public"
	// CategoryLoopback is this machine talking to itself: 127.0.0.0/8, ::1.
	CategoryLoopback Category = "loopback"
	// CategoryPrivate is RFC 1918 and RFC 4193 — the operator's own network.
	CategoryPrivate Category = "private"
	// CategoryLinkLocal is 169.254.0.0/16 and fe80::/10. This is where the
	// cloud metadata endpoint lives (169.254.169.254), which is the single
	// most valuable target an SSRF has: it hands out instance credentials to
	// anything that asks.
	CategoryLinkLocal Category = "link-local"
	// CategoryMulticast is 224.0.0.0/4 and ff00::/8.
	CategoryMulticast Category = "multicast"
	// CategoryUnspecified is 0.0.0.0/8 and ::. On Linux, connecting to
	// 0.0.0.0 connects to localhost.
	CategoryUnspecified Category = "unspecified"
	// CategoryBroadcast is 255.255.255.255.
	CategoryBroadcast Category = "broadcast"
	// CategoryShared is 100.64.0.0/10, the carrier-grade NAT range.
	//
	// Not in the list ADR 016 enumerates, and included deliberately: it is
	// not reachable from the public internet either, so a monitor pointed at
	// it is either a mistake or an attempt to reach the provider's side of a
	// NAT. Blocking a superset of the required set is the safe direction to
	// be wrong in, and the opt-in exists for anyone who genuinely needs it.
	CategoryShared Category = "shared"
	// CategoryReserved covers the remaining ranges nothing on the public
	// internet answers on: 192.0.0.0/24, 192.0.2.0/24, 198.18.0.0/15,
	// 198.51.100.0/24, 203.0.113.0/24 and 240.0.0.0/4.
	CategoryReserved Category = "reserved"
)

// Public reports whether an address of this category may be reached without
// an opt-in.
func (c Category) Public() bool { return c == CategoryPublic }

// blocked is one refused range with the name a rejection reports.
//
// A table rather than a chain of predicate calls, because this table *is* the
// policy: reviewing this package means reviewing these rows, and a rule that
// lived in an `if` three functions away would not be reviewed at all.
var blocked = []struct {
	prefix   netip.Prefix
	category Category
}{
	// IPv4.
	{netip.MustParsePrefix("0.0.0.0/8"), CategoryUnspecified},
	{netip.MustParsePrefix("10.0.0.0/8"), CategoryPrivate},
	{netip.MustParsePrefix("100.64.0.0/10"), CategoryShared},
	{netip.MustParsePrefix("127.0.0.0/8"), CategoryLoopback},
	{netip.MustParsePrefix("169.254.0.0/16"), CategoryLinkLocal},
	{netip.MustParsePrefix("172.16.0.0/12"), CategoryPrivate},
	{netip.MustParsePrefix("192.0.0.0/24"), CategoryReserved},
	{netip.MustParsePrefix("192.0.2.0/24"), CategoryReserved},
	{netip.MustParsePrefix("192.168.0.0/16"), CategoryPrivate},
	{netip.MustParsePrefix("198.18.0.0/15"), CategoryReserved},
	{netip.MustParsePrefix("198.51.100.0/24"), CategoryReserved},
	{netip.MustParsePrefix("203.0.113.0/24"), CategoryReserved},
	{netip.MustParsePrefix("224.0.0.0/4"), CategoryMulticast},
	// Before 240.0.0.0/4, which contains it: the broadcast address has its
	// own name and an operator who typed it deserves to read it back.
	{netip.MustParsePrefix("255.255.255.255/32"), CategoryBroadcast},
	{netip.MustParsePrefix("240.0.0.0/4"), CategoryReserved},

	// IPv6. The mapped and translated forms are handled before this table is
	// consulted; what is left is the natives.
	{netip.MustParsePrefix("::/128"), CategoryUnspecified},
	{netip.MustParsePrefix("::1/128"), CategoryLoopback},
	{netip.MustParsePrefix("100::/64"), CategoryReserved},
	{netip.MustParsePrefix("2001:db8::/32"), CategoryReserved},
	{netip.MustParsePrefix("fc00::/7"), CategoryPrivate},
	{netip.MustParsePrefix("fe80::/10"), CategoryLinkLocal},
	{netip.MustParsePrefix("ff00::/8"), CategoryMulticast},
}

// nat64 is the well-known prefix that carries an IPv4 address inside an IPv6
// one (RFC 6052). A resolver that answers 64:ff9b::7f00:1 for a hostname has
// answered "127.0.0.1" in a costume.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// sixToFour is 6to4 (RFC 3056): the IPv4 address sits in bytes 2..5 of the
// IPv6 address, so 2002:7f00:0001:: is another spelling of 127.0.0.1.
var sixToFour = netip.MustParsePrefix("2002::/16")

// ipv4Compatible is the deprecated ::a.b.c.d form. Deprecated is not the same
// as unparseable: Go still resolves it and the kernel still routes it.
var ipv4Compatible = netip.MustParsePrefix("::/96")

// Classify says what kind of address this is.
//
// Every embedded form of an IPv4 address is unwrapped first — IPv4-mapped,
// IPv4-compatible, NAT64 and 6to4 — because each of them is a way to write
// 127.0.0.1 that a naive check reads as a perfectly ordinary IPv6 address.
// An invalid address is not public: a caller that cannot parse what it is
// about to connect to has no business connecting to it.
func Classify(addr netip.Addr) Category {
	if !addr.IsValid() {
		return CategoryUnspecified
	}
	// Zones say which interface an address is reached through and never
	// change what it is. Dropped so that "fe80::1%eth0" cannot spell its way
	// past a prefix comparison.
	addr = addr.WithZone("").Unmap()

	if embedded, ok := embeddedIPv4(addr); ok {
		return Classify(embedded)
	}

	for _, entry := range blocked {
		if entry.prefix.Contains(addr) {
			return entry.category
		}
	}
	return CategoryPublic
}

// embeddedIPv4 extracts the IPv4 address hidden inside a translated IPv6 one.
func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	if !addr.Is6() {
		return netip.Addr{}, false
	}
	raw := addr.As16()

	switch {
	case sixToFour.Contains(addr):
		return netip.AddrFrom4([4]byte{raw[2], raw[3], raw[4], raw[5]}), true

	case nat64.Contains(addr):
		return netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]}), true

	case ipv4Compatible.Contains(addr):
		// :: and ::1 are in this prefix and are not translated addresses;
		// they are caught by the table, which is why they are excluded here
		// rather than being read as 0.0.0.0 and 0.0.0.1.
		if addr == netip.IPv6Unspecified() || addr == netip.IPv6Loopback() {
			return netip.Addr{}, false
		}
		return netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]}), true

	default:
		return netip.Addr{}, false
	}
}

// Resolver is how the guard turns a host name into addresses. It is an
// interface so a test can drive rebinding — different answers to the same
// question — without a DNS server.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Guard validates targets and dials the ones it approves.
//
// It holds the installation-wide switch. The per-monitor switch travels with
// each call, because both have to be on and keeping them in one place would
// make it possible to satisfy the rule by setting one thing.
type Guard struct {
	resolver Resolver
	// allowPrivate is the installation's `-uptime-allow-private`. It grants
	// nothing on its own: it only makes a monitor's own opt-in effective.
	allowPrivate bool
	// timeout bounds one connection attempt.
	timeout time.Duration
}

// DialTimeout is how long one connection attempt gets. The per-check deadline
// is the monitor's own timeout, applied by the HTTP client above this; this is
// the floor that stops a single unreachable address from consuming all of it.
const DialTimeout = 10 * time.Second

// New builds a guard.
//
// allowPrivate is the installation-wide flag, and it is a constructor argument
// rather than a setter for the reason every switch in this repository is: a
// setter can be forgotten, and a security control that is off because somebody
// forgot to switch it on looks exactly like one that is on.
func New(resolver Resolver, allowPrivate bool) *Guard {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &Guard{resolver: resolver, allowPrivate: allowPrivate, timeout: DialTimeout}
}

// AllowsPrivate reports whether the installation-wide opt-in is on. The server
// logs this at startup: an installation that quietly allows its monitors into
// its own network should say so once, out loud.
func (g *Guard) AllowsPrivate() bool { return g.allowPrivate }

// ParseTarget reads a URL a monitor may be pointed at, without touching the
// network.
//
// It rejects everything that is not a plain http or https request to a named
// host: no file://, no gopher:// (the classic SSRF protocol-smuggling vector),
// no credentials in the URL — those get sent to whatever the host turns out to
// be — and no fragment, which never leaves a client anyway and only makes two
// monitors look different when they are the same.
func ParseTarget(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: a monitor needs a url", ErrInvalidURL)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidURL, err)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("%w: scheme must be http or https, got %q", ErrInvalidURL, parsed.Scheme)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: credentials in the url would be sent to whatever the host resolves to; "+
			"remove the user info", ErrInvalidURL)
	}
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("%w: no host in %q", ErrInvalidURL, trimmed)
	}
	if port := parsed.Port(); port != "" {
		// Parsed here rather than by net.LookupPort, which would also accept a
		// service name and read /etc/services to do it. A URL's port is a
		// number by definition (RFC 3986), and resolving anything at all while
		// validating a string is the kind of hidden I/O this package exists
		// not to have.
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, fmt.Errorf("%w: %q is not a port", ErrInvalidURL, port)
		}
	}
	parsed.Fragment, parsed.RawFragment = "", ""
	return parsed, nil
}

// ParseTarget is the package function as a method, so a *Guard satisfies the
// port the use case declares (ports.URLGuard) without the use case importing
// this package. The rule it enforces has nothing to do with a guard's state,
// which is why the function is the real one.
func (g *Guard) ParseTarget(raw string) (*url.URL, error) { return ParseTarget(raw) }

// Check resolves a URL's host and reports whether every address behind it may
// be visited.
//
// Every address, not the first one. A host that resolves to a public address
// and 127.0.0.1 is a host whose next connection may go to either, and there is
// no order the resolver promises. "Any address is blocked" is the rule, and it
// is the difference between a guard and a formality.
//
// This is the check that answers a person: it runs when a monitor is created,
// so a mistake is a 422 with a sentence in it rather than a monitor that will
// quietly never work. It is not what makes the connection safe — DialContext
// is, because only the address it validated is the address it connects to.
func (g *Guard) Check(ctx context.Context, target *url.URL, monitorAllowsPrivate bool) error {
	_, err := g.resolve(ctx, target.Hostname(), monitorAllowsPrivate)
	return err
}

// resolve turns a host into the addresses that may be dialled, or an error
// naming the first one that may not.
func (g *Guard) resolve(ctx context.Context, host string, monitorAllowsPrivate bool) ([]netip.Addr, error) {
	host = strings.Trim(host, "[]")

	var addrs []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		// An IP literal is not resolved. It is already the answer, and asking
		// a resolver about it would let a resolver change it.
		addrs = []netip.Addr{literal}
	} else {
		found, err := g.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("%w: %s could not be resolved: %w", ErrBlocked, host, err)
		}
		addrs = found
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: %s resolves to no address", ErrBlocked, host)
	}

	allowed := g.allowPrivate && monitorAllowsPrivate
	approved := make([]netip.Addr, 0, len(addrs))
	for _, addr := range addrs {
		normalised := addr.WithZone("").Unmap()
		category := Classify(normalised)
		if !category.Public() && !allowed {
			return nil, blockedError(host, normalised, category, g.allowPrivate, monitorAllowsPrivate)
		}
		approved = append(approved, normalised)
	}
	return approved, nil
}

// blockedError says what was refused and, precisely, what would have to change
// for it to be allowed.
//
// Both halves are named separately on purpose. "Not allowed" sends somebody
// to the documentation; "the monitor allows it but the server was not started
// with -uptime-allow-private" sends them to the thing they have to change,
// and the two opt-ins existing at all is only defensible if the message says
// which one is missing.
func blockedError(host string, addr netip.Addr, category Category, serverAllows, monitorAllows bool) error {
	where := addr.String()
	if host != where {
		where = host + " (" + addr.String() + ")"
	}

	var missing string
	switch {
	case !serverAllows && !monitorAllows:
		missing = "start the server with -uptime-allow-private and set allow_private on the monitor"
	case !serverAllows:
		missing = "the monitor allows private targets, but the server was not started with -uptime-allow-private"
	default:
		missing = "the server allows private targets, but this monitor does not: set allow_private on it"
	}

	return fmt.Errorf("%w: %s is a %s address, and this server does not visit those; %s",
		ErrBlocked, where, category, missing)
}

// DialContext returns a dialer that connects only to validated addresses.
//
// This is the whole point of the package. The address is resolved once,
// checked, and then dialled *as an address* — the host name never reaches
// net.Dial, so there is no second lookup for a hostile resolver to answer
// differently. Handing the name to the dialer after checking it is the bug
// this exists to make impossible, and it is invisible in review: the code
// reads as though the check applied to the connection.
//
// The per-monitor opt-in is bound here, when the client is built, so one
// monitor's permission cannot be used by another's request.
func (g *Guard) DialContext(monitorAllowsPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("%w: %s is not host:port", ErrInvalidURL, address)
		}
		approved, err := g.resolve(ctx, host, monitorAllowsPrivate)
		if err != nil {
			return nil, err
		}

		dialer := net.Dialer{Timeout: g.timeout}
		var lastErr error
		for _, addr := range approved {
			// The family is pinned to the address actually being dialled.
			// Asking for "tcp" with a literal works, but naming it keeps a
			// dual-stack host from being retried on a family that was never
			// validated.
			family := "tcp4"
			if addr.Is6() {
				family = "tcp6"
			}
			if strings.HasSuffix(network, "4") || strings.HasSuffix(network, "6") {
				// The caller pinned a family; honour it and skip what does
				// not match, rather than quietly widening the request.
				if family != network {
					continue
				}
			}
			conn, err := dialer.DialContext(ctx, family, net.JoinHostPort(addr.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				break
			}
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no address of the requested family (%s)", network)
		}
		return nil, fmt.Errorf("connecting to %s: %w", address, lastErr)
	}
}
