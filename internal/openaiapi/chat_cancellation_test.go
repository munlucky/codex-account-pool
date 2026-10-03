package openaiapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/munlucky/codex-account-pool/internal/authbroker"
	"github.com/munlucky/codex-account-pool/internal/gateway"
)

var errDownstreamWrite = errors.New("downstream write failed")

type failingChatResponseWriter struct {
	header http.Header
	mu     sync.Mutex
	writes []string
}

func (w *failingChatResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingChatResponseWriter) WriteHeader(int) {}

func (w *failingChatResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes = append(w.writes, string(p))
	w.mu.Unlock()
	return 0, errDownstreamWrite
}

func (w *failingChatResponseWriter) Flush() {}

type flushFailingChatResponseWriter struct {
	header http.Header
}

func (w *flushFailingChatResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *flushFailingChatResponseWriter) WriteHeader(int) {}

func (w *flushFailingChatResponseWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func (w *flushFailingChatResponseWriter) Flush() {}

func (w *flushFailingChatResponseWriter) FlushError() error {
	return errDownstreamWrite
}

func (w *failingChatResponseWriter) snapshotWrites() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.writes...)
}

func TestChatStreamWriterPropagatesDownstreamWriteError(t *testing.T) {
	dst := &failingChatResponseWriter{}
	sw := newChatStreamWriter(dst, "gpt-test", false)
	payload := []byte("data: {\"type\":\"response.created\"}\n\n")

	n, err := sw.Write(payload)
	if !errors.Is(err, errDownstreamWrite) {
		t.Fatalf("Write error=%v, want %v", err, errDownstreamWrite)
	}
	if n != len(payload) {
		t.Fatalf("Write n=%d, want %d", n, len(payload))
	}
}

func TestChatStreamWriterPropagatesDownstreamFlushError(t *testing.T) {
	dst := &flushFailingChatResponseWriter{}
	sw := newChatStreamWriter(dst, "gpt-test", false)
	payload := []byte("data: {\"type\":\"response.created\"}\n\n")

	n, err := sw.Write(payload)
	if !errors.Is(err, errDownstreamWrite) {
		t.Fatalf("Write error=%v, want flush error %v", err, errDownstreamWrite)
	}
	if n != len(payload) {
		t.Fatalf("Write n=%d, want %d", n, len(payload))
	}
}

func TestChatStreamWriterDoesNotEmitDoneAfterDownstreamWriteError(t *testing.T) {
	dst := &failingChatResponseWriter{}
	sw := newChatStreamWriter(dst, "gpt-test", false)
	payload := []byte("data: {\"type\":\"response.created\"}\n\n")

	_, _ = sw.Write(payload)
	sw.finish()

	writes := dst.snapshotWrites()
	if len(writes) != 1 {
		t.Fatalf("writes=%d, want exactly the failed downstream write", len(writes))
	}
	for _, write := range writes {
		if strings.Contains(write, "[DONE]") {
			t.Fatalf("synthetic DONE emitted after downstream failure: %q", write)
		}
	}
}

type cancellationProvider struct{}

func (cancellationProvider) Credentials(context.Context) (authbroker.Credentials, error) {
	return authbroker.Credentials{ProfileID: "test", AccessToken: "token", AccountID: "account"}, nil
}

func TestChatCompletionDownstreamWriteFailureCancelsCodexUpstream(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := gateway.New(cancellationProvider{}, u)
	if err != nil {
		t.Fatal(err)
	}
	api, err := New(backend, testKey, testClientVersion)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	dst := &failingChatResponseWriter{}
	done := make(chan any, 1)
	go func() {
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			api.ServeHTTP(dst, req)
		}()
		done <- recovered
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("upstream stream did not start")
	}

	select {
	case <-canceled:
	case <-time.After(300 * time.Millisecond):
		close(release)
		<-done
		t.Fatal("downstream write failure did not promptly cancel the Codex upstream request")
	}

	select {
	case recovered := <-done:
		if recovered != nil && !errors.Is(asError(recovered), http.ErrAbortHandler) {
			t.Fatalf("unexpected panic after proxy abort: %v", recovered)
		}
	case <-time.After(time.Second):
		t.Fatal("chat completion handler did not return after upstream cancellation")
	}
}

func asError(value any) error {
	err, _ := value.(error)
	return err
}

func TestChatCompletionClientCancelCancelsCodexUpstream(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := gateway.New(cancellationProvider{}, u)
	if err != nil {
		t.Fatal(err)
	}
	api, err := New(backend, testKey, testClientVersion)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		_ = resp.Body.Close()
		cancel()
		close(release)
		t.Fatal("upstream stream did not start")
	}

	cancel()
	_ = resp.Body.Close()

	select {
	case <-canceled:
	case <-time.After(500 * time.Millisecond):
		close(release)
		t.Fatal("client cancellation did not promptly cancel the Codex upstream request")
	}
}
