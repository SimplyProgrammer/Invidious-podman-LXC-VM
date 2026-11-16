package adaptive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type raceResult struct {
	backend  BackendSnapshot
	started  time.Time
	resp     *http.Response
	err      error
	received time.Time
	cancel   context.CancelFunc
}

type raceActive struct {
	backend  BackendSnapshot
	started  time.Time
	deadline time.Time
	timedOut bool
	cancel   context.CancelFunc
}

type raceSavedResponse struct {
	resp    *http.Response
	backend BackendSnapshot
	started time.Time
	ttfb    time.Duration
	scored  bool
	cancel  context.CancelFunc
}

// serveRaced is an isolated request coordinator. The legacy sequential retry
// loop remains untouched and is still used for requests that cannot safely be
// replayed, for a single allowed attempt, or when max_time_to_response is 0.
func (h *Handler) serveRaced(w http.ResponseWriter, r *http.Request, maxAttempts int, outcome *string) error {
	initialExcluded := make(map[string]struct{})
	if h.breaker != nil {
		for _, candidate := range h.pool.Snapshot() {
			if !h.breaker.Allow(candidate.Name, time.Now()) {
				initialExcluded[candidate.Name] = struct{}{}
			}
		}
	}
	initial, err := h.pool.Select(initialExcluded)
	if err != nil {
		*outcome = "exhausted_retries"
		http.Error(w, "all proxy backends failed", http.StatusBadGateway)
		return nil
	}

	excluded := map[string]struct{}{initial.Name: {}}

	// Each upstream attempt owns an independent context. In particular, the
	// winner's context must remain alive while its response body is streamed to
	// the client; cancelling a shared race context here would truncate playback.
	// Context values from outboundRequest (including Caddy's replacer) are
	// retained, while request cancellation is coordinated explicitly below.
	results := make(chan raceResult, maxAttempts)
	active := make(map[string]raceActive, maxAttempts)
	attemptsStarted := 0
	var saved *raceSavedResponse
	maxResponseTime := time.Duration(*h.MaxTimeToResponse)

	launch := func(backend BackendSnapshot, retry bool) {
		started := time.Now()
		h.metrics.recordAttempt(backend.Name)
		// Register synchronously so the coordinator cannot mistake a request
		// being prepared for an empty race and exit before its result is read.
		active[backend.Name] = raceActive{
			backend: backend, started: started,
			deadline: started.Add(maxResponseTime), cancel: func() {},
		}
		outReq, buildErr := h.outboundRequest(r, backend, retry)
		if buildErr != nil {
			results <- raceResult{backend: backend, started: started, received: time.Now(), err: buildErr}
			return
		}
		// Preserve request-scoped context values but detach from the client
		// context. Client cancellation is handled explicitly by the coordinator.
		attemptCtx, cancel := context.WithCancel(context.WithoutCancel(outReq.Context()))
		outReq = outReq.WithContext(attemptCtx)
		active[backend.Name] = raceActive{
			backend: backend, started: started,
			deadline: started.Add(maxResponseTime), cancel: cancel,
		}
		go func() {
			resp, roundTripErr := http.DefaultTransport.RoundTrip(outReq)
			results <- raceResult{backend: backend, started: started, received: time.Now(), resp: resp, err: roundTripErr, cancel: cancel}
		}()
	}

	launch(initial, false)
	attemptsStarted = 1

	finalizeSavedAsFailure := func() {
		if saved == nil {
			return
		}
		if !saved.scored {
			h.recordHTTPBackendFailure(saved.backend.Name, saved.ttfb, 0, false, saved.resp.StatusCode, saved.ttfb, 0)
			saved.scored = true
		}
		closeAndDrain(saved.resp.Body)
		if saved.cancel != nil {
			saved.cancel()
		}
		saved = nil
	}

	launchNext := func(trigger string) bool {
		if attemptsStarted >= maxAttempts {
			return false
		}
		candidates := h.autoRetryCandidates(excluded)
		if len(candidates) == 0 {
			return false
		}
		backend := candidates[0]
		excluded[backend.Name] = struct{}{}
		h.metrics.recordRetry(trigger)
		launch(backend, true)
		attemptsStarted++
		return true
	}

	for {
		if r.Context().Err() != nil {
			for _, attempt := range active {
				attempt.cancel()
			}
			*outcome = "client_cancellation"
			remaining := len(active)
			if remaining > 0 {
				go h.collectRaceLosers(results, remaining)
			}
			finalizeSavedAsFailure()
			return nil
		}

		if len(active) == 0 && (attemptsStarted >= maxAttempts || len(h.autoRetryCandidates(excluded)) == 0) {
			break
		}

		var timer *time.Timer
		var timerC <-chan time.Time
		var due *raceActive
		var dueName string
		if attemptsStarted < maxAttempts {
			var earliest time.Time
			for name, attempt := range active {
				if attempt.timedOut {
					continue
				}
				if due == nil || attempt.deadline.Before(earliest) {
					copy := attempt
					due = &copy
					dueName = name
					earliest = attempt.deadline
				}
			}
			if due != nil {
				wait := time.Until(due.deadline)
				if wait < 0 {
					wait = 0
				}
				timer = time.NewTimer(wait)
				timerC = timer.C
			}
		}

		select {
		case <-r.Context().Done():
			if timer != nil {
				timer.Stop()
			}
			for _, attempt := range active {
				attempt.cancel()
			}
			*outcome = "client_cancellation"
			remaining := len(active)
			if remaining > 0 {
				go h.collectRaceLosers(results, remaining)
			}
			finalizeSavedAsFailure()
			return nil

		case <-timerC:
			attempt, ok := active[dueName]
			if ok && !attempt.timedOut {
				attempt.timedOut = true
				active[dueName] = attempt
				launchNext(dueName)
			}

		case result := <-results:
			if timer != nil {
				timer.Stop()
			}
			delete(active, result.backend.Name)
			if result.err != nil || result.resp == nil {
				if result.cancel != nil {
					result.cancel()
				}
				if result.err != nil && (r.Context().Err() != nil || isClientCancellation(result.err)) {
					h.metrics.recordCancellation(result.backend.Name)
				} else {
					if result.err == nil {
						result.err = fmt.Errorf("upstream returned an empty response")
					}
					h.recordBackendFailure(result.backend.Name, time.Since(result.started), 0, false, result.err)
					launchNext(result.backend.Name)
				}
				continue
			}

			ttfb := result.received.Sub(result.started)
			if ttfb < 0 {
				ttfb = 0
			}
			if h.shouldRetryStatus(result.resp.StatusCode) {
				result.resp.Body = newFailureCaptureBody(result.resp.Body, func(body string) { h.metrics.setFailureDiagnostic(result.backend.Name, result.resp.StatusCode, body) })
				h.metrics.setFailureDiagnostic(result.backend.Name, result.resp.StatusCode, "")
				h.penalize(result.backend.Name, *h.ErrorPenalty)
				if h.breaker != nil {
					h.breaker.RecordFailure(result.backend.Name, time.Now())
				}
				if saved != nil {
					finalizeSavedAsFailure()
				}
				saved = &raceSavedResponse{resp: result.resp, backend: result.backend, started: result.started, ttfb: ttfb, cancel: result.cancel}
				if launchNext(result.backend.Name) {
					h.recordHTTPBackendFailure(result.backend.Name, ttfb, 0, false, result.resp.StatusCode, ttfb, 0)
					saved.scored = true
				}
				continue
			}

			// This is the first acceptable response. Do not cancel outstanding
			// attempts: a background collector drains and scores them normally.
			finalizeSavedAsFailure()
			remaining := len(active)
			if remaining > 0 {
				go h.collectRaceLosers(results, remaining)
			}
			return h.writeRaceWinner(w, r, result, ttfb, outcome)
		}
	}

	if saved != nil && (h.PassThroughLastError == nil || *h.PassThroughLastError) {
		if h.Headers != nil && h.Headers.Response != nil && h.Headers.Response.HeaderOps != nil {
			h.Headers.Response.ApplyTo(saved.resp.Header, replacerForRequest(r))
		}
		copyResponseHeaders(w.Header(), saved.resp.Header)
		w.WriteHeader(saved.resp.StatusCode)
		bytesRead, copyErr := io.Copy(w, saved.resp.Body)
		_ = saved.resp.Body.Close()
		if saved.cancel != nil {
			saved.cancel()
		}
		if !saved.scored {
			duration := time.Since(saved.started)
			transfer := duration - saved.ttfb
			if transfer < 0 {
				transfer = 0
			}
			slow := duration > time.Duration(h.LatencyThreshold)
			if slow {
				penalty := h.LatencyPenalty.Value
				if h.LatencyPenalty.Proportional {
					penalty *= float64(duration) / float64(time.Duration(h.LatencyThreshold))
				}
				h.penalize(saved.backend.Name, penalty)
			}
			h.recordHTTPBackendFailure(saved.backend.Name, duration, uint64(bytesRead), slow, saved.resp.StatusCode, saved.ttfb, transfer)
		}
		saved = nil
		if copyErr != nil && r.Context().Err() == nil && !isClientCancellation(copyErr) {
			*outcome = "exhausted_retries"
			return fmt.Errorf("streaming saved upstream error response: %w", copyErr)
		}
		*outcome = "exhausted_retries"
		return nil
	}
	if saved != nil && h.PassThroughLastError != nil && !*h.PassThroughLastError {
		bytesRead, _ := io.Copy(io.Discard, saved.resp.Body)
		_ = saved.resp.Body.Close()
		if saved.cancel != nil {
			saved.cancel()
		}
		if !saved.scored {
			duration := time.Since(saved.started)
			transfer := duration - saved.ttfb
			if transfer < 0 {
				transfer = 0
			}
			slow := duration > time.Duration(h.LatencyThreshold)
			if slow {
				penalty := h.LatencyPenalty.Value
				if h.LatencyPenalty.Proportional {
					penalty *= float64(duration) / float64(time.Duration(h.LatencyThreshold))
				}
				h.penalize(saved.backend.Name, penalty)
			}
			h.recordHTTPBackendFailure(saved.backend.Name, duration, uint64(bytesRead), slow, saved.resp.StatusCode, saved.ttfb, transfer)
		}
		saved = nil
	}
	finalizeSavedAsFailure()
	*outcome = "exhausted_retries"
	http.Error(w, "all proxy backends failed", http.StatusBadGateway)
	return nil
}

