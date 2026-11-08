// Package triage runs the intake pipeline for one request:
//
//	Step 2 classify  ─┐  (run concurrently: they only need the raw message)
//	Step 3 enrich    ─┘
//	Steps 4 + 6 route and escalate  (deterministic Go rules, package routing)
//	Step 5 summarize                (LLM, written after routing so it can say where and why)
//
// The LLM handles the fuzzy work (understanding text); code handles every
// decision that must be predictable (queues, thresholds, escalation).
package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"arcvault-triage/internal/llm"
	"arcvault-triage/internal/routing"
)

// LLM is the one capability the pipeline needs from a model provider.
type LLM interface {
	CompleteJSON(ctx context.Context, req llm.Request, out any) (llm.Usage, error)
}

type Pipeline struct {
	LLM               LLM
	Rules             routing.Rules
	Samples           int     // classification samples for self-consistency; 1 = single call
	SampleTemperature float64 // used when Samples > 1; a single call uses temperature 0
	Model             string  // recorded in each output record
	Log               *slog.Logger
}

// Process never fails: any step that errors is recorded as a warning and the
// request is routed to human review, so no message is silently dropped.
func (p *Pipeline) Process(ctx context.Context, req Request) Record {
	start := time.Now()
	log := p.logger().With("id", req.ID)
	meter := &usageMeter{}

	rec := Record{
		ID:         req.ID,
		Source:     req.Source,
		ReceivedAt: req.ReceivedAt,
		Message:    req.Message,
		Warnings:   []string{},
	}

	// Steps 2 and 3, concurrently.
	n := max(p.Samples, 1)
	temperature := 0.0
	if n > 1 {
		temperature = p.SampleTemperature
	}
	user := messageBlock(req)
	outputs := make([]classifierOutput, n)
	errs := make([]error, n)
	var enrichment Enrichment
	var enrichErr error

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, err := p.LLM.CompleteJSON(ctx, llm.Request{
				System: classifyPrompt, User: user,
				SchemaName: "classification", Schema: classifySchema, Temperature: temperature,
			}, &outputs[i])
			meter.add(u)
			if err == nil {
				err = outputs[i].validate()
			}
			errs[i] = err
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		u, err := p.LLM.CompleteJSON(ctx, llm.Request{
			System: enrichPrompt, User: user,
			SchemaName: "enrichment", Schema: enrichmentSchema, Temperature: 0,
		}, &enrichment)
		meter.add(u)
		enrichErr = err
	}()
	wg.Wait()

	failed := false
	var good []classifierOutput
	for i, err := range errs {
		if err != nil {
			rec.Warnings = append(rec.Warnings, fmt.Sprintf("classification sample %d of %d failed: %v", i+1, n, err))
			continue
		}
		good = append(good, outputs[i])
	}
	if len(good) == 0 {
		failed = true
		rec.Classification = Classification{
			Category:         "Unclassified",
			Priority:         "High",
			Rationale:        "Classification failed; see warnings.",
			ConfidenceDetail: ConfidenceDetail{Method: "none", Votes: map[string]int{}},
		}
	} else {
		rec.Classification = aggregate(good)
	}

	if enrichErr != nil {
		failed = true
		rec.Warnings = append(rec.Warnings, "enrichment failed: "+enrichErr.Error())
		enrichment = Enrichment{AffectedScope: "unknown", Urgency: "unknown"}
	}
	rec.Warnings = append(rec.Warnings, ground(req.Message, &enrichment)...)
	rec.Enrichment = enrichment

	// Steps 4 and 6: deterministic routing and escalation.
	amounts := make([]routing.Amount, 0, len(enrichment.Amounts))
	for _, a := range enrichment.Amounts {
		amounts = append(amounts, routing.Amount{Role: a.Role, Value: a.Value, Currency: a.Currency})
	}
	rec.Routing = p.Rules.Decide(routing.Input{
		Message:       req.Message,
		Category:      rec.Classification.Category,
		Confidence:    rec.Classification.Confidence,
		AffectedScope: enrichment.AffectedScope,
		Amounts:       amounts,
		Failed:        failed,
	})

	// Step 5: summary for the receiving team.
	summary, err := p.summarize(ctx, req, rec, meter)
	if err != nil {
		rec.Warnings = append(rec.Warnings, "summary failed, used a template instead: "+err.Error())
		summary = fallbackSummary(rec)
	}
	rec.Summary = summary

	rec.ProcessedAt = time.Now().UTC().Format(time.RFC3339)
	rec.Processing = meter.processing(p.Model, time.Since(start))

	log.Info("processed",
		"category", rec.Classification.Category,
		"priority", rec.Classification.Priority,
		"confidence", rec.Classification.Confidence,
		"queue", rec.Routing.DestinationQueue,
		"escalated", rec.Routing.Escalated,
		"warnings", len(rec.Warnings),
		"ms", rec.Processing.DurationMs,
	)
	return rec
}

