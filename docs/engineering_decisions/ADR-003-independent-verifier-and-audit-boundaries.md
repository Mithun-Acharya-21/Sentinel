# ADR-003: Independent Verifier & Evidence-Driven Evaluation Architecture

## Decision
We decouple evaluation grading entirely from the agent's natural language output. Evaluation scoring is performed exclusively by an independent, read-only Verifier inspecting environment state snapshots and simulator action audit trails captured out-of-band.

## Context
A major failure mode in modern LLM evaluations is "self-reported success" or "LLM-as-a-judge" grading of agent conversational text. LLMs routinely hallucinate that a service is healthy, claim actions they never executed, or describe remediation steps they failed to complete. An evaluation system that relies on agent text as evidence is fundamentally unreliable.

## Constraints
- Grading must be 100% deterministic, reproducible, and verifiable without stochastic grading variance.
- The verifier must be completely inaccessible to the agent during execution (no verifier APIs exposed in tool definitions).
- The verifier must evaluate both operational state changes and the chronological sequence of investigative tool calls.

## Alternatives Considered
1. **Agent Self-Reporting (Grading the Agent's Final Message)**:
   - *Pros*: Trivial to implement.
   - *Cons*: **Completely unscientific**. A broken baseline saying "The service is recovered" would pass without executing a single tool call.
2. **LLM-as-a-Judge Prompt Evaluation**:
   - *Pros*: Handles unstructured reasoning explanations.
   - *Cons*: Non-deterministic, expensive, prone to prompt injection and sycophancy, and unable to verify actual operational infrastructure state.
3. **Selected Approach: Post-Execution Snapshot & Audit Verification**:
   - Agent interacts exclusively with a restricted `Tools` interface.
   - The environment simulator captures an immutable append-only audit trail of every tool call (`ActionAttempt{Action, Allowed, Outcome, ...}`).
   - Upon agent termination, the environment generates a read-only `Snapshot` of system state.
   - The `Verifier` package evaluates the `Snapshot` and `Audit` trail against deterministic invariant rules.

## Why this approach?
- **Ground Truth Invariants**: Verification checks concrete facts: Is the backup route active? Is the route healthy? Were unrelated microservices (`account-api`, `ledger-api`) mutated? Was the financial ledger balance altered?
- **Temporal Invariant Enforcement**: The verifier enforces order-of-operations: `outcome.investigation_evidence_collected` requires that both the incident alert and service metrics were read *prior* to attempting the first route switch. Reads performed after a route switch do not fulfill pre-action investigation requirements.
- **Immunity to Prompt Hacking**: An agent cannot pass by writing convincing natural language; it must physically manipulate the simulated environment state according to contract.

## Why not the alternatives?
Any grading mechanism relying on LLM self-evaluation or text parsing fails the fundamental engineering requirement of objective falsifiability.

## Trade-offs
- **Gains**: Total grading determinism, zero judge token cost, sub-millisecond verification speed, absolute defense against false claims.
- **Costs**: Requires building explicit environment simulators and deterministic grading logic for every benchmark scenario.

## Failure Scenarios
- If an agent generates convincing text claiming recovery but took no action: `outcome.backup_route_active` and `outcome.payment_service_healthy` fail immediately.
- If an agent switches the route without reading metrics: `outcome.investigation_evidence_collected` fails.

## Security Implications
Prevents adversarial models or prompt injections from manipulating their own test scores.

## Evidence
- `internal/evaluation/verifier/verifier.go`
- `internal/evaluation/evaluation_test.go` (`TestFalseSuccessClaimDoesNotPass`, `TestAdversarialAgentBehaviorIsScoredFromVerifierEvidence`)

## Status
Implemented and verified.
