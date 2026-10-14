// Package llm is a minimal OpenAI-compatible chat-completions client used
// for optional dictation polish and meeting summaries. A configured
// non-local endpoint receives the text it is asked to process.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ChatURL accepts an API root, a /v1 base or a full chat URL.
func ChatURL(endpoint string) string {
	base := strings.TrimRight(endpoint, "/")
	switch {
	case strings.HasSuffix(base, "/chat/completions"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

// ModelsURL is the model listing for the same endpoint forms as ChatURL.
func ModelsURL(endpoint string) string {
	base := strings.TrimRight(endpoint, "/")
	base = strings.TrimSuffix(base, "/chat/completions")
	if strings.HasSuffix(base, "/v1") {
		return base + "/models"
	}
	return base + "/v1/models"
}

// HTTPError is a non-2xx response from the endpoint.
type HTTPError struct{ Code int }

func (e *HTTPError) Error() string { return fmt.Sprintf("endpoint returned HTTP %d", e.Code) }

// Client talks to one endpoint.
type Client struct {
	Endpoint  string
	Model     string
	APIKeyEnv string // environment variable holding a bearer key, if any
	HTTP      *http.Client

	mu       sync.Mutex
	resolved string
}

var defaultHTTP = &http.Client{}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

func (c *Client) headers(req *http.Request) error {
	req.Header.Set("Content-Type", "application/json")
	if c.APIKeyEnv == "" {
		return nil
	}
	key := os.Getenv(c.APIKeyEnv)
	if key == "" {
		return fmt.Errorf("credential environment variable %s is unset", c.APIKeyEnv)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return nil
}

// ModelInfo is one listed model.
type ModelInfo struct {
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// Models lists the endpoint's models (largest loaded model first).
func (c *Client) Models(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsURL(c.Endpoint), nil)
	if err != nil {
		return nil, err
	}
	if err := c.headers(req); err != nil {
		return nil, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Code: resp.StatusCode}
	}
	var body struct {
		Data []struct {
			ID      string  `json:"id"`
			Status  any     `json:"status"`
			Size    float64 `json:"size"`
			NParams float64 `json:"n_params"`
			Params  float64 `json:"params"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, errors.New("endpoint did not return a model list")
	}
	out := make([]ModelInfo, 0, len(body.Data))
	for _, m := range body.Data {
		status, _ := m.Status.(string)
		if obj, ok := m.Status.(map[string]any); ok {
			status, _ = obj["value"].(string)
		}
		out = append(out, ModelInfo{ID: m.ID, Status: status, Size: int64(max(m.Size, m.NParams, m.Params))})
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := out[i].Status == "" || out[i].Status == "loaded", out[j].Status == "" || out[j].Status == "loaded"
		if li != lj {
			return li
		}
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		return len(out[i].ID) > len(out[j].ID)
	})
	return out, nil
}

// model returns the configured model or the endpoint's largest loaded one.
func (c *Client) model(ctx context.Context) (string, error) {
	if c.Model != "" {
		return c.Model, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved != "" {
		return c.resolved, nil
	}
	probe, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	models, err := c.Models(probe)
	if err != nil {
		return "", err
	}
	if len(models) == 0 {
		return "", errors.New("endpoint lists no models; set a model name")
	}
	c.resolved = models[0].ID
	return c.resolved, nil
}

// Request is one chat completion.
type Request struct {
	System, User string
	MaxTokens    int
	Temperature  float64
	Timeout      time.Duration
}

// Complete returns the assistant message content.
func (c *Client) Complete(ctx context.Context, r Request) (string, error) {
	if c.Endpoint == "" {
		return "", errors.New("no endpoint configured")
	}
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	model, err := c.model(ctx)
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]any{
		"model":       model,
		"temperature": r.Temperature,
		"max_tokens":  r.MaxTokens,
		"stream":      false,
		"messages": []map[string]string{
			{"role": "system", "content": r.System},
			{"role": "user", "content": r.User},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ChatURL(c.Endpoint), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	if err := c.headers(req); err != nil {
		return "", err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", &HTTPError{Code: resp.StatusCode}
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil || len(body.Choices) == 0 {
		return "", errors.New("endpoint did not return a chat completion")
	}
	content, ok := body.Choices[0].Message.Content.(string)
	if !ok || strings.TrimSpace(content) == "" {
		return "", errors.New("endpoint returned no text")
	}
	return content, nil
}
