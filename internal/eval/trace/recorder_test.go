package trace

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/client"
)

func TestRecorderWaitForServerReadyRequiresConnectedThenHeartbeat(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
		_, _ = io.WriteString(w, `data: {"payload":{"type":"server.connected","properties":{}}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"payload":{"type":"server.heartbeat","properties":{}}}`+"\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	recorder, err := StartRecorder(context.Background(), client.New(client.Config{BaseURL: server.URL}))
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waitDone := make(chan error, 1)
	go func() { waitDone <- recorder.WaitForServerReady(waitCtx) }()
	select {
	case err := <-waitDone:
		t.Fatalf("readiness passed before the protocol barrier: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-waitDone; err != nil {
		t.Fatalf("readiness barrier failed: %v", err)
	}
	events := recorder.Snapshot()
	if len(events) != 2 || events[0].Payload.Type != "server.connected" || events[1].Payload.Type != "server.heartbeat" {
		t.Fatalf("readiness events were not retained: %#v", events)
	}
	recorder.PrepareForRuntimeStop()
	if _, err := recorder.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestRecorderWaitForServerReadyFailsClosed(t *testing.T) {
	t.Parallel()
	t.Run("connected without heartbeat times out", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"payload":{"type":"server.connected","properties":{}}}`+"\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		}))
		defer server.Close()

		recorder, err := StartRecorder(context.Background(), client.New(client.Config{BaseURL: server.URL}))
		if err != nil {
			t.Fatal(err)
		}
		waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err = recorder.WaitForServerReady(waitCtx)
		cancel()
		if !errors.Is(err, ErrGlobalEventStreamNotReady) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("connected-only stream did not fail closed: %v", err)
		}
		recorder.PrepareForRuntimeStop()
		_, _ = recorder.Stop()
	})

	t.Run("heartbeat before connected does not satisfy barrier", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"payload":{"type":"server.heartbeat","properties":{}}}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"payload":{"type":"server.connected","properties":{}}}`+"\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		}))
		defer server.Close()

		recorder, err := StartRecorder(context.Background(), client.New(client.Config{BaseURL: server.URL}))
		if err != nil {
			t.Fatal(err)
		}
		waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err = recorder.WaitForServerReady(waitCtx)
		cancel()
		if !errors.Is(err, ErrGlobalEventStreamNotReady) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("out-of-order readiness events were accepted: %v", err)
		}
		recorder.PrepareForRuntimeStop()
		_, _ = recorder.Stop()
	})

	t.Run("stream ends before heartbeat", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"payload":{"type":"server.connected","properties":{}}}`+"\n\n")
		}))
		defer server.Close()

		recorder, err := StartRecorder(context.Background(), client.New(client.Config{BaseURL: server.URL}))
		if err != nil {
			t.Fatal(err)
		}
		waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := recorder.WaitForServerReady(waitCtx); !errors.Is(err, ErrGlobalEventStreamNotReady) {
			t.Fatalf("ended stream did not fail closed: %v", err)
		}
		_, _ = recorder.Stop()
	})
}

func TestRecorderTreatsUnexpectedEOFAsIsolationEvidenceLoss(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"directory":"/fixture","payload":{"type":"session.created","properties":{"info":{"id":"root"}}}}`+"\n\n")
	}))
	defer server.Close()

	recorder, err := StartRecorder(context.Background(), client.New(client.Config{BaseURL: server.URL}))
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := recorder.WaitForSessionCreated(waitCtx, "root"); err == nil || !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("stream EOF during admission was accepted: %v", err)
	}
	select {
	case <-recorder.done:
	case <-waitCtx.Done():
		t.Fatal("recorder did not observe unexpected stream termination")
	}
	_, err = recorder.Stop()
	if err == nil || !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("unexpected stream EOF was accepted: %v", err)
	}
}
