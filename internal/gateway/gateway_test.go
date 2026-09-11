package gateway

import (
	"bufio"
	"bytes"
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

	"github.com/klauspost/compress/zstd"

	"github.com/munlucky/codex-account-pool/internal/authbroker"
	"github.com/munlucky/codex-account-pool/internal/observability"
)

type staticProvider struct {
	creds authbroker.Credentials
	err   error
}

func (p staticProvider) Credentials(context.Context) (authbroker.Credentials, error) {
	return p.creds, p.err
}

type traceCapturingTransport struct {
	base   http.RoundTripper
	traces chan *requestTrace
}

func (t traceCapturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if trace := traceFromRequest(req); trace != nil {
		select {
		case t.traces <- trace:
		default:
		}
	}
	return t.base.RoundTrip(req)
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

func TestContextObservationPreservesRequestBytesAndCapturesStreamingUsage(t *testing.T) {
	bodies := make(chan string, 2)
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		bodies <- string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-ctx-1\",\"output\":[{\"text\":\"PRIVATE_RESPONSE_TEXT\"}],\"usage\":{\"input_tokens\":120,\"output_tokens\":8,\"total_tokens\":128,\"input_tokens_details\":{\"cached_tokens\":90},\"output_tokens_details\":{\"reasoning_tokens\":3}}}}\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-ctx-2\",\"usage\":{\"input_tokens\":130,\"cached_input_tokens\":95,\"output_tokens\":9,\"total_tokens\":139}}}\n\n")
	}))
	defer upstream.Close()

	h := mustHandler(t, staticProvider{creds: authbroker.Credentials{ProfileID: "account-1", AccessToken: "token", AccountID: "account"}}, upstream.URL)
	traces := make(chan *requestTrace, 2)
	transport := h.proxy.Transport.(*quotaFailoverTransport)
	transport.base = traceCapturingTransport{base: http.DefaultTransport, traces: traces}
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	firstPayload := "{\n  \"input\": [{\"type\":\"message\",\"role\":\"user\",\"content\":\"SUPER_SECRET_PROMPT_938482\"}],\n  \"metadata\": {\"preserve\": \"whitespace\"}\n}"
	resp, err := http.Post(proxy.URL+"/backend-api/codex/responses", "application/json", strings.NewReader(firstPayload))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	firstTrace := <-traces
	if got := <-bodies; got != firstPayload {
		t.Fatalf("upstream request bytes changed: got=%q want=%q", got, firstPayload)
	}
	requestMetrics, delta, responseMetrics := firstTrace.contextSnapshot()
	if requestMetrics.RequestBytes != int64(len(firstPayload)) || requestMetrics.AnalysisStatus != "analyzed" {
		t.Fatalf("request metrics=%+v", requestMetrics)
	}
	if delta.Available {
		t.Fatalf("first request must not claim predecessor: %+v", delta)
	}
	if !responseMetrics.UsageAvailable || responseMetrics.InputTokens != 120 || responseMetrics.CachedInputTokens != 90 || responseMetrics.OutputTokens != 8 || responseMetrics.ReasoningTokens != 3 || responseMetrics.TotalTokens != 128 {
		t.Fatalf("response metrics=%+v", responseMetrics)
	}
	if strings.Contains(fmt.Sprintf("%+v %+v %+v", requestMetrics, delta, responseMetrics), "SUPER_SECRET_PROMPT_938482") || strings.Contains(fmt.Sprintf("%+v", responseMetrics), "PRIVATE_RESPONSE_TEXT") {
		t.Fatal("context metrics retained raw request or response content")
	}

	secondPayload := `{"previous_response_id":"resp-ctx-1","input":[{"type":"message","role":"user","content":"SUPER_SECRET_PROMPT_938482"}]}`
	resp, err = http.Post(proxy.URL+"/backend-api/codex/responses", "application/json", strings.NewReader(secondPayload))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	secondTrace := <-traces
	if got := <-bodies; got != secondPayload {
		t.Fatalf("second upstream request bytes changed: got=%q want=%q", got, secondPayload)
	}
	_, secondDelta, secondResponse := secondTrace.contextSnapshot()
	if !secondDelta.Available || secondDelta.ReusedItemCount != 1 || secondDelta.ReusedContextBytes <= 0 {
		t.Fatalf("previous_response_id lineage was not resolved: %+v", secondDelta)
	}
	if !secondResponse.UsageAvailable || secondResponse.InputTokens != 130 || secondResponse.CachedInputTokens != 95 {
		t.Fatalf("second response metrics=%+v", secondResponse)
	}
}

