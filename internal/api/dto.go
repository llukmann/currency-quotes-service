package api

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// rateScale is how many decimal places a rate is served with. It is the scale
// of the numeric(20,10) column the value comes from, so the answer reproduces
// what is stored rather than an abbreviation of it: decimal.String would drop
// trailing zeros and report 20.13945 for a stored 20.1394500000.
const rateScale = 10

// createTaskRequest is the body of POST /quotes/updates.
//
// The pair stays a string here. Validating it is the service's, so that the
// rule has one caller rather than one per endpoint, and a DTO that parsed it
// would be the second.
type createTaskRequest struct {
	Pair string `json:"pair"`
}

// acceptedResponse is the 202 body of POST /quotes/updates: the identifier a
// client polls with, and the status the task was created in.
//
// The status is the one the row actually carries rather than a literal
// "pending", and it can be any of the four: a post that deduplicates onto
// existing work may find it in progress, and one repeating a live idempotency
// key may find the task long finished, or failed.
//
// Two fields and no more, whichever of those it is. A rate served here would
// invite reading this endpoint as a synchronous quote, which is the one thing
// the asynchronous contract exists to prevent, and a reason for a failure has a
// canonical home in GET /quotes/updates/{id}; duplicating it would be a second
// place to keep true.
type acceptedResponse struct {
	UpdateID string `json:"update_id"`
	Status   string `json:"status"`
}

// taskResponse is the body of GET /quotes/updates/{id}.
//
// The last four fields depend on the status, which is why they are omitted
// when empty: a task still queued carries none of them, a completed one
// carries the rate and its two dates, a failed one the reason it failed.
type taskResponse struct {
	UpdateID  string `json:"update_id"`
	Pair      string `json:"pair"`
	Status    string `json:"status"`
	Rate      string `json:"rate,omitempty"`
	RateDate  string `json:"rate_date,omitempty"`
	FetchedAt string `json:"fetched_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

// quoteResponse is the body of GET /quotes/latest.
//
// Both timestamps are served, and neither is redundant: rate_date says how old
// the rate itself is, fetched_at when this service last confirmed it. Over a
// weekend they differ by days, and a client holding only one of them cannot
// tell a stale rate from a stale service.
type quoteResponse struct {
	Pair      string `json:"pair"`
	Rate      string `json:"rate"`
	RateDate  string `json:"rate_date"`
	FetchedAt string `json:"fetched_at"`
	UpdateID  string `json:"update_id"`
}

// newAcceptedResponse renders a freshly queued task.
func newAcceptedResponse(t domain.UpdateTask) acceptedResponse {
	return acceptedResponse{
		UpdateID: t.ID.String(),
		Status:   string(t.Status),
	}
}

// newTaskResponse renders a task with whatever it has produced so far.
func newTaskResponse(d domain.TaskDetails) taskResponse {
	r := taskResponse{
		UpdateID: d.Task.ID.String(),
		Pair:     string(d.Task.Pair),
		Status:   string(d.Task.Status),
		Error:    d.Task.Error,
	}

	// Present for exactly the tasks that completed: the finalising transaction
	// writes the quote and the status together.
	if d.Quote != nil {
		r.Rate = formatRate(d.Quote.Rate)
		r.RateDate = formatDate(d.Quote.RateDate)
		r.FetchedAt = formatTime(d.Quote.FetchedAt)
	}

	return r
}

// newQuoteResponse renders a stored rate.
func newQuoteResponse(q domain.Quote) quoteResponse {
	return quoteResponse{
		Pair:      string(q.Pair),
		Rate:      formatRate(q.Rate),
		RateDate:  formatDate(q.RateDate),
		FetchedAt: formatTime(q.FetchedAt),
		UpdateID:  q.UpdateID.String(),
	}
}

// formatRate renders a rate as a string rather than a JSON number. A number
// would be parsed into a float64 by most clients, and a rate that survives
// that round trip is a coincidence.
func formatRate(d decimal.Decimal) string {
	return d.StringFixed(rateScale)
}

// formatDate renders the day a rate is valid for. Written without a conversion
// to UTC on purpose: the value stands for a day rather than an instant, and
// shifting the zone of a midnight would move it to the day before.
func formatDate(t time.Time) string {
	return t.Format(time.DateOnly)
}

// formatTime renders an instant as RFC 3339 in UTC. The conversion is needed:
// the driver hands back a timestamptz in the zone of the session, and a Z is
// what the contract shows. RFC3339 rather than RFC3339Nano also truncates to
// the second, which is the precision this field claims.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
