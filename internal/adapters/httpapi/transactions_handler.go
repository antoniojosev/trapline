package httpapi

import (
	"fmt"
	"net/http"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// transactionsResponse is the performance listing.
//
// It echoes back the range and the sampling it actually used, for the same
// reason the stats endpoints echo their range: the caller may have named
// neither end, and the sampling is not a number the caller knows at all. A
// panel that showed "12 traces" without saying what share of the traffic they
// are is a panel somebody will compare against their load balancer's count and
// conclude the product is losing data (ADR 021).
type transactionsResponse struct {
	Project     projectRef                   `json:"project"`
	Range       rangeResponse                `json:"range"`
	Sort        string                       `json:"sort"`
	Total       int64                        `json:"total"`
	TotalFailed int64                        `json:"total_failed"`
	Sampling    usecase.SamplingView         `json:"sampling"`
	Rows        []usecase.TransactionSummary `json:"transactions"`
}

type transactionSeriesResponse struct {
	Project     projectRef                 `json:"project"`
	Transaction string                     `json:"transaction"`
	Range       rangeResponse              `json:"range"`
	Resolution  string                     `json:"resolution"`
	Total       int64                      `json:"total"`
	Summary     usecase.TransactionSummary `json:"summary"`
	Points      []usecase.TransactionPoint `json:"series"`
	// Examples are the stored waterfalls of this transaction, slowest first.
	// They ride along because the question after "this got slower" is always
	// "show me one", and a second round trip for it is a second round trip
	// every client would make every time.
	Examples []usecase.TraceView `json:"examples"`
}

// handleListTransactions answers "what is slow, what is busy, what is
// failing", which are the same table sorted three ways.
func (s *Server) handleListTransactions(w http.ResponseWriter, r *http.Request) {
	project, window, ok := s.statsRequest(w, r)
	if !ok {
		return
	}
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}

	order, err := domain.ParseTransactionSort(r.URL.Query().Get("sort"))
	if err != nil {
		writeError(w, err)
		return
	}

	list, err := s.transactions.List(r.Context(), project.ID, window, order, limit)
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, transactionsResponse{
		Project:     projectRef{ID: project.ID, Name: project.Name},
		Range:       newRangeResponse(list.Range),
		Sort:        string(list.Sort),
		Total:       list.Total,
		TotalFailed: list.TotalFailed,
		Sampling:    list.Sampling,
		Rows:        list.Rows,
	})
}

// handleTransactionSeries is one transaction's history at a chosen
// resolution.
//
// The resolution is the caller's rather than derived from the range, because
// the two answer different questions: minutes are for "what happened during
// the incident", hours are for "did the fix hold". Deriving it would silently
// give a reader the other one.
func (s *Server) handleTransactionSeries(w http.ResponseWriter, r *http.Request) {
	project, window, ok := s.statsRequest(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, errNamelessTransaction)
		return
	}

	resolution, err := domain.ParseResolution(r.URL.Query().Get("resolution"))
	if err != nil {
		writeError(w, err)
		return
	}

	series, err := s.transactions.Series(r.Context(), project.ID, name, window, resolution)
	if err != nil {
		writeError(w, err)
		return
	}

	examples, err := s.transactions.Examples(r.Context(), project.ID, name, exampleTraces)
	if err != nil {
		writeError(w, err)
		return
	}
	if examples == nil {
		examples = []usecase.TraceView{}
	}

	writeJSON(w, http.StatusOK, transactionSeriesResponse{
		Project:     projectRef{ID: project.ID, Name: project.Name},
		Transaction: series.Name,
		Range:       newRangeResponse(series.Range),
		Resolution:  string(series.Resolution),
		Total:       series.Total,
		Summary:     series.Summary,
		Points:      series.Points,
		Examples:    examples,
	})
}

// exampleTraces is how many stored waterfalls ride along with a series.
//
// Five, because they are there to be clicked and not to be read: the question
// is "show me a slow one", and the sixth-slowest request of a fortnight has
// never been the answer to anything.
const exampleTraces = 5

// handleGetTrace is the waterfall.
func (s *Server) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireProject(r, projectID); err != nil {
		writeError(w, err)
		return
	}

	trace, err := s.transactions.Trace(r.Context(), projectID, r.PathValue("traceID"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, trace)
}

// errNamelessTransaction is what a request for the series of nothing gets.
var errNamelessTransaction = fmt.Errorf("%w: a transaction series needs a name in the path", errBadRequest)
