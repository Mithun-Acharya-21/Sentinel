package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"sentinel/internal/evaluation"
	"sentinel/internal/evaluation/llmagent"
	"sentinel/internal/evaluation/verifier"
)

func main() {
	agentMode := flag.String("agent", "baselines", "agent configuration to run: baselines or llm")
	runsPerAgent := flag.Int("runs-per-agent", 1, "number of fresh sequential task runs for each agent configuration")
	outputPath := flag.String("out", "sentinel-evaluation.json", "JSON report path; use - for stdout")
	experimentID := flag.String("experiment-id", "", "optional experiment ID; generated when omitted")
	flag.Parse()

	agents, err := selectedAgents(*agentMode)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sentinel-eval:", err)
		os.Exit(2)
	}
	report, err := evaluation.RunBatchWithOptions(context.Background(), agents, evaluation.BatchOptions{
		RunsPerAgent: *runsPerAgent, Verifier: verifier.New(), ExperimentID: *experimentID,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sentinel-eval:", err)
		os.Exit(2)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sentinel-eval: could not serialize report")
		os.Exit(1)
	}
	data = append(data, '\n')
	if *outputPath == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			fmt.Fprintln(os.Stderr, "sentinel-eval: could not write report to stdout")
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(*outputPath, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "sentinel-eval: could not write report to %q\n", *outputPath)
		os.Exit(1)
	}
	fmt.Printf("report: %s\nexperiment: %s\ntask: %s\ntask version: %s\nevaluation kind: %s\nscheduled: %d\ncompleted: %d\ninfrastructure errors excluded: %d\ntask pass rate: %d/%d (%.3f)\nsafety pass rate: %d/%d (%.3f)\nagent task failures: %d\nsafety violations: %d\nfailed verifier checks: %v\nunscored checks from incomplete runs: %v\nLLM tool calls: %d across %d traced runs\nLLM iterations: %d across %d traced runs\n",
		*outputPath,
		report.ExperimentID,
		report.TaskID,
		report.TaskVersion,
		report.Analysis.EvaluationKind,
		report.Aggregate.ScheduledRuns,
		report.Aggregate.CompletedEvaluations,
		report.Aggregate.ExcludedInfrastructureErrors,
		report.Aggregate.TaskPassRate.Passed,
		report.Aggregate.TaskPassRate.Denominator,
		report.Aggregate.TaskPassRate.Value,
		report.Aggregate.SafetyPassRate.Passed,
		report.Aggregate.SafetyPassRate.Denominator,
		report.Aggregate.SafetyPassRate.Value,
		report.Analysis.AgentTaskFailures,
		report.Analysis.SafetyViolations,
		report.Analysis.FailedVerifierChecks,
		report.Analysis.UnscoredChecks,
		report.Analysis.ToolCalls.Total,
		report.Analysis.ToolCalls.ObservedRuns,
		report.Analysis.AgentIterations.Total,
		report.Analysis.AgentIterations.ObservedRuns,
	)
}

func selectedAgents(mode string) ([]evaluation.AgentConfig, error) {
	switch strings.ToLower(mode) {
	case "baselines":
		return evaluation.BuiltInBaselines(), nil
	case "llm":
		apiKey := os.Getenv("SENTINEL_LLM_API_KEY")
		model := os.Getenv("SENTINEL_LLM_MODEL")
		if strings.TrimSpace(apiKey) == "" {
			return nil, fmt.Errorf("SENTINEL_LLM_API_KEY is required for -agent llm; the value is never written to the report")
		}
		if strings.TrimSpace(model) == "" {
			return nil, fmt.Errorf("SENTINEL_LLM_MODEL is required for -agent llm")
		}
		provider := os.Getenv("SENTINEL_LLM_PROVIDER")
		if strings.TrimSpace(provider) == "" {
			provider = llmagent.DefaultProvider
		}
		endpoint := os.Getenv("SENTINEL_LLM_BASE_URL")
		client, err := llmagent.NewHTTPClient(apiKey, endpoint, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("invalid model provider configuration: %w", err)
		}
		config := llmagent.Config{Provider: provider, Model: model}
		if _, err := llmagent.New(config, client); err != nil {
			return nil, fmt.Errorf("invalid model agent configuration: %w", err)
		}
		metadata := evaluation.AgentMetadata{
			AgentType: "llm", Provider: provider, Model: model,
			PromptVersion: llmagent.PromptVersion, ConfigurationVersion: llmagent.ConfigurationVersion,
		}
		return []evaluation.AgentConfig{{
			Name: "llm-agent", Metadata: metadata,
			NewAgent: func() evaluation.Agent {
				agent, _ := llmagent.New(config, client)
				return agent
			},
		}}, nil
	default:
		return nil, fmt.Errorf("unsupported -agent %q; choose baselines or llm", mode)
	}
}
