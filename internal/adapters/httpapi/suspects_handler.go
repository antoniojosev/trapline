package httpapi

import (
	"net/http"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// Which change probably caused this issue (ADR 019).
//
// A route of its own rather than more fields on the issue detail, for two
// reasons. It reads a release's whole commit set and decodes a stored payload,
// which is work the issue list's reader never wants; and it is the one answer
// on that page that is a guess, so a client should be able to tell the failure
// of the guess apart from the failure of the page.

// suspectReasonResponse is one match between a commit and a frame.
type suspectReasonResponse struct {
	// Path is the file the commit touched, as the repository spells it.
	Path string `json:"path"`
	// Type is the change: A, M or D, the protocol's own vocabulary.
	Type string `json:"type"`
	// FramePath is the file the frame named, as the event spells it. Both are
	// here because they are usually spelled differently, and a reader being
	// told a commit is to blame should see what was compared.
	FramePath string `json:"frame_path"`
	// FrameDepth is the frame's distance from the failing call: 0 is the call
	// that raised.
	FrameDepth int `json:"frame_depth"`
	// Segments is how many trailing path segments matched — the strength of
	// the evidence, not a probability.
	Segments int `json:"segments"`
}

type suspectResponse struct {
	commitResponse
	// Score orders this list and means nothing outside it. Returned anyway,
	// because a client that shows three suspects should be able to show that
	// the first is far ahead rather than barely.
	Score   float64                 `json:"score"`
	Reasons []suspectReasonResponse `json:"reasons"`
}

type suspectsResponse struct {
	// Release is the release whose commits were considered: the one the issue
	// was first seen in (ADR 019).
	Release  string            `json:"release,omitempty"`
	Suspects []suspectResponse `json:"suspects"`
	// Commits are the candidates that were ruled out, bounded. Present even
	// when there is an answer, so "why not that one" is answerable without a
	// second request.
	Commits []commitResponse `json:"commits"`
	// CommitCount is what the release actually holds, which is what says
	// whether Commits was truncated.
	CommitCount int `json:"commit_count"`
	// Warning explains an empty list in terms of what to do about it: no
	// commit set, no patch set, or a stacktrace nobody has uploaded maps for.
	Warning string `json:"warning,omitempty"`
	// Symbolicated says whether the frames considered had been resolved from
	// a source map, which is what tells "this code did not change" apart from
	// "these frames are still minified".
	Symbolicated bool `json:"symbolicated"`
}

func (s *Server) handleIssueSuspects(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	issueID, err := pathID(r, "issueID")
	if err != nil {
		writeError(w, err)
		return
	}

	report, err := s.suspects.For(r.Context(), projectID, issueID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newSuspectsResponse(&report))
}

func newSuspectsResponse(report *usecase.SuspectReport) suspectsResponse {
	response := suspectsResponse{
		Release: report.Release,
		// Empty slices rather than null: a client that has to tell `null`
		// from `[]` before it can loop is a client this API made harder to
		// write for no reason.
		Suspects:     make([]suspectResponse, 0, len(report.Suspects)),
		Commits:      make([]commitResponse, 0, len(report.Commits)),
		CommitCount:  report.CommitCount,
		Warning:      report.Warning,
		Symbolicated: report.Symbolicated,
	}
	for index := range report.Suspects {
		suspect := &report.Suspects[index]
		encoded := suspectResponse{
			commitResponse: newCommitResponse(&suspect.Commit),
			Score:          suspect.Score,
			Reasons:        make([]suspectReasonResponse, 0, len(suspect.Reasons)),
		}
		for _, reason := range suspect.Reasons {
			encoded.Reasons = append(encoded.Reasons, newSuspectReasonResponse(reason))
		}
		response.Suspects = append(response.Suspects, encoded)
	}
	for index := range report.Commits {
		response.Commits = append(response.Commits, newCommitResponse(&report.Commits[index]))
	}
	return response
}

func newSuspectReasonResponse(reason domain.SuspectReason) suspectReasonResponse {
	return suspectReasonResponse{
		Path:       reason.Path,
		Type:       string(reason.ChangeType),
		FramePath:  reason.FramePath,
		FrameDepth: reason.FrameDepth,
		Segments:   reason.Segments,
	}
}
