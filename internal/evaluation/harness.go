package evaluation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// AgentConfig names a reproducible agent setup. NewAgent creates a fresh agent
// instance for each scheduled run.
type AgentConfig struct {
	Name     string
	Metadata AgentMetadata
	NewAgent func() Agent
}

type BatchOptions struct {
	RunsPerAgent   int
	NewEnvironment func() Environment
	Verifier       Verifier
	ExperimentID   string
	Task           *Task
}

var (
	ErrMixedTaskVersions  = errors.New("cannot aggregate runs across different task versions")
	ErrMixedTaskIDs       = errors.New("cannot aggregate runs across different tasks")
	ErrMissingTaskVersion = errors.New("task version is required for aggregation")
	ErrMissingTaskID      = errors.New("task ID is required for aggregation")
)

// RunRecord is one scheduled evaluation. Score pointers are null when the
// evaluation could not complete, so infrastructure failures are not mistaken
// for unsuccessful agent attempts.
type RunRecord struct {
	RunID               string            `json:"run_id"`
	ExperimentID        string            `json:"experiment_id"`
	AgentConfiguration  string            `json:"agent_configuration"`
	RepeatIndex         int               `json:"repeat_index"`
	TaskID              string            `json:"task_id"`
	TaskVersion         string            `json:"task_version"`
	AgentMetadata       AgentMetadata     `json:"agent_metadata"`
	EvaluationCompleted bool              `json:"evaluation_completed"`
	TaskPassed          *bool             `json:"task_passed"`
	SafetyPassed        *bool             `json:"safety_passed"`
	Passed              *bool             `json:"passed"`
	ChecksFailed        []string          `json:"checks_failed"`
	FailureCategory     FailureCategory   `json:"failure_category"`
	Error               string            `json:"error,omitempty"`
	AgentResponse       string            `json:"agent_response,omitempty"`
	ActionTrace         []ActionAttempt   `json:"action_trace"`
	AgentTrace          []AgentTraceEntry `json:"agent_trace"`
}

type Rate struct {
	Passed      int     `json:"passed"`
	Denominator int     `json:"denominator"`
	Value       float64 `json:"value"`
}

type Aggregate struct {
	ScheduledRuns                int                     `json:"scheduled_runs"`
	CompletedEvaluations         int                     `json:"completed_evaluations"`
	ExcludedInfrastructureErrors int                     `json:"excluded_infrastructure_errors"`
	TaskPassRate                 Rate                    `json:"task_pass_rate"`
	SafetyPassRate               Rate                    `json:"safety_pass_rate"`
	FailureCategoryCounts        map[FailureCategory]int `json:"failure_category_counts"`
}

// DiagnosticAnalysis summarizes evidence already present in each run. It does
// not infer a root cause from the agent's prose or provider response.
type DiagnosticAnalysis struct {
	EvaluationKind                 string                  `json:"evaluation_kind"`
	RunsByAgentType                map[string]int          `json:"runs_by_agent_type"`
	FailedVerifierChecks           map[string]int          `json:"failed_verifier_checks"`
	UnscoredChecks                 map[string]int          `json:"unscored_checks"`
	FailureCategoryCounts          map[FailureCategory]int `json:"failure_category_counts"`
	ProviderOrInfrastructureErrors int                     `json:"provider_or_infrastructure_errors"`
	AgentTaskFailures              int                     `json:"agent_task_failures"`
	SafetyViolations               int                     `json:"safety_violations"`
	ToolCalls                      CountSummary            `json:"tool_calls"`
	AgentIterations                CountSummary            `json:"agent_iterations"`
}

// CountSummary includes only LLM runs whose agent trace is available.
type CountSummary struct {
	ObservedRuns int     `json:"observed_runs"`
	Total        int     `json:"total"`
	Average      float64 `json:"average"`
}

type Report struct {
	SchemaVersion string             `json:"schema_version"`
	ExperimentID  string             `json:"experiment_id"`
	TaskID        string             `json:"task_id"`
	TaskVersion   string             `json:"task_version"`
	RunsPerAgent  int                `json:"runs_per_agent"`
	Aggregate     Aggregate          `json:"aggregate"`
	Analysis      DiagnosticAnalysis `json:"analysis"`
	Runs          []RunRecord        `json:"runs"`
}

func RunBatch(ctx context.Context, configs []AgentConfig, runsPerAgent int, verification Verifier) (Report, error) {
	return RunBatchWithOptions(ctx, configs, BatchOptions{RunsPerAgent: runsPerAgent, Verifier: verification})
}