func TestContextObservationIsFailOpenForMalformedJSON(t *testing.T) {
	bodySeen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodySeen <- string(body)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	h := mustHandler(t, staticProvider{creds: authbroker.Credentials{AccessToken: "token", AccountID: "account"}}, upstream.URL)
	traces := make(chan *requestTrace, 1)
	transport := h.proxy.Transport.(*quotaFailoverTransport)
	transport.base = traceCapturingTransport{base: http.DefaultTransport, traces: traces}
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	payload := `{"input":`
	resp, err := http.Post(proxy.URL+"/backend-api/codex/responses", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || <-bodySeen != payload {
		t.Fatalf("malformed JSON must still proxy unchanged: status=%d", resp.StatusCode)
	}
	requestMetrics, _, _ := (<-traces).contextSnapshot()
	if requestMetrics.AnalysisStatus != "skipped" || requestMetrics.SkipReason != "malformed_json" || requestMetrics.RequestBytes != int64(len(payload)) {
		t.Fatalf("unexpected fail-open metrics: %+v", requestMetrics)
	}
}

func TestContextObservationProjectsSafeRequestEndEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-event-safe\",\"output\":[{\"text\":\"PRIVATE_RESPONSE_TEXT\"}],\"usage\":{\"input_tokens\":20,\"input_tokens_details\":{\"cached_tokens\":15},\"output_tokens\":4,\"total_tokens\":24}}}\n\n")
	}))
	defer upstream.Close()

	h := mustHandler(t, staticProvider{creds: authbroker.Credentials{AccessToken: "token", AccountID: "account"}}, upstream.URL)
	events := make(chan RequestEvent, 4)
	h.SetRequestLogger(func(event RequestEvent) { events <- event })
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	payload := `{"prompt_cache_key":"PRIVATE_CACHE_LINEAGE_KEY","instructions":"SUPER_SECRET_PROMPT_938482","metadata":{"private":"PRIVATE_METADATA"},"input":[{"type":"message","role":"developer","content":"PRIVATE_DEVELOPER"},{"type":"reasoning","encrypted_content":"PRIVATE_REASONING"},{"type":"function_call_output","output":"PRIVATE_TOOL_OUTPUT"}],"tools":[{"type":"function","name":"private_tool","description":"PRIVATE_TOOL_DEFINITION"}]}`
	resp, err := http.Post(proxy.URL+"/backend-api/codex/responses", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event.EventType != observability.EventRequestEnd {
				continue
			}
			if event.ContextAnalysis != "analyzed" || event.LineageSource != "prompt_cache_key" || event.RequestBytes != int64(len(payload)) || event.ContextBytes != int64(len(payload)) || event.ToolItems != 1 || event.ToolDefinitionBytes <= 0 || event.DeveloperBytes <= 0 || event.ReasoningBytes <= 0 || event.MetadataBytes <= 0 || !event.UsageAvailable || event.InputTokens != 20 || event.CachedInputTokens != 15 || event.OutputTokens != 4 || event.TotalTokens != 24 {
				t.Fatalf("context end event=%+v", event)
			}
			serialized := fmt.Sprintf("%+v", event)
			for _, secret := range []string{"PRIVATE_CACHE_LINEAGE_KEY", "SUPER_SECRET_PROMPT_938482", "PRIVATE_METADATA", "PRIVATE_DEVELOPER", "PRIVATE_REASONING", "PRIVATE_TOOL_OUTPUT", "PRIVATE_TOOL_DEFINITION", "PRIVATE_RESPONSE_TEXT", "resp-event-safe"} {
				if strings.Contains(serialized, secret) {
					t.Fatalf("request_end event leaked %q: %s", secret, serialized)
				}
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for context request_end")
		}
	}
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
	if len(events) != 3 {
		t.Fatalf("events=%+v", events)
	}
	if events[0].EventType != observability.EventRequestStart || events[1].EventType != observability.EventTransportFallback || events[2].EventType != observability.EventRequestEnd {
		t.Fatalf("event types=%q,%q,%q", events[0].EventType, events[1].EventType, events[2].EventType)
	}
	if !events[1].TransportFallback || events[1].StatusCode != http.StatusUpgradeRequired || events[2].StatusCode != http.StatusUpgradeRequired || events[2].Outcome != observability.OutcomeLocalResponse {
		t.Fatalf("events=%+v", events)
	}
	if events[0].RequestID == "" || events[0].RequestID != events[1].RequestID || events[1].RequestID != events[2].RequestID {
		t.Fatalf("request correlation missing: %+v", events)
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

func TestRequestLoggerEmitsStructuredLifecycle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	h := mustHandler(t, staticProvider{creds: authbroker.Credentials{ProfileID: "account-2", AccessToken: "token", AccountID: "account"}}, upstream.URL)
	events := make(chan RequestEvent, 3)
	h.SetRequestLogger(func(event RequestEvent) { events <- event })
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/backend-api/codex/responses", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := make([]RequestEvent, 0, 3)
	deadline := time.After(time.Second)
	for len(got) < 3 {
		select {
		case event := <-events:
			got = append(got, event)
		case <-deadline:
			t.Fatalf("timed out waiting for lifecycle events: %+v", got)
		}
	}
	startEvent, attemptEvent, endEvent := got[0], got[1], got[2]
	if startEvent.EventType != observability.EventRequestStart || attemptEvent.EventType != observability.EventUpstreamAttempt || endEvent.EventType != observability.EventRequestEnd {
		t.Fatalf("got=%+v", got)
	}
	if endEvent.Method != http.MethodPost || endEvent.StatusCode != http.StatusCreated || endEvent.StatusOrigin != "upstream" || endEvent.Outcome != observability.OutcomeBodyEOF {
		t.Fatalf("end event=%+v", endEvent)
	}
	if endEvent.ProfileRef == "" || strings.Contains(endEvent.ProfileRef, "account-2") || endEvent.ResponseBytes != 2 || endEvent.FirstBodyMS <= 0 {
		t.Fatalf("unsafe or incomplete end event=%+v", endEvent)
	}
	if startEvent.RequestID == "" || startEvent.RequestID != attemptEvent.RequestID || attemptEvent.RequestID != endEvent.RequestID {
		t.Fatalf("request correlation missing: %+v", got)
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
	events := make(chan RequestEvent, 8)
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

	var got []RequestEvent
	deadline := time.After(time.Second)
	for len(got) < 5 {
		select {
		case event := <-events:
			got = append(got, event)
		case <-deadline:
			t.Fatalf("timed out waiting for failover lifecycle: %+v", got)
		}
	}
	var switchEvent, endEvent *RequestEvent
	attempts := 0
	for i := range got {
		switch got[i].EventType {
		case observability.EventUpstreamAttempt:
			attempts++
		case observability.EventAccountSwitch:
			switchEvent = &got[i]
		case observability.EventRequestEnd:
			endEvent = &got[i]
		}
	}
	if attempts != 2 || switchEvent == nil || switchEvent.SwitchFromRef == "" || switchEvent.SwitchToRef == "" || switchEvent.SwitchFromRef == switchEvent.SwitchToRef {
		t.Fatalf("failover events=%+v", got)
	}
	if strings.Contains(switchEvent.SwitchFromRef, "account-1") || strings.Contains(switchEvent.SwitchToRef, "account-2") {
		t.Fatalf("raw profile identifiers leaked: %+v", switchEvent)
	}
	if endEvent == nil || endEvent.StatusCode != http.StatusOK || endEvent.ProfileRef != switchEvent.SwitchToRef {
		t.Fatalf("final event=%+v all=%+v", endEvent, got)
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

func TestSSESemanticCompletionIsObservedSeparatelyFromHTTPStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer upstream.Close()

	h := mustHandler(t, staticProvider{creds: authbroker.Credentials{ProfileID: "account-1", AccessToken: "token", AccountID: "account"}}, upstream.URL)
	events := make(chan RequestEvent, 3)
	h.SetRequestLogger(func(event RequestEvent) { events <- event })
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/backend-api/codex/responses")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event.EventType != observability.EventRequestEnd {
				continue
			}
			if event.StatusCode != http.StatusOK || event.Outcome != observability.OutcomeBodyEOF || event.SemanticOutcome != observability.SemanticCompleted {
				t.Fatalf("end event=%+v", event)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for request_end")
		}
	}
}

