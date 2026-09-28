package channelprobe_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/channelprobe"
)

// TestProbeReachesAServerAndSendsNothing is the property the whole package
// exists for. A check that delivered a message would be run once and then
// avoided, which makes it useless on the day it matters — so the server here
// counts requests and the test fails if any arrive.
func TestProbeReachesAServerAndSendsNothing(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()

	if err := channelprobe.New().Probe(context.Background(), server.URL+"/services/T000/B000/xxx"); err != nil {
		t.Fatalf("probing a live server: %v", err)
	}
	if requests != 0 {
		t.Errorf("the probe sent %d requests; it must send none", requests)
	}
}

// TestProbeAcceptsAHostAndPort is how an SMTP channel is configured: there is
// no URL for a mail server, and inventing a scheme for one would be asking the
// notification subsystem to lie about its own configuration.
func TestProbeAcceptsAHostAndPort(t *testing.T) {
	listener := listen(t)
	defer func() { _ = listener.Close() }()

	// Accept and immediately close, the way a probe's connection is treated.
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()

	if err := channelprobe.New().Probe(context.Background(), listener.Addr().String()); err != nil {
		t.Fatalf("probing a plain TCP listener: %v", err)
	}
}

// TestProbeCompletesTheTLSHandshake is the check a channel configured a year
// ago actually fails: the host resolves, the port answers, and the certificate
// stopped being valid in March.
func TestProbeCompletesTheTLSHandshake(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the probe sent a request over TLS")
	}))
	defer server.Close()

	// The default prober does not trust the test server's ad-hoc certificate,
	// and that is exactly right: an untrusted certificate is a channel that
	// will fail on delivery, so the probe must say so.
	err := channelprobe.New().Probe(context.Background(), server.URL)
	if err == nil {
		t.Fatal("an untrusted certificate was reported as healthy")
	}
	if !strings.Contains(err.Error(), "TLS handshake") {
		t.Errorf("the failure does not name the handshake: %v", err)
	}
}

// TestProbeRejectsACertificateForAnotherName is the same check from the other
// side: a valid certificate for the wrong host is still a channel that cannot
// deliver.
func TestProbeRejectsACertificateForAnotherName(t *testing.T) {
	listener := tlsListener(t, "somewhere.else.test")
	defer func() { _ = listener.Close() }()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("reading the listener address: %v", err)
	}
	err = channelprobe.New().Probe(context.Background(), "https://"+net.JoinHostPort(host, port))
	if err == nil {
		t.Fatal("a certificate for another name was accepted")
	}
}

func TestProbeReportsWhatIsUnreachable(t *testing.T) {
	// Port 1 on loopback: nothing has ever listened there, and the connection
	// is refused immediately rather than timing out.
	err := channelprobe.New().WithTimeout(2*time.Second).
		Probe(context.Background(), "http://127.0.0.1:1/hook")
	if err == nil {
		t.Fatal("a closed port was reported as reachable")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("the failure does not name the address: %v", err)
	}
}

// TestProbeRefusesAnEndpointItCannotAddress covers the configurations that
// cannot be probed at all. Each of them is something a person could type into
// a channel form, and each has to produce a sentence naming what is wrong
// rather than a dial error about an empty address.
func TestProbeRefusesAnEndpointItCannotAddress(t *testing.T) {
	for name, endpoint := range map[string]string{
		"empty":               "",
		"blank":               "   ",
		"a bare hostname":     "smtp.example.test",
		"a port with no host": ":587",
		"a URL with no host":  "https://",
		"an unknown scheme":   "gopher://example.test/x",
	} {
		t.Run(name, func(t *testing.T) {
			if err := channelprobe.New().Probe(context.Background(), endpoint); err == nil {
				t.Errorf("%q was accepted as an endpoint", endpoint)
			}
		})
	}
}

// TestProbeHonoursItsDeadline: doctor probes every channel one after another
// while somebody waits at a terminal, so one unresponsive host must not hold
// the whole report.
func TestProbeHonoursItsDeadline(t *testing.T) {
	// A listener that accepts and then says nothing, so the TLS handshake
	// hangs until the deadline.
	listener := listen(t)
	defer func() { _ = listener.Close() }()
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		// Held open deliberately, and closed when the test's listener closes.
		<-time.After(5 * time.Second)
		_ = connection.Close()
	}()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("reading the listener address: %v", err)
	}

	started := time.Now()
	err = channelprobe.New().WithTimeout(200*time.Millisecond).
		Probe(context.Background(), "https://"+net.JoinHostPort(host, port))
	if err == nil {
		t.Fatal("a silent server completed a handshake")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the probe took %s despite a 200ms timeout", elapsed)
	}
}

func TestWithTimeoutIgnoresANonsenseValue(t *testing.T) {
	prober := channelprobe.New()
	if prober.WithTimeout(0) != prober {
		t.Error("a zero timeout produced a different prober; it would mean 'no timeout'")
	}
}

// listen opens a loopback listener on a port the kernel chooses.
func listen(t *testing.T) net.Listener {
	t.Helper()
	var sockets net.ListenConfig
	listener, err := sockets.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("opening a listener: %v", err)
	}
	return listener
}

// tlsListener starts a TLS listener holding a self-signed certificate for the
// given name.
func tlsListener(t *testing.T, name string) net.Listener {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating a certificate: %v", err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("opening a TLS listener: %v", err)
	}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				// Drive the handshake so the client sees a real failure
				// rather than a hang.
				if tlsConn, ok := connection.(*tls.Conn); ok {
					_ = tlsConn.HandshakeContext(context.Background())
				}
				_ = connection.Close()
			}()
		}
	}()
	return listener
}
