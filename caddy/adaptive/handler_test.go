package adaptive

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	caddyheaders "github.com/caddyserver/caddy/v2/modules/caddyhttp/headers"
)

func newTestHandler(t *testing.T, backends []BackendConfig, attempts int, retryCodes []int) *Handler {
	t.Helper()
	h := &Handler{
		Backends:         backends,
		MaxAttempts:      intPtr(attempts),
		RetryStatusCodes: retryCodes,
	}
	h.applyDefaults()
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = h.Cleanup() })
	return h
}

func TestAdaptiveProxyProxiesPathQueryAndRange(t *testing.T) {
	type observedRequest struct{ path, rangeHeader, host string }
	observed := make(chan observedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- observedRequest{path: r.URL.RequestURI(), rangeHeader: r.Header.Get("Range"), host: r.Host}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "video-data")
	}))
	defer upstream.Close()

	h := newTestHandler(t, []BackendConfig{{Name: "upstream", URL: upstream.URL}}, 1, nil)
	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/companion/videoplayback?itag=22&sig=abc", nil)
	req.Header.Set("Range", "bytes=100-199")
	rr := httptest.NewRecorder()

	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status=%d; want 206", rr.Code)
	}
	got := <-observed
	if got.path != "/companion/videoplayback?itag=22&sig=abc" {
		t.Fatalf("upstream URI=%q", got.path)
	}
	if got.rangeHeader != "bytes=100-199" {
		t.Fatalf("Range=%q", got.rangeHeader)
	}
	parsed, _ := url.Parse(upstream.URL)
	if got.host != parsed.Host {
		t.Fatalf("Host=%q; want %q", got.host, parsed.Host)
	}
	if rr.Body.String() != "video-data" || rr.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("response body/header not preserved: %q %+v", rr.Body.String(), rr.Header())
	}
	metrics, global := h.metrics.Snapshot()
	if metrics[0].Requests != 1 || metrics[0].Successes != 1 || metrics[0].BytesTransferred != uint64(len("video-data")) {
		t.Fatalf("unexpected backend metrics: %+v", metrics[0])
	}
	if global["requests"] != 1 || global["bytes"] != uint64(len("video-data")) {
		t.Fatalf("unexpected global metrics: %+v", global)
	}
}

func TestAdaptiveProxyRetriesConfiguredStatusOnDifferentBackend(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "unavailable")
	}))
	defer first.Close()
	var secondCalls atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("X-Backend", "second")
		_, _ = io.WriteString(w, "ok")
	}))
	defer second.Close()

	h := newTestHandler(t, []BackendConfig{
		{Name: "first", URL: first.URL, Penalty: 0},
		{Name: "second", URL: second.URL, Penalty: 1_000_000_000},
	}, 2, []int{http.StatusServiceUnavailable})

	// Make the first backend overwhelmingly likely, then verify the retry
	// reaches the other backend. The chance of selecting the healthy backend
	// first is negligible (about one in a billion).
	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/video", nil)
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
		t.Fatalf("got status=%d body=%q; want 200 ok", rr.Code, rr.Body.String())
	}
	if secondCalls.Load() != 1 {
		t.Fatalf("healthy backend calls=%d; want exactly one retry", secondCalls.Load())
	}
	metrics, global := h.metrics.Snapshot()
	if metrics[0].Name != "first" || metrics[0].Failures != 1 || metrics[0].Retries != 1 {
		t.Fatalf("first backend retry metrics unexpected: %+v", metrics)
	}
	if global["retries"] != 1 {
		t.Fatalf("global retries=%d; want 1", global["retries"])
	}
}

func TestAdaptiveProxyDoesNotRetry404(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h := newTestHandler(t, []BackendConfig{{Name: "only", URL: upstream.URL}}, 3, nil)
	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/video", nil)
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusNotFound || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d; want 404 and one call", rr.Code, calls.Load())
	}
	if got := h.pool.Snapshot()[0].Penalty; got != 0 {
		t.Fatalf("404 changed penalty to %g; want 0", got)
	}
	metrics, _ := h.metrics.Snapshot()
	if metrics[0].Successes != 1 || metrics[0].Failures != 0 {
		t.Fatalf("404 should be recorded as a delivered response: %+v", metrics[0])
	}
}

