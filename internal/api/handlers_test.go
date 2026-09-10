package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/llukmann/currency-quotes-service/internal/api/contract"
	"github.com/llukmann/currency-quotes-service/internal/domain"
)

// headerIdempotencyKey names one intent of one client. The spec declares it;
// this is the only place left in the package that has to spell it.
const headerIdempotencyKey = "Idempotency-Key"

// creation is one call of CreateTask. The pair is recorded as it reached the
// service: parsing happens at the boundary, so what arrives here is already
// normalised and cannot be anything else.
type creation struct {
	pair domain.Pair
	key  *uuid.UUID
}

// fakeService is the business layer as the handlers see it. Written by hand:
// the interface is three methods, and what the tests are about is the record
// of what was asked of it and the codes its answers turn into.
type fakeService struct {
	task      domain.UpdateTask
	createErr error

	details domain.TaskDetails
	getErr  error

	quote    domain.Quote
	quoteErr error

	// before runs at the start of every call, with the request's own context,
	// so a test can panic from inside a handler or wait for its deadline.
	before func(ctx context.Context)

	created []creation
	fetched []uuid.UUID
	looked  []domain.Pair
}

func (f *fakeService) CreateTask(ctx context.Context, pair domain.Pair, key *uuid.UUID) (domain.UpdateTask, error) {
	f.created = append(f.created, creation{pair: pair, key: key})

	if f.before != nil {
		f.before(ctx)
	}

	if f.createErr != nil {
		return domain.UpdateTask{}, f.createErr
	}

	return f.task, nil
}

func (f *fakeService) GetTask(ctx context.Context, id uuid.UUID) (domain.TaskDetails, error) {
	f.fetched = append(f.fetched, id)

	if f.before != nil {
		f.before(ctx)
	}

	if f.getErr != nil {
		return domain.TaskDetails{}, f.getErr
	}

	return f.details, nil
}

func (f *fakeService) GetLatestQuote(ctx context.Context, pair domain.Pair) (domain.Quote, error) {
	f.looked = append(f.looked, pair)

	if f.before != nil {
		f.before(ctx)
	}

	if f.quoteErr != nil {
		return domain.Quote{}, f.quoteErr
	}

	return f.quote, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func serve(t *testing.T, svc quoteService, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	NewRouter(svc, discardLogger(), 5*time.Second).ServeHTTP(w, r)

	return w
}

// A client branches on the code, so the code is what a test pins.
func requireEnvelope(t *testing.T, w *httptest.ResponseRecorder, status int, code contract.ErrorCode) {
	t.Helper()

	require.Equal(t, status, w.Code)
	require.Equal(t, contentTypeJSON, w.Header().Get("Content-Type"))

	var got contract.Error
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, code, got.Error.Code)
	require.NotEmpty(t, got.Error.Message)
}

func testTask(status domain.Status) domain.UpdateTask {
	return domain.UpdateTask{
		ID:       uuid.MustParse("6f1a2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d"),
		Pair:     domain.Pair("EUR/MXN"),
		Status:   status,
		Attempts: 1,
	}
}

// What a post answers with, including the two cases where the task in the body
// is not the one this request created: a replayed key and a post that landed
// on a refresh already running. The status served is the one the row carries
// now, which is how a client retrying after a failure learns to send a fresh
// key instead of polling forever.
func TestCreateTaskAccepted(t *testing.T) {
	key := uuid.MustParse("0a1b2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d")

	tests := []struct {
		name   string
		body   string
		header string
		status domain.Status

		wantPair domain.Pair
		wantKey  *uuid.UUID
	}{
		{
			name:     "a queued task is answered with its identifier",
			body:     `{"pair":"EUR/MXN"}`,
			status:   domain.StatusPending,
			wantPair: "EUR/MXN",
		},
		{
			// Case is not part of a pair, and the service is handed the pair
			// the domain made of it rather than the one the client typed.
			name:     "a lower case pair reaches the service parsed",
			body:     `{"pair":"eur/mxn"}`,
			status:   domain.StatusPending,
			wantPair: "EUR/MXN",
		},
		{
			name:     "a key is parsed and handed on",
			body:     `{"pair":"EUR/MXN"}`,
			header:   key.String(),
			status:   domain.StatusPending,
			wantPair: "EUR/MXN",
			wantKey:  &key,
		},
		{
			name:     "a post landing on a running refresh is answered with it",
			body:     `{"pair":"EUR/MXN"}`,
			status:   domain.StatusInProgress,
			wantPair: "EUR/MXN",
		},
		{
			name:     "a replayed key is answered with the finished task",
			body:     `{"pair":"EUR/MXN"}`,
			header:   key.String(),
			status:   domain.StatusDone,
			wantPair: "EUR/MXN",
			wantKey:  &key,
		},
		{
			name:     "a replayed key is answered with a failed task as well",
			body:     `{"pair":"EUR/MXN"}`,
			header:   key.String(),
			status:   domain.StatusFailed,
			wantPair: "EUR/MXN",
			wantKey:  &key,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{task: testTask(tt.status)}

			r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(tt.body))
			if tt.header != "" {
				r.Header.Set(headerIdempotencyKey, tt.header)
			}

			w := serve(t, svc, r)

			require.Equal(t, http.StatusAccepted, w.Code)
			require.Equal(t, contentTypeJSON, w.Header().Get("Content-Type"))

			// Two fields and no more: a rate here would invite reading this
			// endpoint as a synchronous quote.
			require.JSONEq(t, `{
				"update_id": "6f1a2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d",
				"status": "`+string(tt.status)+`"
			}`, w.Body.String())

			require.Equal(t, []creation{{pair: tt.wantPair, key: tt.wantKey}}, svc.created)
		})
	}
}

