// sendsamples simulates inbound customer requests by POSTing each sample to
// the intake endpoint, one at a time. The pipeline starts automatically when
// a message arrives.
//
//	go run ./cmd/sendsamples                 # all samples
//	go run ./cmd/sendsamples -only REQ-003   # one sample
//	go run ./cmd/sendsamples -v              # print full records
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"arcvault-triage/internal/triage"
)

func main() {
	url := flag.String("url", "http://localhost:8080/intake", "intake endpoint")
	file := flag.String("file", "samples/requests.json", "JSON array of requests")
	only := flag.String("only", "", "send only the request with this id")
	delay := flag.Duration("delay", 10*time.Second, "pause between requests (Groq free tier allows ~8k tokens/minute)")
	timeout := flag.Duration("timeout", 150*time.Second, "per-request timeout")
	verbose := flag.Bool("v", false, "print the full record")
	flag.Parse()

	raw, err := os.ReadFile(*file)
	if err != nil {
		fail("reading %s: %v", *file, err)
	}
	var reqs []triage.Request
	if err := json.Unmarshal(raw, &reqs); err != nil {
		fail("parsing %s: %v", *file, err)
	}

	client := &http.Client{Timeout: *timeout}
	ok, failed := 0, 0
	for _, r := range reqs {
		if *only != "" && r.ID != *only {
			continue
		}
		if ok+failed > 0 {
			time.Sleep(*delay)
		}
		r.ReceivedAt = time.Now().UTC().Format(time.RFC3339)
		body, _ := json.Marshal(r)

		start := time.Now()
		resp, err := client.Post(*url, "application/json", bytes.NewReader(body))
		if err != nil {
			fmt.Printf("✗ %s  %v\n\n", r.ID, err)
			failed++
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		elapsed := time.Since(start).Round(time.Millisecond)

		if resp.StatusCode >= 300 {
			fmt.Printf("✗ %s  HTTP %d  %s\n\n", r.ID, resp.StatusCode, strings.TrimSpace(string(respBody)))
			failed++
			continue
		}
		ok++

		var rec triage.Record
		if *verbose || json.Unmarshal(respBody, &rec) != nil {
			fmt.Printf("✓ %s  %s\n%s\n\n", r.ID, elapsed, respBody)
			continue
		}
		c, rt := rec.Classification, rec.Routing
		fmt.Printf("✓ %s  %s | %s | confidence %.2f → %s  (%s)\n", rec.ID, c.Category, c.Priority, c.Confidence, rt.DestinationQueue, elapsed)
		if d := c.ConfidenceDetail; d.Samples > 1 && d.Agreement < 1 {
			fmt.Printf("    votes: %v (agreement %.2f, model-reported %.2f)\n", d.Votes, d.Agreement, d.ModelReported)
		}
		for _, reason := range rt.EscalationReasons {
			fmt.Printf("    escalation: %s\n", reason)
		}
		for _, warning := range rec.Warnings {
			fmt.Printf("    warning: %s\n", warning)
		}
		fmt.Printf("    %s\n\n", rec.Summary)
	}

	if ok+failed == 0 {
		fail("no requests matched (check -only)")
	}
	fmt.Printf("done: %d ok, %d failed\n", ok, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
