package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/munlucky/codex-account-pool/internal/authbroker"
	"github.com/munlucky/codex-account-pool/internal/observability"
)

type CredentialProvider interface {
	Credentials(context.Context) (authbroker.Credentials, error)
}

type failoverCredentialProvider interface {
	CredentialProvider
	Failover(context.Context, string, time.Time) (authbroker.Credentials, error)
}

type credentialContextKey struct{}
type traceContextKey struct{}

type RequestEvent = observability.Event
type RequestLogger func(RequestEvent)

type Handler struct {
	provider   CredentialProvider
	proxy      *httputil.ReverseProxy
	logger     RequestLogger
	profileKey []byte
}

type quotaFailoverTransport struct {
	base     http.RoundTripper
	provider CredentialProvider
	handler  *Handler
}

type requestTrace struct {
	mu sync.Mutex

	id              string
	method          string
	routeTemplate   string
	transport       string
	peerClass       string
	started         time.Time
	profileRef      string
	authMS          float64
	attempt         int
	statusCode      int
	statusOrigin    string
	errorCategory   string
	bodyEOF         bool
	upstreamRead    bool
	downstreamWrite bool
	hijacked        bool
	responseBytes   int64
	firstBodyMS     float64
	semantic        *sseObserver
}

type observedBody struct {
	io.ReadCloser
	trace *requestTrace
}

type instrumentedResponseWriter struct {
	http.ResponseWriter
	trace       *requestTrace
	wroteHeader bool
	statusCode  int
}

func New(provider CredentialProvider, upstream *url.URL) (*Handler, error) {
	if provider == nil {
		return nil, fmt.Errorf("credential provider is required")
	}
	if upstream == nil || (upstream.Scheme != "https" && upstream.Scheme != "http") || strings.TrimSpace(upstream.Host) == "" {
		return nil, fmt.Errorf("http(s) upstream is required")
	}
	profileKey := make([]byte, 32)
	if _, err := rand.Read(profileKey); err != nil {
		return nil, fmt.Errorf("initialize profile anonymizer: %w", err)
	}
	h := &Handler{provider: provider, profileKey: profileKey}
	transport := &quotaFailoverTransport{base: http.DefaultTransport, provider: provider, handler: h}
	h.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
			sanitizeAuthHeaders(pr.Out.Header)
			if creds, ok := pr.In.Context().Value(credentialContextKey{}).(authbroker.Credentials); ok {
				applyCredentials(pr.Out.Header, creds)
			}
		},
		Transport:     transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			trace := traceFromRequest(resp.Request)
			if trace == nil {
				return nil
			}
			trace.setStatus(resp.StatusCode, "upstream")
			if resp.StatusCode >= 400 {
				trace.setErrorCategory("upstream_http")
			}
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type"))), "text/event-stream") {
				trace.enableSSE()
			}
			if resp.Body != nil && resp.StatusCode != http.StatusSwitchingProtocols {
				resp.Body = &observedBody{ReadCloser: resp.Body, trace: trace}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if trace := traceFromRequest(r); trace != nil {
				trace.setStatus(http.StatusBadGateway, "gateway")
				trace.setErrorCategory(classifyTransportError(err))
			}
			http.Error(w, "ChatGPT upstream unavailable", http.StatusBadGateway)
		},
	}
	return h, nil
}

