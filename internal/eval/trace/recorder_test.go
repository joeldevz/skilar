package trace

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/client"
)

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
