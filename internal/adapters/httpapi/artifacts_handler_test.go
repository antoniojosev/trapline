package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// This product's own artefact endpoints, as opposed to the emulated ones.
//
// They exist because of ADR 006: uploading source maps must not be an
// operation that requires installing somebody else's tool. What they receive
// is the same archive sentry-cli sends, so a bundle that works here works
// there.

func (s compatStack) postBundle(t *testing.T, path string, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		s.baseURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+s.token)
	request.Header.Set("Content-Type", "application/zip")
	request.Header.Set(CSRFHeader, "1")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func TestTheNativeUploadStoresTheSameArchive(t *testing.T) {
	stack := newCompatStack(t)

	response := stack.postBundle(t, api("/projects/1/artifacts"), testBundle(t, "", ""))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", response.StatusCode)
	}
	var stored artifactListResponse
	decodeInto(t, response, &stored)
	if len(stored.Artifacts) != 2 {
		t.Fatalf("%d artefacts stored", len(stored.Artifacts))
	}
	if stored.Budget == 0 {
		t.Error("the response says nothing about the budget, which is the one thing " +
			"a failing upload raises")
	}

	// The release and the dist ride in the query string, because the body is
	// the archive.
	response = stack.postBundle(t, api("/projects/1/artifacts?release=app@1.0.0&dist=prod"),
		testBundle(t, "", ""))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d for a release-addressed upload", response.StatusCode)
	}

	var listing artifactListResponse
	decodeInto(t, tokenRequest(t, stack.baseURL, stack.token, http.MethodGet,
		api("/projects/1/artifacts?release=app@1.0.0"), nil), &listing)
	if len(listing.Artifacts) != 2 {
		t.Fatalf("the release holds %d artefacts", len(listing.Artifacts))
	}
	if listing.Artifacts[0].Dist != "prod" {
		t.Errorf("dist = %q", listing.Artifacts[0].Dist)
	}
}

func TestDeletingThroughTheNativeAPI(t *testing.T) {
	stack := newCompatStack(t)

	var stored artifactListResponse
	decodeInto(t, stack.postBundle(t, api("/projects/1/artifacts"), testBundle(t, "", "")), &stored)

	path := api("/projects/1/artifacts/" + strconv.FormatInt(stored.Artifacts[0].ID, 10))
	if got := tokenRequest(t, stack.baseURL, stack.token, http.MethodDelete, path, nil).StatusCode; got != http.StatusNoContent {
		t.Fatalf("delete answered %d, want 204", got)
	}
	if got := tokenRequest(t, stack.baseURL, stack.token, http.MethodDelete, path, nil).StatusCode; got != http.StatusNotFound {
		t.Fatalf("deleting twice answered %d, want 404", got)
	}
	if got := tokenRequest(t, stack.baseURL, stack.token, http.MethodDelete,
		api("/projects/1/artifacts/not-a-number"), nil).StatusCode; got != http.StatusBadRequest {
		t.Fatalf("a non-numeric id answered %d, want 400", got)
	}
}

func TestWhatTheNativeUploadRefuses(t *testing.T) {
	stack := newCompatStack(t)

	if got := stack.postBundle(t, api("/projects/1/artifacts"),
		[]byte("not a zip at all")).StatusCode; got != http.StatusBadRequest {
		t.Errorf("bytes that are not an archive answered %d, want 400", got)
	}
	if got := tokenRequest(t, stack.baseURL, stack.token, http.MethodGet,
		api("/projects/1/artifacts?limit=-1"), nil).StatusCode; got != http.StatusBadRequest {
		t.Errorf("a negative limit answered %d", got)
	}
	if got := tokenRequest(t, stack.baseURL, stack.token, http.MethodGet,
		api("/projects/99/artifacts"), nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("a project that does not exist answered %d", got)
	}
}

