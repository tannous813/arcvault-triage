# ArcVault Intake & Triage (Go)

An AI-powered intake pipeline that classifies, enriches, routes and summarizes inbound customer requests, and flags the ones that need a human.

**Stack:** Go (standard library only, no dependencies) · Groq API with `openai/gpt-oss-120b`, using strict JSON-schema output · JSON files plus an optional Webhook.site URL as the downstream queues.

## How it works

```
POST /intake ──► Step 2 classify ×3 ─┐  run concurrently
(Step 1)         Step 3 enrich ──────┘
                        │
                        ▼
                 grounding check (drop identifiers/amounts not in the message)
                        │
                        ▼
                 Steps 4 + 6: routing and escalation rules (plain Go, no LLM)
                        │
                        ▼
                 Step 5 summary (LLM, written after routing)
                        │
                        ▼
                 output/records.json, output/queues/<queue>.json, Webhook.site
```

The LLM does the work that needs language understanding (classification, extraction, summaries). Every decision that must be predictable (queues, thresholds, escalation) is plain Go in `internal/routing` and is covered by table-driven tests.

**Queues:** Bug Report and Incident/Outage → Engineering · Feature Request → Product · Billing Issue → Billing · Technical Question → Support · anything flagged → Escalation (human review).

**Escalation:** a request goes to Escalation instead of its standard queue if any of these holds:

- Confidence is below 0.70.
- The category is Incident/Outage.
- A malfunction affects multiple users or a whole organization.
- The message contains an escalation keyword.
- The billing discrepancy (charged minus expected) exceeds $500.
- An LLM step failed.

Every reason that matched is recorded on the record.

**Confidence:** each message is classified 3 times at temperature 0.7. The final confidence is the lower of two numbers: how often the samples agree, and the model's own average self-reported confidence. With `CLASSIFY_SAMPLES=1` it falls back to the self-reported score.

## Run it

Requires Go 1.22+ and a free Groq API key from https://console.groq.com.

```bash
cp .env.example .env              # then set LLM_API_KEY in .env
go test ./...                     # offline tests, no API key needed
go run ./cmd/triage -only REQ-001 # one message straight through the pipeline (checks your key)
go run ./cmd/server               # start the intake service on :8080
go run ./cmd/sendsamples          # in a second terminal: send all five samples
```

`sendsamples` waits 10 seconds between messages because Groq's free tier allows about 8,000 tokens per minute and each message makes 5 calls. Use `-delay 0` on a paid tier. Rate-limited calls are retried automatically, waiting as long as Groq asks.

Results are written to `output/records.json`, with one file per queue in `output/queues/`. To watch records arrive downstream, set `WEBHOOK_URL` in `.env` to your Webhook.site URL; each record is POSTed there with `?queue=<name>`.

## Layout

```
cmd/server           HTTP intake service: POST /intake, GET /records, GET /healthz
cmd/sendsamples      ingestion simulator: posts samples/requests.json to /intake
cmd/triage           runs the pipeline directly, for checking the key and iterating on prompts
internal/triage      pipeline, self-consistency, grounding check
internal/triage/prompts   the three prompts and their strict JSON schemas
internal/routing     routing table and escalation rules (deterministic, tested)
internal/llm         OpenAI-compatible client: strict JSON schema, retries with backoff
internal/store       records.json, per-queue files, webhook delivery
internal/config      environment variables and .env loading
```

## Configuration

See `.env.example`. Thresholds such as `CONFIDENCE_THRESHOLD`, `BILLING_ESCALATION_USD` and `CLASSIFY_SAMPLES` can be changed without touching code. The client works with any OpenAI-compatible endpoint, so `LLM_BASE_URL` and `LLM_MODEL` can point at another provider or at a local Ollama server.
