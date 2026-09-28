package compression

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestRoundTrip(t *testing.T) {
	payload := []byte(`{"exception":{"values":[{"type":"ValueError","value":"algo"}]}}`)

	compressed := Compress(payload)
	decompressed, err := Decompress(compressed)
	if err != nil {
		t.Fatalf("decompressing: %v", err)
	}
	if !bytes.Equal(decompressed, payload) {
		t.Errorf("round trip changed the payload")
	}
}

func TestCompressionActuallyShrinksJSON(t *testing.T) {
	// The claim that storing whole payloads is affordable rests on this.
	payload := []byte(strings.Repeat(`{"key":"value","other":"thing"},`, 200))

	compressed := Compress(payload)
	if len(compressed) >= len(payload)/2 {
		t.Errorf("compressed %d bytes to %d; JSON should compress far better", len(payload), len(compressed))
	}
}

func TestDecompressRejectsGarbage(t *testing.T) {
	if _, err := Decompress([]byte("no soy zstd")); err == nil {
		t.Error("garbage decompressed without an error")
	}
}

func TestEmptyPayloadRoundTrips(t *testing.T) {
	decompressed, err := Decompress(Compress(nil))
	if err != nil {
		t.Fatalf("decompressing: %v", err)
	}
	if len(decompressed) != 0 {
		t.Errorf("got %d bytes back from an empty payload", len(decompressed))
	}
}

func TestDecompressRequestIdentity(t *testing.T) {
	for _, encoding := range []string{"", "identity", " IDENTITY "} {
		t.Run(encoding, func(t *testing.T) {
			reader, err := DecompressRequest(strings.NewReader("hola"), encoding, 1024)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("reading: %v", err)
			}
			if string(body) != "hola" {
				t.Errorf("body = %q", body)
			}
		})
	}
}

func TestDecompressRequestGzip(t *testing.T) {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte("contenido comprimido")); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	reader, err := DecompressRequest(&buffer, "gzip", 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(body) != "contenido comprimido" {
		t.Errorf("body = %q", body)
	}
}

func TestDecompressRequestZstd(t *testing.T) {
	compressed := Compress([]byte("contenido zstd"))

	reader, err := DecompressRequest(bytes.NewReader(compressed), "zstd", 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(body) != "contenido zstd" {
		t.Errorf("body = %q", body)
	}
}

func TestDecompressionBombIsCapped(t *testing.T) {
	// A few kilobytes of gzip expanding into gigabytes is the classic attack
	// against an endpoint that accepts compressed bodies from unauthenticated
	// clients. The cap is on what the decompressor may PRODUCE, which is a
	// different limit from what it may consume.
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(bytes.Repeat([]byte("A"), 10<<20)); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	t.Logf("%d compressed bytes expand to 10 MiB", buffer.Len())

	reader, err := DecompressRequest(&buffer, "gzip", 4096)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	read, err := io.Copy(io.Discard, reader)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("error = %v, want ErrTooLarge", err)
	}
	if read > 4096 {
		t.Errorf("%d bytes got through a 4096 byte cap", read)
	}
}

func TestUnsupportedEncodings(t *testing.T) {
	// Every codec accepted here is another decompressor exposed to hostile
	// input, so the list is short on purpose.
	for _, encoding := range []string{"deflate", "br", "compress", "algo-raro"} {
		t.Run(encoding, func(t *testing.T) {
			if _, err := DecompressRequest(strings.NewReader("x"), encoding, 1024); err == nil {
				t.Errorf("%q was accepted", encoding)
			}
		})
	}
}

func TestIdentityBodyIsAlsoCapped(t *testing.T) {
	reader, err := DecompressRequest(strings.NewReader(strings.Repeat("x", 10000)), "", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(body) != 100 {
		t.Errorf("read %d bytes through a 100 byte cap", len(body))
	}
}

func TestABudgetIsRequired(t *testing.T) {
	// Zero would widen into "no limit" for the decoder, which is the opposite
	// of what this parameter exists for.
	for _, budget := range []int64{0, -1} {
		if _, err := DecompressRequest(strings.NewReader("x"), "gzip", budget); !errors.Is(err, ErrTooLarge) {
			t.Errorf("budget %d was accepted", budget)
		}
	}
}

// TestTheWorkingSetIsWhatADecompressorCostsBeforeItProducesAnything is the
// figure the ingest budget reserves up front.
//
// It has to be an over-estimate of the real cost and a bound the decoder
// actually enforces, because the caller pays it before the decompressor
// exists — a zstd decoder allocates its history buffer from the window the
// frame declares, so anything checked afterwards is checked too late
// (ADR 039).
func TestTheWorkingSetIsWhatADecompressorCostsBeforeItProducesAnything(t *testing.T) {
	if got := WorkingSet("zstd"); got != MaxRequestWindow {
		t.Errorf("zstd working set = %d, want the window ceiling %d",
			got, MaxRequestWindow)
	}
	if got := WorkingSet("gzip"); got <= 0 || got >= MaxRequestWindow {
		t.Errorf("gzip working set = %d; deflate's window is 32 KiB, so it should be "+
			"small and non-zero", got)
	}
	// Case and whitespace come off the wire exactly as a client typed them.
	if WorkingSet(" GZIP ") != WorkingSet("gzip") {
		t.Error("the working set depends on the spelling of the header")
	}
	if got := WorkingSet(""); got != 0 {
		t.Errorf("an uncompressed body was charged %d for a decompressor it does not have", got)
	}
}

// TestAZstdFrameThatDeclaresAnOversizedWindowIsRefused is the other half of
// the same claim.
//
// Without the window cap the library clamps the maximum window to the maximum
// *decoded size* — 20 MiB here — so a request of a thousand bytes could make
// this process reserve 20 MiB, and a hundred of them 2 GB. The refusal has to
// be a refusal and not a silent success, or the reserved figure above is
// fiction.
func TestAZstdFrameThatDeclaresAnOversizedWindowIsRefused(t *testing.T) {
	var encoded bytes.Buffer
	writer, err := zstd.NewWriter(&encoded,
		zstd.WithWindowSize(MaxRequestWindow*2),
		zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatalf("building an encoder with an oversized window: %v", err)
	}
	// Larger than the window, so the frame cannot shrink its own declaration
	// down to the content size.
	if _, err := writer.Write(bytes.Repeat([]byte("trapline"), (MaxRequestWindow*3)/8)); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	reader, err := DecompressRequest(bytes.NewReader(encoded.Bytes()), "zstd", 20<<20)
	if err == nil {
		_, err = io.ReadAll(reader)
	}
	if err == nil {
		t.Fatal("a frame declaring a 16 MiB window was accepted; the window ceiling is not applied")
	}
}
