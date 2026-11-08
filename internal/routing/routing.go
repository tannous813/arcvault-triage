// Package routing is the deterministic part of triage (Steps 4 and 6): it
// maps a classification to a destination queue and decides when a request
// must go to human review instead. There are no LLM calls here, so every
// rule can be unit-tested and explained line by line.
package routing

import (
	"fmt"
	"math"
	"strings"
)

// Destination queues.
const (
	QueueEngineering = "Engineering"
	QueueProduct     = "Product"
	QueueBilling     = "Billing"
	QueueSupport     = "Support"
	QueueEscalation  = "Escalation" // human review
	QueueUnassigned  = "Unassigned" // standard queue when the category is unknown
)

// AllQueues lists every queue a record can be delivered to.
var AllQueues = []string{QueueEngineering, QueueProduct, QueueBilling, QueueSupport, QueueEscalation}

// Categories the classifier may return. Must match the classification schema.
const (
	CategoryBug       = "Bug Report"
	CategoryFeature   = "Feature Request"
	CategoryBilling   = "Billing Issue"
	CategoryTechnical = "Technical Question"
	CategoryIncident  = "Incident/Outage"
)

var Categories = []string{CategoryBug, CategoryFeature, CategoryBilling, CategoryTechnical, CategoryIncident}

// categoryQueue is the standard routing table: one owning team per category.
var categoryQueue = map[string]string{
	CategoryBug:       QueueEngineering,
	CategoryIncident:  QueueEngineering,
	CategoryFeature:   QueueProduct,
	CategoryBilling:   QueueBilling,
	CategoryTechnical: QueueSupport,
}

type Amount struct {
	Role     string // charged, expected or other
	Value    float64
	Currency string
}

// Input is everything the rules need. It is built from the LLM steps' output.
type Input struct {
	Message       string
	Category      string
	Confidence    float64
	AffectedScope string // single_user, multiple_users, organization_wide, unknown
	Amounts       []Amount
	Failed        bool // an LLM step failed, so the triage result is incomplete
}

type Decision struct {
	StandardQueue         string   `json:"standard_queue"`
	DestinationQueue      string   `json:"destination_queue"`
	Escalated             bool     `json:"escalated"`
	EscalationReasons     []string `json:"escalation_reasons"`
	BillingDiscrepancyUSD *float64 `json:"billing_discrepancy_usd"`
}

type Rules struct {
	ConfidenceThreshold float64  // escalate when confidence is below this
	BillingThresholdUSD float64  // escalate when |charged - expected| is above this
	Keywords            []string // escalate when the raw message contains any of these
}

func DefaultRules() Rules {
	return Rules{
		ConfidenceThreshold: 0.70,
		BillingThresholdUSD: 500,
		Keywords:            []string{"outage", "down for all users", "all users affected", "data loss", "security breach"},
	}
}

// Decide applies the routing table, then every escalation rule. All matching
// reasons are recorded, so a reviewer can see exactly why a request was
// pulled out of the standard queue.
func (r Rules) Decide(in Input) Decision {
	d := Decision{EscalationReasons: []string{}}
	escalate := func(reason string) { d.EscalationReasons = append(d.EscalationReasons, reason) }

	queue, known := categoryQueue[in.Category]
	if !known {
		queue = QueueUnassigned
	}
	d.StandardQueue = queue

	if in.Failed {
		escalate("processing_error: an LLM step failed, so the triage result is incomplete")
	}
	if !known && !in.Failed {
		escalate(fmt.Sprintf("unmapped_category: %q has no owning queue", in.Category))
	}
	if !in.Failed && in.Confidence < r.ConfidenceThreshold {
		escalate(fmt.Sprintf("low_confidence: %.2f is below %.2f", in.Confidence, r.ConfidenceThreshold))
	}
	if in.Category == CategoryIncident {
		escalate("incident: classified as Incident/Outage")
	}
	// Broad impact means a malfunction hitting many users. A feature request
	// "for our whole org" is not an outage, so it is excluded.
	if in.Category != CategoryFeature && (in.AffectedScope == "multiple_users" || in.AffectedScope == "organization_wide") {
		escalate("broad_impact: affected scope is " + in.AffectedScope)
	}
	if hits := matchKeywords(in.Message, r.Keywords); len(hits) > 0 {
		escalate("keyword: message mentions " + strings.Join(hits, ", "))
	}
	if diff, ok := billingDiscrepancy(in.Amounts); ok {
		d.BillingDiscrepancyUSD = &diff
		if diff > r.BillingThresholdUSD {
			escalate(fmt.Sprintf("billing_discrepancy: $%.2f exceeds $%.2f", diff, r.BillingThresholdUSD))
		}
	}

	d.Escalated = len(d.EscalationReasons) > 0
	d.DestinationQueue = d.StandardQueue
	if d.Escalated {
		d.DestinationQueue = QueueEscalation
	}
	return d
}

func matchKeywords(message string, keywords []string) []string {
	text := strings.ToLower(strings.Join(strings.Fields(message), " "))
	var hits []string
	for _, kw := range keywords {
		if strings.Contains(text, strings.ToLower(kw)) {
			hits = append(hits, fmt.Sprintf("%q", kw))
		}
	}
	return hits
}

// billingDiscrepancy compares the first USD "charged" amount with the first
// USD "expected" amount. "Billing error > $500" is read as the size of the
// error (the disputed difference), not the invoice total.
func billingDiscrepancy(amounts []Amount) (float64, bool) {
	var charged, expected *float64
	for i := range amounts {
		a := amounts[i]
		if !strings.EqualFold(a.Currency, "USD") {
			continue
		}
		switch {
		case a.Role == "charged" && charged == nil:
			charged = &amounts[i].Value
		case a.Role == "expected" && expected == nil:
			expected = &amounts[i].Value
		}
	}
	if charged == nil || expected == nil {
		return 0, false
	}
	return math.Round(math.Abs(*charged-*expected)*100) / 100, true
}
