# Architecture

This document describes how the AI Cover Letter Generator is put together: the
deployment shape, the layer boundaries, the generation pipeline, and the
cross-cutting concerns that cut across them.

## Deployment shape

A **modular monolith**: one Go binary, one process, one SQLite file, local disk
for uploads and PDFs. It is not a microservices system and it is not a toy. The
boundaries between concerns are enforced by Go package structure rather than by
network hops, so a service call is a function call and a failure mode is a
returned error.

The single external dependency is the Gemini API. Everything else — database,
storage, metrics, queue — is local and in-process. There is no Redis, no Kafka,
no Prometheus and no container orchestration, because nothing in the requirements
calls for them.

```
                     ┌──────────────────────────────┐
   HTTP clients ───▶ │  cmd/server  →  internal/app  │
                     └──────────────┬───────────────┘
                                    │ composition root: builds every
                                    │ dependency and wires them together
        ┌───────────────────────────┼───────────────────────────┐
        ▼                           ▼                           ▼
   internal/httpapi          internal/worker            internal/coverletter
   (routing, middleware)      (bounded queue)            (application service)
        │                           │                           │
        └───────────────────────────┴───────────────────────────┘
                                    │
              ┌─────────────────────┼─────────────────────┐
              ▼                     ▼                     ▼
        internal/job         internal/resume         internal/ai
        (fetch + parse)      (validate + extract)    (agent + providers)
                                    │
                                    ▼
                            internal/pdf  ──▶  storage
                            (render)            (local disk)
```

## Layering

The dependency direction is strictly inward. Nothing below the HTTP layer knows
that HTTP exists.

| Layer | Package | Responsibility | May depend on |
| --- | --- | --- | --- |
| Transport | `internal/httpapi` | Routing, middleware, request IDs, access logs, metrics endpoint | `httpx`, handler interfaces |
| Transport | `internal/coverletter` (handler) | Multipart parsing, DTO marshalling, status codes | `httpx`, service interfaces |
| Application | `internal/coverletter` (service) | Orchestration, status transitions, error mapping | `job`, `resume`, `ai`, `pdf`, `storage`, repository |
| Domain support | `internal/job`, `internal/resume`, `internal/ai` | The logic of one concern each | their own ports and `apperror` |
| Infrastructure | `internal/database`, `internal/storage`, `internal/pdf` | Concrete adapters | drivers, disk, `go-pdf` |
| Cross-cutting | `apperror`, `config`, `obs`, `logging`, `id`, `migrate` | Shared vocabulary | stdlib only |

Handlers contain no business logic. A handler validates shape, calls one
service method, and translates the result into a status code and an envelope.
Everything a client sees is a DTO from `internal/coverletter/dto.go`; the GORM
model is never serialised, so internal fields like `resume_text`, `error_message`
and storage keys cannot leak by accident.

### Interfaces

Every outward dependency is a small interface owned by the *consuming* package,
so the dependency arrow points inward and any adapter can be swapped in a test:

```go
type AIProvider interface {                       // internal/ai
    Generate(ctx context.Context, request GenerateRequest) (*GenerateResponse, error)
    Name() string
}

type JobFetcher interface {                       // internal/job
    Fetch(ctx context.Context, url string) (*JobPage, error)
}

type ResumeParser interface {                     // internal/resume
    Parse(ctx context.Context, path string) (*Resume, error)
}

type PDFGenerator interface {                     // internal/pdf
    Generate(ctx context.Context, request PDFRequest) ([]byte, error)
}

type Dispatcher interface {                       // internal/coverletter
    Dispatch(ctx context.Context, id uint) error
}
```

`internal/app` is the only place that knows the concrete implementations.
`app.New` accepts `WithProvider`, `WithJobFetcher`, `WithJobParser`,
`WithResumeParser` and `WithPDFGenerator`, which is how the end-to-end test runs
the full pipeline against fakes without touching the network.

## Request lifecycle

