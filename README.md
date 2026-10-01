# AI Cover Letter Generator

A production-quality Go backend that takes a job posting and a PDF resume and
returns a tailored, **evidence-grounded** cover letter as a rendered PDF.

The interesting problem here is not PDF rendering — it is making a language model
write a letter about your experience without letting it invent qualifications you
do not have. That is the constraint the architecture is built around.

---

## Table of contents

1. [Purpose](#1-purpose)
2. [Architecture](#2-architecture)
3. [Tech stack](#3-tech-stack)
4. [Local setup](#4-local-setup)
5. [Configuration](#5-configuration)
6. [Running the service](#6-running-the-service)
7. [API](#7-api)
8. [AI architecture](#8-ai-architecture)
9. [Security considerations](#9-security-considerations)
10. [Testing](#10-testing)
11. [Future improvements](#11-future-improvements)

---

## 1. Purpose

Given a job posting and a resume, produce a cover letter that is specific to that
job and factually defensible.

**What it does**

- Accepts a job posting **by URL** (fetched and parsed) or **as pasted text**.
- Accepts a resume as a PDF upload.
- Runs a five-stage AI pipeline that analyses the job, analyses the resume,
  matches the two, drafts the letter, and validates the draft.
- Renders the result to a PDF and serves it for download.
- Processes asynchronously, so the client is not held open during a multi-step
  model call.

**What it deliberately does not do**

- Invent experience. If the job wants Kubernetes and the resume does not show
  Kubernetes, the letter does not claim Kubernetes. This is enforced in four
  independent places — see [§8](#8-ai-architecture).
- Store or log resume contents, personal information, or API keys.
- Require infrastructure beyond a single binary, a SQLite file, and a local
  directory.

**Non-goals:** user accounts, job tracking, multiple resume versions, letter
history, fine-tuning. The service handles one document per request and keeps the
record for retrieval.

---

## 2. Architecture

A **modular monolith**: one Go binary, one process, SQLite, local disk. Not a
microservices system, not a toy. Boundaries are enforced by Go package structure,
so a service call is a function call and a failure is a returned error.

```
                    ┌───────────────────────────────┐
  HTTP clients ───▶ │  cmd/server → internal/app    │
                    └───────────────┬───────────────┘
                                    │ composition root
        ┌───────────────────────────┼───────────────────────────┐
        ▼                           ▼                           ▼
   internal/httpapi          internal/worker           internal/coverletter
   routing, middleware       bounded task pool         application service
        │                           │                           │
        └───────────────────────────┴───────────────────────────┘
                                    │
              ┌─────────────────────┼─────────────────────┐
              ▼                     ▼                     ▼
        internal/job         internal/resume         internal/ai
        fetch + parse        validate + extract      agent + providers
                                    │
                                    ▼
                             internal/pdf ──▶ storage
                             render          local disk
```

Requests flow strictly inward — handler → service → repository/external
dependency — and nothing below the HTTP layer knows HTTP exists. Every outward
dependency is a small interface owned by the package that *consumes* it
(`AIProvider`, `JobFetcher`, `ResumeParser`, `PDFGenerator`, `Dispatcher`), which
keeps the dependency arrow pointing inward and makes each one substitutable in a
test.

### Request lifecycle

```
POST /api/v1/cover-letters
  ├─ middleware: request id → panic recovery → access log → metrics
  ├─ handler caps the body with MaxBytesReader, then parses the multipart form
  ├─ resume.Service.Store
  │     sniffs %PDF- magic bytes (filename and Content-Type are NOT trusted)
  │     enforces the size cap while streaming, writing nothing on overflow
  │     derives the storage key server-side: resumes/<random id>.pdf
  ├─ coverletter.Service.Create
  │     validates → persists as created → dispatches → created→processing
  └─ 202 Accepted  {"id": 123, "status": "processing"}
        │
        ▼  background worker, request already closed
   worker.Pool → Service.Process
        ├─ resolve job (fetcher → HTML → parser → clean text)
        ├─ parse resume PDF → extract → length-validate
        ├─ ai.Agent.Run  (five stages)
        ├─ pdf.Generate → store cover-letters/<id>/cover-letter.pdf
        └─ processing → completed  (or → failed)
        ▼
   GET /api/v1/cover-letters/{id}/pdf
```

### Status machine

```
        created ──▶ processing ──▶ completed
            │            │
            └────────────┴──────▶ failed
```

Transitions are compare-and-swap, not assignment. The repository issues
`UPDATE ... WHERE id = ? AND status IN (...)` and treats zero affected rows as a
conflict, so two workers racing for one record cannot both win or clobber a
terminal state.

### Packages

| Package | Responsibility |
| --- | --- |
| `cmd/server` | Entry point: config, signals, run |
| `internal/app` | Composition root; the only place that knows every concrete type |
| `internal/httpapi` | Routing, middleware, `/health`, `/metrics` |
| `internal/coverletter` | The application service, status machine, HTTP handlers, DTOs, GORM repository |
| `internal/job` | URL validation, SSRF-guarded fetching, HTML → job description |
| `internal/resume` | PDF validation, text extraction, storage |
| `internal/ai` | Five-stage agent, provider interface, Gemini adapter, prompts |
| `internal/pdf` | Deterministic rendering (go-pdf/fpdf) |
| `internal/worker` | Bounded background task pool |
| `internal/storage` | Local filesystem storage with atomic writes and key validation |
| `internal/apperror` | Stable client-facing error vocabulary |
| `internal/config` | Typed configuration from the environment |
| `internal/obs` | In-process counters, gauges, timings |
| `internal/logging` | slog handler construction |
| `internal/aitest` | Shared scripted fake AI provider used by several test packages |

Full detail, including the Mermaid diagrams and the reasoning behind each
decision, is in [`docs/architecture.md`](docs/architecture.md).

---

## 3. Tech stack

| Concern | Choice | Why |
| --- | --- | --- |
| Language | Go 1.26 | Requested; strong concurrency and a single static binary |
| HTTP | **`net/http` stdlib** | The requirements allow a framework only if genuinely justified. Four endpoints, two of which stream a file. `net/http`'s `ServeMux` supports method-and-path patterns natively, so a framework would add a dependency and not remove code. |
| ORM | GORM v2 | Requested. Also provides `AutoMigrate` for tests and a clean repository seam. |
| Database | SQLite (WAL) | Requested. Right size for this workload: reads dominate, writes are serialised by the status transitions. Connection pool capped at 1 writer. |
| AI | Gemini via `google.golang.org/genai` | Requested. Confined to one file. |
| PDF render | `github.com/go-pdf/fpdf` | Requested a suitable library. Pure Go, built-in fonts (no font assets to ship), byte-deterministic with a fixed creation date. |
| PDF text extract | `github.com/ledongthuc/pdf` | Pure Go, so parsing does not require cgo. |
| HTML parse | `golang.org/x/net/html` | Stdlib-adjacent, spec-compliant, with charset detection. |
| Logging | `log/slog` | Standard library, structured by default. |
| Config | `os.Getenv` into a typed struct | No config library needed for ~30 variables; fails fast at startup. |
| Tests | `testing` stdlib | Requested. `-race`, table-driven, no assertion library. |

The only cgo dependency is SQLite's driver (`mattn/go-sqlite3`).

### Layout

The module is `coverletter` at the repository root rather than under `backend/`.
Two deliberate deviations from the suggested structure: the router package is
`internal/httpapi` (not `internal/http`, to avoid colliding with the stdlib
`net/http` in imports), and two small additions were made — `internal/httpx` for
the error envelope and JSON helpers shared by handlers without importing the
router, and `internal/aitest` for a scripted fake provider reused across test
packages.

---

## 4. Local setup

**Requirements:** Go 1.26+, a C toolchain (Xcode CLT or build-essential — the
SQLite driver needs cgo), and a Gemini API key.

```bash
# Check the toolchain. CGO_ENABLED must be 1 or the SQLite driver will not build.
make check-tools
go version

# Fetch dependencies
go mod download
```

Get an API key from [Google AI Studio](https://aistudio.google.com/apikey), then:

```bash
cp .env.example .env
# edit .env and set GEMINI_API_KEY
```

Verify:

```bash
make check    # go vet + gofmt
make test     # full suite with -race
make run      # starts on :8080
```

```bash
curl -s localhost:8080/health
# {"status":"ok"}
```

---

## 5. Configuration

Everything comes from the environment; nothing is hard-coded to a deployment.
See [`.env.example`](.env.example) for the annotated full list. Invalid
configuration fails at startup rather than on the first request.

### Required

| Variable | Default | Notes |
| --- | --- | --- |
| `GEMINI_API_KEY` | — | **Required.** Startup fails without it. Never logged, never committed. |

### HTTP

| Variable | Default | Notes |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | Bind address |
| `HTTP_READ_TIMEOUT` | `15s` | |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | |
| `HTTP_WRITE_TIMEOUT` | `30s` | Comfortably exceeds any synchronous request |
| `HTTP_IDLE_TIMEOUT` | `60s` | |
| `HTTP_SHUTDOWN_TIMEOUT` | `15s` | Graceful drain window |
| `CORS_ALLOWED_ORIGINS` | `http://localhost:5173,http://127.0.0.1:5173` | Comma-separated browser origins allowed to call the API |
| `CORS_ALLOW_CREDENTIALS` | `false` | Cannot be combined with a wildcard origin |
| `CORS_MAX_AGE_SECONDS` | `600` | Preflight cache lifetime |

### Storage

| Variable | Default | Notes |
| --- | --- | --- |
| `DATABASE_PATH` | `./data/app.db` | |
| `DATABASE_MAX_OPEN_CONNS` | `1` | SQLite serialises writers |
| `DATABASE_MAX_IDLE_CONNS` | `1` | |
| `DATABASE_CONN_MAX_LIFETIME` | `1h` | |
| `UPLOAD_DIR` | `./data/uploads` | Uploaded resumes |
| `PDF_DIR` | `./data/pdfs` | Generated letters |
| `MAX_UPLOAD_SIZE` | `10485760` | 10 MiB |

### AI

| Variable | Default | Notes |
| --- | --- | --- |
| `AI_PROVIDER` | `gemini` | Only `gemini` implemented |
| `GEMINI_MODEL` | `gemini-2.5-flash` | |
| `AI_TIMEOUT` | `60s` | Per model call |
| `AI_MAX_RETRIES` | `2` | Transient failures only |
| `AI_TEMPERATURE` | `0.4` | Stages may override |
| `AI_MAX_OUTPUT_TOKENS` | `8192` | |

### Worker and generation

| Variable | Default | Notes |
| --- | --- | --- |
| `WORKER_COUNT` | `2` | Concurrent generations; the main lever on provider load |
| `WORKER_QUEUE_SIZE` | `32` | Waiting tasks; beyond this the API returns `503` |
| `GENERATION_TIMEOUT` | `5m` | End-to-end budget for one generation |
| `GENERATION_MAX_ATTEMPTS` | `2` | Regenerations after a failed validation; bounded |

### Job fetching

| Variable | Default | Notes |
| --- | --- | --- |
| `JOB_FETCH_TIMEOUT` | `15s` | |
| `JOB_FETCH_MAX_REDIRECTS` | `5` | Each hop re-validated |
| `JOB_FETCH_MAX_HTML_BYTES` | `4194304` | 4 MiB |
| `JOB_FETCH_USER_AGENT` | `CoverLetterGenerator/1.0 (+local development)` | Some sites degrade unknown agents |
| `JOB_MAX_DESCRIPTION_RUNES` | `20000` | Reject pasted descriptions longer than this |
| `JOB_FETCH_ALLOW_PRIVATE_NETWORKS` | `false` | **Tests only.** `true` disables SSRF protection |

### Observability

| Variable | Default | Notes |
| --- | --- | --- |
| `APP_ENV` | `development` | Tagged in logs |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `LOG_FORMAT` | `text` | `text` or `json` |

---

## 6. Running the service

```bash
make run                      # loads .env if present
go run ./cmd/server           # plain environment variables
make build && ./bin/server    # compiled binary
```

Migrations are embedded in the binary (`go:embed`) and applied automatically at
startup — each in its own transaction, recorded in `schema_migrations`. There is
no separate migration step.

`SIGINT`/`SIGTERM` triggers graceful shutdown: the HTTP server stops accepting,
in-flight requests drain within `HTTP_SHUTDOWN_TIMEOUT`, and the worker pool is
given a bounded window to finish.

### Make targets

| Target | Purpose |
| --- | --- |
| `make help` | List targets |
| `make run` | Start the server |
| `make build` | Build `bin/server` |
| `make test` | Full suite with `-race` |
| `make test-short` | Suite without `-race` |
| `make cover` | Coverage summary |
| `make fmt` | Format |
| `make vet` | `go vet` |
| `make check` | `go vet` + gofmt check, pre-commit |
| `make tidy` | Prune module requirements |
| `make clean` | Remove build output, coverage, `data/` |
| `make check-tools` | Verify cgo is enabled |

---

## 7. API

Base path `/api/v1`. All responses are JSON. Errors always use one envelope:

```json
{"error": {"code": "INVALID_JOB_INPUT", "message": "Provide either job_url or job_description.", "request_id": "3f9a…"}}
```

`code` is stable and machine-readable; `message` is written for a human and never
contains stack traces, SQL, filesystem paths, or provider errors.

### CORS

The frontend is served separately from this API, so the browser needs an explicit
grant to read responses. Every response carries
`Access-Control-Allow-Origin` when the request's `Origin` is listed in
`CORS_ALLOWED_ORIGINS`; an unlisted origin gets no grant header, and the browser
then refuses to expose the body to the caller's JavaScript.

`OPTIONS` preflights are answered for `GET`, `POST` and `OPTIONS` with
`Content-Type` and `X-Request-ID` allowed. A wildcard origin (`*`) is supported
but cannot be combined with `CORS_ALLOW_CREDENTIALS=true` — that combination is
rejected at startup, because a browser discards the response rather than
reporting a useful error.

---

### `GET /health`

Liveness. Deliberately does **not** probe the database or the AI provider, so a
provider outage does not take the service out of rotation.

**200**
```json
{"status": "ok"}
```

---

### `POST /api/v1/cover-letters`

Creates a generation job. Returns immediately — generation is asynchronous.

**Content-Type:** `multipart/form-data`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `resume` | file | **yes** | PDF only. Content is sniffed; the filename and `Content-Type` are not trusted |
| `job_url` | string | one of | Public http/https URL, or manual text |
| `job_description` | string | one of | The pasted posting. **Exactly one of `job_url` / `job_description`** |
| `tone` | string | no | Free-form style hint, e.g. `formal`, `enthusiastic` |

**202 Accepted**
```json
{"id": 123, "status": "processing"}
```

**Errors**

| Status | Code | Cause |
| --- | --- | --- |
| 400 | `MALFORMED_REQUEST` | Body is not valid multipart |
| 400 | `VALIDATION_ERROR` | `resume` part absent |
| 400 | `INVALID_RESUME` | Not a PDF, or too little extractable text |
| 400 | `UNSUPPORTED_FILE_TYPE` | Not a PDF by content |
| 400 | `INVALID_JOB_INPUT` | Both or neither of `job_url`/`job_description` |
| 400 | `UNSAFE_URL` | URL resolves to a private, loopback or otherwise blocked address. Checked at submit time, so an SSRF attempt is refused here rather than accepted and failed later |
| 413 | `PAYLOAD_TOO_LARGE` | Body over `MAX_UPLOAD_SIZE` |
| 503 | `QUEUE_FULL` | Worker queue saturated; retry |
| 500 | `INTERNAL_ERROR` | |

---

### `GET /api/v1/cover-letters/{id}`

Polls a generation job.

**200**
```json
{
  "id": 123,
  "status": "completed",
  "cover_letter": "Dear Hiring Team,\n\n…",
  "pdf_url": "/api/v1/cover-letters/123/pdf",
  "candidate_name": "Jia Sheng",
  "error_message": "",
  "validation": {
    "valid": true,
    "unsupported_claims": [],
    "issues": []
  }
}
```

`status` is one of `created`, `processing`, `completed`, `failed`. `error_message` is
populated only on `failed` and carries a client-safe message. Database internals,
`resume_text`, storage paths and raw provider errors are never included.

**Errors:** `400 VALIDATION_ERROR` (non-numeric id) · `404 NOT_FOUND`

---

### `GET /api/v1/cover-letters/{id}/pdf`

Streams the rendered letter.

**200** — `Content-Type: application/pdf`, `Content-Disposition: inline`,
`Content-Length` set, body starting `%PDF-`.

**Errors:** `404 NOT_FOUND` · `409 INVALID_STATE` (not `completed` yet — poll
`GET /{id}` instead).

---

### `GET /metrics`

In-process metrics as JSON: HTTP request counts, durations and errors by status
class; AI call duration, attempts and failures; job fetch duration, successes,
failures and SSRF blocks; PDF generation duration; worker queue depth,
outstanding work and in-flight count.

Deliberately **not** a Prometheus endpoint — scraping would be infrastructure
nothing asked for. See [`docs/architecture.md`](docs/architecture.md) for the
full list.

---

### Walkthrough

```bash
# Create
ID=$(curl -s -X POST localhost:8080/api/v1/cover-letters \
  -F 'job_description=We are hiring a backend engineer with 5+ years of Go experience…' \
  -F 'tone=professional' \
  -F 'resume=@resume.pdf' | jq -r .id)

# Poll
until [ "$(curl -s localhost:8080/api/v1/cover-letters/$ID | jq -r .status)" != "processing" ]; do sleep 2; done

# Fetch
curl -s localhost:8080/api/v1/cover-letters/$ID | jq '{status, pdf_url}'
curl -s -o cover-letter.pdf localhost:8080/api/v1/cover-letters/$ID/pdf
```

Using a URL instead of pasted text:

```bash
curl -s -X POST localhost:8080/api/v1/cover-letters \
  -F 'job_url=https://example.com/careers/senior-backend-engineer' \
  -F 'resume=@resume.pdf' | jq
```

---

## 8. AI architecture

The AI side is five separate model calls, orchestrated by `ai.Agent`, each
constrained to a JSON schema. The agent works on domain models (`job.Description`,
`resume.Resume`) and has no knowledge of HTTP, storage, or Gemini.

```
   job.Description            resume.Resume
         │                          │
         ▼                          ▼
  ┌──────────────┐           ┌──────────────┐
  │ analyzeJob   │           │ analyzeResume│
  │ title, comp. │           │ claims, each │
  │ requirements │           │ WITH quoted  │
  └──────┬───────┘           │ evidence     │
         │                   └──────┬───────┘
         │  requirements              │ highlights
         │                           │ (evidence-less ones dropped)
         └──────────────┬────────────┘
                        ▼
               ┌─────────────────┐
               │ matchCandidate  │  per requirement:
               │                 │  matched / partial / missing
               └────────┬────────┘
                        │ missing requirements carried forward as
                        │ "must NOT be claimed"
                        ▼
               ┌────────────────────┐
               │ generateCoverLetter│
               └─────────┬──────────┘
                         ▼
                   ┌───────────┐   valid?
                   │ validate  │───► regenerate with findings,
                   └─────┬─────┘     bounded by MaxAttempts
                    invalid          (returns best attempt if still invalid)
```

### 1. Job analysis
Extracts title, company and a list of concrete requirements. Prefers JSON-LD
`JobPosting` markup when the page has it, and falls back to DOM heuristics.

### 2. Resume analysis
Produces a summary plus **claims**, each as `{claim, evidence}` where evidence is
quoted verbatim from the resume.

### 3. Candidate / job matching
Classifies every requirement as `matched`, `partial` or `missing`.

### 4. Cover letter generation
Drafts the subject and body, given the job analysis, the resume claims, the match
result, and the tone.

### 5. Validation
Re-reads the letter adversarially against the job and resume, returning
`UnsupportedClaims` and `Issues`. Failures trigger regeneration, bounded so the
model cannot spin.

### How hallucination is prevented

This is the core concern, so it is defended in four independent places. Any one
alone would be weak; together, fabricating a qualification requires the model to
lie consistently at four different layers.

**1 — Evidence anchoring, enforced in Go.** The template instructs the model to
make no claim it cannot quote support for. Then, regardless of what comes back,
the agent **discards every claim whose evidence is empty**. An ungrounded claim
never reaches the generation stage at all.

**2 — The gap list is computed by us, not the model.** `checkMissingRequirements`
is a deterministic comparison over the job's requirements and the resume's
claims. It does not consult the model, so a model that fails to notice
"Kubernetes" as a gap cannot thereby license itself to claim it. The resulting
list is injected into the generation prompt as text that must not be claimed,
implied, or worked around.

**3 — Adversarial validation, merged with deterministic findings.** A separate
call is explicitly instructed to find problems, and its findings are merged with
the deterministic ones from step 2 (`mergeValidation`). Validation is therefore
not solely a model's opinion about its own output.

**4 — Bounded regeneration.** A rejected letter is regenerated with the findings
attached, at most `GENERATION_MAX_ATTEMPTS` times, and the best validated attempt
is what gets rendered.

### Structured output

Each stage requests a specific JSON schema from the SDK (`ResponseSchema`), which
constrains the model's output at generation time. The response is then validated
again in Go: `DecodeStructured` extracts the JSON object — tolerating code fences
and leading prose, correctly ignoring braces inside strings — and `Validate`
checks that required keys are present. Decoding goes via `map[string]any` first so
a contract violation is reported as a contract violation rather than surfacing as
an opaque unmarshal error. A letter under 200 runes is rejected as too short.

### Provider isolation

`AIProvider` is an interface; `gemini.go` is the only file importing the Gemini
SDK. Adding Ollama means writing one more `Generate` implementation and setting
`AI_PROVIDER` — the agent, the service, and the handlers do not change. The
provider handles per-call timeouts and retries with jittered exponential backoff
capped at 8s, and classifies failures: 429, 5xx, transport errors, and empty
responses are retried; 4xx, safety blocks, and cancellations are not.

### PDF is separate and deterministic

The model writes prose; `internal/pdf` renders bytes. This keeps rendering
unit-testable in isolation, keeps a working renderer available when the provider
is down, avoids spending tokens on page layout, and lets `SetCreationDate` make
output byte-identical for identical input.

---

## 9. Security considerations

### SSRF protection

Accepting a user-supplied URL to fetch is the main server-side request forgery
surface, so `internal/job/urlguard.go` defends in depth:

- **Only `http` and `https`.** No `file:`, `gopher:`, or other protocol handlers.
- **URLs with embedded credentials are rejected**, as are control characters and
  URLs over 2048 characters.
- **Hostnames** are rejected for `localhost` and the `.localhost`, `.local`,
  `.internal`, and `.home.arpa` suffixes.
- **The resolved IP is validated, not just the hostname.** This is the important
  one. Checking only the hostname string is defeated instantly by
  `http://127.0.0.1.nip.io`. Every address the hostname resolves to is checked:
  loopback, RFC 1918 private space, link-local (`169.254.0.0/16` — the cloud
  metadata endpoint), CGNAT `100.64.0.0/10`, `0.0.0.0/8`, `192.0.0.0/24`,
  `198.18.0.0/15`, multicast, reserved `240.0.0.0/4`, unspecified, and IPv6
  unique-local `fc00::/7`.
- **IPv4-mapped IPv6 is normalised via `To4()` first**, so `::ffff:127.0.0.1`
  cannot slip past an IPv4-only check.
- **The check runs at submit time and again at connect time.** `Create` resolves
  the host and refuses the request with `400 UNSAFE_URL`, so the client learns
  immediately instead of polling a record that was never going to succeed. The
  authoritative check lives in a custom `DialContext`, which re-validates the
  address at connect time and so closes the DNS-rebinding window between the two
  moments.
- **Every redirect hop is re-validated** via `CheckRedirect`, with a cap
  (`JOB_FETCH_MAX_REDIRECTS`). A public URL that 302s to `169.254.169.254` is
  still caught.
- **Responses are bounded** in time (`JOB_FETCH_TIMEOUT`) and size
  (`JOB_FETCH_MAX_HTML_BYTES`), and non-HTML content types are refused.
- `HTTPFetcher` uses its own `http.Client` carrying this dialer — the shared
  default client has no guard and would bypass all of the above.

The DNS resolver is an interface, so the whole guard is testable against scripted
addresses without touching the network.

> `JOB_FETCH_ALLOW_PRIVATE_NETWORKS=true` exists solely so tests can reach an
> `httptest` server on `127.0.0.1`. It disables the primary defence. Never set it
> in production.

### File upload validation

- **The client's `Content-Type` and filename are never trusted.** The first bytes
  are read and must contain the `%PDF-` magic number.
- A text file renamed `resume.pdf` is rejected. The storage key is derived
  server-side (`resumes/<random id>.pdf`); the client filename is sanitised for
  display and never used in a path.
- Size is enforced *while streaming*, so an oversized upload is aborted mid-read
  rather than buffered, and nothing is written on overflow.
- Storage keys are validated and resolved against the storage root, which
  rejects empty keys, absolute paths, backslashes, null bytes, and `..`
  traversal — enforced at the storage layer rather than by caller convention.
- Writes are atomic (temp file plus rename), so a crash cannot leave a
  half-written PDF that later reads as valid.

### Information disclosure

- Error responses carry a stable `code` and a human-safe `message`. Stack traces,
  SQL text, filesystem paths, and upstream API errors are logged, never returned.
- Background failures map through the same vocabulary, so a record's
  `error_message` is as safe as anything served over HTTP.
- Database models are never serialised. Handlers return DTOs
  (`internal/coverletter/dto.go`), so `resume_text`, storage keys, and
  `error_message` cannot leak by accident.
- `X-Forwarded-For` is **not** trusted in access logs; the real remote address is
  recorded.
- A blocked-address rejection carries a generic message ("Job URL host is not
  allowed."); which address triggered it stays in the logs.
- `Content-Disposition` uses a server-generated name, so a hostile upload
  filename cannot influence the download header.

### Secrets

- No API key is hard-coded. The key comes from `GEMINI_API_KEY` and the process
  fails fast at startup if it is missing.
- Keys are never logged. Configuration errors name the *variable*, never its
  value.
- `.env` is gitignored; `.env.example` carries no real values.

### What is deliberately never logged

Resume contents, full personal information, and generated cover letter text.
Resume text is a database column, not a log line — `Store` logs the sanitised
filename and the byte count only.

### Resource exhaustion

- The body is capped by `MaxBytesReader` **before** multipart parsing, so an
  oversized body is rejected rather than read into memory.
- AI output is bounded by `AI_MAX_OUTPUT_TOKENS` and the extraction limit; the
  fetch response by `JOB_FETCH_MAX_HTML_BYTES`; extracted resume and job text by
  rune caps.
- **No unbounded goroutines.** Work goes through a fixed-size pool. When the
  queue is full the caller gets `503` rather than having work buffered without
  limit.
- Generation has a wall-clock budget (`GENERATION_TIMEOUT`), and the
  regenerate loop is bounded (`GENERATION_MAX_ATTEMPTS`).

### Database

- Every query is parameterised through GORM; no string-built SQL.
- Parameterised transitions prevent a record being resurrected from a terminal
  state under a race.
- The SQLite pool is capped at a single writer, avoiding lock contention as a
  denial-of-service vector.

---

## 10. Testing

```bash
make test          # go test -race ./...
make test-short    # without the race detector
make cover         # coverage summary
```

Standard library `testing` only — no assertion library. Tests are table-driven
wherever there is a table to drive, and every test asserts something that would
fail on a real regression. There are no coverage-only tests.

### What is covered

| Area | Approach |
| --- | --- |
| **Job URL validation** | Table-driven over schemes, credentials, control characters, length, host suffixes; unsafe URLs are refused by `Create` itself, and the message is asserted not to disclose the blocked address |
| **SSRF protection** | A stubbed `ipResolver` feeds scripted DNS answers — every blocked CIDR, IPv4-mapped IPv6, and the "public hostname resolving to a private IP" case, with no network access |
| **Job parsing** | JSON-LD (direct and `@graph`), `itemprop` fallback, `article`/`main` heuristics, chrome stripping, truncation, rejection of non-job pages |
| **Job fetching** | Against `httptest`: redirects, redirect limits, non-HTML, 404, oversized bodies |
| **Job service** | Exactly-one-source validation, length bounds, fetcher error propagation |
| **Resume validation** | Magic-byte sniffing; a text file renamed `.pdf` is rejected; a real PDF with a hostile filename is accepted and stored under a server-generated key |
| **Resume parsing** | A PDF generated in-test with `fpdf` is parsed back and its words recovered; normalisation (ligatures, curly quotes, dashes, hyphen-at-line-break); filename sanitisation |
| **Cover letter service** | Real repository against real SQLite, scripted AI provider: validation failures, dispatch, queue-full backpressure, success and failure paths |
| **Status transitions** | The full `(from, to)` matrix against the allowed set |
| **AI response validation** | JSON decoding including fences, prose, nested objects and braces inside strings; required-field checks; retry classification; backoff bounds |
| **Agent behaviour** | Stage ordering, evidence-less claims dropped, missing requirements reaching the prompt, regeneration on validation failure, bounded attempts |
| **PDF generation** | Valid output, determinism for identical input, pagination, empty-body rejection |
| **Worker pool** | Task execution, backpressure, context cancellation, panic isolation, shutdown, and that observed concurrency never exceeds the configured bound |
| **HTTP endpoints** | All four endpoints, success and failure, including 400/404/405/409/413/503; error envelope shape; explicit assertions that no stack trace, SQL, or key appears |
| **End to end** | The full pipeline: real SQLite, real local storage, a real PDF, a fake AI provider, and an `httptest` job server |

### Test doubles

- **`internal/aitest`** — a scripted provider, importable by several packages,
  that returns queued or repeating per-stage responses and records which stages
  were called. It lets the agent and service tests exercise real orchestration
  logic without a network call or an API key.
- **`internal/logging.Discard()`** and `obs.New()` — real implementations with
  output suppressed, so tests exercise the real logger and metrics paths.
- **Real dependencies where cheap.** SQLite, local storage and PDF rendering are
  real in tests; only the model provider, the job-fetcher HTTP boundary, and DNS
  are substituted. A test that stubs the filesystem would not catch the traversal
  bugs it claims to.

### Notes

`-race` is part of `make test`; the worker pool and metrics registry are the
concurrent parts worth watching. Tests never reach the network or require an API
key, so the suite runs in CI with no secrets. `httptest` binds to loopback, which
the SSRF guard blocks — those tests enable the guard's private-network allowance
explicitly rather than weakening the guard for everyone.

---

## 11. Future improvements

Ordered roughly by value, given the requirements as written.

### High value

- **Job-fetching coverage.** Many job boards render client-side, where an HTTP
  fetcher gets an empty shell. A headless-browser `JobFetcher` would slot in
  behind the existing interface with no change above it. Add Playwright behind a
  feature flag with a shorter timeout and stricter limits, since it is a much
  heavier and less predictable dependency.
- **Structured logging to stdout in production.** Already possible via
  `LOG_FORMAT=json`; what is missing is a documented destination and a rotation
  policy, which is a deployment decision rather than a code one.
- **Cleanup of orphaned uploads.** A failed job leaves its uploaded resume on
  disk. A periodic sweep deleting uploads for records in a terminal state older
  than a retention window would bound disk usage.
- **Rate limiting.** Per-client request limits would need a store; a single-process
  limiter is fine at this scale and is cheap to add.
- **Pagination and history.** `GET /cover-letters` with the existing indexes is
  the natural next endpoint, if clients need it beyond a single id.

### Medium value

- **A second AI provider.** The interface exists; an Ollama or OpenAI-compatible
  implementation plus `AI_PROVIDER=ollama` would make local development possible
  without an API key — genuinely useful given the current key requirement.
- **Prompt and model evaluation.** A golden-set test (fixtures of real
  job/resume pairs with expected letter properties) would catch regressions in
  the anti-hallucination chain when prompts or models change. The highest-value
  guard for the property that matters most, and the first thing to add before
  touching a prompt.
- **Cover letter templates.** Tone and structure are currently free-form model
  output; a small set of structurally distinct templates would give more reliable
  formatting and let `internal/pdf` handle layout variety.
- **Streaming status via SSE.** Polling works; a server-sent event stream would
  remove the wait for a ~30s generation.
- **A stronger storage abstraction.** `internal/storage` has a clean interface but
  one implementation. Object storage would be a natural second, though it is not
  justified at current scale.

### Lower value

- **Multi-version resumes and letters** for one candidate.
- **Internationalisation** of prompts and PDF layout.
- **A/B comparison** of prompt variants against the golden set above.
- **Database migrations beyond forward-only.** `internal/migrate` is
  forward-only by design; down-migrations would be needed only if a bad release
  had to be rolled back in place.
- **Service decomposition.** Not indicated. If the AI pipeline ever needed
  independent scaling, `internal/worker.Pool` behind a queue is the seam, and
  everything above it already depends on interfaces rather than on process
  boundaries.

---

## License

Provided as a reference implementation.