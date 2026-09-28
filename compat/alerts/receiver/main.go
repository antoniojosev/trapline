// Command receiver stands in for every service the alerting gate delivers to.
//
// One process imitates four of them, on the routes they actually use:
//
//	/bot<token>/sendMessage   Telegram's Bot API
//	/services/...             a Slack incoming webhook
//	/api/webhooks/...         a Discord channel webhook
//	/hook                     trapline's own signed webhook
//
// Imitating the routes rather than accepting anything on "/" is the point. A
// receiver that answered every path would prove the notifier can make an HTTP
// request, which was never in doubt; these paths prove it builds the right one.
//
// The signature check is written from docs/alerts/webhooks.md and imports
// nothing from trapline. That is deliberate: a verification routine that only
// exists as a snippet in a document is one nobody has ever run, and a receiver
// that used trapline's own function would only show that the code agrees with
// itself.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxBody bounds what this will read. It is a test double, not a service, but
// an unbounded read in something that listens on a port is a habit worth not
// having.
const maxBody = 1 << 20

// delivery is one thing that arrived.
type delivery struct {
	Channel string `json:"channel"`
	Path    string `json:"path"`
	Event   string `json:"event"`
	ID      string `json:"delivery_id"`
	// Signed is nil for the channels that carry no signature, true or false
	// for the one that does.
	Signed *bool  `json:"signed"`
	Body   string `json:"body"`
	At     string `json:"at"`
}

type store struct {
	mu         sync.Mutex
	deliveries []delivery
	// bad counts signatures that did not verify. The gate asserts it is zero:
	// a receiver that accepted a forged body would make the whole signing
	// exercise decorative.
	bad int
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9604", "listen address")
	secret := flag.String("secret", "", "the signing secret of the webhook channel")
	flag.Parse()

	if *secret == "" {
		log.Fatal("receiver: -secret is required; an unverified receiver proves nothing")
	}

	kept := &store{}
	mux := http.NewServeMux()

	// Telegram: the bot token is in the path, which is Telegram's design.
	//
	// The pattern is a whole segment because net/http only allows a wildcard
	// to be one, so the "bot" prefix is checked here instead. It is checked,
	// rather than accepted: the route is the assertion, and a receiver that
	// took /anything/sendMessage would pass even if the adapter stopped
	// building Telegram's URL.
	mux.HandleFunc("POST /{botToken}/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.PathValue("botToken"), "bot") {
			http.Error(w, "telegram expects /bot<token>/sendMessage", http.StatusNotFound)
			return
		}
		kept.record(w, r, "telegram", nil)
	})
	// Slack and Discord: an incoming webhook, whose path is the credential.
	mux.HandleFunc("POST /services/{a}/{b}/{c}", func(w http.ResponseWriter, r *http.Request) {
		kept.record(w, r, "slack", nil)
	})
	mux.HandleFunc("POST /api/webhooks/{id}/{token}", func(w http.ResponseWriter, r *http.Request) {
		kept.record(w, r, "discord", nil)
	})
	// The signed one.
	mux.HandleFunc("POST /hook", func(w http.ResponseWriter, r *http.Request) {
		kept.record(w, r, "webhook", secret)
	})

	mux.HandleFunc("GET /deliveries", func(w http.ResponseWriter, _ *http.Request) {
		kept.mu.Lock()
		defer kept.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"deliveries": kept.deliveries,
			"bad":        kept.bad,
		})
	})

	// count is what a shell script can actually use: a number on a line.
	mux.HandleFunc("GET /count", func(w http.ResponseWriter, r *http.Request) {
		channel := r.URL.Query().Get("channel")
		contains := r.URL.Query().Get("contains")

		kept.mu.Lock()
		defer kept.mu.Unlock()
		matching := 0
		for _, seen := range kept.deliveries {
			if channel != "" && seen.Channel != channel {
				continue
			}
			if contains != "" && !strings.Contains(seen.Body, contains) {
				continue
			}
			matching++
		}
		fmt.Fprintln(w, matching)
	})

	mux.HandleFunc("GET /bad", func(w http.ResponseWriter, _ *http.Request) {
		kept.mu.Lock()
		defer kept.mu.Unlock()
		fmt.Fprintln(w, kept.bad)
	})

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("receiver listening on %s", *addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (s *store) record(w http.ResponseWriter, r *http.Request, channel string, secret *string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "could not read the body", http.StatusBadRequest)
		return
	}

	seen := delivery{
		Channel: channel,
		Path:    r.URL.Path,
		Event:   r.Header.Get("X-Trapline-Event"),
		ID:      r.Header.Get("X-Trapline-Delivery"),
		Body:    string(body),
		At:      time.Now().UTC().Format(time.RFC3339Nano),
	}

	if secret != nil {
		valid := verify(*secret, r.Header.Get("X-Trapline-Signature"), body, time.Now()) == nil
		seen.Signed = &valid
		if !valid {
			s.mu.Lock()
			s.bad++
			s.mu.Unlock()
			// Answered with a 400, the way a real receiver should: an
			// unverified body is not a request that happened.
			http.Error(w, "bad signature", http.StatusBadRequest)
			return
		}
	}

	s.mu.Lock()
	s.deliveries = append(s.deliveries, seen)
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// tolerance is how far the signature's timestamp may be from this clock.
// Without it, a captured request is replayable forever.
const tolerance = 5 * time.Minute

// verify is the routine documented in docs/alerts/webhooks.md, written from
// that document and nothing else.
func verify(secret, header string, body []byte, now time.Time) error {
	var timestamp, mac string
	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch key {
		case "t":
			timestamp = value
		case "v1":
			mac = value
		}
	}
	if timestamp == "" || mac == "" {
		return errors.New("expected t=<unix>,v1=<hex>")
	}

	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("the timestamp is not a unix time: %w", err)
	}
	drift := now.Sub(time.Unix(unix, 0))
	if drift < 0 {
		drift = -drift
	}
	if drift > tolerance {
		return fmt.Errorf("signed %s away from now", drift)
	}

	// The signed material is the timestamp, a dot, and the body exactly as it
	// arrived — never a re-encoding of it.
	expected := hmac.New(sha256.New, []byte(secret))
	expected.Write([]byte(timestamp))
	expected.Write([]byte("."))
	expected.Write(body)

	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(expected.Sum(nil))), []byte(mac)) != 1 {
		return errors.New("the body does not match the signature")
	}
	return nil
}