// summaryInput is what the summary prompt sees, besides the raw message.
type summaryInput struct {
	Channel               string       `json:"channel"`
	Category              string       `json:"category"`
	Priority              string       `json:"priority"`
	Confidence            float64      `json:"confidence"`
	CoreIssue             string       `json:"core_issue"`
	Identifiers           []Identifier `json:"identifiers"`
	Amounts               []Amount     `json:"amounts"`
	TimeReferences        []string     `json:"time_references"`
	AffectedScope         string       `json:"affected_scope"`
	Urgency               string       `json:"urgency"`
	DestinationQueue      string       `json:"destination_queue"`
	Escalated             bool         `json:"escalated"`
	EscalationReasons     []string     `json:"escalation_reasons"`
	BillingDiscrepancyUSD *float64     `json:"billing_discrepancy_usd"`
}

func (p *Pipeline) summarize(ctx context.Context, req Request, rec Record, meter *usageMeter) (string, error) {
	in, err := json.MarshalIndent(summaryInput{
		Channel:               rec.Source,
		Category:              rec.Classification.Category,
		Priority:              rec.Classification.Priority,
		Confidence:            rec.Classification.Confidence,
		CoreIssue:             rec.Enrichment.CoreIssue,
		Identifiers:           rec.Enrichment.Identifiers,
		Amounts:               rec.Enrichment.Amounts,
		TimeReferences:        rec.Enrichment.TimeReferences,
		AffectedScope:         rec.Enrichment.AffectedScope,
		Urgency:               rec.Enrichment.Urgency,
		DestinationQueue:      rec.Routing.DestinationQueue,
		Escalated:             rec.Routing.Escalated,
		EscalationReasons:     rec.Routing.EscalationReasons,
		BillingDiscrepancyUSD: rec.Routing.BillingDiscrepancyUSD,
	}, "", "  ")
	if err != nil {
		return "", err
	}

	var out struct {
		Summary string `json:"summary"`
	}
	u, err := p.LLM.CompleteJSON(ctx, llm.Request{
		System:     summarizePrompt,
		User:       messageBlock(req) + "\n<triage_result>\n" + string(in) + "\n</triage_result>",
		SchemaName: "summary", Schema: summarySchema, Temperature: 0,
	}, &out)
	meter.add(u)
	if err != nil {
		return "", err
	}
	if s := strings.TrimSpace(out.Summary); s != "" {
		return s, nil
	}
	return "", errors.New("model returned an empty summary")
}

func fallbackSummary(rec Record) string {
	s := fmt.Sprintf("%s (%s priority), routed to %s.", rec.Classification.Category, rec.Classification.Priority, rec.Routing.DestinationQueue)
	if rec.Enrichment.CoreIssue != "" {
		s += " " + rec.Enrichment.CoreIssue
	}
	if rec.Routing.Escalated {
		s += " Needs human review: " + strings.Join(rec.Routing.EscalationReasons, "; ") + "."
	}
	return s
}

// messageBlock wraps the customer text in tags so the prompts can tell the
// model to treat it as data. A closing tag inside the message is neutralised
// so a customer cannot "close" the block and inject instructions.
func messageBlock(req Request) string {
	msg := strings.ReplaceAll(req.Message, "</customer_message>", "</customer-message>")
	return fmt.Sprintf("Channel: %s\n<customer_message>\n%s\n</customer_message>", req.Source, msg)
}

func (p *Pipeline) logger() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

type usageMeter struct {
	mu                       sync.Mutex
	calls, prompt, completed int
}

func (m *usageMeter) add(u llm.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.prompt += u.PromptTokens
	m.completed += u.CompletionTokens
}

func (m *usageMeter) processing(model string, d time.Duration) Processing {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Processing{
		Model:            model,
		DurationMs:       d.Milliseconds(),
		LLMCalls:         m.calls,
		PromptTokens:     m.prompt,
		CompletionTokens: m.completed,
	}
}
