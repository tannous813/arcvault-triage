package triage

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

var scopes = []string{"single_user", "multiple_users", "organization_wide", "unknown"}

// ground checks the enrichment against the original message and drops any
// identifier or amount that does not appear in it. Strict JSON mode
// guarantees the shape of the output, not that its content is true; this is
// the guard against hallucinated invoice numbers or amounts, which would
// otherwise feed straight into the billing escalation rule.
func ground(message string, e *Enrichment) []string {
	var warnings []string
	lower := strings.ToLower(message)
	noSeparators := strings.ReplaceAll(message, ",", "")

	ids := []Identifier{}
	for _, id := range e.Identifiers {
		v := strings.TrimSpace(id.Value)
		needle := strings.ToLower(strings.TrimPrefix(v, "#"))
		if needle == "" {
			continue
		}
		if !strings.Contains(lower, needle) {
			warnings = append(warnings, fmt.Sprintf("dropped identifier not found in message: %q", v))
			continue
		}
		ids = append(ids, Identifier{Type: id.Type, Value: v})
	}
	e.Identifiers = ids

	amounts := []Amount{}
	for _, a := range e.Amounts {
		s := strconv.FormatFloat(a.Value, 'f', -1, 64)
		if !strings.Contains(noSeparators, s) {
			warnings = append(warnings, fmt.Sprintf("dropped amount not found in message: %s", s))
			continue
		}
		a.Currency = strings.ToUpper(strings.TrimSpace(a.Currency))
		amounts = append(amounts, a)
	}
	e.Amounts = amounts

	if e.TimeReferences == nil {
		e.TimeReferences = []string{}
	}
	if !slices.Contains(scopes, e.AffectedScope) {
		e.AffectedScope = "unknown"
	}
	return warnings
}
