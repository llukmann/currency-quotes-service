package api

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/llukmann/currency-quotes-service/internal/api/contract"
	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// rateScale is how many decimal places a rate is served with. It is the scale
// of the numeric(20,10) column the value comes from, so the answer reproduces
// what is stored rather than an abbreviation of it: decimal.String would drop
// trailing zeros and report 20.13945 for a stored 20.1394500000.
const rateScale = 10

// newAcceptedResponse renders a freshly queued task.
func newAcceptedResponse(t domain.UpdateTask) contract.AcceptedResponse {
	return contract.AcceptedResponse{
		UpdateId: t.ID,
		Status:   contract.Status(t.Status),
	}
}

// newTaskResponse renders a task with whatever it has produced so far.
func newTaskResponse(d domain.TaskDetails) contract.TaskResponse {
	r := contract.TaskResponse{
		UpdateId: d.Task.ID,
		Pair:     string(d.Task.Pair),
		Status:   contract.Status(d.Task.Status),
	}

	if d.Task.Error != "" {
		r.Error = &d.Task.Error
	}

	// Present for exactly the tasks that completed: the finalising transaction
	// writes the quote and the status together.
	if d.Quote != nil {
		rate := formatRate(d.Quote.Rate)
		rateDate := contract.RateDate{Time: d.Quote.RateDate}
		fetchedAt := normaliseTime(d.Quote.FetchedAt)

		r.Rate = &rate
		r.RateDate = &rateDate
		r.FetchedAt = &fetchedAt
	}

	return r
}

// newQuoteResponse renders a stored rate.
func newQuoteResponse(q domain.Quote) contract.QuoteResponse {
	return contract.QuoteResponse{
		Pair:      string(q.Pair),
		Rate:      formatRate(q.Rate),
		RateDate:  contract.RateDate{Time: q.RateDate},
		FetchedAt: normaliseTime(q.FetchedAt),
		UpdateId:  q.UpdateID,
	}
}

// formatRate renders a rate as a string rather than a JSON number. A number
// would be parsed into a float64 by most clients, and a rate that survives
// that round trip is a coincidence.
func formatRate(d decimal.Decimal) string {
	return d.StringFixed(rateScale)
}

// normaliseTime is what makes the spec true of an instant on the wire: the
// generated type is a time.Time, which marshals in its own zone and with
// whatever precision it carries, while the driver hands back a timestamptz in
// the zone of the session. Truncating here also drops the fraction, since
// RFC 3339 omits an empty one.
//
// The day a rate is valid for is not passed through this: it stands for a day
// rather than an instant, and shifting the zone of a midnight would move it to
// the day before.
func normaliseTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Second)
}