func TestZstdContextObservationPreservesWireBytesAcrossFailover(t *testing.T) {
	provider := &failoverProvider{
		current:  authbroker.Credentials{ProfileID: "account-1", AccessToken: "token-1", AccountID: "acct-1"},
		fallback: authbroker.Credentials{ProfileID: "account-2", AccessToken: "token-2", AccountID: "acct-2"},
	}
	payload := []byte(`{"conversation_id":"conv-zstd","instructions":"SUPER_SECRET_ZSTD_INSTRUCTION","input":[{"type":"message","role":"user","content":"SUPER_SECRET_ZSTD_USER"}]}`)
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(payload, nil)
	encoder.Close()

	var bodies [][]byte
	var encodings []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, append([]byte(nil), body...))
		encodings = append(encodings, r.Header.Get("Content-Encoding"))
		if r.Header.Get("ChatGPT-Account-ID") == "acct-1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","resets_at":2000003600}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	h := mustHandler(t, provider, upstream.URL)
	traces := make(chan *requestTrace, 2)
	transport := h.proxy.Transport.(*quotaFailoverTransport)
	transport.base = traceCapturingTransport{base: http.DefaultTransport, traces: traces}
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/backend-api/codex/responses", bytes.NewReader(compressed))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || provider.failoverCalls != 1 {
		t.Fatalf("status=%d failovers=%d", resp.StatusCode, provider.failoverCalls)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], compressed) || !bytes.Equal(bodies[1], compressed) {
		t.Fatalf("compressed request was not replayed byte-for-byte: lens=%v", []int{len(bodies[0]), len(bodies[1])})
	}
	if len(encodings) != 2 || encodings[0] != "zstd" || encodings[1] != "zstd" {
		t.Fatalf("content encoding changed across replay: %v", encodings)
	}

	firstTrace := <-traces
	metrics, _, _ := firstTrace.contextSnapshot()
	if metrics.AnalysisStatus != "analyzed" || metrics.RequestBytes != int64(len(compressed)) || metrics.ContextBytes != int64(len(payload)) || metrics.UserItems != 1 {
		t.Fatalf("zstd context metrics=%+v", metrics)
	}
	if strings.Contains(fmt.Sprintf("%+v", metrics), "SUPER_SECRET_ZSTD") {
		t.Fatalf("context metrics leaked decoded content: %+v", metrics)
	}
}

