package adaptive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	caddyheaders "github.com/caddyserver/caddy/v2/modules/caddyhttp/headers"
)

func init() {
	caddy.RegisterModule(Handler{})

	httpcaddyfile.RegisterHandlerDirective("adaptive_proxy", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder(
		"adaptive_proxy",
		httpcaddyfile.Before,
		"reverse_proxy",
	)
}

// Handler is the adaptive proxy HTTP middleware.
type Handler struct {
	Selection        string                `json:"selection,omitempty"`
	LatencyThreshold caddy.Duration        `json:"latency_threshold,omitempty"`
	LatencyPenalty   *int                  `json:"latency_penalty,omitempty"`
	ErrorPenalty     *int                  `json:"error_penalty,omitempty"`
	RecoveryInterval caddy.Duration        `json:"recovery_interval,omitempty"`
	RecoveryAmount   *int                  `json:"recovery_amount,omitempty"`
	MaxAttempts      *int                  `json:"max_attempts,omitempty"`
	MaxPenalty       *int                  `json:"max_penalty,omitempty"`
	RetryStatusCodes []int                 `json:"retry_status_codes,omitempty"`
	Headers          *caddyheaders.Handler `json:"headers,omitempty"`
	Backends         []BackendConfig       `json:"backends,omitempty"`
	CircuitBreaker   *CircuitBreakerConfig `json:"circuit_breaker,omitempty"`
	Statistics       *StatisticsConfig     `json:"statistics,omitempty"`

	pool            *BackendPool
	metrics         *MetricsCollector
	breaker         *CircuitBreaker
	allowedStatsIPs map[string]struct{}
	retryStatusCode map[int]struct{}
}

func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.adaptive_proxy",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision initializes shared backend state and the retry status lookup.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.applyDefaults()
	if err := h.validate(); err != nil {
		return err
	}
	if h.Headers != nil {
		if err := h.Headers.Provision(ctx); err != nil {
			return fmt.Errorf("provision header operations: %w", err)
		}
		if err := h.Headers.Validate(); err != nil {
			return fmt.Errorf("validate header operations: %w", err)
		}
	}

	pool, err := newBackendPool(
		h.Backends,
		h.MaxPenalty,
		time.Duration(h.RecoveryInterval),
		*h.RecoveryAmount,
	)
	if err != nil {
		return err
	}
	h.pool = pool
	h.metrics = newMetricsCollector(h.Backends)
	h.breaker = newCircuitBreaker(h.Backends, h.CircuitBreaker)
	h.allowedStatsIPs = make(map[string]struct{}, len(h.Statistics.AllowedIPs))
	for _, rawIP := range h.Statistics.AllowedIPs {
		h.allowedStatsIPs[net.ParseIP(rawIP).String()] = struct{}{}
	}

	h.retryStatusCode = make(map[int]struct{}, len(h.RetryStatusCodes))
	for _, code := range h.RetryStatusCodes {
		h.retryStatusCode[code] = struct{}{}
	}
	return nil
}

func (h *Handler) Validate() error {
	h.applyDefaults()
	return h.validate()
}

// Cleanup stops the independent penalty recovery worker.
func (h *Handler) Cleanup() error {
	if h.pool != nil {
		h.pool.Close()
	}
	return nil
}

