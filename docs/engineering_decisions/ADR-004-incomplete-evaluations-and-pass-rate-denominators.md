# ADR-004: Incomplete Evaluations & Pass-Rate Denominator Policy

## Decision
We enforce an explicit separation between **Completed Evaluations** and **Incomplete Evaluations** (caused by external provider rate limits, network timeouts, or environment failures). Incomplete runs receive `evaluation_completed: false` and explicitly `null` scores (`task_passed: null`, `safety_passed: null`, `passed: null`). Aggregate task and safety pass rates are strictly calculated over completed evaluations:

$$\text{Pass Rate} = \frac{\text{Passed Runs}}{\text{Completed Evaluations}}$$

Incomplete infrastructure/provider failures are tracked and reported in separate top-level diagnostic counters (`excluded_infrastructure_errors`, `failure_category_counts["provider_error"]`), never silently dropped and never lumped into the agent failure denominator.

## Context
When benchmarking LLM agents against live API endpoints (such as Groq, OpenAI, or Anthropic), runs frequently fail due to external infrastructure disruptions: HTTP 429 (Rate Limit / Quota Exceeded), HTTP 503 (Provider Overloaded), or network socket timeouts. 

If an evaluation system treats an HTTP 429 provider rate limit as a failed task run:
1. An agent that was performing flawlessly gets graded as a reasoning failure simply because an upstream API gateway rejected a packet.
2. The benchmark conflates **agent task capability** with **upstream cloud availability**.
3. Pass rates become meaningless and vary wildly depending on what time of day or quota tier the experiment was executed.

Conversely, if an evaluation system simply deletes or hides interrupted runs:
1. Benchmark reporting becomes dishonest.
2. High provider error rates are concealed from researchers.

## Constraints
- Reports must be statistically transparent and honest.
- Incomplete runs must preserve diagnostic observations (what did the agent execute before being interrupted?) without scoring those observations as graded task outcomes.
- Pass rates must accurately reflect agent competence on runs where the agent actually had the opportunity to complete the task.

## Alternatives Considered
1. **Lump Provider Failures into Task Failure Denominator (`pass_rate = passed / scheduled_runs`)**:
   - *Pros*: Naively simple formula.
   - *Cons*: **Scientifically invalid**. In Sentinel's Increment 12, 3 of 5 runs were interrupted by Groq HTTP 429 rate limits. Scoring this as 2/5 (40% pass rate) penalizes the agent for Groq's API quota limits, falsely claiming the agent failed the task in 60% of trials.
2. **Silently Drop Incomplete Runs from Reports**:
   - *Pros*: Cleans up the JSON report.
   - *Cons*: **Dishonest and non-reproducible**. Hides provider unreliability and destroys trial index continuity.
3. **Selected Approach: Null Scores + Completed Denominators + Explicit Diagnostic Counts**:
   - Every scheduled trial (e.g. repeat 1 to 5) preserves its run record and unique `run_id`.
   - Incomplete runs are marked `evaluation_completed: false`.
   - All score fields (`task_passed`, `safety_passed`, `passed`) are serialized as JSON `null`.
   - Denominator is strictly `completed_evaluations`.
   - Intermediate verifier checks observed prior to interruption are archived in `analysis.unscored_checks` rather than scored `failed_verifier_checks`.
   - Infrastructure disruptions are counted explicitly in `aggregate.excluded_infrastructure_errors` and `aggregate.failure_category_counts`.

## Why this approach?
- Decouples LLM reasoning evaluation from API provider SLA fluctuations.
- Preserves full auditability of every scheduled trial.
- Accurately conveys to researchers whether an engineering problem requires prompt tuning (agent task failure) or upstream quota adjustments (provider error).

## Trade-offs
- **Gains**: True measurement of agent intelligence and safety; clear operational diagnostics.
- **Costs**: Users and automated parsers must handle nullable score fields (`*bool`) instead of primitive booleans.

## Failure Scenarios
- If all 5 trials hit rate limits: `scheduled_runs: 5`, `completed_evaluations: 0`, `excluded_infrastructure_errors: 5`, and pass rate is cleanly reported as `0/0` (empty denominator), not `0%` failure.

## Evidence
- `internal/evaluation/task.go` (`Result` nullable score pointers)
- `internal/evaluation/harness.go` (`aggregate()` and `analyze()` functions)
- `internal/evaluation/harness_test.go` (`TestProviderErrorChecksAreUnscoredButPreservedPerRun`, `TestFiveSequentialTrialsExperiment`)
- `docs/evaluation-results.md`

## Status
Implemented and verified.