```
POST /api/v1/cover-letters
  │
  ├─ middleware: request id → panic recovery → access log → metrics
  │
  ├─ handler: MaxBytesReader caps the body *before* parsing
  │           multipart: job_url | job_description (exactly one) + resume (PDF)
  │
  ├─ resume.Service.Store
  │     ├─ sniffs %PDF- magic bytes            ← the client filename and
  │     │                                        Content-Type are not trusted
  │     ├─ enforces MaxUploadSize while streaming, writing nothing on overflow
  │     ├─ derives the storage key server-side: resumes/<random id>.pdf
  │     └─ stores the sanitised filename for display only
  │
  ├─ coverletter.Service.Create
  │     ├─ validates input → job.Service.Resolve (URL validated, or manual text)
  │     ├─ inserts the row with status = created
  │     ├─ dispatches to the worker pool (TrySubmit → fail fast)
  │     └─ CAS transition created → processing
  │
  └─ 202 Accepted  {"id": 123, "status": "processing"}
        │
        ▼  (background, not holding the request open)
   worker.Pool ──▶ coverletter.Service.Process
        │              ├─ resolve job description (fetcher → HTML → parser → clean)
        │              ├─ parse resume PDF → extracted text, length-validated
        │              ├─ persist both extracted inputs before the AI call
        │              ├─ ai.Agent.Run  (five stages, see below)
        │              ├─ pdf.Generator.Generate → store cover-letters/<id>/cover-letter.pdf
        │              └─ CAS transition processing → completed (or → failed)
        ▼
   GET /api/v1/cover-letters/{id}          → status, text, pdf_url
   GET /api/v1/cover-letters/{id}/pdf     → the rendered PDF (409 until completed)
```

### Status machine

```
        created ──▶ processing ──▶ completed
            │            │
            └────────────┴──────▶ failed
```

Transitions are not assignments. `Repository.TransitionStatus` issues a
conditional update —

```sql
UPDATE cover_letters SET status = ? WHERE id = ? AND status IN (...)
```

— and treats `RowsAffected == 0` as a conflict. If two workers ever race for the
same record, exactly one wins and the other gets `ErrTransitionConflict` instead
of silently overwriting a terminal state.

The queue-full case is handled explicitly: if the pool has no admission slot, the
row is marked `failed` with a safe message and the client receives `503` with
`QUEUE_FULL`. Work is never buffered indefinitely.

## The generation pipeline

The AI side is five stages, each a separate model call with a JSON schema
constraint, orchestrated by `ai.Agent` operating on domain models.

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
         │  requirements              │ highlights (evidence-less ones dropped)
         └──────────────┬────────────┘
                        ▼
               ┌─────────────────┐
               │ matchCandidate  │  requirement-by-requirement:
               │                 │  matched / partial / missing
               └────────┬────────┘
                        │  missing requirements travel forward as
                        │  "must NOT be claimed"
                        ▼
               ┌────────────────────┐   ← letter + its grounding context
               │ generateCoverLetter│      (including the missing list)
               └─────────┬──────────┘
                         ▼
                   ┌───────────┐   valid?
                   │ validate  │──────► loop back to generate, bounded by
                   └─────┬─────┘         MaxAttempts; final failure still
                         │               returns the best letter so far
                    invalid
