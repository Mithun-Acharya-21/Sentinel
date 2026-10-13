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

type HTTPClient struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func NewHTTPClient(apiKey, endpoint string, timeout time.Duration) (*HTTPClient, error) {
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
	return &HTTPClient{endpoint: endpoint, apiKey: apiKey, client: &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *HTTPClient) Complete(ctx context.Context, input CompletionRequest) (CompletionResponse, error) {
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
	response, err := c.client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return CompletionResponse{}, &HTTPError{Category: "timeout"}
		}
		return CompletionResponse{}, &HTTPError{Category: "network_error"}
	}
	defer response.Body.Close()
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
		return CompletionResponse{}, &HTTPError{Category: category, StatusCode: response.StatusCode}
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
	}
	if err := json.Unmarshal(body, &decoded); err != nil || len(decoded.Choices) != 1 || decoded.Choices[0].Message.Role != "assistant" {
		return CompletionResponse{}, &HTTPError{Category: "malformed_response"}
	}
	choice := decoded.Choices[0]
	content := ""
	if choice.Message.Content != nil {
		content = *choice.Message.Content
	}
	return CompletionResponse{Content: content, ToolCalls: choice.Message.ToolCalls, FinishReason: choice.FinishReason}, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