// Every way a post is turned away. The codes are the point: 400 says the
// request is wrong and 409 says the request is fine but the history behind the
// key is not, and a client reading the code alone has to be able to tell those
// apart.
func TestCreateTaskRefused(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		header string
		// createErr is what the service answers with, nil when the request is
		// expected not to reach it at all.
		createErr error

		wantStatus int
		wantCode   contract.ErrorCode
		wantCalled bool
	}{
		{
			name:       "a body that is not JSON",
			body:       `not json`,
			wantStatus: http.StatusBadRequest,
			wantCode:   contract.ErrorCodeInvalidRequest,
		},
		{
			name:       "a truncated body",
			body:       `{"pair":`,
			wantStatus: http.StatusBadRequest,
			wantCode:   contract.ErrorCodeInvalidRequest,
		},
		{
			name:       "an empty body",
			body:       ``,
			wantStatus: http.StatusBadRequest,
			wantCode:   contract.ErrorCodeInvalidRequest,
		},
		{
			// Refused by the reader rather than buffered: the only body this
			// API reads holds a currency pair.
			name:       "a body over the size limit",
			body:       `{"pair":"` + strings.Repeat("E", maxRequestBody) + `"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   contract.ErrorCodeInvalidRequest,
		},
		{
			// Held to the contract by the generated binder, which is what
			// bounds the size of something a client controls before it
			// reaches a column.
			name:       "a key that is not a UUID",
			body:       `{"pair":"EUR/MXN"}`,
			header:     "not-a-uuid",
			wantStatus: http.StatusBadRequest,
			wantCode:   contract.ErrorCodeInvalidRequest,
		},
		{
			// Refused at the boundary, so the service never hears about it.
			name:       "an unsupported pair",
			body:       `{"pair":"EUR/RUB"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   contract.ErrorCodeInvalidPair,
		},
		{
			// A missing field arrives as an empty string and is refused by the
			// same rule, with the same code: both mean the request named no
			// pair this service can quote.
			name:       "a body with no pair in it",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   contract.ErrorCodeInvalidPair,
		},
		{
			name:       "a key already spent on another pair",
			body:       `{"pair":"EUR/MXN"}`,
			header:     uuid.NewString(),
			createErr:  domain.ErrKeyConflict,
			wantStatus: http.StatusConflict,
			wantCode:   contract.ErrorCodeKeyConflict,
			wantCalled: true,
		},
		{
			name:       "a failure of ours",
			body:       `{"pair":"EUR/MXN"}`,
			createErr:  errors.New("connection refused"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   contract.ErrorCodeInternalError,
			wantCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{task: testTask(domain.StatusPending), createErr: tt.createErr}

			r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(tt.body))
			if tt.header != "" {
				r.Header.Set(headerIdempotencyKey, tt.header)
			}

			w := serve(t, svc, r)

			requireEnvelope(t, w, tt.wantStatus, tt.wantCode)
			require.Len(t, svc.created, boolToInt(tt.wantCalled))
		})
	}
}

// Where a failure of ours stops. A driver's message, a statement or a
// connection string says nothing to whoever made the request and something to
// whoever did not, so the client gets the code alone.
func TestCreateTaskHidesTheCause(t *testing.T) {
	svc := &fakeService{createErr: errors.New(`dial tcp 10.0.0.7:5432: connect: connection refused`)}

	r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(`{"pair":"EUR/MXN"}`))
	w := serve(t, svc, r)

	requireEnvelope(t, w, http.StatusInternalServerError, contract.ErrorCodeInternalError)

	body := w.Body.String()
	require.NotContains(t, body, "10.0.0.7")
	require.NotContains(t, body, "connection refused")
	require.Contains(t, body, internalErrorMessage)
}