func (h *Handler) SetRequestLogger(logger RequestLogger) {
	h.logger = logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	trace := &requestTrace{
		id:            newRequestID(),
		method:        r.Method,
		routeTemplate: routeTemplate(r.URL.Path),
		transport:     requestTransport(r),
		peerClass:     peerClass(r.RemoteAddr),
		started:       time.Now(),
	}
	ctx := context.WithValue(r.Context(), traceContextKey{}, trace)
	r = r.Clone(ctx)
	iw := &instrumentedResponseWriter{ResponseWriter: w, trace: trace}

	h.emit(trace.startEvent())
	defer func() {
		recovered := recover()
		if recovered != nil {
			if recovered == http.ErrAbortHandler {
				trace.noteAbort()
			}
		}
		h.emit(trace.endEvent(r.Context().Err(), iw.statusCode))
		if recovered != nil {
			panic(recovered)
		}
	}()

	if r.URL.Path != "/backend-api" && !strings.HasPrefix(r.URL.Path, "/backend-api/") {
		trace.setStatus(http.StatusNotFound, "local")
		http.NotFound(iw, r)
		return
	}
	if isResponsesWebsocketUpgrade(r) {
		trace.setStatus(http.StatusUpgradeRequired, "local")
		h.emit(observability.Event{
			EventType: observability.EventTransportFallback,
			RequestID: trace.id, Method: trace.method, RouteTemplate: trace.routeTemplate,
			Transport: trace.transport, PeerClass: trace.peerClass,
			StatusCode: http.StatusUpgradeRequired, StatusOrigin: "local", TransportFallback: true,
		})
		iw.Header().Set("Connection", "close")
		http.Error(iw, "GPT Codex Router requires HTTP transport for quota-aware Responses routing", http.StatusUpgradeRequired)
		return
	}

	authStart := time.Now()
	creds, err := h.provider.Credentials(r.Context())
	trace.setAuth(time.Since(authStart), h.profileRef(creds.ProfileID))
	if err != nil {
		trace.setStatus(http.StatusBadGateway, "local")
		trace.setErrorCategory("auth_unavailable")
		http.Error(iw, "GPT Codex Router authentication unavailable", http.StatusBadGateway)
		return
	}
	ctx = context.WithValue(r.Context(), credentialContextKey{}, creds)
	h.proxy.ServeHTTP(iw, r.Clone(ctx))
}

func (h *Handler) emit(event observability.Event) {
	if h.logger != nil {
		h.logger(event)
	}
}

func (h *Handler) profileRef(profileID string) string {
	if profileID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, h.profileKey)
	_, _ = mac.Write([]byte(profileID))
	return "p_" + hex.EncodeToString(mac.Sum(nil)[:6])
}

func (h *Handler) emitAttempt(req *http.Request, attempt int, profileID string, elapsed time.Duration, resp *http.Response, err error) {
	trace := traceFromRequest(req)
	if trace == nil {
		return
	}
	event := observability.Event{
		EventType: observability.EventUpstreamAttempt,
		RequestID: trace.id, Method: trace.method, RouteTemplate: trace.routeTemplate,
		Transport: trace.transport, PeerClass: trace.peerClass, ProfileRef: h.profileRef(profileID),
		Attempt: attempt, UpstreamHeadersMS: durationMS(elapsed),
	}
	if resp != nil {
		event.StatusCode = resp.StatusCode
		event.StatusOrigin = "upstream"
		if resp.StatusCode >= 400 {
			event.ErrorCategory = "upstream_http"
		}
	}
	if err != nil {
		event.StatusOrigin = "gateway"
		event.ErrorCategory = classifyTransportError(err)
	}
	h.emit(event)
}

func (h *Handler) emitSwitch(req *http.Request, from, to string) {
	trace := traceFromRequest(req)
	if trace == nil {
		return
	}
	toRef := h.profileRef(to)
	trace.setProfileRef(toRef)
	h.emit(observability.Event{
		EventType: observability.EventAccountSwitch,
		RequestID: trace.id, Method: trace.method, RouteTemplate: trace.routeTemplate,
		Transport: trace.transport, PeerClass: trace.peerClass,
		SwitchFromRef: h.profileRef(from), SwitchToRef: toRef,
	})
}

