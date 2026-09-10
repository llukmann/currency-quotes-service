# currency-quotes-service

[![ci](https://github.com/llukmann/currency-quotes-service/actions/workflows/ci.yml/badge.svg)](https://github.com/llukmann/currency-quotes-service/actions/workflows/ci.yml)

An asynchronous currency quotes service in Go.

A client never fetches a rate directly. It posts an update, which is queued and
answered at once with an identifier, while a background worker performs the
refresh; the two read endpoints answer from the database alone. There is no
synchronous way to obtain a fresh rate — that is the point of the contract, not
a limitation of it.

Rates come from [frankfurter.dev](https://frankfurter.dev), which needs no API
key and no account. That is why the stack below starts with no configuration of
any kind.

Supported currencies are USD, EUR and MXN. A pair is written `BASE/QUOTE`, and a
rate says how many units of the quote currency one unit of the base currency
buys: `20.13` for `EUR/MXN` means 20.13 pesos to the euro.

## Quick start

Requires Docker. Nothing else — no local Go, no database, no `.env`.

```
docker compose up --build
```

That starts PostgreSQL, applies the migrations, and serves on `localhost:8080`.

The full scenario, from an empty database to a stored rate:

```bash
# 1. Ask for a refresh. Answers immediately; no rate is fetched in this handler.
curl -s -X POST localhost:8080/quotes/updates \
     -H 'Content-Type: application/json' \
     -d '{"pair":"EUR/MXN"}'

{"status":"pending","update_id":"bae4a71d-0751-45f5-beea-94c92dbbcb6f"}

# 2. Poll the identifier until the status leaves pending or in_progress.
curl -s localhost:8080/quotes/updates/bae4a71d-0751-45f5-beea-94c92dbbcb6f

{"fetched_at":"2026-09-09T22:26:33Z","pair":"EUR/MXN","rate":"19.6948000000",
 "rate_date":"2026-09-09","status":"done",
 "update_id":"bae4a71d-0751-45f5-beea-94c92dbbcb6f"}

# 3. Or read the last known rate for the pair, whichever update produced it.
curl -s 'localhost:8080/quotes/latest?pair=EUR/MXN'

{"fetched_at":"2026-09-09T22:26:33Z","pair":"EUR/MXN","rate":"19.6948000000",
 "rate_date":"2026-09-09","update_id":"bae4a71d-0751-45f5-beea-94c92dbbcb6f"}
```

A refresh usually completes in under a second: posting the task also wakes a
worker, rather than leaving it to the next poll of the queue. Step 3 answers
`404` until one has finished, since it reads what is stored rather than asking
for it.

To stop and remove the database volume with it:

```
docker compose down -v
```

## Where things are

| Asked for | Where |
| --- | --- |
| Three API operations | [API](#api) |
| Unit tests | [Tests](#tests) — `go test ./...` |
| Containerisation | `docker-compose.yml`, `Dockerfile` |
| Idempotent updates | `Idempotency-Key` on `POST`, see [Design decisions](#design-decisions) |
| OpenAPI specification | [`api/openapi.yaml`](api/openapi.yaml) |
| Database schema | [`migrations/`](migrations) |

## API

The contract is [`api/openapi.yaml`](api/openapi.yaml) — request and response
shapes, every status code, and the closed set of error codes. It is not
duplicated here; the table below is a map, not a specification.

| Method | Path | Answers |
| --- | --- | --- |
| `POST` | `/quotes/updates` | `202` with `update_id` and the status of the task |
| `GET` | `/quotes/updates/{id}` | `200` with the status, and the rate once it is `done` |
| `GET` | `/quotes/latest?pair=` | `200` with the last stored rate for a pair |

A task is `pending`, `in_progress`, `done` or `failed`. The first two mean the
same thing to a client waiting for a rate; the last two are terminal. An
unfinished task answers `200` with its actual status, not `404` — the identifier
is known, the work is not done.

`POST` accepts an optional `Idempotency-Key` header, a UUID. Every response
carries `X-Request-Id`, which is the value to quote when reporting a problem:
the log line describing what went wrong carries the same one.

`GET /healthz` exists for the compose healthcheck. It is not part of the
business API and is not described in the spec.

## How it works

```
POST /quotes/updates    ->  row in quote_updates (pending)  ->  202 with update_id
        a worker        ->  claims it, calls the provider, writes quotes, done
GET /quotes/updates/{id}
GET /quotes/latest      ->  read from the database, never from the provider
```

The handler writes a row and returns. It holds no path to the provider at all,
and neither do the read endpoints — everything a client is told comes out of the
database.

Workers claim tasks with `SELECT ... FOR UPDATE SKIP LOCKED`, so several of them
drain one queue without waiting on each other. A claim also increments
`attempts`, which doubles as the token that makes finalisation safe: a worker
whose task was taken away cannot overwrite the result of whoever has it now.
Finishing a task writes the quote and moves the status in one transaction, so
there is no state in which a rate is stored but the task still looks unfinished.

A second background pass returns tasks that have been `in_progress` for too long
— a process killed mid-task leaves one behind — and gives up on a task claimed
`WORKER_MAX_ATTEMPTS` times, closing it as `failed` rather than releasing it
forever.

Two things deduplicate a post, and they cover different windows. A partial
unique index allows at most one unfinished task per pair, so a burst of requests
for `EUR/MXN` becomes one call to the provider. An `Idempotency-Key` binds a
request to the task it was answered with, so a client that retries without
knowing whether the first attempt arrived gets the same `update_id` back.

## Data model

Three tables, defined in [`migrations/`](migrations), which is the source of
truth for the schema.

`quote_updates` is the queue and the history: one row per task, with its status,
the number of times it was claimed, and the reason it failed. A `CHECK`
constraint refuses a `failed` row with no reason. Two partial indexes serve the
queue — pending tasks by age, running tasks by when they were claimed — and a
partial unique index on `pair` enforces the deduplication above. Terminal rows
fall outside all three, so history never slows the queue down or blocks a new
task.

`quotes` holds what a completed task produced: the rate as `numeric(20,10)`, the
day it is valid for, and when this service received it. Its primary key is the
update that produced it, and `pair` is a denormalised copy so that reading the
latest rate for a pair does not have to join.

`idempotency_keys` maps a key to the task it was answered with. Several keys can
point at one task, which is why it is a table rather than a column.

## Running the binary against the compose database

Needs Go 1.26. The database and the migrations still come from compose — the
service does not apply migrations itself.

```bash
docker compose up -d postgres
docker compose run --rm migrate          # arguments already in docker-compose.yml
export DATABASE_URL='postgres://quotes:quotes@localhost:5432/quotes?sslmode=disable'
go run .
```

In PowerShell the only line that differs is the variable:
`$env:DATABASE_URL = "postgres://quotes:quotes@localhost:5432/quotes?sslmode=disable"`

Configuration is read from the environment. `DATABASE_URL` is the only variable
without a default; the rest are listed with theirs in
[`.env.example`](.env.example).

## Tests

```
go test ./...
```

On a clean clone this is green, and it does not cover the SQL. The storage tests
— the only ones that exercise `SKIP LOCKED`, the partial indexes, the
transactions and the `numeric` round trip — skip themselves unless
`TEST_DATABASE_URL` names a database to run against. Go prints `ok` for a
package whose tests all skipped, so a green run says less than it looks like.

To run them, give them a database of their own. They truncate every table
between cases, so it must not be the one the service is using:

```bash
docker compose exec postgres createdb -U quotes quotes_test
docker compose run --rm migrate -path=/migrations -database "postgres://quotes:quotes@postgres:5432/quotes_test?sslmode=disable" up
export TEST_DATABASE_URL='postgres://quotes:quotes@localhost:5432/quotes_test?sslmode=disable'
go test ./...
```

In PowerShell, again, only the variable differs:
`$env:TEST_DATABASE_URL = "postgres://quotes:quotes@localhost:5432/quotes_test?sslmode=disable"`

CI runs exactly this on every pull request, plus `go test -race ./...` against
the same database, `golangci-lint`, a check that the generated contract still
matches the spec, and a build of the service image.

## Design decisions

**The handler never performs the update.** It writes a row and returns; the
package it lives in holds no path to the provider at all, so the asynchronous
contract is a property of the wiring rather than a promise in prose. The two
read endpoints are the same: they answer from the database, and a rate a client
is shown has been through it.

**A post can be answered with a task it did not create.** At most one unfinished
task exists per pair, so two clients asking for `EUR/MXN` at the same moment
share one refresh and one `update_id`. This is a deliberate reading of "the
service assigns an identifier to an update request": every request gets an
identifier, but not always a private one. It saves the provider a call per
duplicate, and it costs two things worth naming — a client may be handed a rate
fetched moments before it asked, and if the shared task fails, a client is told
`failed` about an attempt that was never made on its behalf. The way out of the
second is to post again: a terminal row leaves the unique index, so the next
request creates a task of its own.

**An idempotency key answers the same way as the first time, whatever happened
since** — including `failed`. The key caches the answer to an HTTP request, not
the outcome of background work. A binding lives for `IDEMPOTENCY_TTL`, enforced
by the lookup rather than by a background sweep: nothing deletes an expired
binding, the lookup stops seeing it, and the next post carrying that key takes
the row over.

**Rates are `numeric` in the database and strings on the wire.** `float64` is
absent from the path a rate travels. A JSON number would be parsed into a float
by most clients, and a rate that survives that round trip intact is a
coincidence.

**The spec comes first and the transport is generated from it.**
`internal/api/contract` is produced from `api/openapi.yaml` by `oapi-codegen`,
and the handlers implement the interface generated with it. A route, a parameter
or a field changed in one place and not the other stops the build; CI
regenerates and compares, so the two cannot drift.

**Migrations are applied by compose, not by the binary.** The service waits for
that step rather than running it, which keeps the schema tool out of `go.mod`
and out of the runtime image. The cost is the extra command above.

**The supported currencies live in code, not in configuration.** They are
bounded by what the provider knows, so a currency added through an environment
variable would pass validation and then fail every refresh — after the request
had already been accepted with `202`. A failure that appears only in the
background is worse than one at the boundary.

## Not done on purpose

**Idempotency bindings are never deleted.** The table grows with the number of
distinct keys. It is bounded and small, and the sweep that used to remove them
existed to enforce the lifetime rather than to reclaim space — the lifetime now
lives in the query. In production this belongs outside the application: a
scheduled `DELETE`, or a partition to drop.

**The service layer is thin.** After parsing moved to the boundary, two of its
three methods are one-line calls into storage; what is left is the wake-up
signal and a seam that keeps handlers out of the repository.

**No authentication, rate limiting, pagination or metrics.** None are in the
assignment, and each would be more surface than three endpoints have.

**No Swagger UI.** The page and the API would be different origins, so "Try it
out" needs CORS headers the service does not serve. The assignment asks for a
specification file, and that is what `api/openapi.yaml` is.

**One provider, and no tests around `main`.** Swapping the upstream means
implementing one interface. Process wiring — the order of goroutines in the
errgroup, the shutdown path — is covered by running the thing, not by a unit
test.
