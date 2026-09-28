package artifactbundle_test

import (
	"testing"

	"github.com/antoniojosev/trapline/internal/artifactbundle"
)

// FuzzRead throws bytes at the bundle reader.
//
// It exists for the same reason the envelope parser is fuzzed (ADR 002): this
// is a parser of a compressed container format, reading input chosen by
// whoever holds an upload token, and the two things that go wrong with such a
// parser — a panic on a malformed header, an allocation the sender chose the
// size of — are both invisible to example-based tests. The contract is narrow
// and total: Read either returns a bundle or an error wrapping ErrNotABundle,
// and it never panics.
func FuzzRead(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("PK\x03\x04"))
	f.Add(zipOfSeed(f))

	f.Fuzz(func(t *testing.T, data []byte) {
		bundle, err := artifactbundle.Read(data, artifactbundle.DefaultLimits)
		if err != nil {
			if bundle != nil {
				t.Fatal("a failed read returned a bundle as well as an error")
			}
			return
		}
		if len(bundle.Files) == 0 {
			t.Fatal("a bundle that read successfully has to carry at least one file")
		}
		// Sorted output is part of the contract: a caller storing these rows
		// in this order must get the same order out of the same archive.
		for index := 1; index < len(bundle.Files); index++ {
			if bundle.Files[index-1].Path > bundle.Files[index].Path {
				t.Fatal("the files came back unsorted")
			}
		}
	})
}

func zipOfSeed(f *testing.F) []byte {
	f.Helper()
	encoded, err := artifactbundle.Write(&artifactbundle.Bundle{
		DebugID: "9676ae7d-7fec-50bc-a1b9-085910f75007",
		Project: "venekambio",
		Files: []artifactbundle.File{{
			Path:      artifactbundle.EntryPath("bundle.min.js"),
			Kind:      artifactbundle.KindMinifiedSource,
			URL:       artifactbundle.ArtifactURL("bundle.min.js"),
			DebugID:   "fc31aec3-520c-531b-836e-0d5e872e8a18",
			SourceMap: "bundle.min.js.map",
			Content:   []byte("console.log(1)"),
		}},
	})
	if err != nil {
		f.Fatalf("building the seed: %v", err)
	}
	return encoded
}
