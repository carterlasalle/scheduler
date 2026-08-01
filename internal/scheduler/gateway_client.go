package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GatewayClient calls the Hermes gateway API instead of spawning processes.
type GatewayClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	timeout    time.Duration
}

// NewGatewayClient creates a client targeting the Hermes gateway API.
func NewGatewayClient(baseURL, apiKey string, timeout time.Duration) *GatewayClient {
	return &GatewayClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		timeout: timeout,
	}
}

// ResponseRequest mirrors the Hermes /v1/responses request body.
type ResponseRequest struct {
	Input           string `json:"input"`
	Model           string `json:"model,omitempty"`
	RequireApproval *bool  `json:"require_approval,omitempty"` // nil = use gateway default, false = disable approvals
}

// Response mirrors the Hermes /v1/responses response body.
type Response struct {
	ID     string         `json:"id"`
	Status string         `json:"status"`
	Model  string         `json:"model"`
	Output []OutputItem   `json:"output"`
	Usage  Usage          `json:"usage"`
	Error  *ResponseError `json:"error,omitempty"`
}

// OutputItem is a message or tool call in the response output.
type OutputItem struct {
	Type    string         `json:"type"`
	Role    string         `json:"role,omitempty"`
	Content []ContentBlock `json:"content,omitempty"`
}

// ContentBlock is a block of content (text, tool_use, etc.)
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Usage holds token usage info.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ResponseError is an error from the API.
type ResponseError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// ExtractText returns the first output_text block content, or empty string.
func (r *Response) ExtractText() string {
	for _, item := range r.Output {
		if item.Type == "message" {
			for _, block := range item.Content {
				if block.Type == "output_text" {
					return block.Text
				}
			}
		}
	}
	return ""
}

// Ping checks whether the gateway API is reachable and authenticated.
func (g *GatewayClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", g.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	g.setAuth(req, "")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return fmt.Errorf("gateway health: HTTP %d", resp.StatusCode)
	}
	return nil
}

// SendResponse sends a prompt to the gateway and returns the text result.
// This replaces exec.Command("hermes", "chat", "-q", prompt, ...)
//
// key overrides the client's default API key for this one request. Pass ""
// to use the daemon-level shared key (--gateway-key). Foreman spawns pass
// project.GatewayKey when set, so each foreman authenticates with its own
// key (Bane 2026-07-31).
func (g *GatewayClient) SendResponse(ctx context.Context, prompt, model, key string) (*Response, error) {
	noApproval := false
	reqBody := ResponseRequest{
		Input:           prompt,
		Model:           model,
		RequireApproval: &noApproval, // scheduler agents never need approval
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", g.baseURL+"/v1/responses",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	g.setAuth(req, key)

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gateway POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result Response
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if result.Error != nil {
		return nil, fmt.Errorf("gateway error: %s — %s", result.Error.Type, result.Error.Message)
	}

	return &result, nil
}

// setAuth sets the Authorization header. A non-empty key overrides the
// client default (per-foreman key); empty falls back to the shared daemon key.
func (g *GatewayClient) setAuth(req *http.Request, key string) {
	effective := key
	if effective == "" {
		effective = g.apiKey
	}
	if effective != "" {
		req.Header.Set("Authorization", "Bearer "+effective)
	}
}

// ResetHttpClient replaces the internal http.Client with a fresh one,
// avoiding stale connection pools after a gateway restart.
func (g *GatewayClient) ResetHttpClient() {
	g.httpClient = &http.Client{Timeout: g.timeout}
}