// The endpoint a client polls. An unfinished task is a 200 carrying the status
// it is actually in, never a 404: the update exists, and saying it does not
// would tell a client to stop asking.
func TestGetTask(t *testing.T) {
	id := uuid.MustParse("6f1a2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d")

	quote := &domain.Quote{
		UpdateID: id,
		Pair:     domain.Pair("EUR/MXN"),
		// Stored in a numeric(20,10), and served with every one of those
		// places: decimal.String would report 19.6552 for it.
		Rate:      decimal.RequireFromString("19.6552"),
		RateDate:  time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		FetchedAt: time.Date(2026, 9, 9, 10, 30, 15, 0, time.UTC),
	}

	tests := []struct {
		name    string
		details domain.TaskDetails

		wantBody string
	}{
		{
			name: "a task still queued carries the status alone",
			details: domain.TaskDetails{
				Task: domain.UpdateTask{ID: id, Pair: domain.Pair("EUR/MXN"), Status: domain.StatusPending},
			},
			wantBody: `{
				"update_id": "6f1a2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d",
				"pair": "EUR/MXN",
				"status": "pending"
			}`,
		},
		{
			name: "a task in progress is answered the same way",
			details: domain.TaskDetails{
				Task: domain.UpdateTask{
					ID: id, Pair: domain.Pair("EUR/MXN"), Status: domain.StatusInProgress, Attempts: 1,
				},
			},
			wantBody: `{
				"update_id": "6f1a2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d",
				"pair": "EUR/MXN",
				"status": "in_progress"
			}`,
		},
		{
			name: "a completed task carries the rate and both dates",
			details: domain.TaskDetails{
				Task: domain.UpdateTask{
					ID: id, Pair: domain.Pair("EUR/MXN"), Status: domain.StatusDone, Attempts: 1,
				},
				Quote: quote,
			},
			wantBody: `{
				"update_id": "6f1a2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d",
				"pair": "EUR/MXN",
				"status": "done",
				"rate": "19.6552000000",
				"rate_date": "2026-09-07",
				"fetched_at": "2026-09-09T10:30:15Z"
			}`,
		},
		{
			// A 200 as well: the request worked, and what it found is that the
			// refresh did not.
			name: "a failed task carries the reason instead",
			details: domain.TaskDetails{
				Task: domain.UpdateTask{
					ID:       id,
					Pair:     domain.Pair("EUR/MXN"),
					Status:   domain.StatusFailed,
					Attempts: 3,
					Error:    "provider unavailable after 3 attempts",
				},
			},
			wantBody: `{
				"update_id": "6f1a2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d",
				"pair": "EUR/MXN",
				"status": "failed",
				"error": "provider unavailable after 3 attempts"
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{details: tt.details}

			w := serve(t, svc, httptest.NewRequest(http.MethodGet, "/quotes/updates/"+id.String(), nil))

			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, contentTypeJSON, w.Header().Get("Content-Type"))
			require.JSONEq(t, tt.wantBody, w.Body.String())
			require.Equal(t, []uuid.UUID{id}, svc.fetched)
		})
	}
}

// The two ways a poll fails: an identifier that is not one, which never
// reaches the service, and one that names no task.
func TestGetTaskRefused(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		getErr error

		wantStatus int
		wantCode   contract.ErrorCode
		wantCalled bool
	}{
		{
			name:       "an identifier that is not a UUID",
			id:         "not-a-uuid",
			wantStatus: http.StatusBadRequest,
			wantCode:   contract.ErrorCodeInvalidRequest,
		},
		{
			name:       "a well formed identifier naming no task",
			id:         uuid.NewString(),
			getErr:     domain.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   contract.ErrorCodeNotFound,
			wantCalled: true,
		},
		{
			name:       "a failure of ours",
			id:         uuid.NewString(),
			getErr:     errors.New("connection refused"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   contract.ErrorCodeInternalError,
			wantCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{getErr: tt.getErr}

			w := serve(t, svc, httptest.NewRequest(http.MethodGet, "/quotes/updates/"+tt.id, nil))

			requireEnvelope(t, w, tt.wantStatus, tt.wantCode)
			require.Len(t, svc.fetched, boolToInt(tt.wantCalled))
		})
	}
}

// The read that answers from the database alone. Both timestamps are served:
// rate_date says how old the rate is, fetched_at when this service last
// confirmed it, and over a weekend they differ by days.
func TestGetLatestQuote(t *testing.T) {
	svc := &fakeService{quote: domain.Quote{
		UpdateID:  uuid.MustParse("6f1a2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d"),
		Pair:      domain.Pair("EUR/MXN"),
		Rate:      decimal.RequireFromString("19.6552"),
		RateDate:  time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		FetchedAt: time.Date(2026, 9, 9, 10, 30, 15, 0, time.UTC),
	}}

	w := serve(t, svc, httptest.NewRequest(http.MethodGet, "/quotes/latest?pair=EUR/MXN", nil))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, contentTypeJSON, w.Header().Get("Content-Type"))
	require.JSONEq(t, `{
		"pair": "EUR/MXN",
		"rate": "19.6552000000",
		"rate_date": "2026-09-07",
		"fetched_at": "2026-09-09T10:30:15Z",
		"update_id": "6f1a2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d"
	}`, w.Body.String())
	require.Equal(t, []domain.Pair{"EUR/MXN"}, svc.looked)
}

// What arrives at the service. A percent-encoded separator is what a correct
// client sends, and Query unescapes it for us, so both spellings have to reach
// the service as the same pair.
func TestGetLatestQuoteReadsTheParameter(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  domain.Pair
	}{
		{name: "a bare separator", query: "?pair=EUR/MXN", want: "EUR/MXN"},
		{name: "an encoded separator", query: "?pair=EUR%2FMXN", want: "EUR/MXN"},
		{name: "a lower case pair is upper cased by the domain", query: "?pair=eur/mxn", want: "EUR/MXN"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{quoteErr: domain.ErrNotFound}

			serve(t, svc, httptest.NewRequest(http.MethodGet, "/quotes/latest"+tt.query, nil))

			require.Equal(t, []domain.Pair{tt.want}, svc.looked)
		})
	}
}

// The two 4xx apart on purpose: an unsupported pair will never work, while a
// supported one that has not been quoted yet will as soon as an update
// completes.
func TestGetLatestQuoteRefused(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		quoteErr error

		wantStatus int
		wantCode   contract.ErrorCode
		wantCalled bool
	}{
		{
			// Turned away at the boundary: the service is never asked about a
			// pair this service cannot quote.
			name:       "an unsupported pair",
			query:      "?pair=EUR/RUB",
			wantStatus: http.StatusBadRequest,
			wantCode:   contract.ErrorCodeInvalidPair,
		},
		{
			// Refused by the generated binder before any handler runs, and
			// answered as a pair that cannot be parsed: missing, malformed and
			// unsupported are one answer here.
			name:       "no pair at all",
			wantStatus: http.StatusBadRequest,
			wantCode:   contract.ErrorCodeInvalidPair,
		},
		{
			name:       "a supported pair nobody has quoted",
			query:      "?pair=EUR/MXN",
			quoteErr:   domain.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   contract.ErrorCodeNotFound,
			wantCalled: true,
		},
		{
			name:       "a failure of ours",
			query:      "?pair=EUR/MXN",
			quoteErr:   errors.New("connection refused"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   contract.ErrorCodeInternalError,
			wantCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{quoteErr: tt.quoteErr}

			w := serve(t, svc, httptest.NewRequest(http.MethodGet, "/quotes/latest"+tt.query, nil))

			requireEnvelope(t, w, tt.wantStatus, tt.wantCode)
			require.Len(t, svc.looked, boolToInt(tt.wantCalled))
		})
	}
}

// A panic reaches the client as the envelope the contract promises rather than
// as a dropped connection, which is what net/http's own recovery would leave.
func TestRouterRecoversFromAPanic(t *testing.T) {
	svc := &fakeService{before: func(context.Context) { panic("something in the service gave way") }}

	r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(`{"pair":"EUR/MXN"}`))
	w := serve(t, svc, r)

	requireEnvelope(t, w, http.StatusInternalServerError, contract.ErrorCodeInternalError)
}

// A handler cannot outlive its deadline: without one a query against a
// database that has stopped answering holds a goroutine and a pooled
// connection for as long as the outage lasts.
//
// The service waits for the deadline rather than for a duration, so the expiry
// is a fact rather than a race with a timer.
func TestRouterAppliesTheRequestDeadline(t *testing.T) {
	svc := &fakeService{
		before:    func(ctx context.Context) { <-ctx.Done() },
		createErr: context.DeadlineExceeded,
	}

	r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(`{"pair":"EUR/MXN"}`))
	w := httptest.NewRecorder()

	NewRouter(svc, discardLogger(), time.Millisecond).ServeHTTP(w, r)

	// Served as internal_error and not as a bare 504: the spec lists no 504 for
	// any endpoint, and a deadline of ours is a failure of ours.
	requireEnvelope(t, w, http.StatusInternalServerError, contract.ErrorCodeInternalError)
}

// The one failure that is not answered at all. Everything in flight fails when
// the connection goes away, but that is the consequence of the client leaving
// rather than a fault of ours, and writing a status into a connection that is
// gone reports it to nobody.
func TestRouterAnswersNothingToAClientThatLeft(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	svc := &fakeService{
		before:    func(context.Context) { cancel() },
		createErr: context.Canceled,
	}

	r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(`{"pair":"EUR/MXN"}`)).
		WithContext(ctx)

	w := serve(t, svc, r)

	// Nothing written: no status of our own and no envelope. That is also what
	// the access log records as a status of zero.
	require.Equal(t, http.StatusOK, w.Code, "a status was written to a connection that is gone")
	require.Empty(t, w.Body.String())
}

// Every business response carries the identifier its log lines are written
// under: it is the only way a client can quote one when reporting a problem.
func TestRouterSetsARequestID(t *testing.T) {
	svc := &fakeService{task: testTask(domain.StatusPending)}

	r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(`{"pair":"EUR/MXN"}`))
	r.Header.Set(requestIDHeader, "id-chosen-by-the-client")

	w := serve(t, svc, r)

	got := w.Header().Get(requestIDHeader)

	// Ignored rather than adopted: honouring it would put a string of the
	// client's choosing into every log line of the request.
	require.NotEqual(t, "id-chosen-by-the-client", got)
	_, err := uuid.Parse(got)
	require.NoError(t, err)
}

// The probe is answered without any of the middleware. The compose healthcheck
// calls it every five seconds, and an access log line each time would bury
// anything worth reading.
func TestHealthzStaysOutsideTheGroup(t *testing.T) {
	w := serve(t, &fakeService{}, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"status":"ok"}`, w.Body.String())
	require.Empty(t, w.Header().Get(requestIDHeader), "the probe went through the middleware group")
}

// The surface to the three the contract describes plus the probe. The check is
// here rather than in a document because a fourth route is added in this
// package.
func TestRouterServesNoOtherEndpoints(t *testing.T) {
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/quotes/updates"},
		{http.MethodDelete, "/quotes/updates/" + uuid.NewString()},
		{http.MethodPost, "/quotes/latest"},
		{http.MethodGet, "/quotes"},
		{http.MethodGet, "/"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			w := serve(t, &fakeService{}, httptest.NewRequest(tt.method, tt.path, nil))

			require.Contains(t,
				[]int{http.StatusNotFound, http.StatusMethodNotAllowed},
				w.Code,
			)
		})
	}
}