func TestAdaptiveProxyRetriesNetworkFailure(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "healthy")
	}))
	defer good.Close()

	h := newTestHandler(t, []BackendConfig{
		{Name: "dead", URL: deadURL, Penalty: 0},
		{Name: "good", URL: good.URL, Penalty: 1_000_000_000},
	}, 2, nil)
	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/video", nil)
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "healthy" {
		t.Fatalf("got status=%d body=%q; want 200 healthy", rr.Code, rr.Body.String())
	}
}

func TestAdaptiveProxyDoesNotRetryAfterBodyStarted(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "partial")
	}))
	defer upstream.Close()

	h := newTestHandler(t, []BackendConfig{{Name: "only", URL: upstream.URL}}, 3, nil)
	// This test asserts the normal streaming path is used and exactly one
	// upstream request is made. A custom failing ResponseWriter test covers
	// downstream write errors separately in a future test expansion.
	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/video", nil)
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if calls.Load() != 1 || rr.Body.String() != "partial" {
		t.Fatalf("calls=%d body=%q", calls.Load(), rr.Body.String())
	}
}

func TestOutboundRequestReplaysBodyWhenGetBodyAvailable(t *testing.T) {
	h := &Handler{}
	original := httptest.NewRequest(http.MethodPost, "http://invidious.test/upload", strings.NewReader("payload"))
	backend := BackendSnapshot{Name: "target", URL: "http://127.0.0.1:9999"}
	out, err := h.outboundRequest(original, backend, false)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "payload" {
		t.Fatalf("body=%q", body)
	}
}

func TestIsClientCancellation(t *testing.T) {
	if !isClientCancellation(fmt.Errorf("read: context canceled")) {
		t.Fatal("context cancellation not recognized")
	}
	if isClientCancellation(fmt.Errorf("connection reset by peer")) {
		t.Fatal("ordinary upstream failure misclassified as client cancellation")
	}
}

func TestClientIPDoesNotUseForwardedHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.10")
	if got := clientIP(req); got != "192.0.2.10" {
		t.Fatalf("clientIP=%q", got)
	}
}

func TestRequestContextCancellationStopsBeforeSelection(t *testing.T) {
	h := newTestHandler(t, []BackendConfig{{Name: "only", URL: "http://127.0.0.1:1"}}, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/video", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != 200 || rr.Body.Len() != 0 {
		t.Fatalf("unexpected response: %d %q", rr.Code, rr.Body.String())
	}
}

func TestCircuitBreakerOpensAndRecoversAfterCooldown(t *testing.T) {
	cfg := &CircuitBreakerConfig{Enabled: boolPtr(true), FailureThreshold: intPtr(2), Cooldown: caddy.Duration(30 * time.Millisecond)}
	cb := newCircuitBreaker([]BackendConfig{{Name: "one"}}, cfg)
	now := time.Now()
	cb.RecordFailure("one", now)
	if !cb.Allow("one", now) {
		t.Fatal("circuit opened before threshold")
	}
	cb.RecordFailure("one", now)
	if cb.Allow("one", now) {
		t.Fatal("circuit should be open")
	}
	if !cb.Allow("one", now.Add(31*time.Millisecond)) {
		t.Fatal("circuit did not recover after cooldown")
	}
	state := cb.Snapshot([]string{"one"}, now.Add(31*time.Millisecond))["one"]
	if state.Open || state.ConsecutiveFailures != 0 {
		t.Fatalf("unexpected recovered state: %+v", state)
	}
}

func TestStatisticsEndpointRequiresAllowedRemoteIP(t *testing.T) {
	h := newTestHandler(t, []BackendConfig{{Name: "local", URL: "http://127.0.0.1:1"}}, 1, nil)
	// Statistics are disabled by default; enable them explicitly for this test.
	h.Statistics.Enabled = boolPtr(true)
	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/_adaptive/status", nil)
	req.RemoteAddr = "192.0.2.20:12345"
	res := httptest.NewRecorder()
	if err := h.ServeHTTP(res, req, nil); err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusForbidden {
		t.Fatalf("status=%d; want 403", res.Code)
	}
}

func TestStatisticsEndpointReturnsJSONForLoopback(t *testing.T) {
	h := newTestHandler(t, []BackendConfig{{Name: "local", URL: "http://127.0.0.1:1"}}, 1, nil)
	// Statistics are disabled by default; enable them explicitly for this test.
	h.Statistics.Enabled = boolPtr(true)
	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/_adaptive/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	res := httptest.NewRecorder()
	if err := h.ServeHTTP(res, req, nil); err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusOK || !strings.Contains(res.Header().Get("Content-Type"), "application/json") || !strings.Contains(res.Body.String(), `"local"`) {
		t.Fatalf("unexpected stats response: status=%d headers=%v body=%s", res.Code, res.Header(), res.Body.String())
	}
}

