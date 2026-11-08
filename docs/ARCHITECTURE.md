# Architecture

## Overview

ArcVault Intake & Triage is a Go service that turns unstructured customer messages into structured, routed tickets. A message arrives at a webhook. Two LLM steps classify it and extract its facts. Deterministic Go rules route it and decide whether a human needs to look at it. A final LLM step writes a handoff note for the receiving team. Every record is written to `output/records.json`, to a file for its queue, and optionally to a downstream webhook.

The design rests on one split. The LLM does only the work that needs language understanding: classifying, extracting and summarizing. Every decision that must be predictable and auditable (which queue, which threshold, when to escalate) is plain Go covered by table-driven tests. A bad prompt can produce a wrong label, but it cannot change the routing rules.

```
POST /intake ──► classify ×3 ──┐  concurrent (goroutines)
                 enrich ───────┘
                       │
                       ▼
                grounding check   drop identifiers/amounts not present in the message
                       │
                       ▼
                routing + escalation rules   plain Go, no LLM
                       │
                       ▼
                summary           LLM, written after routing
                       │
                       ▼
                records.json · queues/<queue>.json · webhook
```

**Stack.** Go 1.22 using only the standard library. The LLM is `openai/gpt-oss-120b` on Groq's free tier. I chose it for two reasons. First, Groq supports strict JSON-schema output for this model: constrained decoding guarantees every response matches the schema, so there is no malformed-JSON code path. Second, the model is open-weight, so it can later be self-hosted for cost or data-residency reasons without changing prompts. The client speaks the OpenAI-compatible API, so switching providers is a configuration change (`LLM_BASE_URL`, `LLM_MODEL`).

## Pipeline

**Ingestion (Step 1).** `POST /intake` is the webhook trigger. It validates the request and assigns an ID and timestamp when missing. `cmd/sendsamples` simulates inbound traffic by posting the five sample messages.

**Classification and enrichment (Steps 2 and 3)** both need only the raw message, so they run concurrently. Classification returns a category, priority, confidence and one-sentence rationale. Enrichment returns the core issue, identifiers (account, invoice, error code, other), money amounts with their role (charged, expected, other), time references, affected scope, product area and urgency. Both prompts wrap the customer text in `<customer_message>` tags and tell the model to treat it as data, and the code neutralises any closing tag inside the message so a customer cannot inject instructions.

**Confidence: self-consistency.** LLM self-reported confidence is poorly calibrated. In the test runs the model reported 0.94 to 0.96 for nearly every message. So each message is classified three times at temperature 0.7. The final category is the majority vote, and the final confidence is the lower of two numbers: the share of samples that agree, and the mean self-reported confidence of the winning samples. In the first run, two of three samples misclassified REQ-001 as an incident; the agreement of 0.67 is what pulled it below the 0.70 threshold and into human review. The breakdown (votes, agreement, model-reported score) is stored on every record.

**Grounding check.** Strict JSON mode guarantees the output's shape, not its truth. After enrichment, any identifier or amount that does not literally appear in the message is dropped and logged as a warning. This matters because extracted amounts feed straight into the billing escalation rule; a hallucinated amount must not be able to trigger it.

**Routing and escalation (Steps 4 and 6)** are covered below. **Summary (Step 5)** runs last, so the handoff note can say where the ticket went and why it was escalated. If the summary call fails, a template summary is used and the failure is recorded.

## Routing

| Category | Queue |
|---|---|
| Bug Report | Engineering |
| Incident/Outage | Engineering (on-call) |
| Feature Request | Product |
| Billing Issue | Billing |
| Technical Question | Support |
| any escalated request | Escalation (human review) |

Each category has exactly one owning queue, so routing is a lookup table rather than a judgment call. A unit test asserts that every category in the classification schema has a queue, so adding a category without an owner fails the build. Each record keeps both its `standard_queue` and its final `destination_queue`, so an escalated ticket still shows which team owns it.

## Escalation

A request goes to the Escalation queue instead of its standard queue if any rule matches. All matching reasons are recorded, not just the first one.

