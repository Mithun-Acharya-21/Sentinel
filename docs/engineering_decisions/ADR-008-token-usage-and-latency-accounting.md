# ADR-008: Token Usage Accounting & Request Latency Telemetry

## Decision
We extend Sentinel's evaluation trace model (`AgentTraceEntry`) to capture OpenAI-compatible token usage (`prompt_tokens`, `completion_tokens`, `total_tokens`), individual request attempt durations, and total operational latency. Missing provider usage is explicitly marked as unavailable (`usage_available: false`) and is **never fabricated or estimated**.

## Context
Evaluating AI agent performance requires analyzing economic and latency efficiency alongside task accuracy. An agent that takes 20 tool calls and 50,000 tokens to fix an incident is vastly inferior to an agent that accomplishes the same fix in 2 tool calls and 1,500 tokens. 

However, many benchmarking frameworks fabricate token counts (e.g. dividing character counts by 4) when providers omit usage data. This corrupts scientific reporting. Furthermore, when retries occur, frameworks often fail to distinguish between **network request latency** (how fast the model responded) and **total operation latency** (including backoff sleep time).

## Constraints
- Must remain 100% backward compatible with historical JSON reports (`sentinel-groq-gpt-oss-20b-increment*.json`).
- Must not invent or interpolate token counts if a provider omits the `usage` object.
- Must accurately report attempt counts so researchers can observe whether a model required retries.

## Alternatives Considered
1. **Character-Based Token Estimation (`chars / 4`)**:
   - *Pros*: Always produces a number.
   - *Cons*: Highly inaccurate across tokenizers, especially for code, punctuation, and multi-byte unicode characters. Falsifies empirical evidence.
2. **Track Only Overall Loop Duration**:
   - *Pros*: Simple `time.Since(start)`.
   - *Cons*: Obscures whether a slow run was caused by a slow model inference, network congestion, or backoff sleep.
3. **Selected Approach: First-Class Telemetry with Explicit Availability Flags**:
   - `Usage`: Captures exact integer tokens from provider response. If absent, `Available: false`.
   - `RequestLatencyMs`: Monotonic duration of the successful/final HTTP roundtrip.
   - `TotalLatencyMs`: Total monotonic elapsed duration including all retry attempts and backoff sleeps.
   - `Attempts`: Integer count of attempts executed.
   - All fields serialized with `omitempty` in `AgentTraceEntry` to preserve compatibility with existing reports.

## Trade-offs
- **Gains**: Granular performance and cost visibility; absolute empirical honesty.
- **Costs**: Slightly larger trace JSON payload per run.

## Evidence
- `internal/evaluation/task.go` (`AgentTraceEntry` telemetry fields)
- `internal/evaluation/llmagent/agent.go`
- `internal/evaluation/llmagent/retry_test.go` (`TestAgent_TraceTelemetryAndUsageAccounting`, `TestAgent_MissingUsageMetadataHandledCleanly`)

## Status
Implemented and verified.
