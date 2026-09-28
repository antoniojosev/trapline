package envelope

import (
	"bytes"
	"strings"
	"testing"
)

// FuzzParse drives the parser with arbitrary bytes.
//
// The ingest endpoint is public by design and authenticated only by a key that
// ships inside browser bundles, so every byte reaching this parser is
// attacker-chosen. The property being asserted is not that parsing succeeds —
// most inputs are garbage and should fail — but that it always *terminates*
// and never panics, never allocates past its limits, and never reports success
// while returning something incoherent.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"",
		"{}\n",
		"{}",
		"\n",
		"\n\n\n",
		`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}` + "\n",
		"{}\n" + `{"type":"event","length":7}` + "\n" + `{"a":1}` + "\n",
		"{}\n" + `{"type":"event"}` + "\n" + `{"a":1}`,
		"{}\n" + `{"type":"event","length":0}` + "\n\n",
		"{}\n" + `{"type":"attachment","length":4,"filename":"a"}` + "\nab\ncd\n",
		"{}\n" + `{"type":"event","length":99999}` + "\nshort",
		"{}\n" + `{"type":"event","length":-1}` + "\nx\n",
		"{}\n" + `{"type":""}` + "\nx\n",
		"{}\n" + `{"length":2}` + "\nxy\n",
		"[]\n",
		"null\n",
		"0\n",
		`{"sent_at":"not a time"}` + "\n",
		strings.Repeat("{}\n", 50),
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	// Small limits so the fuzzer explores the boundary paths cheaply instead
	// of spending its budget building megabyte inputs.
	limits := Limits{
		MaxEnvelopeBytes: 4096,
		MaxItems:         8,
		MaxItemBytes:     512,
		MaxHeaderBytes:   256,
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		envelope, err := Parse(bytes.NewReader(data), limits)
		if err != nil {
			if envelope != nil {
				t.Fatalf("an error was returned alongside a non-nil envelope: %v", err)
			}
			return
		}
		if envelope == nil {
			t.Fatal("success reported with a nil envelope")
		}

		// Success has to mean the limits held. If any of these trip, the
		// bounds are advisory rather than enforced, which on this endpoint is
		// the difference between a limit and a suggestion.
		if len(envelope.Items) > limits.MaxItems {
			t.Fatalf("%d items past the limit of %d", len(envelope.Items), limits.MaxItems)
		}
		for i, item := range envelope.Items {
			if len(item.Payload) > limits.MaxItemBytes {
				t.Fatalf("item %d payload is %d bytes, past the limit of %d",
					i, len(item.Payload), limits.MaxItemBytes)
			}
			if item.Type == "" {
				t.Fatalf("item %d parsed successfully with no type", i)
			}
		}
	})
}
