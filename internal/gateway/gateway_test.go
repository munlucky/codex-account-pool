package gateway

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/munlucky/gpt-codex-router/internal/authbroker"
)

type staticProvider struct {
	creds authbroker.Credentials
	err   error
}

func (p staticProvider) Credentials(context.Context) (authbroker.Credentials, error) {
	return p.creds, p.err
}

func testWebSocketKey() string {
	return base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
}

func TestProxyPreservesBackendPathAndReplacesInboundAuth(t *testing.T) {
	observed := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := r.Clone(context.Background())
		observed <- clone
		w.Header().Set("X-Upstream", "yes")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	handler := mustHandler(t, staticProvider{creds: authbroker.Credentials{AccessToken: "selected-token", AccountID: "selected-account"}}, upstream.URL)
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/backend-api/codex/responses?stream=true", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer inbound-token")
	req.Header.Set("ChatGPT-Account-ID", "inbound-account")
	req.Header.Set("Proxy-Authorization", "Basic inbound")
	req.Header.Set("Cookie", "session=inbound")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Upstream") != "yes" {
		t.Fatalf("status=%d header=%q", resp.StatusCode, resp.Header.Get("X-Upstream"))
	}

	got := <-observed
	if got.URL.Path != "/backend-api/codex/responses" || got.URL.RawQuery != "stream=true" {
		t.Fatalf("upstream URL=%s", got.URL.String())
	}
	if got.Header.Get("Authorization") != "Bearer selected-token" || got.Header.Get("ChatGPT-Account-ID") != "selected-account" {
		t.Fatalf("auth headers=%q account=%q", got.Header.Get("Authorization"), got.Header.Get("ChatGPT-Account-ID"))
	}
	if got.Header.Get("Proxy-Authorization") != "" || got.Header.Get("Cookie") != "" {
		t.Fatalf("inbound auth material leaked upstream: proxy=%q cookie=%q", got.Header.Get("Proxy-Authorization"), got.Header.Get("Cookie"))
	}
}

func TestProxyRejectsNonBackendAPIPaths(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()
	handler := mustHandler(t, staticProvider{creds: authbroker.Credentials{AccessToken: "token", AccountID: "account"}}, upstream.URL)
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || called {
		t.Fatalf("status=%d upstreamCalled=%v", resp.StatusCode, called)
	}
}

func TestProxyStreamsSSEWithoutBufferingUntilCompletion(t *testing.T) {
	firstFlushed := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(firstFlushed)
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer upstream.Close()

	handler := mustHandler(t, staticProvider{creds: authbroker.Credentials{AccessToken: "token", AccountID: "account"}}, upstream.URL)
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/backend-api/codex/responses")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case <-firstFlushed:
	case <-time.After(time.Second):
		t.Fatal("upstream never flushed first SSE event")
	}
	lineCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		if line != "data: first\n" {
			t.Fatalf("first line=%q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy buffered first SSE event until upstream completion")
	}
	close(release)
}

func TestProxyPreservesUpgradeTunnel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Connection"), "Upgrade") || r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "missing upgrade", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("upstream response does not support hijacking")
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nhello")
		_ = rw.Flush()
	}))
	defer upstream.Close()

	handler := mustHandler(t, staticProvider{creds: authbroker.Credentials{AccessToken: "token", AccountID: "account"}}, upstream.URL)
	proxy := httptest.NewServer(handler)
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	conn, err := net.DialTimeout("tcp", proxyURL.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = fmt.Fprintf(conn, "GET /backend-api/wham/remote/control/server HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", proxyURL.Host, testWebSocketKey())
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status=%q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	body := make([]byte, 5)
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Fatalf("tunneled payload=%q", body)
	}
}