| Rule | Why it exists |
|---|---|
| Confidence below 0.70 | The brief's threshold; the self-consistency score makes it meaningful. |
| Category is Incident/Outage | Outages need a human immediately, whatever the confidence. |
| A malfunction affects multiple users or an organization | Catches outages the classifier mislabels. REQ-005 never says "outage", so a keyword rule alone would miss it. Feature requests are excluded: a request "for our whole org" is not an outage. |
| Message contains "outage", "down for all users", "all users affected", "data loss" or "security breach" | A cheap rule that works even if the LLM is wrong. |
| Billing discrepancy above $500 | Computed in Go as the gap between the charged and expected USD amounts. |
| Any LLM step failed | A partial result is never trusted. The message goes to a human instead of being dropped. |

**The billing threshold was ambiguous.** "Billing error > $500" can mean the invoice total or the size of the error. I read it as the size of the error, since that is the money in dispute. For REQ-003 the invoice is $1,240 against a $980 contract rate, a $260 discrepancy, so it is not escalated. The threshold is configurable (`BILLING_ESCALATION_USD`), and the computed discrepancy is stored on the record either way.

## Output and failure handling

Each record holds the original message, classification (with confidence breakdown), enrichment, routing decision, summary, warnings, and processing metrics (duration, LLM calls, tokens). Records are upserted by ID, so re-sending a message replaces its record instead of duplicating it. Files are written atomically via a temp file and rename. When `WEBHOOK_URL` is set, each record is also POSTed downstream, tagged with its queue in a query parameter and header.

LLM calls retry on rate limits, server errors, timeouts and undecodable output, with exponential backoff up to six times. They honour Groq's `Retry-After` header and its "try again in 8.025s" message. Each attempt has a 30-second timeout so one stuck call cannot consume the whole budget, and each request has a 2-minute budget overall. Every retry is logged. In testing, one run hit the free tier's 8,000 tokens-per-minute limit and every call for REQ-005 ran out of time. The record was not lost: it went to human review with each failure listed, and re-sending it replaced it with a clean result (`docs/runs/`).

## Results

| Request | Category | Priority | Confidence | Queue | Escalation reasons |
|---|---|---|---|---|---|
| REQ-001 | Bug Report | High | 0.94 | Engineering | none |
| REQ-002 | Feature Request | Low | 0.95 | Product | none |
| REQ-003 | Billing Issue | Medium | 0.95 | Billing | none ($260 discrepancy is below $500) |
| REQ-004 | Technical Question | Low | 0.95 | Support | none |
| REQ-005 | Incident/Outage | High | 0.96 | Escalation | incident, multi-user impact |

Offline tests (`go test ./...`) cover every routing and escalation rule including boundaries, the self-consistency calculation, the grounding check, retry behaviour, and full pipeline runs against a fake LLM, including failure paths.

## At production scale

**Reliability.** Ingestion would return `202 Accepted` and push to a durable queue (NATS, SQS or Postgres) consumed by workers, so a slow LLM never blocks the sender. An outbox would keep saving and delivering a record in sync, and a second provider plus a dead-letter queue would cover outages. JSON files would become a database, and the queues real integrations: Jira or Zendesk for teams, PagerDuty for incidents.

**Cost.** Each message makes five LLM calls, and each record stores its token usage, so cost per message can be read straight from the output. The largest saving is adaptive sampling: classify once, and only take extra samples when the first answer is below 0.90 confidence or falls in a high-risk category. Classification samples could also move to the smaller `gpt-oss-20b`.

**Latency.** Classification and enrichment already run in parallel, and reasoning effort is set to low. On a paid tier without rate-limit waits, a message completes in roughly the time of two sequential LLM calls. A client-side token-bucket limiter sized to the provider's tokens-per-minute quota would replace retry-driven pacing.

**Quality.** Prompt changes would be gated by an evaluation set of real, labelled messages run in CI, with prompt versions recorded on each record. The confidence threshold would be tuned from reviewer outcomes, meaning how often humans overturn the model, rather than set by hand. Personal data would be redacted before it reaches the model.

## Phase 2

With another week I would add:

- **A review screen for the Escalation queue.** Reviewer corrections become labelled data for the evaluation set and few-shot examples.
- **Customer context.** Look up account tier and SLA from the CRM, so priority reflects who the customer is, not only what they wrote.
- **Incident clustering.** Twenty customers reporting the same outage become one incident with twenty linked tickets.
- **Multi-intent handling.** A message containing two separate requests is split into two tickets.
- **A sales-signal flag.** REQ-004 ("we're evaluating switching our auth provider") is a support question and also a buying signal; it should reach both Support and the account owner.
