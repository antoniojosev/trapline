package app

import (
	"context"
	"net"
	"strings"
	"testing"
)

// closeAll is what every case here does with what it opened.
func closeAll(t *testing.T, listeners []net.Listener) {
	t.Helper()
	for _, listener := range listeners {
		_ = listener.Close()
	}
}

// The bug this exists for is not "cannot connect" — every one of these sockets
// can be connected to. It is which family the socket is *bound* to, because
// Docker Desktop's WSL2 relay mirrors only the IPv4 ones (ADR 034). So the
// assertion is on the bound address, which is the thing that was wrong.
func TestAnAddressFamilyThatWasNamedIsTheOneBound(t *testing.T) {
	for name, test := range map[string]struct {
		addr string
		want string
	}{
		"the IPv4 wildcard":  {addr: "0.0.0.0:0", want: "0.0.0.0"},
		"IPv4 loopback":      {addr: "127.0.0.1:0", want: "127.0.0.1"},
		"the IPv6 loopback":  {addr: "[::1]:0", want: "::1"},
		"the IPv6 wildcard":  {addr: "[::]:0", want: "::"},
		"a hostname resolve": {addr: "localhost:0", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			listeners, err := listen(context.Background(), test.addr)
			if err != nil {
				t.Fatalf("listening on %s: %v", test.addr, err)
			}
			defer closeAll(t, listeners)

			if len(listeners) != 1 {
				t.Fatalf("opened %d sockets for a named host, want one", len(listeners))
			}
			if test.want == "" {
				return // a hostname resolves to whatever this machine has.
			}
			host, _, err := net.SplitHostPort(listeners[0].Addr().String())
			if err != nil {
				t.Fatalf("reading the bound address: %v", err)
			}
			if host != test.want {
				t.Errorf("bound to %q, want %q: a socket on the other family is "+
					"invisible to whatever is watching for this one", host, test.want)
			}
		})
	}
}

// "Every interface" has to mean both families and not merely accept both, or
// the dual-stack socket is one relay away from being unreachable while `ss`
// says it is listening.
func TestNoHostMeansOneSocketPerFamily(t *testing.T) {
	// A port is chosen by the first listener and then reused by the second, so
	// this also covers the ordering: IPv4 first, because Go marks a "tcp6"
	// listener v6-only and a dual-stack one would already own the port.
	var config net.ListenConfig
	probe, err := config.Listen(context.Background(), "tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	_, port, err := net.SplitHostPort(probe.Addr().String())
	if err != nil {
		t.Fatalf("reading the probe's port: %v", err)
	}
	_ = probe.Close()

	listeners, err := listen(context.Background(), ":"+port)
	if err != nil {
		t.Fatalf("listening on every interface: %v", err)
	}
	defer closeAll(t, listeners)

	families := map[string]bool{}
	for _, listener := range listeners {
		host, bound, err := net.SplitHostPort(listener.Addr().String())
		if err != nil {
			t.Fatalf("reading a bound address: %v", err)
		}
		if bound != port {
			t.Errorf("a socket landed on port %s, want %s", bound, port)
		}
		if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
			families["ipv4"] = true
		} else {
			families["ipv6"] = true
		}
	}

	// IPv4 is required. IPv6 is not: a machine with it disabled is a machine
	// this must still start on, which is why a missing family is skipped
	// rather than fatal.
	if !families["ipv4"] {
		t.Errorf("no IPv4 socket among %d listeners; every container on a "+
			"Docker Desktop host would be unable to reach this", len(listeners))
	}
	if !families["ipv6"] {
		t.Log("no IPv6 socket on this machine; skipped rather than failed, which " +
			"is the documented behaviour for a family that is unavailable")
	}
}

func TestAnAddressThatIsNotOneIsRefusedByName(t *testing.T) {
	listeners, err := listen(context.Background(), "nonsense-without-a-port")
	if err == nil {
		closeAll(t, listeners)
		t.Fatal("an address with no port was accepted")
	}
	// The message has to carry the address, because the operator typed it and
	// a bare "missing port in address" leaves them guessing which of -addr,
	// TRAPLINE_ADDR or a config file it came from.
	if !strings.Contains(err.Error(), "nonsense-without-a-port") {
		t.Errorf("error %q does not name the address it rejected", err)
	}
}

func TestAPortAlreadyInUseIsRefusedBeforeAnythingStarts(t *testing.T) {
	var config net.ListenConfig
	held, err := config.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("holding a port: %v", err)
	}
	defer func() { _ = held.Close() }()

	listeners, err := listen(context.Background(), held.Addr().String())
	if err == nil {
		closeAll(t, listeners)
		t.Fatal("a port somebody else holds was accepted")
	}
	if !strings.Contains(err.Error(), held.Addr().String()) {
		t.Errorf("error %q does not name the address that was taken", err)
	}
}
