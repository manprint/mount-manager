package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestWebhookPostsJSON(t *testing.T) {
	received := make(chan Event, 1)
	var gotMethod, gotContentType, gotToken string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotToken = r.Header.Get("X-Token")
		var ev Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			t.Errorf("decode body: %v", err)
		}
		received <- ev
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := newWebhook(srv.URL, map[string]string{"X-Token": "secret"}, time.Second, testLogger(), 64)

	ev := Event{
		Event:     EventStartup,
		Timestamp: time.Now().UTC().Truncate(time.Second),
		Host:      "host1",
		MountPath: "/mnt/m",
		Detail:    "detail",
		Attempt:   1,
	}
	w.Notify(ev)

	select {
	case got := <-received:
		if gotMethod != http.MethodPost {
			t.Errorf("method = %q, want POST", gotMethod)
		}
		if gotContentType != "application/json" {
			t.Errorf("content-type = %q, want application/json", gotContentType)
		}
		if gotToken != "secret" {
			t.Errorf("X-Token = %q, want secret", gotToken)
		}
		if !got.Timestamp.Equal(ev.Timestamp) || got.Event != ev.Event || got.Host != ev.Host ||
			got.MountPath != ev.MountPath || got.Detail != ev.Detail || got.Attempt != ev.Attempt {
			t.Errorf("decoded event = %+v, want %+v", got, ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook request")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.Close(ctx)
}

func TestWebhookOrder(t *testing.T) {
	received := make(chan string, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		received <- ev.Event
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := newWebhook(srv.URL, nil, time.Second, testLogger(), 64)
	w.Notify(Event{Event: "1"})
	w.Notify(Event{Event: "2"})
	w.Notify(Event{Event: "3"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.Close(ctx)

	close(received)
	var got []string
	for ev := range received {
		got = append(got, ev)
	}
	want := []string{"1", "2", "3"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestWebhookNon2xxNotRetried(t *testing.T) {
	var mu sync.Mutex
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	w := newWebhook(srv.URL, nil, time.Second, testLogger(), 64)
	w.Notify(Event{Event: "1"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.Close(ctx)

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("request count = %d, want 1", count)
	}
}

func TestWebhookTimeoutDoesNotBlockNotify(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-unblock:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	w := newWebhook(srv.URL, nil, 100*time.Millisecond, testLogger(), 64)

	start := time.Now()
	for i := 0; i < 5; i++ {
		w.Notify(Event{Event: "e"})
	}
	elapsed := time.Since(start)
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("5 Notify calls took %v, want < 50ms", elapsed)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.Close(ctx)
}

func TestWebhookQueueFullDrops(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(block)
		srv.Close()
	}()

	w := newWebhook(srv.URL, nil, 5*time.Second, testLogger(), 2)

	for i := 0; i < 10; i++ {
		w.Notify(Event{Event: "e"})
	}

	if got := w.Dropped(); got < 7 {
		t.Fatalf("Dropped() = %d, want >= 7", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w.Close(ctx)
}

func TestCloseDrains(t *testing.T) {
	received := make(chan Event, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		received <- ev
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := newWebhook(srv.URL, nil, time.Second, testLogger(), 64)
	w.Notify(Event{Event: "1"})
	w.Notify(Event{Event: "2"})
	w.Notify(Event{Event: "3"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.Close(ctx)

	if len(received) != 3 {
		t.Fatalf("received %d events, want 3", len(received))
	}
}

func TestNotifyAfterCloseNoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := newWebhook(srv.URL, nil, time.Second, testLogger(), 64)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.Close(ctx)

	// Should not panic (send on closed channel) and should not block.
	w.Notify(Event{Event: "after-close"})
	w.Close(ctx)
}

func TestNopNotify(t *testing.T) {
	var n Nop
	// Should not panic.
	n.Notify(Event{Event: EventShutdown})
}
