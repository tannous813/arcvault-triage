// Package config loads settings from environment variables, reading a local
// .env file first if one exists. Real environment variables take precedence
// over values in .env.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Addr                string
	LLMBaseURL          string
	LLMAPIKey           string
	LLMModel            string
	ReasoningEffort     string
	ClassifySamples     int
	SampleTemperature   float64
	ConfidenceThreshold float64
	BillingThresholdUSD float64
	OutputDir           string
	WebhookURL          string
}

func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("reading .env: %w", err)
	}

	var errs []error
	c := Config{
		Addr:                str("ADDR", ":8080"),
		LLMBaseURL:          str("LLM_BASE_URL", "https://api.groq.com/openai/v1"),
		LLMAPIKey:           os.Getenv("LLM_API_KEY"),
		LLMModel:            str("LLM_MODEL", "openai/gpt-oss-120b"),
		ReasoningEffort:     str("LLM_REASONING_EFFORT", "low"),
		ClassifySamples:     integer("CLASSIFY_SAMPLES", 3, &errs),
		SampleTemperature:   float("SAMPLE_TEMPERATURE", 0.7, &errs),
		ConfidenceThreshold: float("CONFIDENCE_THRESHOLD", 0.70, &errs),
		BillingThresholdUSD: float("BILLING_ESCALATION_USD", 500, &errs),
		OutputDir:           str("OUTPUT_DIR", "output"),
		WebhookURL:          os.Getenv("WEBHOOK_URL"),
	}

	if c.LLMAPIKey == "" {
		errs = append(errs, errors.New("LLM_API_KEY is not set (copy .env.example to .env and fill it in)"))
	}
	if c.ClassifySamples < 1 || c.ClassifySamples > 10 {
		errs = append(errs, fmt.Errorf("CLASSIFY_SAMPLES must be between 1 and 10, got %d", c.ClassifySamples))
	}
	if c.ConfidenceThreshold < 0 || c.ConfidenceThreshold > 1 {
		errs = append(errs, fmt.Errorf("CONFIDENCE_THRESHOLD must be between 0 and 1, got %v", c.ConfidenceThreshold))
	}
	return c, errors.Join(errs...)
}

func str(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func integer(key string, def int, errs *[]error) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not an integer", key, v))
		return def
	}
	return n
}

func float(key string, def float64, errs *[]error) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a number", key, v))
		return def
	}
	return f
}

// loadDotEnv sets KEY=VALUE pairs from path without overriding variables
// that are already set in the environment.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}
