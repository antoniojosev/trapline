package httpapi

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

// bombBody builds an envelope that expands to about expandMB mebibytes.
//
// Its items are a type this server does not recognise, so the parser skips
// them and keeps reading (ADR 002) — which is the point. A bomb made of junk
// is refused by the first malformed byte and proves nothing about the budget
// it is aimed at.
func bombBody(expandMB int) string {
	const filler = 1 << 20
	item := "{\"type\":\"attachment_unknown_to_this_server\",\"length\":" +
		strconv.Itoa(filler) + "}\n" + strings.Repeat("A", filler) + "\n"

	var out strings.Builder
	out.WriteString("{}\n")
	for out.Len() < expandMB<<20 {
		out.WriteString(item)
	}
	return out.String()
}

// TestADecompressionBombIsRefusedAsTooLargeNotAsAServerError pins the answer,
// not just the refusal.
//
// It used to be 500. The envelope budget was enforced correctly — memory never
// grew past it — but the sentinel that says "the client sent too much" was
// only translated on the path that reads a header line, not on the one that
// reads an item payload. So the same condition produced 413 or 500 depending
// on which byte of the stream it landed on, and the payload path is the one a
// bomb always takes.
//
// Three things followed from the wrong code, and none of them is cosmetic: an
// SDK treats 5xx as the server's fault and retries forever, so an oversized
// envelope became a retry loop; every bomb wrote an ERROR line, which makes
// the log a cheaper attack than the memory ever was; and an operator reading
// `doctor` saw internal errors for something no part of this server had got
// wrong.
func TestADecompressionBombIsRefusedAsTooLargeNotAsAServerError(t *testing.T) {
	server := newTestServer(t)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	for _, encoding := range []string{"", "gzip"} {
		name := encoding
		if name == "" {
			name = "identity"
		}
		t.Run(name, func(t *testing.T) {
			response := sendEnvelope(t, server.URL, dsn, bombBody(24), encoding)
			if response.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413: a body that exceeds the envelope "+
					"ceiling is the client's problem and a terminal one", response.StatusCode)
			}
		})
	}
}

// TestTheIngestArenaIsReleasedWhateverTheOutcome is the invariant that decides
// whether the budget is a defence or a way to wedge the server.
//
// A hold that is not returned is permanent: the next request finds less budget
// than the one before it, and after enough refusals the endpoint answers 429
// to everybody, for ever, with nothing in flight. That failure would look
// exactly like the attack it was meant to stop.
func TestTheIngestArenaIsReleasedWhateverTheOutcome(t *testing.T) {
	stack, _ := newStack(t)
	api := NewServer(stack.Auth, stack.Projects, stack.Tokens, stack.Ingest, stack.Issues,
		stack.Stats, domain.Origin{Scheme: "https", Host: "errors.example.com"}, "test")
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	client := newClient(t, server)
	dsn := createProjectForIngest(t, client)

	total := api.limits.ingestArena.total

	// One of each outcome: accepted, refused as too large, refused as
	// malformed. All three must end with the budget whole.
	sendEnvelope(t, server.URL, dsn, "{}\n{\"type\":\"event\",\"length\":2}\n{}\n", "")
	sendEnvelope(t, server.URL, dsn, bombBody(24), "gzip")
	sendEnvelope(t, server.URL, dsn, "not an envelope at all", "")

	if free := api.limits.ingestArena.free(); free != total {
		t.Fatalf("arena has %d of %d bytes free after three requests; a hold leaked",
			free, total)
	}
}

func TestIngestArenaRefusesRatherThanQueueing(t *testing.T) {
	arena := newIngestArena(1000)

	first, ok := arena.hold(600)
	if !ok {
		t.Fatal("the first hold was refused by an empty arena")
	}
	if _, ok := arena.hold(600); ok {
		t.Fatal("a second hold fitted into a budget that had 400 bytes left")
	}
	second, ok := arena.hold(400)
	if !ok {
		t.Fatal("a hold that exactly fits was refused")
	}
	if free := arena.free(); free != 0 {
		t.Fatalf("free = %d, want 0", free)
	}

	first.release()
	second.release()
	if free := arena.free(); free != 1000 {
		t.Fatalf("free = %d after releasing everything, want 1000", free)
	}

	// Releasing twice must not manufacture budget out of nothing.
	first.release()
	if free := arena.free(); free != 1000 {
		t.Fatalf("free = %d after a double release, want 1000", free)
	}
}

// TestTheArenaChargesWhatIsReadAndNotWhatIsPossible is why this is a byte
// budget and not a count of requests.
//
// A count would have to assume every request costs the maximum, and the
// maximum is a 20 MiB envelope — which would admit one or two clients at a
// time and throttle an installation that is behaving perfectly. Charging what
// is actually produced is what lets hundreds of ordinary SDKs share the same
// budget a single bomb would exhaust on its own.
func TestTheArenaChargesWhatIsReadAndNotWhatIsPossible(t *testing.T) {
	arena := newIngestArena(10 * ingestArenaChunk)
	hold, ok := arena.hold(0)
	if !ok {
		t.Fatal("a zero-cost hold was refused")
	}

	reader := hold.wrap(bytes.NewReader(make([]byte, 100)))
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("reading 100 bytes: %v", err)
	}
	if want := int64(9 * ingestArenaChunk); arena.free() != want {
		t.Fatalf("free = %d after reading 100 bytes, want %d: a small body should "+
			"cost one chunk and not its ceiling", arena.free(), want)
	}

	hold.release()
	if arena.free() != 10*ingestArenaChunk {
		t.Fatalf("free = %d after release, want the whole arena back", arena.free())
	}
}

func TestTheArenaStopsAReaderItCannotFund(t *testing.T) {
	arena := newIngestArena(2 * ingestArenaChunk)
	hold, _ := arena.hold(0)

	// Three chunks' worth of data through a two-chunk budget.
	reader := hold.wrap(bytes.NewReader(make([]byte, 3*ingestArenaChunk)))
	_, err := io.ReadAll(reader)
	if !errors.Is(err, errIngestBusy) {
		t.Fatalf("err = %v, want errIngestBusy once the budget ran out", err)
	}
}

// TestTheArenaHoldsTheLineUnderConcurrency is the question the per-request
// ceilings could not answer.
//
// Every bound on this path was already correct for one request. What nothing
// bounded was how many requests could hold those bounds at once, and the
// answer to "what does a flood cost" was therefore "however many attackers
// there are, times 20 MiB". This asserts the shape of the fix: the total
// handed out never exceeds the budget, whatever the number of callers.
func TestTheArenaHoldsTheLineUnderConcurrency(t *testing.T) {
	const (
		budget   = 64 << 10
		each     = 8 << 10
		claimers = 200
	)
	arena := newIngestArena(budget)

	var (
		mutex    sync.Mutex
		holds    []*arenaHold
		admitted int
		wait     sync.WaitGroup
	)
	for range claimers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			hold, ok := arena.hold(each)
			if !ok {
				return
			}
			mutex.Lock()
			holds = append(holds, hold)
			admitted++
			mutex.Unlock()
		}()
	}
	wait.Wait()

	if want := budget / each; admitted != want {
		t.Fatalf("%d of %d claimers were admitted, want exactly %d: the budget is %d "+
			"bytes and each asked for %d", admitted, claimers, want, budget, each)
	}
	if free := arena.free(); free != 0 {
		t.Fatalf("free = %d with the arena full, want 0", free)
	}
	for _, hold := range holds {
		hold.release()
	}
	if free := arena.free(); free != budget {
		t.Fatalf("free = %d after every holder released, want %d", free, budget)
	}
}