// An oversized body is refused rather than buffered: the reader stops at the
// limit, so the rest of what a client sent is never held in memory.
func TestCreateTaskReadsNoMoreThanTheLimit(t *testing.T) {
	body := &countingReader{r: strings.NewReader(`{"pair":"` + strings.Repeat("E", 1<<20) + `"}`)}

	svc := &fakeService{}
	w := serve(t, svc, httptest.NewRequest(http.MethodPost, "/quotes/updates", body))

	requireEnvelope(t, w, http.StatusBadRequest, contract.ErrorCodeInvalidRequest)
	require.Empty(t, svc.created)
	require.LessOrEqual(t, body.read, maxRequestBody+1,
		"the whole body was read before it was refused")
}

// countingReader records how much of a body was actually read.
type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n

	return n, err
}

// One or none.
func boolToInt(b bool) int {
	if b {
		return 1
	}

	return 0
}

// logEntry is one structured record, decoded from what the logger wrote.
//
// Only the fields the tests below ask about. Every one of them is part of a
// decision written down in this package: which log a failure goes to, whether
// the cause was recorded at all, and whether the line can be tied to the
// identifier the client was handed.
type logEntry struct {
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	Error     string `json:"error"`
	Panic     string `json:"panic"`
	RequestID string `json:"request_id"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
}

func (e logEntry) level() slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(e.Level)); err != nil {
		return slog.LevelInfo
	}

	return level
}

// The tests that use it are the ones about the log itself. Everywhere else the
// discard logger stays: what a handler answers and what it writes down are
// separate questions, and only the second needs this.
func captureLogger() (*slog.Logger, func() []logEntry) {
	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return logger, func() []logEntry {
		var entries []logEntry

		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}

			var entry logEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				continue
			}

			entries = append(entries, entry)
		}

		return entries
	}
}

func requireEntry(t *testing.T, entries []logEntry, msg string) logEntry {
	t.Helper()

	for _, entry := range entries {
		if entry.Msg == msg {
			return entry
		}
	}

	t.Fatalf("no log record %q among %d written", msg, len(entries))

	return logEntry{}
}

// The other half of the rule TestCreateTaskHidesTheCause states. The client is
// told the code alone, which only works because the cause is written down
// somewhere -- and nothing else in this package would notice if it were not:
// the answer is identical either way, and the request would become
// unexplainable rather than merely opaque.
func TestLogRecordsTheCauseOfAFailure(t *testing.T) {
	const cause = "dial tcp 10.0.0.7:5432: connect: connection refused"

	logger, records := captureLogger()

	svc := &fakeService{createErr: errors.New(cause)}

	r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(`{"pair":"EUR/MXN"}`))
	w := httptest.NewRecorder()

	NewRouter(svc, logger, 5*time.Second).ServeHTTP(w, r)

	requireEnvelope(t, w, http.StatusInternalServerError, contract.ErrorCodeInternalError)
	require.NotContains(t, w.Body.String(), cause)

	entry := requireEntry(t, records(), "request failed")

	require.Equal(t, slog.LevelError, entry.level())
	require.Contains(t, entry.Error, cause)
	require.Equal(t, http.MethodPost, entry.Method)
	require.Equal(t, "/quotes/updates", entry.Path)
	require.NotEmpty(t, entry.RequestID)
}

// The level, which is the whole of the decision. Nothing of ours failed: there
// is no answer to write and nobody to write it to. At error the line would sit
// in the error log beside a real outage, to be told apart by hand at exactly
// the moment nobody has time for it -- a closed tab and a database that has
// stopped answering would look alike.
func TestLogRecordsAnAbandonedRequestAtInfo(t *testing.T) {
	logger, records := captureLogger()

	ctx, cancel := context.WithCancel(t.Context())

	svc := &fakeService{
		before:    func(context.Context) { cancel() },
		createErr: context.Canceled,
	}

	r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(`{"pair":"EUR/MXN"}`)).
		WithContext(ctx)

	NewRouter(svc, logger, 5*time.Second).ServeHTTP(httptest.NewRecorder(), r)

	entry := requireEntry(t, records(), "request abandoned by the client")
	require.Equal(t, slog.LevelInfo, entry.level())

	// The cancellation is carried in all the same: a database already refusing
	// connections when the client hung up would otherwise be written down
	// nowhere at all.
	require.NotEmpty(t, entry.Error)

	for _, got := range records() {
		require.NotEqual(t, slog.LevelError, got.level(), "a client leaving was reported as a failure of ours")
	}
}

// Why recoverPanic sits inside accessLog rather than around it. The other way
// round the panic would pass the access log on its way out, the line would
// report a status of zero, and the 500 the client actually received would
// appear nowhere.
func TestAccessLogReportsTheStatusOfARecoveredPanic(t *testing.T) {
	logger, records := captureLogger()

	svc := &fakeService{before: func(context.Context) { panic("something in the service gave way") }}

	r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(`{"pair":"EUR/MXN"}`))
	w := httptest.NewRecorder()

	NewRouter(svc, logger, 5*time.Second).ServeHTTP(w, r)

	require.Equal(t, http.StatusInternalServerError, w.Code)

	entry := requireEntry(t, records(), "request")
	require.Equal(t, http.StatusInternalServerError, entry.Status)
}

// The two ends of the identifier together. It is returned in a header so that
// a client can quote it when reporting a problem, which is worth nothing
// unless the same value is the one the log lines of that request were written
// under.
func TestAccessLogCarriesTheRequestIDTheClientGot(t *testing.T) {
	logger, records := captureLogger()

	svc := &fakeService{task: testTask(domain.StatusPending)}

	r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(`{"pair":"EUR/MXN"}`))
	w := httptest.NewRecorder()

	NewRouter(svc, logger, 5*time.Second).ServeHTTP(w, r)

	served := w.Header().Get(requestIDHeader)
	require.NotEmpty(t, served)

	entry := requireEntry(t, records(), "request")
	require.Equal(t, served, entry.RequestID)
	require.Equal(t, http.StatusAccepted, entry.Status)
	require.Equal(t, http.MethodPost, entry.Method)
	require.Equal(t, "/quotes/updates", entry.Path)
}

// The shape an abandoned request takes in the access log. writeInternal
// declined to answer a connection that is gone and net/http had nobody to send
// its default 200 to either, so the line carries no status at all -- and the
// info line with the same request id beside it is what says why.
func TestAccessLogReportsZeroWhenNothingWasWritten(t *testing.T) {
	logger, records := captureLogger()

	ctx, cancel := context.WithCancel(t.Context())

	svc := &fakeService{
		before:    func(context.Context) { cancel() },
		createErr: context.Canceled,
	}

	r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(`{"pair":"EUR/MXN"}`)).
		WithContext(ctx)

	NewRouter(svc, logger, 5*time.Second).ServeHTTP(httptest.NewRecorder(), r)

	access := requireEntry(t, records(), "request")
	abandoned := requireEntry(t, records(), "request abandoned by the client")

	require.Zero(t, access.Status)
	require.Equal(t, access.RequestID, abandoned.RequestID, "the two lines of one request cannot be tied together")
}

// Directly what the missing request id only implies. The compose healthcheck
// calls the probe every five seconds -- seventeen thousand lines a day, in
// which anything worth reading would be lost -- and keeping it out of the log
// is the reason it is registered outside the middleware group at all.
func TestHealthzWritesNoLogLine(t *testing.T) {
	logger, records := captureLogger()

	w := httptest.NewRecorder()
	NewRouter(&fakeService{}, logger, 5*time.Second).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, records(), "the probe wrote to the log")
}

// brokenWriter is a response writer whose body cannot be written. It stands in
// for the one cause writeJSON has left once marshalling is ruled out: a
// connection that has gone while the answer was being sent.
//
// The status line is recorded rather than sent, since it is already out by the
// time the body fails -- that is the whole difficulty the branch exists for.
type brokenWriter struct {
	header http.Header
	status int
	err    error
}

func (b *brokenWriter) Header() http.Header {
	if b.header == nil {
		b.header = make(http.Header)
	}

	return b.header
}

func (b *brokenWriter) WriteHeader(status int) {
	b.status = status
}

func (b *brokenWriter) Write([]byte) (int, error) {
	return 0, b.err
}

// What happens when the body cannot be written. The status and the headers are
// already gone, so there is no way left to tell the client anything: all that
// remains is to write down what happened, and which log that goes to depends
// on whose fault it was.
func TestWriteJSONFailureIsLoggedNotAnswered(t *testing.T) {
	tests := []struct {
		name string
		// abandoned cancels the request context, which is what net/http does
		// when the connection goes away.
		abandoned bool

		wantLevel slog.Level
		wantMsg   string
	}{
		{
			// Nothing of ours failed and there is nobody to write to. At error
			// this would sit in the error log beside a real outage.
			name:      "a client that hung up mid-body",
			abandoned: true,
			wantLevel: slog.LevelInfo,
			wantMsg:   "request abandoned by the client",
		},
		{
			name:      "a write that failed for any other reason",
			wantLevel: slog.LevelError,
			wantMsg:   "write response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, records := captureLogger()

			svc := &fakeService{task: testTask(domain.StatusPending)}

			r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(`{"pair":"EUR/MXN"}`))

			if tt.abandoned {
				ctx, cancel := context.WithCancel(r.Context())
				cancel()

				r = r.WithContext(ctx)
			}

			w := &brokenWriter{err: errors.New("connection reset by peer")}

			NewRouter(svc, logger, 5*time.Second).ServeHTTP(w, r)

			// The status was already on the wire when the body failed, so it
			// stands: nothing tries to replace it with an error.
			require.Equal(t, http.StatusAccepted, w.status)

			entry := requireEntry(t, records(), tt.wantMsg)
			require.Equal(t, tt.wantLevel, entry.level())
			require.NotEmpty(t, entry.RequestID)
		})
	}
}

// The one panic that must not be turned into an answer. http.ErrAbortHandler
// is the documented way for a handler to drop a response without a word;
// net/http raises it and expects to catch it again, so swallowing it here
// would turn a deliberate abort into a 500 and log a fault that never
// happened.
func TestRecoverPanicPassesOnAnAbortedHandler(t *testing.T) {
	logger, records := captureLogger()

	svc := &fakeService{before: func(context.Context) { panic(http.ErrAbortHandler) }}

	r := httptest.NewRequest(http.MethodPost, "/quotes/updates", strings.NewReader(`{"pair":"EUR/MXN"}`))
	w := httptest.NewRecorder()

	router := NewRouter(svc, logger, 5*time.Second)

	require.PanicsWithError(t, http.ErrAbortHandler.Error(), func() {
		router.ServeHTTP(w, r)
	})

	require.Empty(t, w.Body.String(), "an aborted handler was answered")

	for _, entry := range records() {
		require.NotEqual(t, "panic in handler", entry.Msg, "a deliberate abort was logged as a fault")
	}
}

// The guard in front of the envelope. A panic after the status has gone out
// cannot be reported to the client at all: a second WriteHeader is refused
// with a complaint of its own, and the envelope would land at the end of a
// body that is already valid.
//
// The chain is built here rather than taken from NewRouter because no handler
// of this service writes and then panics -- what is under test is the
// middleware, and it needs a handler that does.
func TestRecoverPanicLeavesAnAnsweredRequestAlone(t *testing.T) {
	logger, records := captureLogger()

	const answer = `{"update_id":"6f1a2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d","status":"pending"}`

	// The same nesting the router uses: the wrapper accessLog installs is what
	// makes the question answerable at all.
	chain := accessLog(logger)(recoverPanic(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, answer)

		panic("something gave way after the answer had gone")
	})))

	w := httptest.NewRecorder()
	chain.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/quotes/updates", nil))

	require.Equal(t, http.StatusAccepted, w.Code)
	require.JSONEq(t, answer, w.Body.String())
	require.Equal(t, answer, w.Body.String(), "an envelope was appended to an answer that had already gone")

	// Reported all the same: the client cannot be told, but the log can.
	requireEntry(t, records(), "panic in handler")
}

func requirePairRefusal(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()

	require.Equal(t, http.StatusBadRequest, w.Code)

	var got contract.Error
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, contract.ErrorCodeInvalidPair, got.Error.Code)

	return got.Error.Message
}

// The sentence naming the currency at fault is written by ParsePair to be read
// from outside and reaches the client unchanged: a wrapper added on the way
// out would put the name of an internal operation into an answer meant for a
// person. Compared against what ParsePair returns rather than a literal, since
// the wording is the domain's to change.
func TestPairRefusalCarriesTheDomainSentence(t *testing.T) {
	t.Run("posting an update", func(t *testing.T) {
		_, want := domain.ParsePair("EUR/RUB")
		require.Error(t, want)

		svc := &fakeService{}
		body := strings.NewReader(`{"pair":"EUR/RUB"}`)

		w := serve(t, svc, httptest.NewRequest(http.MethodPost, "/quotes/updates", body))

		require.Equal(t, want.Error(), requirePairRefusal(t, w))
		require.Empty(t, svc.created)
	})

	t.Run("reading the latest quote", func(t *testing.T) {
		_, want := domain.ParsePair("EUR/RUB")
		require.Error(t, want)

		svc := &fakeService{}

		w := serve(t, svc, httptest.NewRequest(http.MethodGet, "/quotes/latest?pair=EUR/RUB", nil))

		require.Equal(t, want.Error(), requirePairRefusal(t, w))
		require.Empty(t, svc.looked)
	})

	// The parameter never reaches a handler: the generated binder turns the
	// request away and writeParamError answers it. What it answers with has to
	// be the same sentence an empty pair would have produced anywhere else.
	t.Run("reading the latest quote without a pair", func(t *testing.T) {
		_, want := domain.ParsePair("")
		require.Error(t, want)

		svc := &fakeService{}

		w := serve(t, svc, httptest.NewRequest(http.MethodGet, "/quotes/latest", nil))

		require.Equal(t, want.Error(), requirePairRefusal(t, w))
		require.Empty(t, svc.looked)
	})
}

// The two halves of what the spec says about fetched_at, neither of which the
// generated type gives on its own: the driver hands back a timestamptz in the
// zone of the session, and a time.Time marshals in its own zone and with
// whatever precision it carries.
func TestGetLatestQuoteNormalisesTheInstant(t *testing.T) {
	mexicoCity := time.FixedZone("CST", -6*60*60)

	svc := &fakeService{quote: domain.Quote{
		UpdateID:  uuid.MustParse("6f1a2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d"),
		Pair:      domain.Pair("EUR/MXN"),
		Rate:      decimal.RequireFromString("19.6552"),
		RateDate:  time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		FetchedAt: time.Date(2026, 9, 9, 4, 30, 15, 123456789, mexicoCity),
	}}

	w := serve(t, svc, httptest.NewRequest(http.MethodGet, "/quotes/latest?pair=EUR/MXN", nil))

	require.Equal(t, http.StatusOK, w.Code)

	require.JSONEq(t, `{
		"pair": "EUR/MXN",
		"rate": "19.6552000000",
		"rate_date": "2026-09-07",
		"fetched_at": "2026-09-09T10:30:15Z",
		"update_id": "6f1a2c3d-4e5f-4a6b-8c9d-0e1f2a3b4c5d"
	}`, w.Body.String())
}
