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
	// The host is a setting, the path is not: a host serving something else
	// under it would not be the same upstream.
	latestPath = "/v1/latest"

	// A rate answer is a few hundred bytes; reading more whole would let a
	// misbehaving upstream exhaust this process.
	maxBodySize = 64 << 10

	// How much of a body is quoted back in an error, which reaches the log on
	// every failed attempt.
	snippetSize = 256

	// A bare day, which parses as midnight UTC.
	rateDateLayout = "2006-01-02"
)

type Client struct {
	baseURL string
	http    *http.Client
}

var _ RateProvider = (*Client)(nil)

// The client is built here rather than taken as an argument so that
// http.DefaultClient cannot be passed by accident: it has no timeout at all, so
// an upstream that accepts a connection and then says nothing would hold a
// worker for the whole task budget instead of failing and being retried.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

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

	// The transport error already names the URL it failed on, so the wrapping
	// does not repeat it.
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

// The rate arrives as a JSON number, and decimal.Decimal unmarshals it from the
// raw token, so no float64 exists at any point of the path. That is the entire
// reason the map is typed this way: as map[string]float64 it would decode
// 20.13945000000000000123456789 to 20.139450000000000073896, and every later
// use of decimal would be decoration over a value that had already lost its
// precision.
type latestResponse struct {
	Date string `json:"date"`
	// Keyed by quote currency. One symbol is requested, so it holds one entry.
	Rates map[string]decimal.Decimal `json:"rates"`
}

func parseRate(body []byte, quote string) (Rate, error) {
	var resp latestResponse

	if err := json.Unmarshal(body, &resp); err != nil {
		return Rate{}, fmt.Errorf("decode response: %w: %s", err, snippet(body))
	}

	value, ok := resp.Rates[quote]
	if !ok {
		return Rate{}, fmt.Errorf("response carries no rate for %s: %s", quote, snippet(body))
	}

	// Also catches a null rate, which is why it is not redundant next to the
	// lookup above: "rates": {"MXN": null} unmarshals into a zero decimal with
	// no error and with the key present, and reaching the database would fail
	// the insert inside the transaction that had just closed the task as done.
	if !value.IsPositive() {
		return Rate{}, fmt.Errorf("rate for %s is not positive: %s", quote, value)
	}

	date, err := time.Parse(rateDateLayout, resp.Date)
	if err != nil {
		return Rate{}, fmt.Errorf("parse rate date %q: %w", resp.Date, err)
	}

	return Rate{Value: value, Date: date}, nil
}

// Three answers are the upstream unable to serve rather than unwilling: 408 is
// the same accident as a timeout on our side, 429 asks to be left alone, 5xx is
// failing outright. Every other rejection would be repeated word for word -- an
// unknown currency is a 404 -- so retrying buys nothing but the backoff.
func statusError(pair domain.Pair, status int, body []byte) error {
	if status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError {
		return fmt.Errorf("fetch rate %s: %w: upstream status %d: %s", pair, ErrTransient, status, snippet(body))
	}

	return fmt.Errorf("fetch rate %s: upstream status %d: %s", pair, status, snippet(body))
}

func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > snippetSize {
		return s[:snippetSize] + "..."
	}

	return s
}
