package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Webhook is a Notifier that POSTs events as JSON to a configured URL.
// Delivery happens on a single background goroutine; Notify never blocks
// the caller on network I/O.
type Webhook struct {
	url     string
	headers map[string]string
	client  *http.Client
	queue   chan Event
	log     *slog.Logger

	mu     sync.Mutex
	closed bool
	done   chan struct{}

	dropped atomic.Int64
}

// NewWebhook creates a Webhook with a default queue capacity of 64.
func NewWebhook(url string, headers map[string]string, timeout time.Duration, log *slog.Logger) *Webhook {
	return newWebhook(url, headers, timeout, log, 64)
}

// newWebhook creates a Webhook with the given queue capacity and starts the
// sender goroutine.
func newWebhook(url string, headers map[string]string, timeout time.Duration, log *slog.Logger, capacity int) *Webhook {
	w := &Webhook{
		url:     url,
		headers: headers,
		client:  &http.Client{Timeout: timeout},
		queue:   make(chan Event, capacity),
		log:     log,
		done:    make(chan struct{}),
	}
	go func() {
		for ev := range w.queue {
			w.send(ev)
		}
		close(w.done)
	}()
	return w
}

// send POSTs the event as JSON. Errors and non-2xx responses are logged; no
// retry is attempted.
func (w *Webhook) send(ev Event) {
	body, err := json.Marshal(ev)
	if err != nil {
		w.log.Warn("webhook delivery failed", "event", ev.Event, "error", err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		w.log.Warn("webhook delivery failed", "event", ev.Event, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		w.log.Warn("webhook delivery failed", "event", ev.Event, "error", err)
		return
	}
	defer func() {
		// Drain so the keep-alive connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 300 {
		w.log.Warn("webhook delivery failed", "event", ev.Event, "status", resp.StatusCode)
	}
}

// Notify enqueues the event for delivery. It never blocks: if the queue is
// full, the event is dropped and counted.
func (w *Webhook) Notify(ev Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	select {
	case w.queue <- ev:
	default:
		w.dropped.Add(1)
		w.log.Warn("webhook queue full, dropping event", "event", ev.Event)
	}
}

// Close stops accepting new events, closes the queue, and waits for the
// sender goroutine to drain it or for ctx to be done. It is idempotent.
func (w *Webhook) Close(ctx context.Context) {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()

	select {
	case <-w.done:
	case <-ctx.Done():
	}
}

// Dropped returns the number of events dropped because the queue was full.
func (w *Webhook) Dropped() int64 {
	return w.dropped.Load()
}