func isResponsesWebsocketUpgrade(r *http.Request) bool {
	return r != nil &&
		r.URL.Path == "/backend-api/codex/responses" &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

func (t *quotaFailoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := makeReplayable(req)
	if err != nil {
		return nil, err
	}
	attempted := map[string]bool{}
	if creds, ok := requestCredentials(req); ok {
		attempted[creds.ProfileID] = true
	}

	current := req
	for {
		attempt := 1
		if trace := traceFromRequest(current); trace != nil {
			attempt = trace.nextAttempt()
		}
		currentCreds, _ := requestCredentials(current)
		started := time.Now()
		resp, err := t.base.RoundTrip(current)
		elapsed := time.Since(started)
		if t.handler != nil {
			t.handler.emitAttempt(current, attempt, currentCreds.ProfileID, elapsed, resp, err)
		}
		if err != nil {
			if trace := traceFromRequest(current); trace != nil {
				trace.setErrorCategory(classifyTransportError(err))
			}
			return nil, err
		}
		resp.Request = current
		limited, resetAt, limitErr := usageLimitResponse(resp)
		if limitErr != nil || !limited {
			if limitErr != nil {
				if trace := traceFromRequest(current); trace != nil {
					trace.setErrorCategory("upstream_read")
				}
			}
			return resp, limitErr
		}
		provider, ok := t.provider.(failoverCredentialProvider)
		currentCreds, hasCreds := requestCredentials(current)
		if !ok || !hasCreds || currentCreds.ProfileID == "" {
			return resp, nil
		}
		fallback, failoverErr := provider.Failover(current.Context(), currentCreds.ProfileID, resetAt)
		if failoverErr != nil || fallback.ProfileID == "" || attempted[fallback.ProfileID] {
			return resp, nil
		}
		attempted[fallback.ProfileID] = true
		_ = resp.Body.Close()
		if t.handler != nil {
			t.handler.emitSwitch(current, currentCreds.ProfileID, fallback.ProfileID)
		}
		current = cloneForRetry(current, body, fallback)
	}
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.trace.observeUpstreamBytes(p[:n])
	}
	if errors.Is(err, io.EOF) {
		b.trace.noteBodyEOF()
	} else if err != nil {
		b.trace.noteUpstreamReadError()
	}
	return n, err
}

func (w *instrumentedResponseWriter) Header() http.Header {
	return w.ResponseWriter.Header()
}

func (w *instrumentedResponseWriter) WriteHeader(statusCode int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.statusCode = statusCode
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *instrumentedResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.trace.noteFirstBody()
	n, err := w.ResponseWriter.Write(p)
	w.trace.noteDownstreamWrite(n, err)
	return n, err
}

func (w *instrumentedResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *instrumentedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err == nil {
		w.trace.noteHijacked()
	}
	return conn, rw, err
}

func (w *instrumentedResponseWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (w *instrumentedResponseWriter) CloseNotify() <-chan bool {
	if notifier, ok := w.ResponseWriter.(http.CloseNotifier); ok {
		return notifier.CloseNotify()
	}
	ch := make(chan bool)
	return ch
}

func (t *requestTrace) startEvent() observability.Event {
	return observability.Event{
		Timestamp: t.started.UTC(), EventType: observability.EventRequestStart,
		RequestID: t.id, Method: t.method, RouteTemplate: t.routeTemplate,
		Transport: t.transport, PeerClass: t.peerClass,
	}
}

func (t *requestTrace) endEvent(ctxErr error, writerStatus int) observability.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.statusCode == 0 {
		t.statusCode = writerStatus
	}
	if t.statusCode == 0 {
		t.statusCode = http.StatusOK
	}
	outcome := observability.OutcomeUnknown
	switch {
	case errors.Is(ctxErr, context.Canceled):
		outcome = observability.OutcomeClientCancel
		t.errorCategory = "client_cancel"
	case t.upstreamRead:
		outcome = observability.OutcomeUpstreamReadError
		t.errorCategory = "upstream_read"
	case t.downstreamWrite:
		outcome = observability.OutcomeDownstreamWriteError
		t.errorCategory = "downstream_write"
	case t.hijacked:
		outcome = observability.OutcomeUpgradeClosed
	case t.bodyEOF:
		outcome = observability.OutcomeBodyEOF
	case t.statusOrigin == "local" || t.statusOrigin == "gateway":
		outcome = observability.OutcomeLocalResponse
	}
	semantic := observability.SemanticNotApplicable
	if t.semantic != nil {
		semantic = t.semantic.Outcome()
	}
	return observability.Event{
		EventType: observability.EventRequestEnd,
		RequestID: t.id, Method: t.method, RouteTemplate: t.routeTemplate,
		Transport: t.transport, PeerClass: t.peerClass, ProfileRef: t.profileRef,
		StatusCode: t.statusCode, StatusOrigin: t.statusOrigin, Outcome: outcome,
		ErrorCategory: t.errorCategory, GatewayTotalMS: durationMS(time.Since(t.started)),
		AuthMS: t.authMS, FirstBodyMS: t.firstBodyMS, ResponseBytes: t.responseBytes,
		SemanticOutcome: semantic,
	}
}

