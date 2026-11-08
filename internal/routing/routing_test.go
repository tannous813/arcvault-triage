package routing

import (
	"slices"
	"strings"
	"testing"
)

func TestDecide(t *testing.T) {
	rules := DefaultRules()

	tests := []struct {
		name          string
		in            Input
		wantStandard  string
		wantDest      string
		wantReasons   []string // prefixes of expected escalation reasons, in order
		wantBillingUS *float64
	}{
		{
			name: "REQ-001 single-user bug goes to Engineering",
			in: Input{
				Message:  "Hi, I tried logging in this morning and keep getting a 403 error.",
				Category: CategoryBug, Confidence: 0.9, AffectedScope: "single_user",
			},
			wantStandard: QueueEngineering, wantDest: QueueEngineering,
		},
		{
			name: "REQ-002 org-wide feature request stays in Product",
			in: Input{
				Message:  "We'd love to see a bulk export feature for our audit logs.",
				Category: CategoryFeature, Confidence: 0.95, AffectedScope: "organization_wide",
			},
			wantStandard: QueueProduct, wantDest: QueueProduct,
		},
		{
			name: "REQ-003 $260 discrepancy stays in Billing",
			in: Input{
				Message:  "Invoice #8821 shows a charge of $1,240 but our contract rate is $980/month.",
				Category: CategoryBilling, Confidence: 0.95, AffectedScope: "unknown",
				Amounts: []Amount{{"charged", 1240, "USD"}, {"expected", 980, "USD"}},
			},
			wantStandard: QueueBilling, wantDest: QueueBilling, wantBillingUS: ptr(260),
		},
		{
			name: "billing discrepancy above $500 escalates",
			in: Input{
				Message:  "Invoice #9001 shows $1,600 but our rate is $980.",
				Category: CategoryBilling, Confidence: 0.95, AffectedScope: "unknown",
				Amounts: []Amount{{"charged", 1600, "USD"}, {"expected", 980, "USD"}},
			},
			wantStandard: QueueBilling, wantDest: QueueEscalation,
			wantReasons: []string{"billing_discrepancy"}, wantBillingUS: ptr(620),
		},
		{
			name: "non-USD amounts are not compared",
			in: Input{
				Message:  "Charged 2000 EUR instead of 900 EUR.",
				Category: CategoryBilling, Confidence: 0.9, AffectedScope: "unknown",
				Amounts: []Amount{{"charged", 2000, "EUR"}, {"expected", 900, "EUR"}},
			},
			wantStandard: QueueBilling, wantDest: QueueBilling,
		},
		{
			name: "REQ-004 low-confidence question escalates",
			in: Input{
				Message:  "Is there a way to set up SSO with Okta?",
				Category: CategoryTechnical, Confidence: 0.62, AffectedScope: "unknown",
			},
			wantStandard: QueueSupport, wantDest: QueueEscalation, wantReasons: []string{"low_confidence"},
		},
		{
			name: "confidence exactly at threshold does not escalate",
			in: Input{
				Message:  "Is there a way to set up SSO with Okta?",
				Category: CategoryTechnical, Confidence: 0.70, AffectedScope: "unknown",
			},
			wantStandard: QueueSupport, wantDest: QueueSupport,
		},
		{
			name: "REQ-005 incident with multiple users escalates on two rules",
			in: Input{
				Message:  "Your dashboard stopped loading for us around 2pm EST. Multiple users affected.",
				Category: CategoryIncident, Confidence: 0.92, AffectedScope: "multiple_users",
			},
			wantStandard: QueueEngineering, wantDest: QueueEscalation,
			wantReasons: []string{"incident", "broad_impact"},
		},
		{
			name: "REQ-005 misclassified as a bug still escalates on scope",
			in: Input{
				Message:  "Your dashboard stopped loading for us around 2pm EST. Multiple users affected.",
				Category: CategoryBug, Confidence: 0.85, AffectedScope: "multiple_users",
			},
			wantStandard: QueueEngineering, wantDest: QueueEscalation, wantReasons: []string{"broad_impact"},
		},
		{
			name: "keyword escalates even when the LLM missed it",
			in: Input{
				Message:  "Is this OUTAGE   related to the maintenance window?",
				Category: CategoryTechnical, Confidence: 0.9, AffectedScope: "unknown",
			},
			wantStandard: QueueSupport, wantDest: QueueEscalation, wantReasons: []string{"keyword"},
		},
		{
			name: "failed LLM step escalates with a single processing reason",
			in: Input{
				Message:  "anything",
				Category: "Unclassified", Confidence: 0, AffectedScope: "unknown", Failed: true,
			},
			wantStandard: QueueUnassigned, wantDest: QueueEscalation, wantReasons: []string{"processing_error"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := rules.Decide(tc.in)

			if d.StandardQueue != tc.wantStandard {
				t.Errorf("standard queue = %q, want %q", d.StandardQueue, tc.wantStandard)
			}
			if d.DestinationQueue != tc.wantDest {
				t.Errorf("destination queue = %q, want %q", d.DestinationQueue, tc.wantDest)
			}
			if d.Escalated != (tc.wantDest == QueueEscalation) {
				t.Errorf("escalated = %v, want %v", d.Escalated, tc.wantDest == QueueEscalation)
			}
			if len(d.EscalationReasons) != len(tc.wantReasons) {
				t.Fatalf("reasons = %v, want prefixes %v", d.EscalationReasons, tc.wantReasons)
			}
			for i, prefix := range tc.wantReasons {
				if !strings.HasPrefix(d.EscalationReasons[i], prefix) {
					t.Errorf("reason[%d] = %q, want prefix %q", i, d.EscalationReasons[i], prefix)
				}
			}
			switch {
			case tc.wantBillingUS == nil && d.BillingDiscrepancyUSD != nil:
				t.Errorf("billing discrepancy = %v, want none", *d.BillingDiscrepancyUSD)
			case tc.wantBillingUS != nil && (d.BillingDiscrepancyUSD == nil || *d.BillingDiscrepancyUSD != *tc.wantBillingUS):
				t.Errorf("billing discrepancy = %v, want %v", d.BillingDiscrepancyUSD, *tc.wantBillingUS)
			}
		})
	}
}

func TestEveryCategoryHasAQueue(t *testing.T) {
	for _, c := range Categories {
		q, ok := categoryQueue[c]
		if !ok {
			t.Errorf("category %q has no queue", c)
		}
		if !slices.Contains(AllQueues, q) {
			t.Errorf("category %q maps to unknown queue %q", c, q)
		}
	}
}

func ptr(f float64) *float64 { return &f }