func (h *Handler) writeRaceWinner(w http.ResponseWriter, r *http.Request, result raceResult, ttfb time.Duration, outcome *string) error {
	resp := result.resp
	if h.Headers != nil && h.Headers.Response != nil && h.Headers.Response.HeaderOps != nil {
		h.Headers.Response.ApplyTo(resp.Header, replacerForRequest(r))
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	tracked := &responseWriteTracker{ResponseWriter: w}
	bytesWritten, copyErr := io.Copy(tracked, resp.Body)
	_ = resp.Body.Close()
	if result.cancel != nil {
		result.cancel()
	}
	duration := time.Since(result.started)
	transfer := duration - ttfb
	if transfer < 0 {
		transfer = 0
	}
	if copyErr != nil {
		if tracked.writeErr == nil && r.Context().Err() == nil && !isClientCancellation(copyErr) {
			h.recordBackendFailure(result.backend.Name, duration, uint64(bytesWritten), false, copyErr)
			*outcome = "interrupted_transfer"
			return fmt.Errorf("streaming response from backend %q: %w", result.backend.Name, copyErr)
		}
		h.metrics.recordCancellation(result.backend.Name)
		*outcome = "client_cancellation"
		return nil
	}
	slow := duration > time.Duration(h.LatencyThreshold)
	if slow {
		penalty := h.LatencyPenalty.Value
		if h.LatencyPenalty.Proportional {
			penalty *= float64(duration) / float64(time.Duration(h.LatencyThreshold))
		}
		h.penalize(result.backend.Name, penalty)
	}
	if h.shouldRetryStatus(resp.StatusCode) {
		h.recordHTTPBackendFailure(result.backend.Name, duration, uint64(bytesWritten), slow, resp.StatusCode, ttfb, transfer)
		if h.breaker != nil {
			h.breaker.RecordFailure(result.backend.Name, time.Now())
		}
		*outcome = "exhausted_retries"
	} else {
		h.metrics.recordCompletion(result.backend.Name, duration, ttfb, transfer, uint64(bytesWritten), slow, true)
		if h.breaker != nil {
			h.breaker.RecordSuccess(result.backend.Name)
		}
		if resp.StatusCode >= 400 {
			*outcome = "non_retryable_response"
		} else {
			*outcome = "completed_successfully"
		}
	}
	return nil
}

// collectRaceLosers runs after the client response has been selected (or the
// client has disconnected). It never writes to the client response writer.
func (h *Handler) collectRaceLosers(results <-chan raceResult, count int) {
	for i := 0; i < count; i++ {
		result := <-results
		h.scoreRaceLoser(result)
		if result.cancel != nil {
			result.cancel()
		}
	}
}

func (h *Handler) scoreRaceLoser(result raceResult) {
	if result.err != nil || result.resp == nil {
		if result.err != nil && isClientCancellation(result.err) {
			h.metrics.recordCancellation(result.backend.Name)
			return
		}
		failureErr := result.err
		if failureErr == nil {
			failureErr = errors.New("upstream returned an empty response")
		}
		h.recordBackendFailure(result.backend.Name, time.Since(result.started), 0, false, failureErr)
		return
	}
	resp := result.resp
	ttfb := result.received.Sub(result.started)
	if ttfb < 0 {
		ttfb = 0
	}
	if h.shouldRetryStatus(resp.StatusCode) {
		resp.Body = newFailureCaptureBody(resp.Body, func(body string) { h.metrics.setFailureDiagnostic(result.backend.Name, resp.StatusCode, body) })
		h.metrics.setFailureDiagnostic(result.backend.Name, resp.StatusCode, "")
		h.penalize(result.backend.Name, *h.ErrorPenalty)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		h.recordHTTPBackendFailure(result.backend.Name, ttfb, 0, false, resp.StatusCode, ttfb, 0)
		if h.breaker != nil {
			h.breaker.RecordFailure(result.backend.Name, time.Now())
		}
		return
	}
	bytesRead, copyErr := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	duration := time.Since(result.started)
	transfer := duration - ttfb
	if transfer < 0 {
		transfer = 0
	}
	if copyErr != nil {
		h.recordBackendFailure(result.backend.Name, duration, uint64(bytesRead), false, copyErr)
		return
	}
	slow := duration > time.Duration(h.LatencyThreshold)
	if slow {
		penalty := h.LatencyPenalty.Value
		if h.LatencyPenalty.Proportional {
			penalty *= float64(duration) / float64(time.Duration(h.LatencyThreshold))
		}
		h.penalize(result.backend.Name, penalty)
	}
	h.metrics.recordCompletion(result.backend.Name, duration, ttfb, transfer, uint64(bytesRead), slow, true)
	if h.breaker != nil {
		h.breaker.RecordSuccess(result.backend.Name)
	}
}
