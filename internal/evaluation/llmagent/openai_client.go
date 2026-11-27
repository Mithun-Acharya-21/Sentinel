package llmagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultChatCompletionsURL = "https://api.openai.com/v1/chat/completions"
	maxProviderResponseBytes  = 1 << 20
)

// HTTPError contains only safe classification data; response bodies and headers
// are intentionally never retained or included in Error().
type HTTPError struct {
	Category   string
	StatusCode int
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string { return e.SafeMessage() }

func (e *HTTPError) SafeMessage() string {
	switch e.Category {
	case "timeout":
		return "provider request timed out"
	case "rate_limit":
		return "provider rate limit reached"
	case "authentication":
		return "provider authentication failed"
	case "provider_server_error":
		return "provider returned a server error"
	case "malformed_response":
		return "provider returned a malformed response"
	case "network_error":
		return "provider network request failed"
	default:
		return "provider rejected the request"
	}
}

type RoundTripper interface {
	RoundTrip(*http.Request) (*http.Response, error)
}

type HTTPClient struct {
	endpoint  string
	apiKey    string
	transport http.RoundTripper
	timeout   time.Duration
	retry     RetryPolicy
}

// Client returns an *http.Client configured with the HTTPClient's transport and timeout.
// This preserves backward compatibility for existing tests that inspect or inject Transport.
func (c *HTTPClient) Client() *http.Client {
	return &http.Client{
		Timeout:       c.timeout,
		Transport:     c.transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func NewHTTPClient(apiKey, endpoint string, timeout time.Duration) (*HTTPClient, error) {
	return NewHTTPClientWithRetry(apiKey, endpoint, timeout, DefaultRetryPolicy())
}

func NewHTTPClientWithRetry(apiKey, endpoint string, timeout time.Duration, retry RetryPolicy) (*HTTPClient, error) {
	return NewHTTPClientWithTransport(apiKey, endpoint, timeout, retry, nil)
}

func NewHTTPClientWithTransport(apiKey, endpoint string, timeout time.Duration, retry RetryPolicy, transport http.RoundTripper) (*HTTPClient, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("model API key is required")
	}
	if endpoint == "" {
		endpoint = DefaultChatCompletionsURL
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("provider endpoint must be an absolute HTTP(S) URL")
	}
	if parsed.Scheme == "http" && !isLoopback(parsed.Hostname()) {
		return nil, errors.New("non-loopback provider endpoints must use HTTPS")
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if timeout < time.Second || timeout > 2*time.Minute {
		return nil, errors.New("provider timeout must be between 1 second and 2 minutes")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &HTTPClient{
		endpoint:  endpoint,
		apiKey:    apiKey,
		transport: transport,
		timeout:   timeout,
		retry:     retry,
	}, nil
}

func (c *HTTPClient) Complete(ctx context.Context, input CompletionRequest) (CompletionResponse, error) {
	maxAttempts := c.retry.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	startTotal := time.Now()
	var diagnostics []AttemptDiagnostic
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return CompletionResponse{
				Attempts:     attempt - 1,
				TotalLatency: time.Since(startTotal),
				Diagnostics:  diagnostics,
			}, &HTTPError{Category: "timeout"}
		}

		attemptStart := time.Now()
		resp, httpErr := c.doSingleRequest(ctx, input)
		duration := time.Since(attemptStart)

		diag := AttemptDiagnostic{
			Attempt:    attempt,
			DurationMs: duration.Milliseconds(),
		}

		if httpErr == nil {
			resp.Attempts = attempt
			resp.RequestLatency = duration
			resp.TotalLatency = time.Since(startTotal)
			diag.StatusCode = 200
			diag.Category = "success"
			diagnostics = append(diagnostics, diag)
			resp.Diagnostics = diagnostics
			return resp, nil
		}

		lastErr = httpErr
		diag.StatusCode = httpErr.StatusCode
		diag.Category = httpErr.Category
		diag.Error = httpErr.SafeMessage()

		// Determine if retryable and not at max attempts
		retryable := IsRetryable(httpErr.StatusCode, httpErr) && attempt < maxAttempts
		diag.Retryable = &retryable
		if !retryable {
			diagnostics = append(diagnostics, diag)
			break
		}

		// Calculate jittered delay, taking into account Retry-After
		delay := c.retry.ComputeDelay(attempt, httpErr.RetryAfter)
		diag.RetryDelayMs = delay.Milliseconds()
		diagnostics = append(diagnostics, diag)

		// Context-aware sleep
		sleeper := c.retry.Sleeper
		if sleeper == nil {
			sleeper = defaultSleep
		}
		if sleepErr := sleeper(ctx, delay); sleepErr != nil {
			return CompletionResponse{
				Attempts:     attempt,
				TotalLatency: time.Since(startTotal),
				Diagnostics:  diagnostics,
			}, &HTTPError{Category: "timeout"}
		}
	}

	return CompletionResponse{
		Attempts:     len(diagnostics),
		TotalLatency: time.Since(startTotal),
		Diagnostics:  diagnostics,
	}, lastErr
}

func (c *HTTPClient) doSingleRequest(ctx context.Context, input CompletionRequest) (CompletionResponse, *HTTPError) {
	requestBody := struct {
		Model               string           `json:"model"`
		Messages            []Message        `json:"messages"`
		Tools               []ToolDefinition `json:"tools"`
		ToolChoice          string           `json:"tool_choice"`
		ParallelToolCalls   bool             `json:"parallel_tool_calls"`
		MaxCompletionTokens int              `json:"max_completion_tokens"`
	}{input.Model, input.Messages, input.Tools, "auto", false, input.MaxOutputTokens}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return CompletionResponse{}, &HTTPError{Category: "malformed_request"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return CompletionResponse{}, &HTTPError{Category: "malformed_request"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.apiKey)

	reqCtx := ctx
	var cancel context.CancelFunc
	if c.timeout > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	request = request.WithContext(reqCtx)

	response, err := c.transport.RoundTrip(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(reqCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return CompletionResponse{}, &HTTPError{Category: "timeout"}
		}
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(reqCtx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return CompletionResponse{}, &HTTPError{Category: "context_cancelled"}
		}
		return CompletionResponse{}, &HTTPError{Category: "network_error"}
	}
	defer response.Body.Close()

	retryAfter := ParseRetryAfter(response.Header.Get("Retry-After"), time.Now())

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		category := "api_error"
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			category = "authentication"
		case http.StatusTooManyRequests:
			category = "rate_limit"
		default:
			if response.StatusCode >= 500 {
				category = "provider_server_error"
			}
		}
		return CompletionResponse{}, &HTTPError{Category: category, StatusCode: response.StatusCode, RetryAfter: retryAfter}
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponseBytes+1))
	if err != nil || len(body) > maxProviderResponseBytes {
		return CompletionResponse{}, &HTTPError{Category: "malformed_response"}
	}
	var decoded struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role      string     `json:"role"`
				Content   *string    `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || len(decoded.Choices) != 1 || decoded.Choices[0].Message.Role != "assistant" {
		return CompletionResponse{}, &HTTPError{Category: "malformed_response"}
	}
	choice := decoded.Choices[0]
	content := ""
	if choice.Message.Content != nil {
		content = *choice.Message.Content
	}

	usage := Usage{Available: false}
	if decoded.Usage != nil {
		usage = Usage{
			PromptTokens:     decoded.Usage.PromptTokens,
			CompletionTokens: decoded.Usage.CompletionTokens,
			TotalTokens:      decoded.Usage.TotalTokens,
			Available:        true,
		}
	}

	return CompletionResponse{
		Content:      content,
		ToolCalls:    choice.Message.ToolCalls,
		FinishReason: choice.FinishReason,
		Usage:        usage,
	}, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
