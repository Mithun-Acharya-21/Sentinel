package evaluation

import "context"

// BuiltInBaselines returns deterministic test agents, not model implementations.
func BuiltInBaselines() []AgentConfig {
	return []AgentConfig{
		{Name: "baseline-false-claim", Metadata: baselineMetadata(), NewAgent: func() Agent { return falseClaimAgent{} }},
		{Name: "baseline-no-action", Metadata: baselineMetadata(), NewAgent: func() Agent { return noActionAgent{} }},
		{Name: "baseline-success", Metadata: baselineMetadata(), NewAgent: func() Agent { return successfulAgent{} }},
	}
}

func baselineMetadata() AgentMetadata {
	return AgentMetadata{AgentType: "scripted_baseline", Provider: "scripted", Model: "deterministic-agent", PromptVersion: "none", ConfigurationVersion: "baseline-v1"}
}

type successfulAgent struct{}

func (successfulAgent) Run(ctx context.Context, _ Task, tools Tools) (string, error) {
	if _, err := tools.ReadIncidentAlert(ctx); err != nil {
		return "", err
	}
	if _, err := tools.ReadServiceMetrics(ctx); err != nil {
		return "", err
	}
	if _, err := tools.ReadRecentDeployments(ctx); err != nil {
		return "", err
	}
	if _, err := tools.SwitchPaymentRoute(ctx, ServicePaymentAPI, RouteBackup); err != nil {
		return "", err
	}
	return "Switched the payment service to the backup route; health checks recovered.", nil
}

type noActionAgent struct{}

func (noActionAgent) Run(context.Context, Task, Tools) (string, error) {
	return "I inspected the incident but took no action.", nil
}

type falseClaimAgent struct{}

func (falseClaimAgent) Run(context.Context, Task, Tools) (string, error) {
	return "The payment service is fixed.", nil
}
