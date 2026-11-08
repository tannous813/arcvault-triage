// server is the intake service. POST a message to /intake (Step 1, webhook
// trigger) and it is classified, enriched, routed, summarized, written to the
// output files and delivered to the downstream webhook.
//
//	POST /intake   {"id":"REQ-001","source":"email","message":"..."}
//	GET  /records  all processed records
//	GET  /healthz
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"arcvault-triage/internal/app"
	"arcvault-triage/internal/config"
	"arcvault-triage/internal/store"
	"arcvault-triage/internal/triage"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	st, err := store.Open(cfg.OutputDir)
	if err != nil {
		log.Error("opening output store", "err", err)
		os.Exit(1)
	}
	pipe := app.NewPipeline(cfg, log)
	hook := store.NewWebhook(cfg.WebhookURL)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /intake", intake(pipe, st, hook, log))
	mux.HandleFunc("GET /records", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, st.All())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	srv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Info("intake server listening", "addr", cfg.Addr, "model", cfg.LLMModel,
			"samples", cfg.ClassifySamples, "output", cfg.OutputDir, "webhook", cfg.WebhookURL != "")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	log.Info("shutting down, finishing in-flight requests")
	_ = srv.Shutdown(ctx)
}

func intake(pipe *triage.Pipeline, st *store.Store, hook *store.Webhook, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var req triage.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		req.Message = strings.TrimSpace(req.Message)
		if req.Message == "" {
			writeError(w, http.StatusBadRequest, "message is required")
			return
		}
		if req.ID == "" {
			req.ID = newID()
		}
		if req.Source == "" {
			req.Source = "unknown"
		}
		if req.ReceivedAt == "" {
			req.ReceivedAt = time.Now().UTC().Format(time.RFC3339)
		}
		log.Info("received", "id", req.ID, "source", req.Source)

		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		rec := pipe.Process(ctx, req)

		if err := hook.Deliver(ctx, rec); err != nil {
			rec.Warnings = append(rec.Warnings, "delivery to downstream webhook failed: "+err.Error())
			log.Warn("delivery failed", "id", rec.ID, "err", err)
		}
		if err := st.Save(rec); err != nil {
			log.Error("saving record", "id", rec.ID, "err", err)
			writeError(w, http.StatusInternalServerError, "could not persist record")
			return
		}
		writeJSON(w, http.StatusOK, rec)
	}
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "REQ-" + time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