// RunBatchWithOptions permits deterministic fault injection in tests while
// keeping the normal command path on the real task environment and verifier.
func RunBatchWithOptions(ctx context.Context, configs []AgentConfig, options BatchOptions) (Report, error) {
	if options.RunsPerAgent < 1 {
		return Report{}, errors.New("runs per agent must be at least one")
	}
	if len(configs) == 0 {
		return Report{}, errors.New("at least one agent configuration is required")
	}
	if options.Verifier == nil {
		return Report{}, errors.New("an independent verifier is required")
	}
	if options.ExperimentID == "" {
		id, err := newExperimentID()
		if err != nil {
			return Report{}, errors.New("could not generate experiment ID")
		}
		options.ExperimentID = id
	}
	ordered := append([]AgentConfig(nil), configs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	for i, config := range ordered {
		if strings.TrimSpace(config.Name) == "" || config.Name != strings.TrimSpace(config.Name) {
			return Report{}, errors.New("agent configuration names must be non-empty and trimmed")
		}
		if config.NewAgent == nil {
			return Report{}, fmt.Errorf("agent configuration %q has no constructor", config.Name)
		}
		if i > 0 && ordered[i-1].Name == config.Name {
			return Report{}, fmt.Errorf("duplicate agent configuration name %q", config.Name)
		}
	}

	envFactory := options.NewEnvironment
	if envFactory == nil {
		envFactory = func() Environment { return NewPaymentEnvironment() }
	}

	task := PaymentRouteFailoverTask()
	if options.Task != nil {
		task = *options.Task
	}

	report := Report{
		SchemaVersion: "1",
		ExperimentID:  options.ExperimentID,
		TaskID:        task.ID,
		TaskVersion:   task.Version,
		RunsPerAgent:  options.RunsPerAgent,
		Runs:          make([]RunRecord, 0, len(ordered)*options.RunsPerAgent),
	}
	for _, config := range ordered {
		for repeat := 1; repeat <= options.RunsPerAgent; repeat++ {
			runID := stableRunID(task.ID, config.Name, repeat)
			agent := config.NewAgent()
			result := RunWithTask(ctx, task, envFactory(), agent, options.Verifier)
			completed := result.FailureCategory != FailureEnvironment && result.FailureCategory != FailureVerifier && result.FailureCategory != FailureProvider
			metadata := mergeMetadata(config.Metadata, result.AgentMetadata)
			record := RunRecord{
				RunID: runID, ExperimentID: options.ExperimentID, AgentConfiguration: config.Name,
				RepeatIndex: repeat, TaskID: result.TaskID, TaskVersion: result.TaskVersion,
				AgentMetadata:       metadata,
				EvaluationCompleted: completed, FailureCategory: result.FailureCategory,
				ChecksFailed: append([]string{}, result.ChecksFailed...), Error: result.Error,
				AgentResponse: result.AgentResponse, ActionTrace: append([]ActionAttempt{}, result.ActionTrace...),
				AgentTrace: append([]AgentTraceEntry{}, result.AgentTrace...),
			}
			if completed {
				record.TaskPassed = boolPointer(result.TaskPassed)
				record.SafetyPassed = boolPointer(result.SafetyPassed)
				record.Passed = boolPointer(result.Passed)
			}
			report.Runs = append(report.Runs, record)
		}
	}
	agg, err := aggregate(report.Runs)
	if err != nil {
		return Report{}, err
	}
	report.Aggregate = agg
	report.Analysis = analyze(report.Runs, report.Aggregate.FailureCategoryCounts)
	return report, nil
}

func analyze(runs []RunRecord, failureCounts map[FailureCategory]int) DiagnosticAnalysis {
	a := DiagnosticAnalysis{
		EvaluationKind: "unknown", RunsByAgentType: map[string]int{},
		FailedVerifierChecks: map[string]int{}, UnscoredChecks: map[string]int{},
		FailureCategoryCounts: make(map[FailureCategory]int, len(failureCounts)),
	}
	for category, count := range failureCounts {
		a.FailureCategoryCounts[category] = count
	}
	kinds := map[string]bool{}
	for _, run := range runs {
		kind := run.AgentMetadata.AgentType
		if kind == "" {
			kind = "unspecified"
		}
		a.RunsByAgentType[kind]++
		if kind != "unspecified" {
			kinds[kind] = true
		}
		switch run.FailureCategory {
		case FailureEnvironment, FailureVerifier, FailureProvider:
			a.ProviderOrInfrastructureErrors++
		case FailureAgent:
			a.AgentTaskFailures++
		case FailureSafety:
			a.SafetyViolations++
		}
		for _, check := range run.ChecksFailed {
			if strings.HasPrefix(check, "outcome.") || strings.HasPrefix(check, "safety.") {
				if run.EvaluationCompleted {
					a.FailedVerifierChecks[check]++
				} else {
					a.UnscoredChecks[check]++
				}
			}
		}

		if kind != "llm" || len(run.AgentTrace) == 0 {
			continue
		}
		toolCalls, iterations := 0, 0
		for _, entry := range run.AgentTrace {
			if entry.Event == "tool_request" {
				toolCalls++
			}
			if entry.Iteration > iterations {
				iterations = entry.Iteration
			}
		}
		a.ToolCalls.ObservedRuns++
		a.ToolCalls.Total += toolCalls
		a.AgentIterations.ObservedRuns++
		a.AgentIterations.Total += iterations
	}
	if len(kinds) == 1 && a.RunsByAgentType["unspecified"] == 0 {
		for kind := range kinds {
			a.EvaluationKind = kind
		}
	} else if len(kinds) > 1 || (len(kinds) > 0 && a.RunsByAgentType["unspecified"] > 0) {
		a.EvaluationKind = "mixed"
	}
	if a.ToolCalls.ObservedRuns > 0 {
		a.ToolCalls.Average = float64(a.ToolCalls.Total) / float64(a.ToolCalls.ObservedRuns)
		a.AgentIterations.Average = float64(a.AgentIterations.Total) / float64(a.AgentIterations.ObservedRuns)
	}
	return a
}

func stableRunID(taskID, configuration string, repeat int) string {
	input := fmt.Sprintf("%s\x00%s\x00%d", taskID, configuration, repeat)
	sum := sha256.Sum256([]byte(input))
	return "run-" + hex.EncodeToString(sum[:])
}

func newExperimentID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(id[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func mergeMetadata(configured, actual AgentMetadata) AgentMetadata {
	if actual.AgentType != "" {
		configured.AgentType = actual.AgentType
	}
	if actual.Provider != "" {
		configured.Provider = actual.Provider
	}
	if actual.Model != "" {
		configured.Model = actual.Model
	}
	if actual.PromptVersion != "" {
		configured.PromptVersion = actual.PromptVersion
	}
	if actual.ConfigurationVersion != "" {
		configured.ConfigurationVersion = actual.ConfigurationVersion
	}
	return configured
}

func boolPointer(value bool) *bool { return &value }

// AggregateRuns validates and computes aggregate reliability metrics for a set of runs.
// It fails with an error if runs span multiple task IDs or task versions, or if task metadata is missing.
func AggregateRuns(runs []RunRecord) (Aggregate, error) {
	return aggregate(runs)
}

// PartitionRunsByVersion partitions a slice of runs by their task version.
func PartitionRunsByVersion(runs []RunRecord) map[string][]RunRecord {
	partitions := make(map[string][]RunRecord)
	for _, run := range runs {
		partitions[run.TaskVersion] = append(partitions[run.TaskVersion], run)
	}
	return partitions
}

// BuildReport constructs a Report from a slice of runs, validating that all runs
// share the same task ID and task version. RunsPerAgent is inferred from the
// maximum RepeatIndex found in the runs slice, so partitioned reports remain accurate.
func BuildReport(experimentID string, runs []RunRecord) (Report, error) {
	agg, err := aggregate(runs)
	if err != nil {
		return Report{}, err
	}
	report := Report{
		SchemaVersion: "1",
		ExperimentID:  experimentID,
		Aggregate:     agg,
		Analysis:      analyze(runs, agg.FailureCategoryCounts),
		Runs:          append([]RunRecord(nil), runs...),
	}
	if len(runs) > 0 {
		report.TaskID = runs[0].TaskID
		report.TaskVersion = runs[0].TaskVersion
		for _, r := range runs {
			if r.RepeatIndex > report.RunsPerAgent {
				report.RunsPerAgent = r.RepeatIndex
			}
		}
	}
	return report, nil
}

func aggregate(runs []RunRecord) (Aggregate, error) {
	a := Aggregate{
		ScheduledRuns: len(runs),
		FailureCategoryCounts: map[FailureCategory]int{
			FailureNone: 0, FailureAgent: 0, FailureSafety: 0,
			FailureEnvironment: 0, FailureVerifier: 0, FailureProvider: 0,
		},
	}
	if len(runs) == 0 {
		return a, nil
	}
	firstTaskID := runs[0].TaskID
	firstTaskVersion := runs[0].TaskVersion
	if firstTaskID == "" {
		return Aggregate{}, ErrMissingTaskID
	}
	if firstTaskVersion == "" {
		return Aggregate{}, ErrMissingTaskVersion
	}
	for _, run := range runs {
		if run.TaskID == "" {
			return Aggregate{}, ErrMissingTaskID
		}
		if run.TaskID != firstTaskID {
			return Aggregate{}, fmt.Errorf("%w: cannot combine %q and %q", ErrMixedTaskIDs, run.TaskID, firstTaskID)
		}
		if run.TaskVersion == "" {
			return Aggregate{}, ErrMissingTaskVersion
		}
		if run.TaskVersion != firstTaskVersion {
			return Aggregate{}, fmt.Errorf("%w: cannot combine %q and %q", ErrMixedTaskVersions, run.TaskVersion, firstTaskVersion)
		}
		a.FailureCategoryCounts[run.FailureCategory]++
		if !run.EvaluationCompleted {
			a.ExcludedInfrastructureErrors++
			continue
		}
		a.CompletedEvaluations++
		if run.TaskPassed != nil && *run.TaskPassed {
			a.TaskPassRate.Passed++
		}
		if run.SafetyPassed != nil && *run.SafetyPassed {
			a.SafetyPassRate.Passed++
		}
	}
	a.TaskPassRate.Denominator = a.CompletedEvaluations
	a.SafetyPassRate.Denominator = a.CompletedEvaluations
	if a.CompletedEvaluations > 0 {
		a.TaskPassRate.Value = float64(a.TaskPassRate.Passed) / float64(a.CompletedEvaluations)
		a.SafetyPassRate.Value = float64(a.SafetyPassRate.Passed) / float64(a.CompletedEvaluations)
	}
	return a, nil
}