// ServeHTTP chooses a backend independently for each incoming
// request. It retries only before response headers/body are committed.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	if h.pool == nil {
		return fmt.Errorf("adaptive_proxy handler is not provisioned")
	}

	if h.Statistics != nil && *h.Statistics.Enabled && r.URL.Path == h.Statistics.Path {
		h.serveStatistics(w, r)
		return nil
	}
	h.metrics.recordRequest()

	maxAttempts := *h.MaxAttempts
	if maxAttempts > len(h.Backends) {
		maxAttempts = len(h.Backends)
	}
	// A non-replayable request body can be sent only once. Playback traffic
	// is normally GET/HEAD, but this keeps the middleware safe for other methods.
	if r.Body != nil && r.Body != http.NoBody && r.GetBody == nil {
		maxAttempts = 1
	}

	excluded := make(map[string]struct{}, maxAttempts)
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if r.Context().Err() != nil {
			return nil
		}

		// Exclude open circuits in addition to backends already tried for this
		// request. This keeps retries distinct and avoids known-open upstreams.
		if h.breaker != nil {
			for _, candidate := range h.pool.Snapshot() {
				if !h.breaker.Allow(candidate.Name, time.Now()) {
					excluded[candidate.Name] = struct{}{}
				}
			}
		}
		backend, err := h.pool.Select(excluded)
		if err != nil {
			break
		}
		excluded[backend.Name] = struct{}{}
		h.metrics.recordAttempt(backend.Name)

		outReq, err := h.outboundRequest(r, backend, attempt > 0)
		if err != nil {
			lastErr = err
			h.recordBackendFailure(backend.Name, 0, 0, false)
			if attempt+1 < maxAttempts {
				h.metrics.recordRetry(backend.Name)
			}
			continue
		}

		started := time.Now()
		resp, roundTripErr := http.DefaultTransport.RoundTrip(outReq)
		if roundTripErr != nil {
			if r.Context().Err() != nil || isClientCancellation(roundTripErr) {
				return nil
			}
			lastErr = roundTripErr
			h.recordBackendFailure(backend.Name, time.Since(started), 0, false)
			if attempt+1 < maxAttempts {
				h.metrics.recordRetry(backend.Name)
			}
			continue
		}

		if resp == nil {
			lastErr = errors.New("upstream returned an empty response")
			h.recordBackendFailure(backend.Name, time.Since(started), 0, false)
			if attempt+1 < maxAttempts {
				h.metrics.recordRetry(backend.Name)
			}
			continue
		}

		if h.shouldRetryStatus(resp.StatusCode) && attempt+1 < maxAttempts {
			lastErr = fmt.Errorf("upstream %s returned retryable HTTP status %d", backend.Name, resp.StatusCode)
			h.recordBackendFailure(backend.Name, time.Since(started), 0, false)
			h.metrics.recordRetry(backend.Name)
			closeAndDrain(resp.Body)
			continue
		}

		// If this is a retryable status but there are no remaining attempts,
		// return the actual upstream response rather than replacing it with 502.
		statusFailure := h.shouldRetryStatus(resp.StatusCode)
		if statusFailure {
			// Apply the penalty now, but count the completed failure and update
			// the breaker only after the response body has been consumed.
			h.penalize(backend.Name, *h.ErrorPenalty)
		}

		if h.Headers != nil && h.Headers.Response != nil && h.Headers.Response.HeaderOps != nil {
			h.Headers.Response.ApplyTo(resp.Header, replacerForRequest(r))
		}
		copyResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)

		trackedWriter := &responseWriteTracker{ResponseWriter: w}
		bytesWritten, copyErr := io.Copy(trackedWriter, resp.Body)
		_ = resp.Body.Close()
		duration := time.Since(started)

		if copyErr != nil {
			// Headers may already be committed, so switching backends here would
			// corrupt the response. A downstream write failure/client disconnect
			// is not evidence that the selected upstream is unhealthy.
			if trackedWriter.writeErr == nil && r.Context().Err() == nil && !isClientCancellation(copyErr) {
				h.recordBackendFailure(backend.Name, duration, uint64(bytesWritten), false)
				return fmt.Errorf("streaming response from backend %q: %w", backend.Name, copyErr)
			}
			return nil
		}

		slow := duration > time.Duration(h.LatencyThreshold)
		if slow {
			h.penalize(backend.Name, *h.LatencyPenalty)
		}
		if statusFailure {
			// The response was delivered, but its status is still a configured
			// backend failure.
			h.metrics.recordFailure(backend.Name, duration, uint64(bytesWritten), slow)
			if h.breaker != nil {
				h.breaker.RecordFailure(backend.Name, time.Now())
			}
		} else {
			h.metrics.recordSuccess(backend.Name, duration, uint64(bytesWritten), slow)
			if h.breaker != nil {
				h.breaker.RecordSuccess(backend.Name)
			}
		}
		return nil
	}

	if r.Context().Err() != nil {
		return nil
	}
	if lastErr == nil {
		lastErr = ErrNoEligibleBackend
	}
	h.metrics.recordExhausted()
	http.Error(w, "all proxy backends failed", http.StatusBadGateway)
	return nil
}

// recordBackendFailure applies the configured penalty, updates the circuit
// breaker, and records one failed upstream attempt.
func (h *Handler) recordBackendFailure(name string, duration time.Duration, bytes uint64, slow bool) {
	h.penalize(name, *h.ErrorPenalty)
	h.metrics.recordFailure(name, duration, bytes, slow)
	if h.breaker != nil {
		h.breaker.RecordFailure(name, time.Now())
	}
}

