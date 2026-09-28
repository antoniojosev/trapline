package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/ports"
)

// fakeProber answers with whatever a test decided about each endpoint.
type fakeProber struct {
	refuse  map[string]error
	visited []string
}

func newFakeProber() *fakeProber { return &fakeProber{refuse: map[string]error{}} }

func (p *fakeProber) Probe(_ context.Context, endpoint string) error {
	p.visited = append(p.visited, endpoint)
	return p.refuse[endpoint]
}

func TestChannelHealthWithoutASubsystem(t *testing.T) {
	health := NewChannelHealth(nil, newFakeProber())

	if health.Configured() {
		t.Error("a build with no notification subsystem reports itself as configured")
	}
	statuses, err := health.Check(context.Background())
	if err != nil {
		t.Fatalf("checking: %v", err)
	}
	if len(statuses) != 0 {
		t.Errorf("checked %d channels that do not exist", len(statuses))
	}
}

func TestChannelHealthChecksBothHalves(t *testing.T) {
	channels := &fakeChannels{channels: []ports.AlertChannel{
		{ID: 1, Type: "slack", Name: "#alerts", Digest: true, Endpoint: "https://hooks.test/1"},
	}}
	prober := newFakeProber()
	health := NewChannelHealth(channels, prober)

	statuses, err := health.Check(context.Background())
	if err != nil {
		t.Fatalf("checking: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("got %d statuses, want 1", len(statuses))
	}

	status := statuses[0]
	if !status.OK() || !status.Secret || !status.Reachable {
		t.Errorf("a healthy channel came back as %+v", status)
	}
	if !status.Digest {
		t.Error("the digest flag was not carried through, so an operator cannot see who asked for one")
	}
	if !strings.Contains(status.Detail, "nothing was sent") {
		t.Errorf("the detail does not say that nothing was sent: %q", status.Detail)
	}
	if len(prober.visited) != 1 || prober.visited[0] != "https://hooks.test/1" {
		t.Errorf("the prober visited %v", prober.visited)
	}
}

// TestAnUndecryptableChannelIsNotProbed is the ordering rule: reporting
// "unreachable" for a channel whose key does not open would send whoever reads
// it looking at their firewall for a missing key file.
func TestAnUndecryptableChannelIsNotProbed(t *testing.T) {
	channels := &fakeChannels{channels: []ports.AlertChannel{
		{ID: 2, Type: "telegram", Name: "ops", SecretError: "the key file is not the one this was encrypted with"},
	}}
	prober := newFakeProber()
	health := NewChannelHealth(channels, prober)

	statuses, err := health.Check(context.Background())
	if err != nil {
		t.Fatalf("checking: %v", err)
	}
	status := statuses[0]

	if status.Secret {
		t.Error("a channel that could not be decrypted reports its secret as fine")
	}
	if status.OK() {
		t.Error("a channel that could not be decrypted reports itself as healthy")
	}
	if !strings.Contains(status.Detail, "could not be decrypted") {
		t.Errorf("the detail does not name the cause: %q", status.Detail)
	}
	if len(prober.visited) != 0 {
		t.Errorf("an undecryptable channel was probed anyway: %v", prober.visited)
	}
}

func TestAnUnreachableChannelSaysWhy(t *testing.T) {
	prober := newFakeProber()
	prober.refuse["https://hooks.test/gone"] = errors.New("cannot reach hooks.test:443: no such host")
	health := NewChannelHealth(&fakeChannels{channels: []ports.AlertChannel{
		{ID: 3, Type: "discord", Name: "guild", Endpoint: "https://hooks.test/gone"},
	}}, prober)

	statuses, err := health.Check(context.Background())
	if err != nil {
		t.Fatalf("checking: %v", err)
	}
	status := statuses[0]

	if !status.Secret {
		t.Error("the secret was reported as broken when only the network was")
	}
	if status.Reachable || status.OK() {
		t.Errorf("an unreachable channel reports itself as %+v", status)
	}
	if !strings.Contains(status.Detail, "no such host") {
		t.Errorf("the detail does not carry the reason: %q", status.Detail)
	}
}

// TestWithoutAProberTheCheckSaysSo, because a check that claims a verification
// it did not perform is worse than one that admits it was skipped.
func TestWithoutAProberTheCheckSaysSo(t *testing.T) {
	health := NewChannelHealth(&fakeChannels{channels: []ports.AlertChannel{
		{ID: 4, Type: "webhook", Name: "hook", Endpoint: "https://hooks.test/4"},
	}}, nil)

	statuses, err := health.Check(context.Background())
	if err != nil {
		t.Fatalf("checking: %v", err)
	}
	if statuses[0].Reachable {
		t.Error("connectivity was claimed without anything to check it with")
	}
	if !strings.Contains(statuses[0].Detail, "not checked") {
		t.Errorf("the detail does not admit the gap: %q", statuses[0].Detail)
	}
}

func TestChannelHealthPropagatesAStoreFailure(t *testing.T) {
	health := NewChannelHealth(&fakeChannels{listErr: errors.New("the database is locked")}, newFakeProber())
	if _, err := health.Check(context.Background()); err == nil {
		t.Error("a failing store produced a clean bill of health")
	}
}