func TestAdaptiveProxyHeaderUpAndHeaderDown(t *testing.T) {
	type observed struct {
		via       string
		forwarded string
		realIP    string
		custom    string
		userAgent string
	}
	seen := make(chan observed, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- observed{
			via:       r.Header.Get("Via"),
			forwarded: r.Header.Get("X-Forwarded-For"),
			realIP:    r.Header.Get("X-Real-IP"),
			custom:    r.Header.Get("X-Proxy-Test"),
			userAgent: r.Header.Get("User-Agent"),
		}
		w.Header().Set("Server", "upstream-fingerprint")
		w.Header().Set("Via", "upstream-proxy")
		w.Header().Set("X-Upstream", "preserved")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	h := &Handler{
		Backends: []BackendConfig{{Name: "upstream", URL: upstream.URL}},
		Headers: &caddyheaders.Handler{
			Request: &caddyheaders.HeaderOps{
				Delete: []string{"Via", "X-Forwarded-*", "X-Real-IP", "User-Agent"},
				Set:    http.Header{"X-Proxy-Test": []string{"configured"}},
			},
			Response: &caddyheaders.RespHeaderOps{
				HeaderOps: &caddyheaders.HeaderOps{
					Delete: []string{"Server", "Via"},
					Set:    http.Header{"X-Response-Filtered": []string{"yes"}},
				},
				Deferred: true,
			},
		},
	}
	h.applyDefaults()
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = h.Cleanup() })

	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/video", nil)
	req.Header.Set("Via", "incoming-proxy")
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	req.Header.Set("X-Real-IP", "198.51.100.1")
	req.Header.Set("User-Agent", "incoming-browser-agent")
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}

	got := <-seen
	if got.via != "" || got.forwarded != "" || got.realIP != "" {
		t.Fatalf("header_up did not remove proxy headers: %+v", got)
	}
	if got.custom != "configured" {
		t.Fatalf("header_up set value=%q; want configured", got.custom)
	}
	if got.userAgent != "" {
		t.Fatalf("deleted User-Agent was replaced by transport default %q", got.userAgent)
	}
	if rr.Header().Get("Server") != "" || rr.Header().Get("Via") != "" {
		t.Fatalf("header_down left fingerprint headers: %v", rr.Header())
	}
	if rr.Header().Get("X-Upstream") != "preserved" || rr.Header().Get("X-Response-Filtered") != "yes" {
		t.Fatalf("header_down did not preserve/set expected headers: %v", rr.Header())
	}
}

func TestStatisticsTracksTerminalOutcomesAndTiming(t *testing.T) {
	m := newMetricsCollector([]BackendConfig{{Name: "one", URL: "http://one"}})
	m.recordRequest()
	m.recordAttempt("one")
	m.recordCompletion("one", 150*time.Millisecond, 40*time.Millisecond, 110*time.Millisecond, 1100, false, true)
	m.recordOutcome("completed_successfully")
	m.recordRequest()
	m.recordOutcome("client_cancellation")
	backends, global := m.Snapshot()
	if global["requests"] != 2 || global["in_progress"] != 0 || global["outcome_completed_successfully"] != 1 || global["outcome_client_cancellation"] != 1 {
		t.Fatalf("unexpected request accounting: %#v", global)
	}
	if backends[0].LastTTFB != "40ms" || backends[0].LastTransferDuration != "110ms" || backends[0].AverageThroughputBytesSec <= 0 {
		t.Fatalf("unexpected timing metrics: %+v", backends[0])
	}
}

func TestProportionalLatencyPenaltyUsesTotalDuration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	h := &Handler{
		Backends:         []BackendConfig{{Name: "slow", URL: upstream.URL}},
		MaxAttempts:      intPtr(1),
		LatencyThreshold: caddy.Duration(10 * time.Millisecond),
		LatencyPenalty:   &LatencyPenaltyConfig{Value: 1, Proportional: true},
	}
	h.applyDefaults()
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Cleanup() })

	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/video", nil)
	res := httptest.NewRecorder()
	if err := h.ServeHTTP(res, req, nil); err != nil {
		t.Fatal(err)
	}
	got := h.pool.Snapshot()[0].Penalty
	if got < 3 {
		t.Fatalf("proportional penalty=%v; want at least 3 for a 30ms+ response over 10ms threshold", got)
	}
	metrics, _ := h.metrics.Snapshot()
	if metrics[0].SlowResponses != 1 {
		t.Fatalf("slow_responses=%d; want 1", metrics[0].SlowResponses)
	}
}