func TestResponsesPostObservesAndStreamsSSEWithAlternateContentType(t *testing.T) {
	firstFlushed := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"arguments\":\"ARG_MARKER_123\"}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(firstFlushed)
		<-release
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-alt-content-type\",\"usage\":{\"input_tokens\":321,\"input_tokens_details\":{\"cached_tokens\":222},\"output_tokens\":17,\"output_tokens_details\":{\"reasoning_tokens\":9},\"total_tokens\":338}}}\n\n")
	}))
	defer upstream.Close()

	h := mustHandler(t, staticProvider{creds: authbroker.Credentials{ProfileID: "account-1", AccessToken: "token", AccountID: "account"}}, upstream.URL)
	events := make(chan RequestEvent, 8)
	h.SetRequestLogger(func(event RequestEvent) { events <- event })
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/backend-api/codex/responses", "application/json", strings.NewReader(`{"input":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	select {
	case <-firstFlushed:
	case <-time.After(time.Second):
		t.Fatal("upstream did not flush first alternate-content-type SSE event")
	}
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "response.output_item.done") {
		t.Fatalf("first streamed line=%q", line)
	}
	close(release)
	_, _ = io.Copy(io.Discard, reader)

	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event.EventType != observability.EventRequestEnd {
				continue
			}
			if event.SemanticOutcome != observability.SemanticCompleted || event.SSEEventCount != 2 || event.ToolCallCount != 1 {
				t.Fatalf("stream observation event=%+v", event)
			}
			if !event.UsageAvailable || event.InputTokens != 321 || event.CachedInputTokens != 222 || event.OutputTokens != 17 || event.ReasoningTokens != 9 || event.TotalTokens != 338 {
				t.Fatalf("usage observation event=%+v", event)
			}
			serialized := fmt.Sprintf("%+v", event)
			if strings.Contains(serialized, "ARG_MARKER_123") || strings.Contains(serialized, "resp-alt-content-type") {
				t.Fatalf("stream metadata leaked raw content: %+v", event)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for alternate-content-type request_end")
		}
	}
}
