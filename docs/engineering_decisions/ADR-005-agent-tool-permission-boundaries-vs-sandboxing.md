# ADR-005: Agent Capability Restrictions vs. Operating System Sandboxing

## Decision
We formally document that Sentinel's current agent tool isolation constitutes an **application-level interface capability restriction**, and explicitly acknowledge that it does **not** provide an operating system or container security boundary.

## Context
In agentic AI engineering, teams frequently confuse in-process tool boundaries with security sandboxes. Sentinel restricts an agent by providing only four specific Go function signatures via the `evaluation.Tools` interface (`ReadIncidentAlert`, `ReadServiceMetrics`, `ReadRecentDeployments`, `SwitchPaymentRoute`). The LLM adapter validates tool names, JSON schemas, target services, and route allowlists before invocation.

However, because the agent runs within the same Go host process:
- A malicious or compromised binary agent could inspect host memory or use reflection to reach unexported data.
- The process has direct access to host environment variables and network sockets unless isolated at the OS/hypervisor level.
- Software claims must accurately describe real security boundaries rather than inflating marketing buzzwords.

## Constraints
- In-process execution is essential for rapid, deterministic unit testing and local developer workflows.
- True sandboxing (e.g. gVisor, Firecracker microVMs, or Docker container isolation) requires virtualization privileges, container daemons, or Linux kernel capabilities not universally available on all developer workstations (such as standard unprivileged Windows desktops).

## Alternatives Considered
1. **Claim the In-Process Interface is a "Security Sandbox"**:
   - *Pros*: Sounds impressive to superficial reviewers.
   - *Cons*: **Technically false and dangerous**. Any senior security engineer or tech lead will immediately flag this as an unverified security claim.
2. **Mandate Docker / MicroVM Isolation for All Unit Tests**:
   - *Pros*: True OS boundary.
   - *Cons*: Breaks zero-dependency local testing. As proven during our baseline audit, local Docker daemons on developer machines can be unprivileged, stopped, or inaccessible, causing `go test` to fail.
3. **Selected Approach: Two-Tier Architecture with Explicit Honesty**:
   - Tier 1 (Current Implementation): Process-local restricted interface with deterministic audit logging and validation for benchmark grading and testing.
   - Tier 2 (Architecture Proposal & Roadmap): Out-of-process worker execution over standard I/O or gRPC sockets for untrusted agent code in production deployments.
   - All documentation explicitly clarifies the boundary limitation.

## Why this approach?
- Preserves absolute technical integrity during technical interviews.
- Demonstrates deep systems understanding: knowing the difference between an application interface boundary and a POSIX/kernel isolation boundary.

## Trade-offs
- **Gains**: Blazing-fast deterministic test suites (18 seconds for full test suite); 100% reproducible on any OS without Docker.
- **Costs**: Must acknowledge that running arbitrary binary agents in-process is unsuitable for untrusted third-party code.

## Evidence
- `docs/architecture.md` (Section: "Tool permissions versus process isolation")
- `docs/engineering_audit.md`
- `internal/evaluation/llmagent/agent.go`

## Status
Implemented and documented.
