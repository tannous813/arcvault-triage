// Package store persists triage records (Step 5 output) and delivers them to
// downstream queues.
//
// Files written under the output directory:
//
//	records.json           every record, in arrival order (the deliverable)
//	queues/<queue>.json    the records currently routed to each queue,
//	                       including queues/escalation.json for human review
//
// Records are upserted by ID, so re-sending the samples replaces records
// instead of duplicating them. Both files are rewritten atomically.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"arcvault-triage/internal/routing"
	"arcvault-triage/internal/triage"
)

type Store struct {
	dir     string
	mu      sync.Mutex
	order   []string
	records map[string]triage.Record
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "queues"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, records: map[string]triage.Record{}}

	b, err := os.ReadFile(filepath.Join(dir, "records.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var existing []triage.Record
	if err := json.Unmarshal(b, &existing); err != nil {
		return nil, fmt.Errorf("parsing existing records.json: %w", err)
	}
	for _, r := range existing {
		s.put(r)
	}
	return s, nil
}

func (s *Store) Save(r triage.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.put(r)

	all := s.list()
	if err := writeJSON(filepath.Join(s.dir, "records.json"), all); err != nil {
		return err
	}
	byQueue := map[string][]triage.Record{}
	for _, q := range routing.AllQueues {
		byQueue[q] = []triage.Record{}
	}
	for _, rec := range all {
		q := rec.Routing.DestinationQueue
		byQueue[q] = append(byQueue[q], rec)
	}
	for q, recs := range byQueue {
		if err := writeJSON(filepath.Join(s.dir, "queues", strings.ToLower(q)+".json"), recs); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) All() []triage.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list()
}

func (s *Store) put(r triage.Record) {
	if _, ok := s.records[r.ID]; !ok {
		s.order = append(s.order, r.ID)
	}
	s.records[r.ID] = r
}

func (s *Store) list() []triage.Record {
	out := make([]triage.Record, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.records[id])
	}
	return out
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Webhook posts each record to a downstream URL (for example a Webhook.site
// URL), tagged with its destination queue as a query parameter and header.
// An empty URL disables delivery.
type Webhook struct {
	URL  string
	HTTP *http.Client
}

func NewWebhook(rawURL string) *Webhook {
	return &Webhook{URL: rawURL, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (w *Webhook) Deliver(ctx context.Context, r triage.Record) error {
	if w == nil || w.URL == "" {
		return nil
	}
	u, err := url.Parse(w.URL)
	if err != nil {
		return fmt.Errorf("bad WEBHOOK_URL: %w", err)
	}
	queue := strings.ToLower(r.Routing.DestinationQueue)
	q := u.Query()
	q.Set("queue", queue)
	u.RawQuery = q.Encode()

	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ArcVault-Queue", queue)

	resp, err := w.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
