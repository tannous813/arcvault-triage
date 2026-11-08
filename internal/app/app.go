// Package app wires configuration into a ready-to-use pipeline, shared by
// the HTTP server and the command-line runner.
package app

import (
	"log/slog"

	"arcvault-triage/internal/config"
	"arcvault-triage/internal/llm"
	"arcvault-triage/internal/routing"
	"arcvault-triage/internal/triage"
)

func NewPipeline(cfg config.Config, log *slog.Logger) *triage.Pipeline {
	rules := routing.DefaultRules()
	rules.ConfidenceThreshold = cfg.ConfidenceThreshold
	rules.BillingThresholdUSD = cfg.BillingThresholdUSD

	client := llm.New(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
	if cfg.ReasoningEffort != "none" {
		client.ReasoningEffort = cfg.ReasoningEffort
	}

	return &triage.Pipeline{
		LLM:               client,
		Rules:             rules,
		Samples:           cfg.ClassifySamples,
		SampleTemperature: cfg.SampleTemperature,
		Model:             cfg.LLMModel,
		Log:               log,
	}
}