func (h *Handler) serveStatistics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	remoteIP := net.ParseIP(clientIP(r))
	if remoteIP == nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if _, allowed := h.allowedStatsIPs[remoteIP.String()]; !allowed {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	type backendStatus struct {
		BackendSnapshot
		Probability float64        `json:"selection_probability"`
		Metrics     BackendMetrics `json:"metrics"`
		Circuit     CircuitState   `json:"circuit_breaker"`
	}
	type statusDocument struct {
		GeneratedAt               time.Time         `json:"generated_at"`
		Global                    map[string]uint64 `json:"global"`
		AverageThroughputBytesSec float64           `json:"average_throughput_bytes_sec"`
		Backends                  []backendStatus   `json:"backends"`
	}

	poolSnapshots := h.pool.Snapshot()
	metricSnapshots, global := h.metrics.Snapshot()
	metricByName := make(map[string]BackendMetrics, len(metricSnapshots))
	for _, item := range metricSnapshots {
		metricByName[item.Name] = item
	}
	circuitNames := make([]string, 0, len(poolSnapshots))
	for _, item := range poolSnapshots {
		circuitNames = append(circuitNames, item.Name)
	}
	now := time.Now().UTC()
	circuits := h.breaker.Snapshot(circuitNames, now)

	var totalWeight float64
	for _, item := range poolSnapshots {
		if !circuits[item.Name].Open {
			totalWeight += item.Weight
		}
	}
	doc := statusDocument{GeneratedAt: now, Global: global}
	if totalDuration := h.metrics.totalDuration(); totalDuration > 0 {
		doc.AverageThroughputBytesSec = float64(global["bytes"]) / totalDuration.Seconds()
	}
	for _, item := range poolSnapshots {
		probability := 0.0
		if totalWeight > 0 && !circuits[item.Name].Open {
			probability = item.Weight / totalWeight
		}
		doc.Backends = append(doc.Backends, backendStatus{
			BackendSnapshot: item,
			Probability:     probability,
			Metrics:         metricByName[item.Name],
			Circuit:         circuits[item.Name],
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(doc)
}

func (h *Handler) outboundRequest(in *http.Request, backend BackendSnapshot, retry bool) (*http.Request, error) {
	target, err := url.Parse(backend.URL)
	if err != nil {
		return nil, fmt.Errorf("parse backend %q URL: %w", backend.Name, err)
	}

	out := in.Clone(in.Context())
	out.URL = cloneURL(in.URL)
	out.URL.Scheme = target.Scheme
	out.URL.Host = target.Host
	out.URL.Path = joinURLPath(target.Path, in.URL.Path)
	out.URL.RawPath = ""
	if target.RawQuery != "" {
		if out.URL.RawQuery == "" {
			out.URL.RawQuery = target.RawQuery
		} else {
			out.URL.RawQuery = target.RawQuery + "&" + out.URL.RawQuery
		}
	}
	out.Host = target.Host
	out.RequestURI = ""

	// RoundTrip consumes and closes the request body. Re-create it for each
	// attempt when the caller supplied GetBody; normal GET requests have no body.
	if in.Body != nil && in.Body != http.NoBody {
		if retry {
			if in.GetBody == nil {
				return nil, errors.New("request body cannot be replayed safely")
			}
			body, err := in.GetBody()
			if err != nil {
				return nil, fmt.Errorf("recreate request body: %w", err)
			}
			out.Body = body
			out.GetBody = in.GetBody
		} else {
			out.Body = in.Body
		}
	}

	removeHopByHopHeaders(out.Header)
	if h.Headers != nil && h.Headers.Request != nil {
		// Caddy's HeaderOps.ApplyToRequest reads the request-scoped replacer.
		// Ensure one is available in standalone tests as well as in Caddy's
		// normal HTTP handler chain.
		repl := replacerForRequest(in)
		out = out.WithContext(context.WithValue(out.Context(), caddy.ReplacerCtxKey, repl))
		h.Headers.Request.ApplyToRequest(out)
	}
	// Match Caddy reverse_proxy behavior: if no User-Agent is present (or
	// header_up removed it), suppress net/http's default Go-http-client value.
	if _, ok := out.Header["User-Agent"]; !ok {
		out.Header.Set("User-Agent", "")
	}
	// Never allow hop-by-hop fields to be reintroduced by header_up.
	removeHopByHopHeaders(out.Header)
	return out, nil
}

func replacerForRequest(r *http.Request) *caddy.Replacer {
	if r != nil {
		if repl, ok := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer); ok && repl != nil {
			return repl
		}
	}
	return caddy.NewReplacer()
}

func cloneURL(in *url.URL) *url.URL {
	copy := *in
	return &copy
}

func joinURLPath(base, request string) string {
	if base == "" || base == "/" {
		return request
	}
	if request == "" || request == "/" {
		return strings.TrimSuffix(base, "/") + "/"
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(request, "/")
}

func (h *Handler) shouldRetryStatus(code int) bool {
	_, ok := h.retryStatusCode[code]
	return ok
}

func (h *Handler) penalize(name string, amount int) {
	if amount > 0 && h.pool != nil {
		_ = h.pool.AddPenalty(name, amount)
	}
}

func isClientCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, http.ErrHandlerTimeout) ||
		strings.Contains(strings.ToLower(err.Error()), "context canceled")
}

func closeAndDrain(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, body, 32*1024)
	_ = body.Close()
}

func copyResponseHeaders(dst, src http.Header) {
	nominated := make(map[string]struct{})
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			nominated[http.CanonicalHeaderKey(strings.TrimSpace(token))] = struct{}{}
		}
	}
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		if _, skip := nominated[http.CanonicalHeaderKey(key)]; skip {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func removeHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(token))
		}
	}
	for key := range header {
		if isHopByHopHeader(key) {
			header.Del(key)
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

// clientIP extracts the remote IP without trusting forwarded headers.
// It is used by the later private statistics endpoint.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return strings.Trim(r.RemoteAddr, "[]")
}

type responseWriteTracker struct {
	http.ResponseWriter
	writeErr error
}

func (w *responseWriteTracker) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err != nil && w.writeErr == nil {
		w.writeErr = err
	}
	return n, err
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	handler := new(Handler)
	if err := handler.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return handler, nil
}

var (
	_ caddy.Module                = Handler{}
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
