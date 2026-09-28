// Package compression compresses stored payloads and decompresses request
// bodies.
//
// Two directions with different threat models, and they must not be confused:
// compressing our own payloads is trusted work on data we produced, while
// decompressing a request body is untrusted work on bytes chosen by whoever is
// talking to the public ingest endpoint. Only the second needs a bomb guard,
// and it is the reason this package exists rather than the calls being
// scattered around.
package compression

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// ErrTooLarge means a decompressed body exceeded its budget.
var ErrTooLarge = errors.New("decompressed body too large")

// maxDecodedPayload bounds what the shared decoder will expand a stored
// payload into. Stored payloads are ours and already bounded by the ingest
// limits, so this is a guard against a corrupted row rather than an attacker.
const maxDecodedPayload = 32 << 20

// Codec names the compression used for a stored payload, written next to the
// blob so a future change of codec can be read back instead of needing every
// old row rewritten.
const Codec = "zstd"

// Compressing our own payloads is a hot path — once per ingested event — so
// the encoder and decoder are built once and shared. Both are safe for
// concurrent use.
var (
	encoderOnce sync.Once
	encoder     *zstd.Encoder
	decoderOnce sync.Once
	decoder     *zstd.Decoder
)

// Compress packs a stored payload.
//
// SpeedDefault rather than best compression: this runs on every ingested
// event, and event payloads are JSON, which even a fast level compresses to a
// fraction of its size. Spending more CPU per event to save a few more bytes
// on a store measured in gigabytes is the wrong trade for a product whose
// promise is a small footprint.
func Compress(payload []byte) []byte {
	encoderOnce.Do(func() {
		built, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			// One worker, not one per core. The library defaults to
			// GOMAXPROCS workers, each with its own window: on a 24-core
			// machine that is tens of megabytes of resident memory reserved
			// before a single event arrives, which broke the product's
			// footprint budget the moment ingestion landed. Compression here
			// is a per-event operation on a small payload, so the parallelism
			// buys nothing anyway.
			zstd.WithEncoderConcurrency(1),
		)
		if err != nil {
			// The only error here is an invalid option, which is a
			// programming mistake in this file rather than a runtime
			// condition, so there is nothing a caller could do with it.
			panic("compression: building the zstd encoder: " + err.Error())
		}
		encoder = built
	})
	return encoder.EncodeAll(payload, nil)
}

// Decompress unpacks a stored payload.
func Decompress(payload []byte) ([]byte, error) {
	decoderOnce.Do(func() {
		built, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			// Stored payloads are ours and bounded by the ingest limits, so a
			// small window is enough and keeps the reserved memory small.
			zstd.WithDecoderMaxMemory(maxDecodedPayload),
		)
		if err != nil {
			panic("compression: building the zstd decoder: " + err.Error())
		}
		decoder = built
	})
	decompressed, err := decoder.DecodeAll(payload, nil)
	if err != nil {
		return nil, fmt.Errorf("decompressing payload: %w", err)
	}
	return decompressed, nil
}

// MaxRequestWindow bounds the history buffer a request's decompressor may
// allocate.
//
// A zstd frame declares its own window size and the decoder allocates a
// history buffer that large before it produces a byte. Left to the library's
// clamp, that ceiling is whatever maximum decoded size we asked for — 20 MiB
// here — so a 1 179-byte request could make this process reserve 20 MiB, and
// a hundred of them 2 GB. Eight MiB is the largest window any mainstream
// encoder produces at its default or best settings, so nothing that a real
// client sends is refused by this, and a frame that declares more is refused
// with an error rather than honoured.
//
// Deflate's window is 32 KiB and is not negotiable, so this bound only
// concerns zstd; WorkingSet reports the difference.
const MaxRequestWindow = 8 << 20

// WorkingSet is what a decompressor for this encoding may allocate before it
// has produced a single byte of output.
//
// It exists so a caller can reserve that cost against a budget *before*
// building the decompressor, rather than discovering it as resident memory
// afterwards. The whole reason the ingest path needs the figure is that the
// per-request ceilings answer "what does one cost" and nothing was answering
// "what do a thousand cost" (ADR 039).
func WorkingSet(contentEncoding string) int64 {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "zstd":
		return MaxRequestWindow
	case "gzip", "x-gzip":
		// A 32 KiB history window plus the library's own buffers.
		return 64 << 10
	default:
		return 0
	}
}

// DecompressRequest wraps a request body according to its Content-Encoding,
// bounded by maxBytes.
//
// The bound is the point. A few kilobytes of gzip can expand into gigabytes —
// the classic decompression bomb — and this endpoint accepts compressed bodies
// from unauthenticated clients by design. Reading through a limit means the
// expansion is capped no matter what the header claims (SECURITY.md).
func DecompressRequest(body io.Reader, contentEncoding string, maxBytes int64) (io.Reader, error) {
	// A non-positive budget would mean "no limit" once widened for the
	// decoder, which is the opposite of what this parameter is for.
	if maxBytes <= 0 {
		return nil, fmt.Errorf("%w: a decompression budget is required", ErrTooLarge)
	}
	decoderBudget := uint64(maxBytes)

	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return io.LimitReader(body, maxBytes), nil

	case "gzip", "x-gzip":
		reader, err := gzip.NewReader(io.LimitReader(body, maxBytes))
		if err != nil {
			return nil, fmt.Errorf("reading gzip body: %w", err)
		}
		return &boundedReader{r: reader, remaining: maxBytes, closer: reader}, nil

	case "zstd":
		// Built per request rather than shared: a streaming decoder holds
		// per-stream state, and one shared across concurrent requests would
		// interleave them. Concurrency and window are pinned for the same
		// reason as above — this must not reserve per-core memory on a
		// product whose first promise is a small footprint.
		reader, err := zstd.NewReader(io.LimitReader(body, maxBytes),
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(decoderBudget),
			// Without this the window ceiling silently becomes the decoded-size
			// ceiling, so the cheapest possible request reserves the most
			// expensive possible buffer. See MaxRequestWindow.
			zstd.WithDecoderMaxWindow(MaxRequestWindow),
		)
		if err != nil {
			return nil, fmt.Errorf("reading zstd body: %w", err)
		}
		return &boundedReader{r: reader.IOReadCloser(), remaining: maxBytes, closer: reader.IOReadCloser()}, nil

	case "deflate":
		// Deliberately unsupported: no official SDK sends it, and every codec
		// accepted here is another decompressor exposed to hostile input.
		return nil, fmt.Errorf("unsupported content encoding %q", contentEncoding)

	default:
		return nil, fmt.Errorf("unsupported content encoding %q", contentEncoding)
	}
}

// boundedReader caps how much a decompressor may produce, which is a different
// limit from how much it may consume.
type boundedReader struct {
	r         io.Reader
	remaining int64
	closer    io.Closer
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("reading decompressed body: %w", err)
	}
	return n, err //nolint:wrapcheck // io.EOF must reach the caller unwrapped.
}

// Close releases the decompressor.
func (b *boundedReader) Close() error {
	if b.closer == nil {
		return nil
	}
	if err := b.closer.Close(); err != nil {
		return fmt.Errorf("closing decompressor: %w", err)
	}
	return nil
}
