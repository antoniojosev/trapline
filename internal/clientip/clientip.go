// Package clientip decides which address a request is attributed to.
//
// It is a pure package on purpose: it reads two strings — the address of the
// direct peer and whatever `X-Forwarded-For` carried — and answers with one
// address. No HTTP types, no configuration lookup, no clock. That is what
// makes it exhaustively testable, and this is a security boundary: everything
// downstream that counts, limits or logs "the client" is counting whatever
// this function returned. A mistake here does not produce a bug, it produces a
// rate limiter that an attacker chooses the keys of (ADR 023).
package clientip

import (
	"fmt"
	"net/netip"
	"strings"
)

// Resolver attributes a request to an address, given who is allowed to speak
// on someone else's behalf.
type Resolver struct {
	// trusted holds the proxies whose X-Forwarded-For is believed, already
	// masked. Empty means believe nobody, which is the default and the only
	// safe one: an unverified forwarded header is a list of addresses the
	// attacker wrote.
	trusted []netip.Prefix
}

// New builds a resolver from the configured trusted proxies.
//
// Entries are addresses or CIDR blocks, in either family. Both forms are
// accepted because both appear in real deployments: a single reverse proxy on
// the same host is one address, while a container network or a load-balancer
// fleet is a range, and forcing an operator to expand a /24 by hand is how
// lists go stale.
func New(trustedProxies []string) (*Resolver, error) {
	prefixes := make([]netip.Prefix, 0, len(trustedProxies))
	for _, raw := range trustedProxies {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		prefix, err := parsePrefix(entry)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix)
	}
	return &Resolver{trusted: prefixes}, nil
}

// Validate reports whether a list of trusted proxies is usable, without
// building anything. Configuration calls it so a typo fails at startup with
// the offending entry named, rather than silently disabling the header
// handling an operator believed they had switched on.
func Validate(trustedProxies []string) error {
	_, err := New(trustedProxies)
	return err
}

func parsePrefix(entry string) (netip.Prefix, error) {
	if strings.Contains(entry, "/") {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("trusted proxy %q is not a valid CIDR block: %w", entry, err)
		}
		return prefix.Masked(), nil
	}

	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("trusted proxy %q is not a valid address or CIDR block: %w", entry, err)
	}
	addr = normalise(addr)
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// TrustsAnyone reports whether any proxy is trusted at all. The server logs
// this at startup: "behind a proxy but trusting nobody" and "not behind a
// proxy" look identical from inside the process and have very different
// consequences.
func (r *Resolver) TrustsAnyone() bool { return len(r.trusted) > 0 }

// Resolve returns the address a request is attributed to.
//
// remoteAddr is the direct peer, as net/http reports it. forwarded is every
// value of the X-Forwarded-For header, in the order received.
//
// The rule, in full:
//
//   - If no proxy is trusted, or the direct peer is not one of them, the
//     answer is the direct peer and the forwarded header is ignored entirely.
//     Not "used as a hint", not "logged": ignored. Anyone can send that header.
//   - If the direct peer is trusted, the chain is walked from the right,
//     skipping hops that are themselves trusted, and the first hop that is not
//     is the client. Walking from the left instead is the classic mistake: the
//     leftmost entry is whatever the original caller chose to put there.
//   - An unparseable hop stops the walk. Nothing to its left can be vouched
//     for, so the answer falls back to the direct peer.
//
// The second return value is false only when the direct peer itself is
// unparseable, which in production means net/http handed us something
// impossible; a caller should treat it as "no per-IP decision can be made".
func (r *Resolver) Resolve(remoteAddr string, forwarded []string) (netip.Addr, bool) {
	peer, ok := ParseAddr(remoteAddr)
	if !ok {
		return netip.Addr{}, false
	}
	if !r.trusts(peer) {
		return peer, true
	}

	// The last trusted hop seen, used only when every hop in the chain is
	// trusted: then the leftmost entry is the origin of the request and was
	// written by a proxy we believe, so it is both accurate and safe. Falling
	// back to the peer there would file every client behind an internal proxy
	// into a single bucket.
	var leftmostTrusted netip.Addr

	for index := len(forwarded) - 1; index >= 0; index-- {
		hops := strings.Split(forwarded[index], ",")
		for hop := len(hops) - 1; hop >= 0; hop-- {
			token := strings.TrimSpace(hops[hop])
			if token == "" {
				// A stray comma, not a hop. Skipping it is safe: it carries
				// no claim about anyone.
				continue
			}
			addr, valid := ParseAddr(token)
			if !valid {
				return peer, true
			}
			if !r.trusts(addr) {
				return addr, true
			}
			leftmostTrusted = addr
		}
	}

	if leftmostTrusted.IsValid() {
		return leftmostTrusted, true
	}
	return peer, true
}

func (r *Resolver) trusts(addr netip.Addr) bool {
	for _, prefix := range r.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// ParseAddr reads an address that may or may not carry a port.
//
// It handles every shape these two sources actually produce: net/http's
// RemoteAddr is always "host:port", while an X-Forwarded-For hop is usually a
// bare address but is sometimes given with a port by a proxy that copied the
// peer verbatim. IPv6 appears bracketed when a port is attached and bare when
// it is not, and may carry a zone.
//
// The result is normalised — IPv4-mapped IPv6 unmapped, zone dropped — so that
// one machine cannot occupy several buckets in a limiter by varying how it
// spells its own address.
func ParseAddr(raw string) (netip.Addr, bool) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return netip.Addr{}, false
	}

	switch {
	case strings.HasPrefix(host, "["):
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return netip.Addr{}, false
		}
		rest := host[end+1:]
		if rest != "" && !isPort(rest) {
			return netip.Addr{}, false
		}
		host = host[1:end]

	default:
		// Exactly one colon means host:port. More than one means a bare IPv6
		// address: the protocol requires brackets whenever a port is attached,
		// so "2001:db8::1" is an address and never an address with a port.
		if index := strings.IndexByte(host, ':'); index >= 0 && index == strings.LastIndexByte(host, ':') {
			if isPort(host[index:]) {
				host = host[:index]
			}
		}
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return normalise(addr), true
}

// isPort reports whether rest is ":" followed by at least one digit.
func isPort(rest string) bool {
	if len(rest) < 2 || rest[0] != ':' {
		return false
	}
	for _, char := range rest[1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func normalise(addr netip.Addr) netip.Addr {
	return addr.Unmap().WithZone("")
}

// IPv6BucketBits is how much of an IPv6 address identifies "one client" for
// the purposes of a limit.
//
// A /64 is the smallest block a subscriber is normally assigned, so an
// attacker who has one has 2^64 addresses to rotate through at no cost. A
// limiter keyed on the full address would therefore never see the same key
// twice, which makes it decorative. Counting the /64 is what makes an IPv6
// limit mean the same thing an IPv4 limit does — at the cost of filing several
// genuinely distinct clients behind one big residential prefix together, which
// is the right trade for a defence whose ceiling is deliberately generous.
const IPv6BucketBits = 64

// Bucket is the key a per-IP limiter counts against: the address itself for
// IPv4, its /64 for IPv6.
func Bucket(addr netip.Addr) netip.Prefix {
	if addr.Is4() {
		return netip.PrefixFrom(addr, addr.BitLen())
	}
	return netip.PrefixFrom(addr, IPv6BucketBits).Masked()
}
