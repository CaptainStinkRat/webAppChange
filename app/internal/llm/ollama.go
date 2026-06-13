// Package llm provides a client for calling Ollama's API from Go.
// Ollama exposes an OpenAI-compatible /v1/chat/completions endpoint.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Message represents a chat message in the OpenAI-compatible format.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest maps to the OpenAI chat completions request body.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream,omitempty"`
}

// ChatResponse maps to the OpenAI chat completions response body.
type ChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Client talks to an Ollama server via its OpenAI-compatible API.
type Client struct {
	baseURL    string
	model      string
	httpClient *http.Client
}

// NewClient creates an LLM client pointing at the given Ollama base URL and model.
func NewClient(baseURL, model string) *Client {
	return &Client{
		baseURL: baseURL,
		model:   model,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// Chat sends a single user prompt and returns the model's text response.
func (c *Client) Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	body := ChatRequest{
		Model: c.model,
		Messages: []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}
	return c.doChat(ctx, body)
}

// ChatWithMessages sends a full message history and returns the response.
func (c *Client) ChatWithMessages(ctx context.Context, messages []Message) (string, error) {
	body := ChatRequest{
		Model:    c.model,
		Messages: messages,
	}
	return c.doChat(ctx, body)
}

func (c *Client) doChat(ctx context.Context, req ChatRequest) (string, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(req); err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("ollama returned empty choices")
	}

	return chatResp.Choices[0].Message.Content, nil
}
