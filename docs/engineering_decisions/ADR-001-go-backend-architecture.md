# ADR-001: Go Standard Library & Clean Architecture for Sentinel

## Decision
We select Go (using standard library `net/http` and clean hexagonal package boundaries) as the implementation foundation for Sentinel's HTTP API and evaluation runtime, rather than heavy third-party web frameworks (e.g. Gin, Echo, Fiber) or dynamic Python-based agent frameworks (e.g. LangChain, CrewAI, AutoGen).

## Context
Sentinel serves two core functions:
1. High-throughput, idempotent incident event ingestion.
2. High-precision, deterministic simulation and verification of AI incident-response agents.

Both requirements demand predictable concurrency, low memory overhead, explicit context propagation with cancellation, and zero hidden magic.

## Constraints
- Windows development environment and Linux container deployment target.
- Process-local evaluation speed: tests must run within milliseconds without external broker dependencies.
- Absolute clarity during technical interviews: every concurrency primitive and data structure must be explainable from standard language fundamentals.

## Alternatives Considered
1. **Python + LangChain/CrewAI**:
   - *Pros*: Vast ecosystem of off-the-shelf agent integrations.
   - *Cons*: Opaque abstractions, heavy runtime dependency surface, difficult concurrency management, and lack of strict static typing for deterministic verifiers.
2. **Go with Third-Party Web Framework (Gin/Fiber)**:
   - *Pros*: Syntactic sugar for route grouping and middleware.
   - *Cons*: Unnecessary dependencies; standard library `net/http` (with Go 1.22+ routing patterns) fully supports path parameters, strict context timeouts, and custom middleware without third-party coupling.
3. **Selected Approach: Go Standard Library (`net/http`) + Explicit Domain Packages**:
   - Clear separation: `cmd/` for entrypoints, `internal/httpapi` for HTTP transport, `internal/incident` for domain contracts, `internal/storage` for persistence, and `internal/evaluation` for benchmark execution.

## Why this approach?
- **Static Safety & Concurrency**: Go's native goroutines, channels, and `context.Context` provide first-class primitives for request deadlines, cancellation cascades, and connection pool timeouts.
- **Auditability**: Zero external magic ensures that tool dispatching, audit trails, and grading rubrics can be stepped through line-by-line during technical interviews or code reviews.
- **Minimal Footprint**: Compact compiled binaries with zero runtime dependencies.

## Why not the alternatives?
- Framework abstractions in agent runtimes frequently mask model failures, inject implicit retries, or mutate state out-of-band, corrupting benchmark integrity.
- Third-party web routers offer no measurable benefit over `net/http` for Sentinel's scoped endpoint surface.

## Trade-offs
- **Gains**: Total control over memory allocations, timeout enforcement, test fixture isolation, and reproducible execution.
- **Costs**: Manual serialization and middleware composition instead of one-line framework annotations.

## Failure Scenarios
- If an HTTP request exceeds its deadline, standard Go `context.Context` cancels active database queries or tool operations immediately.
- If a goroutine panics, standard middleware catches and logs the failure without terminating the main daemon.

## Security Implications
Reduces supply chain risk to near-zero by keeping direct external dependencies limited strictly to database drivers (`jackc/pgx`).

## Testing Strategy
100% of standard HTTP routing and validation is verified via standard `httptest.ResponseRecorder` unit tests.

## Operational Implications
Zero runtime interpreter dependencies; simple systemd service or Docker container deployment with single static binary.

## Evidence
- `cmd/sentinel-api/main.go`
- `internal/httpapi/incidents.go`
- `internal/httpapi/incidents_test.go`

## Status
Implemented and verified.