func (t *requestTrace) nextAttempt() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attempt++
	return t.attempt
}

func (t *requestTrace) setAuth(elapsed time.Duration, profileRef string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.authMS = durationMS(elapsed)
	if profileRef != "" {
		t.profileRef = profileRef
	}
}

func (t *requestTrace) setProfileRef(profileRef string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.profileRef = profileRef
}

func (t *requestTrace) setStatus(code int, origin string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.statusCode = code
	if origin != "" {
		t.statusOrigin = origin
	}
}

func (t *requestTrace) setErrorCategory(category string) {
	if category == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.errorCategory = category
}

func (t *requestTrace) enableSSE() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.semantic == nil {
		t.semantic = newSSEObserver()
	}
}

func (t *requestTrace) observeUpstreamBytes(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.semantic != nil {
		t.semantic.Observe(p)
	}
}

func (t *requestTrace) noteBodyEOF() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bodyEOF = true
}

func (t *requestTrace) noteUpstreamReadError() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.upstreamRead = true
}

func (t *requestTrace) noteFirstBody() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.firstBodyMS == 0 {
		t.firstBodyMS = durationMS(time.Since(t.started))
	}
}

func (t *requestTrace) noteDownstreamWrite(n int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.responseBytes += int64(n)
	if err != nil {
		t.downstreamWrite = true
	}
}

func (t *requestTrace) noteHijacked() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hijacked = true
}

func (t *requestTrace) noteAbort() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.upstreamRead && !t.downstreamWrite {
		t.upstreamRead = true
	}
}

func traceFromRequest(req *http.Request) *requestTrace {
	if req == nil {
		return nil
	}
	trace, _ := req.Context().Value(traceContextKey{}).(*requestTrace)
	return trace
}

func newRequestID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		now := time.Now().UnixNano()
		return fmt.Sprintf("r_%x", now)
	}
	return "r_" + hex.EncodeToString(raw[:])
}

func routeTemplate(path string) string {
	switch path {
	case "/backend-api":
		return "/backend-api"
	case "/backend-api/codex/responses":
		return "/backend-api/codex/responses"
	case "/backend-api/models":
		return "/backend-api/models"
	}
	switch {
	case strings.HasPrefix(path, "/backend-api/codex/"):
		return "/backend-api/codex/*"
	case strings.HasPrefix(path, "/backend-api/wham/"):
		return "/backend-api/wham/*"
	case strings.HasPrefix(path, "/backend-api/"):
		return "/backend-api/*"
	default:
		return "unknown"
	}
}

func requestTransport(r *http.Request) string {
	if r != nil && strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return "websocket"
	}
	return "http"
}

func peerClass(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return "unknown"
	}
	if ip.IsLoopback() {
		return "loopback"
	}
	if ip.IsPrivate() {
		return "container-network"
	}
	return "other"
}

func classifyTransportError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "client_cancel"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "upstream_timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return "tls"
	}
	var recordHeader tls.RecordHeaderError
	if errors.As(err, &recordHeader) {
		return "tls"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return "upstream_timeout"
		}
		if opErr.Op == "dial" {
			return "connect"
		}
	}
	return "unknown"
}

func durationMS(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / float64(time.Millisecond)
}

type sseObserver struct {
	line     []byte
	overflow bool
	outcome  string
}

const maxObservedSSELine = 16 << 10

func newSSEObserver() *sseObserver {
	return &sseObserver{line: make([]byte, 0, 256), outcome: observability.SemanticUnobserved}
}