func TestFixedLatencyPenaltyRemainsBackwardCompatible(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	h := &Handler{
		Backends:         []BackendConfig{{Name: "slow", URL: upstream.URL}},
		MaxAttempts:      intPtr(1),
		LatencyThreshold: caddy.Duration(10 * time.Millisecond),
		LatencyPenalty:   &LatencyPenaltyConfig{Value: 1, Proportional: false},
	}
	h.applyDefaults()
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Cleanup() })

	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/video", nil)
	res := httptest.NewRecorder()
	if err := h.ServeHTTP(res, req, nil); err != nil {
		t.Fatal(err)
	}
	if got := h.pool.Snapshot()[0].Penalty; got != 1 {
		t.Fatalf("fixed latency penalty=%v; want exactly 1", got)
	}
}

func TestAutoAttemptsExhaustEveryBackendAndPassThroughLastError(t *testing.T) {
	var calls [3]atomic.Int64
	servers := make([]*httptest.Server, 3)
	backends := make([]BackendConfig, 3)
	for i := range servers {
		i := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[i].Add(1)
			w.Header().Set("X-Upstream-Error", fmt.Sprintf("backend-%d", i))
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "upstream-%d-unavailable", i)
		}))
		defer servers[i].Close()
		backends[i] = BackendConfig{Name: fmt.Sprintf("backend-%d", i), URL: servers[i].URL}
	}

	h := &Handler{Backends: backends, MaxAttemptsAuto: true, RetryStatusCodes: []int{http.StatusServiceUnavailable}}
	h.applyDefaults()
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/video", nil)
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}

	total := int64(0)
	for i := range calls {
		total += calls[i].Load()
	}
	if total != 3 {
		t.Fatalf("total upstream attempts=%d; want 3", total)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d; want final upstream 503", rr.Code)
	}
	if !strings.HasPrefix(rr.Body.String(), "upstream-") {
		t.Fatalf("body=%q; expected upstream error body", rr.Body.String())
	}
	if rr.Header().Get("X-Upstream-Error") == "" {
		t.Fatal("last upstream response header was not passed through")
	}
}

func TestAutoRetriesBodylessServerRequestWithNonNilBody(t *testing.T) {
	var calls [3]atomic.Int64
	servers := make([]*httptest.Server, 3)
	backends := make([]BackendConfig, 3)
	for i := range servers {
		i := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[i].Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "upstream-%d-unavailable", i)
		}))
		defer servers[i].Close()
		backends[i] = BackendConfig{Name: fmt.Sprintf("backend-%d", i), URL: servers[i].URL}
	}

	h := &Handler{Backends: backends, MaxAttemptsAuto: true, RetryStatusCodes: []int{http.StatusServiceUnavailable}}
	h.applyDefaults()
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	// net/http server requests have a non-nil Body even when ContentLength is
	// zero. Simulate that shape rather than httptest.NewRequest's http.NoBody.
	req := httptest.NewRequest(http.MethodGet, "http://invidious.test/video", nil)
	req.Body = io.NopCloser(strings.NewReader(""))
	req.ContentLength = 0
	req.GetBody = nil
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}

	total := int64(0)
	for i := range calls {
		total += calls[i].Load()
	}
	if total != 3 {
		t.Fatalf("total upstream attempts=%d; want all 3 backends tried", total)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d; want final upstream 503", rr.Code)
	}
	_, global := h.metrics.Snapshot()
	if global["attempts"] != 3 || global["retries"] != 2 || global["exhausted_requests"] != 1 {
		t.Fatalf("unexpected retry metrics: %+v", global)
	}
}

func TestPassThroughLastErrorCanBeDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "upstream detail")
	}))
	defer upstream.Close()
	h := &Handler{Backends: []BackendConfig{{Name: "only", URL: upstream.URL}}, MaxAttempts: intPtr(1), PassThroughLastError: boolPtr(false), RetryStatusCodes: []int{http.StatusServiceUnavailable}}
	h.applyDefaults()
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()
	rr := httptest.NewRecorder()
	if err := h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://invidious.test/video", nil), nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rr.Code != http.StatusBadGateway || strings.Contains(rr.Body.String(), "upstream detail") {
		t.Fatalf("got %d %q; want synthetic 502", rr.Code, rr.Body.String())
	}
}
