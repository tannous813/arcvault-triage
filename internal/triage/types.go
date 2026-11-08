package triage

import (
	"embed"
	"encoding/json"

	"arcvault-triage/internal/routing"
)

// Request is one inbound customer message (Step 1).
type Request struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	Message    string `json:"message"`
	ReceivedAt string `json:"received_at"`
}

// Record is the structured output for one request (Step 5).
type Record struct {
	ID             string           `json:"id"`
	Source         string           `json:"source"`
	ReceivedAt     string           `json:"received_at"`
	ProcessedAt    string           `json:"processed_at"`
	Message        string           `json:"message"`
	Classification Classification   `json:"classification"`
	Enrichment     Enrichment       `json:"enrichment"`
	Routing        routing.Decision `json:"routing"`
	Summary        string           `json:"summary"`
	Warnings       []string         `json:"warnings"`
	Processing     Processing       `json:"processing"`
}

// Classification is the Step 2 result after combining all samples.
type Classification struct {
	Category         string           `json:"category"`
	Priority         string           `json:"priority"`
	Confidence       float64          `json:"confidence"`
	Rationale        string           `json:"rationale"`
	ConfidenceDetail ConfidenceDetail `json:"confidence_detail"`
}

// ConfidenceDetail shows how the confidence score was produced.
type ConfidenceDetail struct {
	Method        string         `json:"method"`         // self_consistency, model_reported or none
	Samples       int            `json:"samples"`        // successful classification calls
	Agreement     float64        `json:"agreement"`      // share of samples that chose the final category
	ModelReported float64        `json:"model_reported"` // mean self-reported confidence of those samples
	Votes         map[string]int `json:"votes"`
}

// Enrichment is the Step 3 result.
type Enrichment struct {
	CoreIssue      string       `json:"core_issue"`
	Identifiers    []Identifier `json:"identifiers"`
	Amounts        []Amount     `json:"amounts"`
	TimeReferences []string     `json:"time_references"`
	AffectedScope  string       `json:"affected_scope"`
	ProductArea    string       `json:"product_area"`
	Urgency        string       `json:"urgency"`
	UrgencyReason  string       `json:"urgency_reason"`
}

type Identifier struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type Amount struct {
	Role     string  `json:"role"`
	Value    float64 `json:"value"`
	Currency string  `json:"currency"`
}

// Processing records cost and latency, for the production-scale discussion.
type Processing struct {
	Model            string `json:"model"`
	DurationMs       int64  `json:"duration_ms"`
	LLMCalls         int    `json:"llm_calls"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
}

//go:embed prompts
var promptFS embed.FS

var (
	classifyPrompt   = mustRead("prompts/classify.md")
	enrichPrompt     = mustRead("prompts/enrich.md")
	summarizePrompt  = mustRead("prompts/summarize.md")
	classifySchema   = json.RawMessage(mustRead("prompts/classification.schema.json"))
	enrichmentSchema = json.RawMessage(mustRead("prompts/enrichment.schema.json"))
	summarySchema    = json.RawMessage(mustRead("prompts/summary.schema.json"))
)

func mustRead(name string) string {
	b, err := promptFS.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return string(b)
}