// The upload surface reads bodies chosen by whoever holds a token, so what it
// refuses matters as much as what it accepts.
func TestMalformedUploadsAreRefusedWithAReadableReason(t *testing.T) {
	stack := newCompatStack(t)

	t.Run("a chunk part with no filename", func(t *testing.T) {
		// The filename is the checksum; without it there is nothing telling
		// the parts of one request apart, since they share a field name.
		response := stack.postParts(t, chunkUploadPath(t), []uploadPart{
			{field: "file", body: []byte("a chunk")},
		})
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", response.StatusCode)
		}
		if detail := errorDetail(t, response); !strings.Contains(detail, "sha1") {
			t.Errorf("detail = %q; it has to point at the filename, not at some other field", detail)
		}
	})

	t.Run("a body that is not multipart", func(t *testing.T) {
		response := stack.postBundle(t, chunkUploadPath(t), []byte("{}"))
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", response.StatusCode)
		}
	})

	t.Run("gzip that is not gzip", func(t *testing.T) {
		response := stack.postParts(t, chunkUploadPath(t), []uploadPart{
			{field: "file_gzip", filename: strings.Repeat("a", 40), body: []byte("plain bytes")},
		})
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", response.StatusCode)
		}
	})

	t.Run("a field this endpoint does not know", func(t *testing.T) {
		// Ignored rather than refused, like every other unknown thing on this
		// surface (ADR 013).
		response := stack.postParts(t, chunkUploadPath(t), []uploadPart{
			{field: "some_future_field", body: []byte("x")},
		})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d; an unknown field must not break an upload", response.StatusCode)
		}
	})

	t.Run("a legacy upload with no file", func(t *testing.T) {
		path := recordedPath(t, fixtureRoot+"/2.57.0/legacy-release-files",
			"002-post-api-0-projects-acme-venekambio-releases-app-1-0-0-files.json")
		response := stack.postParts(t, path, []uploadPart{
			{field: "name", body: []byte("~/bundle.min.js")},
		})
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", response.StatusCode)
		}
	})

	t.Run("a legacy upload that names no url falls back to the file name", func(t *testing.T) {
		path := recordedPath(t, fixtureRoot+"/2.57.0/legacy-release-files",
			"002-post-api-0-projects-acme-venekambio-releases-app-1-0-0-files.json")
		response := stack.postParts(t, path, []uploadPart{
			{field: "file", filename: "vendor.min.js", body: []byte("x")},
		})
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", response.StatusCode)
		}
		var stored map[string]any
		decodeInto(t, response, &stored)
		if stored["name"] != "~/vendor.min.js" {
			t.Fatalf("name = %v", stored["name"])
		}
	})

	t.Run("a legacy upload against a project that is not there", func(t *testing.T) {
		response := stack.postParts(t,
			"/api/0/projects/acme/nobody/releases/app@1.0.0/files/", []uploadPart{
				{field: "file", filename: "a.js", body: []byte("x")},
			})
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", response.StatusCode)
		}
	})

	t.Run("an assemble body that is not JSON", func(t *testing.T) {
		response := stack.postBundle(t, assemblePath(t), []byte("{"))
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", response.StatusCode)
		}
	})
}

// errorDetail reads the `detail` field the emulated surface answers with. The
// client prints it, so it is the only channel this protocol has for a sentence.
func errorDetail(t *testing.T, response *http.Response) string {
	t.Helper()
	var body struct {
		Detail string `json:"detail"`
	}
	decodeInto(t, response, &body)
	return body.Detail
}

// http.Request.MultipartReader captures r.Body at the moment it is called, so
// a MaxBytesReader installed after it bounds a stream nobody is reading. The
// limit compiles, the code reads correctly, and it does nothing — which is
// exactly the kind of defence that is discovered missing by an upload that
// should have been refused.
func TestTheBodyCeilingIsOnTheStreamThatIsRead(t *testing.T) {
	stack := newCompatStack(t)

	// One part far past the per-chunk ceiling. Whether it is the body limit or
	// the part limit that stops it does not matter; what matters is that
	// something does, before the whole thing is in memory.
	huge := bytes.Repeat([]byte("a"), maxChunkPart+4096)
	response := stack.postParts(t, chunkUploadPath(t), []uploadPart{
		{field: "file", filename: sha1Of(huge), body: huge},
	})
	if response.StatusCode < http.StatusBadRequest {
		t.Fatalf("a part past the ceiling answered %d", response.StatusCode)
	}
}
