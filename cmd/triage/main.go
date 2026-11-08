// triage runs the pipeline directly, without the server or output files.
// Use it to check your API key and to iterate on prompts quickly.
//
//	go run ./cmd/triage                          # all samples
//	go run ./cmd/triage -only REQ-004            # one sample
//	go run ./cmd/triage -message "Is SSO supported?"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"arcvault-triage/internal/app"
	"arcvault-triage/internal/config"
	"arcvault-triage/internal/triage"
)

func main() {
	file := flag.String("file", "samples/requests.json", "JSON array of requests")
	only := flag.String("only", "", "run only the request with this id")
	message := flag.String("message", "", "run a single ad-hoc message instead of the file")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(1)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	pipe := app.NewPipeline(cfg, log)

	var reqs []triage.Request
	if *message != "" {
		reqs = []triage.Request{{ID: "ADHOC", Source: "cli", Message: *message}}
	} else {
		raw, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := json.Unmarshal(raw, &reqs); err != nil {
			fmt.Fprintln(os.Stderr, "parsing", *file+":", err)
			os.Exit(1)
		}
	}

	ran := 0
	for _, req := range reqs {
		if *only != "" && req.ID != *only {
			continue
		}
		req.ReceivedAt = time.Now().UTC().Format(time.RFC3339)
		rec := pipe.Process(context.Background(), req)
		out, _ := json.MarshalIndent(rec, "", "  ")
		fmt.Println(string(out))
		ran++
	}
	if ran == 0 {
		fmt.Fprintln(os.Stderr, "no requests matched")
		os.Exit(1)
	}
}
