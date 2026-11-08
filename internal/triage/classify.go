package triage

import (
	"fmt"
	"math"
	"slices"

	"arcvault-triage/internal/routing"
)

// classifierOutput is one raw answer from the classification prompt.
type classifierOutput struct {
	Rationale  string  `json:"rationale"`
	Category   string  `json:"category"`
	Priority   string  `json:"priority"`
	Confidence float64 `json:"confidence"`
}

var priorities = []string{"Low", "Medium", "High"}

func (o classifierOutput) validate() error {
	if !slices.Contains(routing.Categories, o.Category) {
		return fmt.Errorf("unknown category %q", o.Category)
	}
	if !slices.Contains(priorities, o.Priority) {
		return fmt.Errorf("unknown priority %q", o.Priority)
	}
	return nil
}

// aggregate combines several classification samples (self-consistency).
//
// LLM self-reported confidence is poorly calibrated: models say 0.9 for
// almost everything. Sampling the same prompt several times at a non-zero
// temperature and measuring how often the answers agree is a cheap, more
// honest signal. The final confidence is the lower of the two, so either
// disagreement between samples or low self-reported certainty pulls it down.
//
// With one sample this reduces to the model's self-reported confidence.
func aggregate(samples []classifierOutput) Classification {
	votes := map[string]int{}
	confSum := map[string]float64{}
	for _, s := range samples {
		votes[s.Category]++
		confSum[s.Category] += clamp01(s.Confidence)
	}

	// Majority vote; ties go to the higher mean confidence, then alphabetical
	// order, so the result never depends on map iteration order.
	mean := func(cat string) float64 { return confSum[cat] / float64(votes[cat]) }
	winner := ""
	for cat := range votes {
		switch {
		case winner == "",
			votes[cat] > votes[winner],
			votes[cat] == votes[winner] && mean(cat) > mean(winner),
			votes[cat] == votes[winner] && mean(cat) == mean(winner) && cat < winner:
			winner = cat
		}
	}

	// Among the winning samples: most common priority (ties go to the more
	// severe one) and the rationale of the most confident sample.
	prioVotes := map[string]int{}
	var best classifierOutput
	bestConf := -1.0
	for _, s := range samples {
		if s.Category != winner {
			continue
		}
		prioVotes[s.Priority]++
		if c := clamp01(s.Confidence); c > bestConf {
			bestConf, best = c, s
		}
	}
	priority := ""
	for _, p := range []string{"High", "Medium", "Low"} {
		if prioVotes[p] > prioVotes[priority] {
			priority = p
		}
	}

	agreement := float64(votes[winner]) / float64(len(samples))
	reported := mean(winner)
	method := "model_reported"
	if len(samples) > 1 {
		method = "self_consistency"
	}

	return Classification{
		Category:   winner,
		Priority:   priority,
		Confidence: round2(math.Min(agreement, reported)),
		Rationale:  best.Rationale,
		ConfidenceDetail: ConfidenceDetail{
			Method:        method,
			Samples:       len(samples),
			Agreement:     round2(agreement),
			ModelReported: round2(reported),
			Votes:         votes,
		},
	}
}

func clamp01(f float64) float64 { return math.Max(0, math.Min(1, f)) }

func round2(f float64) float64 { return math.Round(f*100) / 100 }