func (o *sseObserver) Observe(p []byte) {
	if o == nil || o.outcome != observability.SemanticUnobserved {
		return
	}
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		chunk := p
		complete := false
		if idx >= 0 {
			chunk = p[:idx]
			p = p[idx+1:]
			complete = true
		} else {
			p = nil
		}
		if !o.overflow {
			remaining := maxObservedSSELine - len(o.line)
			if len(chunk) <= remaining {
				o.line = append(o.line, chunk...)
			} else {
				o.overflow = true
				o.line = o.line[:0]
			}
		}
		if complete {
			if !o.overflow {
				o.observeLine(o.line)
			}
			o.line = o.line[:0]
			o.overflow = false
			if o.outcome != observability.SemanticUnobserved {
				return
			}
		}
	}
}

func (o *sseObserver) observeLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	if bytes.HasPrefix(line, []byte("event:")) {
		o.setTerminal(strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:")))))
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		var meta struct {
			Type string `json:"type"`
		}
		if len(data) <= maxObservedSSELine && json.Unmarshal(data, &meta) == nil {
			o.setTerminal(meta.Type)
		}
	}
}

func (o *sseObserver) setTerminal(eventType string) {
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "response.completed":
		o.outcome = observability.SemanticCompleted
	case "response.failed":
		o.outcome = observability.SemanticFailed
	case "response.incomplete":
		o.outcome = observability.SemanticIncomplete
	}
}

func (o *sseObserver) Outcome() string {
	if o == nil {
		return observability.SemanticNotApplicable
	}
	if o.outcome == "" {
		return observability.SemanticUnobserved
	}
	return o.outcome
}

func makeReplayable(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("buffer request for quota failover: %w", err)
	}
	_ = req.Body.Close()
	setRequestBody(req, body)
	return body, nil
}

func cloneForRetry(req *http.Request, body []byte, creds authbroker.Credentials) *http.Request {
	ctx := context.WithValue(req.Context(), credentialContextKey{}, creds)
	retry := req.Clone(ctx)
	retry.Header = req.Header.Clone()
	sanitizeAuthHeaders(retry.Header)
	applyCredentials(retry.Header, creds)
	if req.Body != nil || body != nil {
		setRequestBody(retry, body)
	}
	return retry
}

func setRequestBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

func sanitizeAuthHeaders(header http.Header) {
	header.Del("Authorization")
	header.Del("ChatGPT-Account-ID")
	header.Del("Proxy-Authorization")
	header.Del("Cookie")
}

func applyCredentials(header http.Header, creds authbroker.Credentials) {
	header.Set("Authorization", "Bearer "+creds.AccessToken)
	header.Set("ChatGPT-Account-ID", creds.AccountID)
}

func requestCredentials(req *http.Request) (authbroker.Credentials, bool) {
	creds, ok := req.Context().Value(credentialContextKey{}).(authbroker.Credentials)
	return creds, ok
}

func usageLimitResponse(resp *http.Response) (bool, time.Time, error) {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests || resp.Body == nil {
		return false, time.Time{}, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, time.Time{}, err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))

	var payload struct {
		Type     string          `json:"type"`
		ResetsAt json.RawMessage `json:"resets_at"`
		Error    struct {
			Type     string          `json:"type"`
			ResetsAt json.RawMessage `json:"resets_at"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, time.Time{}, nil
	}
	errorType := strings.ToLower(strings.TrimSpace(payload.Error.Type))
	if errorType == "" {
		errorType = strings.ToLower(strings.TrimSpace(payload.Type))
	}
	if errorType != "usage_limit_reached" && errorType != "usage_limit_exceeded" {
		return false, time.Time{}, nil
	}
	resetAt := parseResetAt(payload.Error.ResetsAt)
	if resetAt.IsZero() {
		resetAt = parseResetAt(payload.ResetsAt)
	}
	return true, resetAt, nil
}

func parseResetAt(raw json.RawMessage) time.Time {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if value == "" || value == "null" {
		return time.Time{}
	}
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
		if unix > 10_000_000_000 {
			unix /= 1000
		}
		return time.Unix(unix, 0)
	}
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}
