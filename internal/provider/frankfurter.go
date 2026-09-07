package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/llukmann/currency-quotes-service/internal/domain"
)

const (
	// latestPath is the endpoint returning the most recent rates. The host is a
	// setting, the path is not: it belongs to this API, and a host serving
	// something else under it would not be the same upstream.
	latestPath = "/v1/latest"

	// maxBodySize caps what is read from a response. A rate answer is a few
	// hundred bytes; anything past this is not one, and reading it whole would
	// let a misbehaving upstream exhaust this process.
	maxBodySize = 64 << 10

	// snippetSize caps how much of a body is quoted back in an error. These
	// errors reach the log on every failed attempt, and the body they describe
	// is bounded only by maxBodySize.
	snippetSize = 256

	// rateDateLayout is the format of the "date" field: a bare day, which
	// parses as midnight UTC.
	rateDateLayout = "2006-01-02"
)

// Client fetches rates from the frankfurter API. It asks for exactly one
// symbol per call, since a task refreshes exactly one pair.
type Client struct {
	baseURL string
	http    *http.Client
}

var _ RateProvider = (*Client)(nil)

// NewClient returns a client reading from baseURL, which is the scheme and host
// of the API without a path, and giving each request timeout to complete.
//
// The client is built here rather than taken as an argument so that
// http.DefaultClient cannot be passed by accident: it has no timeout at all, so
// a request to an upstream that accepts a connection and then says nothing
// would hang until the context expires, holding a worker for the whole task
// budget instead of failing and letting the retry happen.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// FetchRate returns the current rate for pair.
//
// Failures the upstream may not repeat wrap ErrTransient; everything else --
// a rejected request, a body that does not parse, a rate that is not a rate --
// does not, because asking again would produce the same answer. A cancelled ctx
// is returned as itself.
func (c *Client) FetchRate(ctx context.Context, pair domain.Pair) (Rate, error) {
	base, quote := pair.Currencies()

	query := url.Values{"base": {base}, "symbols": {quote}}
	endpoint := c.baseURL + latestPath + "?" + query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Rate{}, fmt.Errorf("fetch rate %s: build request: %w", pair, err)
	}

	// The transport error already names the URL it failed on, so the log gets
	// the upstream address without this wrapping repeating it.
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Rate{}, fmt.Errorf("fetch rate %s: %w", pair, ctxErr)
		}

		return Rate{}, fmt.Errorf("fetch rate %s: %w: %w", pair, ErrTransient, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read before branching on the status: a failing upstream explains itself
	// in the body, and that explanation is what makes the log entry worth
	// having.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Rate{}, fmt.Errorf("fetch rate %s: %w", pair, ctxErr)
		}

		return Rate{}, fmt.Errorf("fetch rate %s: read body: %w: %w", pair, ErrTransient, err)
	}

	if resp.StatusCode != http.StatusOK {
		return Rate{}, statusError(pair, resp.StatusCode, body)
	}

	rate, err := parseRate(body, quote)
	if err != nil {
		return Rate{}, fmt.Errorf("fetch rate %s: %w", pair, err)
	}

	return rate, nil
}

// latestResponse is the part of the answer this service uses.
//
// The rate arrives as a JSON number, and decimal.Decimal unmarshals it from the
// raw token through NewFromString, so no float64 exists at any point of the
// path. That is the entire reason the map is typed this way: as
// map[string]float64 it would decode 20.13945000000000000123456789 to
// 20.139450000000000073896 and every later use of decimal would be decoration
// over a value that had already lost its precision.
type latestResponse struct {
	// Date is the day the rates are valid for.
	Date string `json:"date"`
	// Rates is keyed by quote currency. One symbol is requested, so it holds
	// one entry.
	Rates map[string]decimal.Decimal `json:"rates"`
}

// parseRate reads the rate for quote out of a successful response body.
func parseRate(body []byte, quote string) (Rate, error) {
	var resp latestResponse

	if err := json.Unmarshal(body, &resp); err != nil {
		return Rate{}, fmt.Errorf("decode response: %w: %s", err, snippet(body))
	}

	value, ok := resp.Rates[quote]
	if !ok {
		return Rate{}, fmt.Errorf("response carries no rate for %s: %s", quote, snippet(body))
	}

	// This also catches a null rate, which is why it is not redundant next to
	// the lookup above or the CHECK on the column: "rates": {"MXN": null}
	// unmarshals into a zero decimal with no error and with the key present, so
	// nothing else here sees it. Reaching the database instead would fail the
	// insert after the task had already been closed as done, inside the same
	// transaction that closed it.
	if !value.IsPositive() {
		return Rate{}, fmt.Errorf("rate for %s is not positive: %s", quote, value)
	}

	date, err := time.Parse(rateDateLayout, resp.Date)
	if err != nil {
		return Rate{}, fmt.Errorf("parse rate date %q: %w", resp.Date, err)
	}

	return Rate{Value: value, Date: date}, nil
}

// statusError classifies a response that is not 200. Too many requests and the
// server's own failures are the upstream being momentarily unable to answer;
// everything else is a request it will reject just as firmly next time -- a
// currency it does not know is answered with 404 -- so retrying would only
// delay the failure by the whole backoff.
func statusError(pair domain.Pair, status int, body []byte) error {
	if status == http.StatusTooManyRequests || status >= http.StatusInternalServerError {
		return fmt.Errorf("fetch rate %s: %w: upstream status %d: %s", pair, ErrTransient, status, snippet(body))
	}

	return fmt.Errorf("fetch rate %s: upstream status %d: %s", pair, status, snippet(body))
}

// snippet shortens a body for an error message.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > snippetSize {
		return s[:snippetSize] + "..."
	}

	return s
}