```

### Why the AI cannot invent experience

Hallucinated qualifications are the failure mode that matters most for a cover
letter, so it is defended in four independent places. Any one of them alone
would be weak; together they make fabrication require the model to lie
consistently at four different layers.

1. **Evidence anchoring.** `analyzeResume` returns each candidate claim as
   `{claim, evidence}` where `evidence` must be quoted verbatim from the resume.
   The template instructs the model to make no claim at all when it cannot quote
   support. After decoding, `Agent` **discards every claim whose evidence is
   empty**, so an ungrounded claim never reaches the generation stage regardless
   of what the model returned.

2. **The missing-requirement list is computed by us, not the model.**
   `checkMissingRequirements` is a deterministic string comparison over the job's
   requirements and the resume's claims. It does not consult the model, so a
   model that fails to notice "Kubernetes" as a gap cannot thereby permit
   itself to claim it. The resulting list is injected into the generation prompt
   as text that **must not** be claimed, implied, or worked around.

3. **Adversarial validation.** A separate call re-reads the letter against the
   job and the resume, with a prompt explicitly instructing it to find problems.
   Its `UnsupportedClaims` and `Issues` are merged with the deterministic
   findings from stage 2 (`mergeValidation`), so validation is not solely a
   model's opinion about its own output.

4. **Bounded regeneration.** If validation fails, the letter is regenerated with
   the findings attached, at most `GENERATION_MAX_ATTEMPTS` times. The loop is
   bounded, so a model that never converges cannot spin, and the final result is
   the best validated attempt rather than nothing.

### Provider isolation

`internal/ai` owns `AIProvider`; `internal/ai/gemini.go` is the only file that
imports `google.golang.org/genai`. Adding Ollama means writing one more
implementation of `Generate` and setting `AI_PROVIDER` — the agent, the service,
and the handlers do not change. The provider itself handles per-call timeouts,
retries with jittered exponential backoff (capped at 8s), and retry
classification: 429/5xx/transport errors and empty responses are retried; 4xx,
safety blocks and cancellations are not.

Structured output is enforced at the SDK level with `ResponseSchema`, then
validated again in Go: `DecodeStructured` extracts the JSON object (tolerating
code fences and leading prose, and correctly ignoring braces inside strings) and
`Validate` checks that the required keys are present. Responses are decoded into
a `map[string]any` first so a contract violation is reported as such rather than
surfacing as an opaque unmarshal error.

## Cross-cutting concerns

### SSRF protection

Fetching a user-supplied URL is the service's main server-side request forgery
surface, so `internal/job/urlguard.go` defends in depth:

- Only `http` and `https`; credentials in the URL are rejected.
- Hostnames are rejected for `localhost` and the `.localhost`/`.local`/
  `.internal`/`.home.arpa` suffixes, and control characters and over-long URLs are
  rejected before parsing.
- **The hostname is resolved and every resulting IP is checked.** Checking only
  the hostname string would be trivially bypassed by `http://127.0.0.1.nip.io`.
  Blocked ranges: loopback, RFC 1918 private space, link-local, CGNAT `100.64/10`,
  `0.0.0.0/8`, `192.0.0/24`, `198.18/15`, multicast, reserved `240/4`, unspecified,
  and IPv6 unique-local `fc00::/7`. IPv4-mapped IPv6 is normalised via `To4()`
  first, so `::ffff:127.0.0.1` cannot slip past an IPv4-only check.
- **The check runs again inside a custom `DialContext`**, closing the
  DNS-rebinding window between validation and connection.
- Every redirect hop is re-validated via `CheckRedirect`, with a redirect cap.
- **The URL is checked at submit time as well as at dial time.**
  `coverletter.Service.Create` calls `job.Service.ValidateURL`, so an SSRF
  attempt is refused with `400 UNSAFE_URL` before any record is written — the
  client learns at once instead of polling a job that was never going to run.
  The dial-time check stays authoritative, because a DNS answer can change
  between the two moments.
- The response is bounded in both time (`JOB_FETCH_TIMEOUT`) and size
  (`JOB_FETCH_MAX_HTML_BYTES`), and non-HTML content types are refused.
- `HTTPFetcher` uses its own `http.Client` with this dialer — the shared default
  client has no guard and would bypass all of the above.

The resolver is an interface (`ipResolver`), so tests script DNS responses instead
of reaching the network. `JOB_FETCH_ALLOW_PRIVATE_NETWORKS` exists only for tests
against `httptest` servers and disables the primary defence; it must stay `false`
in production.

### Error handling

`internal/apperror` is the stable client-facing vocabulary: a machine-readable
`Code`, a message written for a human, and an HTTP status. Wrapping preserves the
cause for logs via `errors.Is`/`errors.As` while `httpx.WriteError` serialises
**only** the safe message. Stack traces, SQL text, filesystem paths, upstream API
errors and keys stay in the log stream. Every error response has one shape:

```json
{"error": {"code": "INVALID_JOB_INPUT", "message": "...", "request_id": "..."}}
```

The background path maps failures through the same vocabulary, so a record's
`error_message` is as safe as anything returned over HTTP.

### Logging

`log/slog` throughout, with request ID, operation, cover letter ID, duration,
status and error as structured attributes. Two rules are enforced by
construction rather than by convention:

- Resume contents are never logged. `resume.Service.Store` logs the sanitised
  filename only; the extracted text is a column, not a log line.
- API keys are never logged. The Gemini provider logs the model name and error
  class, and configuration errors name the *variable* that is missing, not its
  value.

### Observability

`internal/obs` is a small in-process registry of counters, gauges and timings,
rendered as JSON at `GET /metrics`. It deliberately is not a Prometheus client —
a scrape endpoint would be infrastructure nobody asked for. It does cover the
things worth watching: HTTP request duration, count and error count by status
class; AI request duration, attempts and failures; job fetch duration, successes,
failures and SSRF blocks; PDF generation duration; and worker queue depth,
outstanding work and in-flight count.

### Background execution

`internal/worker` separates admission from scheduling. A `slots` channel of
capacity `concurrency + queueSize` is the admission gate: a token is taken when a
task is accepted and returned only when it has finished running, capping total
outstanding work. The `tasks` channel is only the hand-off to the workers. Using
the hand-off channel's own capacity as the bound would be wrong — workers drain
it eagerly, so the queue depth would never be observable and `queueSize` would be
unreachable. `TrySubmit` is a non-blocking token acquisition, which is what turns
saturation into a `503` instead of a hanging request. Each task runs under panic
recovery so one bad task cannot silently cost the pool a worker.

### Migrations

Forward-only SQL files embedded in the `migrations` package via `go:embed` and
applied by `internal/migrate`, each in its own transaction and recorded in
`schema_migrations`. The binary carries its own schema, so a fresh checkout needs
no migration tooling.

## Design decisions and trade-offs

**SQLite with a single writer.** The pool is capped at `MaxOpenConns=1` because
SQLite serialises writers anyway; pretending otherwise produces lock contention
rather than throughput. WAL mode plus a busy timeout covers the concurrent-reader
case, which is the read-heavy `/status` polling path.

**Storage keys, not absolute paths, in the database.** Only `resumes/<id>.pdf` is
stored, never `/var/lib/app/uploads/...`. Paths are therefore validated and
resolved against the storage root at access time, which keeps the database
portable and makes traversal a storage-layer invariant rather than a caller
convention. Writes are atomic (temp file plus rename), so a crash cannot leave a
half-written PDF that later reads as valid.

**PDF generation is separate from the AI, and deterministic.** The model produces
prose; `go-pdf/fpdf` produces bytes. This keeps rendering unit-testable in
isolation, keeps a working renderer available when the provider is down, avoids
paying model tokens to lay out a page, and lets `SetCreationDate` make the output
byte-identical for identical input — which is what makes the determinism test
meaningful.

**Manual job descriptions are a first-class input, not a fallback.** Many users
paste the posting rather than sharing a URL, and the URL path is the one that
needs SSRF defences. Both feed the same `job.Description`, so the AI pipeline
never knows which was used.

**Single in-process queue.** At the stated volumes a database-backed queue would
add a polling loop and a failure mode without adding durability the SQLite record
does not already provide: a record left in `created` after a crash is detectable
by status, not silently lost in a queue.

## Deliberate omissions

Each of these was considered and left out because nothing required it:

- **Headless browser fetching.** The HTTP fetcher covers the common case;
  a `JobFetcher` implementation using a browser can be added without touching
  anything else, which is why the interface exists.
- **Redis / a message broker.** A single-process bounded queue is sufficient and
  is one fewer thing to operate.
- **Prometheus / OpenTelemetry.** The in-process registry answers the same
  questions locally.
- **Generics in the domain model.** Plain concrete types are easier to read and
  to generate documentation for.
- **A DI framework or code generator.** `internal/app` is one readable
  constructor.