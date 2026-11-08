// Package llm is a minimal client for OpenAI-compatible chat completion APIs
// (Groq by default). It does one thing: send a system prompt and a user
// message with a strict JSON schema, and decode the model's JSON answer into
// a Go value. Strict mode uses constrained decoding, so a successful response
// always matches the schema.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Request struct {
	System      string
	User        string
	SchemaName  string
	Schema      json.RawMessage
	Temperature float64
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type Client struct {
	BaseURL         string
	APIKey          string
	Model           string
	ReasoningEffort string // low, medium or high for reasoning models; empty omits it
	HTTP            *http.Client
	MaxRetries      int
	BaseDelay       time.Duration
	Log             *slog.Logger // optional; logs each retry
}

func New(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		Model:      model,
		HTTP:       &http.Client{Timeout: 30 * time.Second}, // per attempt: a stuck call is abandoned and retried
		MaxRetries: 6,                                       // 1+2+4+8+16+30s: enough to ride out a per-minute rate-limit window
		BaseDelay:  time.Second,
	}
}

// StatusError is returned when the API answers with a non-200 status.
type StatusError struct {
	Code       int
	Body       string
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("LLM API returned HTTP %d: %s", e.Code, truncate(e.Body, 300))
}

// ErrBadOutput means the API answered but the content could not be decoded.
var ErrBadOutput = errors.New("model output could not be decoded")

// CompleteJSON sends the request and decodes the JSON answer into out.
// Rate limits (429), server errors (5xx), network errors and undecodable
// output are retried with exponential backoff; other 4xx errors are not.
func (c *Client) CompleteJSON(ctx context.Context, req Request, out any) (Usage, error) {
	body := map[string]any{
		"model":       c.Model,
		"temperature": req.Temperature,
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   req.SchemaName,
				"strict": true,
				"schema": req.Schema,
			},
		},
	}
	if c.ReasoningEffort != "" {
		body["reasoning_effort"] = c.ReasoningEffort
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Usage{}, fmt.Errorf("encoding request: %w", err)
	}

	for attempt := 1; ; attempt++ {
		usage, err := c.send(ctx, payload, out)
		if err == nil {
			return usage, nil
		}
		if !retryable(ctx, err) {
			return usage, err
		}
		if attempt > c.MaxRetries {
			return Usage{}, fmt.Errorf("giving up after %d attempts: %w", attempt, err)
		}
		wait := c.backoff(attempt, err)
		if c.Log != nil {
			c.Log.Warn("LLM call failed, retrying", "attempt", attempt, "wait", wait.Round(time.Millisecond), "err", truncate(err.Error(), 160))
		}
		if serr := sleep(ctx, wait); serr != nil {
			return Usage{}, fmt.Errorf("%w while waiting to retry (last error: %v)", serr, err)
		}
	}
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

func (c *Client) send(ctx context.Context, payload []byte, out any) (Usage, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Usage{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Usage{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Usage{}, err
	}

	if resp.StatusCode != http.StatusOK {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		if hint := retryHint(string(body)); hint > retryAfter {
			retryAfter = hint
		}
		return Usage{}, &StatusError{Code: resp.StatusCode, Body: string(body), RetryAfter: retryAfter}
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil || len(cr.Choices) == 0 {
		return Usage{}, fmt.Errorf("%w: unexpected response shape: %s", ErrBadOutput, truncate(string(body), 200))
	}
	choice := cr.Choices[0]
	if choice.FinishReason == "length" {
		return cr.Usage, fmt.Errorf("%w: output was truncated", ErrBadOutput)
	}
	if err := json.Unmarshal([]byte(choice.Message.Content), out); err != nil {
		return cr.Usage, fmt.Errorf("%w: %v", ErrBadOutput, err)
	}
	return cr.Usage, nil
}

func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	return true // network errors and undecodable output are worth another try
}

func (c *Client) backoff(attempt int, lastErr error) time.Duration {
	d := c.BaseDelay << (attempt - 1)
	d += time.Duration(rand.Int63n(int64(c.BaseDelay)/2 + 1)) // jitter
	var se *StatusError
	if errors.As(lastErr, &se) && se.RetryAfter > d {
		d = se.RetryAfter
	}
	return min(d, 30*time.Second)
}

func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

// retryHintRe matches rate-limit messages such as Groq's
// "Please try again in 8.025s" or "try again in 1m2.5s".
var retryHintRe = regexp.MustCompile(`try again in ([0-9.]+(?:ms|s|m|h)(?:[0-9.]+(?:ms|s))*)`)

func retryHint(body string) time.Duration {
	m := retryHintRe.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	d, err := time.ParseDuration(m[1])
	if err != nil {
		return 0
	}
	return d + 250*time.Millisecond // small margin so the window has actually reset
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