func TestResponsesWebsocketForcesHTTPFallbackWithoutCallingUpstream(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()

	handler := mustHandler(t, staticProvider{creds: authbroker.Credentials{ProfileID: "account-1", AccessToken: "token", AccountID: "account"}}, upstream.URL)
	var events []RequestEvent
	handler.SetRequestLogger(func(event RequestEvent) { events = append(events, event) })

	req := httptest.NewRequest(http.MethodGet, "http://local/backend-api/codex/responses", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", testWebSocketKey())
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUpgradeRequired {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	if called {
		t.Fatal("Responses websocket fallback must not call upstream")
	}
	if len(events) != 1 || !events[0].TransportFallback || events[0].StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("events=%+v", events)
	}
}

func TestProxyDoesNotCallUpstreamWhenBrokerFails(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()
	handler := mustHandler(t, staticProvider{err: fmt.Errorf("profile unavailable")}, upstream.URL)
	proxy := httptest.NewServer(handler)
	defer proxy.Close()
	resp, err := http.Get(proxy.URL + "/backend-api/codex/responses")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || called {
		t.Fatalf("status=%d upstreamCalled=%v", resp.StatusCode, called)
	}
}

func TestRequestLoggerEmitsCompactProfileStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(staticProvider{creds: authbroker.Credentials{ProfileID: "account-2", AccessToken: "token", AccountID: "account"}}, u)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan RequestEvent, 1)
	h.SetRequestLogger(func(event RequestEvent) { events <- event })
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/backend-api/codex/responses", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case event := <-events:
		if event.Method != http.MethodPost || event.ProfileID != "account-2" || event.StatusCode != http.StatusCreated {
			t.Fatalf("event=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("request logger did not emit")
	}
}

type failoverProvider struct {
	current       authbroker.Credentials
	fallback      authbroker.Credentials
	failoverCalls int
}

func (p *failoverProvider) Credentials(context.Context) (authbroker.Credentials, error) {
	return p.current, nil
}

func (p *failoverProvider) Failover(_ context.Context, exhausted string, _ time.Time) (authbroker.Credentials, error) {
	p.failoverCalls++
	if exhausted == p.fallback.ProfileID || p.fallback.ProfileID == "" {
		return authbroker.Credentials{}, fmt.Errorf("no fallback")
	}
	p.current = p.fallback
	return p.fallback, nil
}

func TestUsageLimitAutomaticallySwitchesAndReplaysRequest(t *testing.T) {
	provider := &failoverProvider{
		current:  authbroker.Credentials{ProfileID: "account-1", AccessToken: "token-1", AccountID: "acct-1"},
		fallback: authbroker.Credentials{ProfileID: "account-2", AccessToken: "token-2", AccountID: "acct-2"},
	}
	var calls int
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		switch r.Header.Get("ChatGPT-Account-ID") {
		case "acct-1":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","resets_at":2000003600}}`)
		case "acct-2":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
		default:
			t.Fatalf("unexpected account header %q", r.Header.Get("ChatGPT-Account-ID"))
		}
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	h, err := New(provider, u)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan RequestEvent, 4)
	h.SetRequestLogger(func(event RequestEvent) { events <- event })
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	payload := `{"input":"same-request"}`
	resp, err := http.Post(proxy.URL+"/backend-api/codex/responses", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if calls != 2 || provider.failoverCalls != 1 {
		t.Fatalf("calls=%d failovers=%d", calls, provider.failoverCalls)
	}
	if len(bodies) != 2 || bodies[0] != payload || bodies[1] != payload {
		t.Fatalf("request was not replayed exactly: %#v", bodies)
	}

	first := <-events
	second := <-events
	if first.SwitchFrom != "account-1" || first.SwitchTo != "account-2" {
		t.Fatalf("switch event=%+v", first)
	}
	if second.ProfileID != "account-2" || second.StatusCode != http.StatusOK {
		t.Fatalf("final event=%+v", second)
	}
}

func TestGeneric429DoesNotSwitchAccounts(t *testing.T) {
	provider := &failoverProvider{
		current:  authbroker.Credentials{ProfileID: "account-1", AccessToken: "token-1", AccountID: "acct-1"},
		fallback: authbroker.Credentials{ProfileID: "account-2", AccessToken: "token-2", AccountID: "acct-2"},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	h, err := New(provider, u)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(h)
	defer proxy.Close()
	resp, err := http.Get(proxy.URL + "/backend-api/wham/usage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || provider.failoverCalls != 0 {
		t.Fatalf("status=%d failovers=%d", resp.StatusCode, provider.failoverCalls)
	}
	if !strings.Contains(string(body), "rate_limit_exceeded") {
		t.Fatalf("429 body was not preserved: %s", body)
	}
}

func mustHandler(t *testing.T, provider CredentialProvider, upstream string) *Handler {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(provider, u)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
