package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompleteJSONSendsStrictSchemaAndRetries429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body struct {
			Model          string `json:"model"`
			ResponseFormat struct {
				Type       string `json:"type"`
				JSONSchema struct {
					Name   string          `json:"name"`
					Strict bool            `json:"strict"`
					Schema json.RawMessage `json:"schema"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ResponseFormat.Type != "json_schema" || !body.ResponseFormat.JSONSchema.Strict ||
			body.ResponseFormat.JSONSchema.Name != "demo" || string(body.ResponseFormat.JSONSchema.Schema) != `{"type":"object"}` {
			t.Errorf("response_format not sent as strict json_schema: %+v", body.ResponseFormat)
		}

		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": `{"answer":"ok"}`}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 12, "completion_tokens": 3},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key", "test-model")
	c.BaseDelay = 0

	var out struct{ Answer string }
	usage, err := c.CompleteJSON(context.Background(), Request{
		System: "s", User: "u", SchemaName: "demo", Schema: json.RawMessage(`{"type":"object"}`),
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.Answer != "ok" || usage.PromptTokens != 12 || calls.Load() != 2 {
		t.Errorf("answer=%q usage=%+v calls=%d", out.Answer, usage, calls.Load())
	}
}

func TestCompleteJSONDoesNotRetryBadRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":"invalid schema"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "m")
	c.BaseDelay = 0
	var out map[string]any
	_, err := c.CompleteJSON(context.Background(), Request{Schema: json.RawMessage(`{}`)}, &out)

	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadRequest || calls.Load() != 1 {
		t.Errorf("err=%v calls=%d, want one 400 with no retry", err, calls.Load())
	}
}

func TestRetryHint(t *testing.T) {
	cases := map[string]time.Duration{
		`Rate limit reached ... Please try again in 8.025s. Need more tokens?`: 8025*time.Millisecond + 250*time.Millisecond,
		`try again in 1m2.5s`:  62500*time.Millisecond + 250*time.Millisecond,
		`try again in 520ms`:   770 * time.Millisecond,
		`no hint in this body`: 0,
	}
	for body, want := range cases {
		if got := retryHint(body); got != want {
			t.Errorf("retryHint(%q) = %v, want %v", body, got, want)
		}
	}
}
