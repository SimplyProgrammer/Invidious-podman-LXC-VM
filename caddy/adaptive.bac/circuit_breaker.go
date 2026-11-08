package adaptive

import (
	"sync"
	"time"
)

// CircuitState is the public diagnostic view of a backend circuit.
type CircuitState struct {
	Name                string    `json:"name"`
	Enabled             bool      `json:"enabled"`
	Open                bool      `json:"open"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	OpenUntil           time.Time `json:"open_until,omitempty"`
}

type circuitEntry struct {
	failures  int
	openUntil time.Time
}

// CircuitBreaker temporarily excludes backends after consecutive failures.
// It deliberately has no background goroutine: cooldown expiry is evaluated
// when a request selects a backend.
type CircuitBreaker struct {
	mu        sync.Mutex
	entries   map[string]*circuitEntry
	threshold int
	cooldown  time.Duration
}

func newCircuitBreaker(backends []BackendConfig, cfg *CircuitBreakerConfig) *CircuitBreaker {
	if cfg == nil || cfg.Enabled == nil || !*cfg.Enabled {
		return nil
	}
	cb := &CircuitBreaker{
		entries:   make(map[string]*circuitEntry, len(backends)),
		threshold: *cfg.FailureThreshold,
		cooldown:  time.Duration(cfg.Cooldown),
	}
	for _, backend := range backends {
		cb.entries[backend.Name] = &circuitEntry{}
	}
	return cb
}

func (cb *CircuitBreaker) Allow(name string, now time.Time) bool {
	if cb == nil {
		return true
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	entry := cb.entries[name]
	if entry == nil {
		return false
	}
	if entry.openUntil.IsZero() {
		return true
	}
	if now.Before(entry.openUntil) {
		return false
	}
	// Cooldown elapsed. Permit traffic again and reset the consecutive count.
	entry.openUntil = time.Time{}
	entry.failures = 0
	return true
}

func (cb *CircuitBreaker) RecordFailure(name string, now time.Time) {
	if cb == nil {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	entry := cb.entries[name]
	if entry == nil {
		return
	}
	entry.failures++
	if entry.failures >= cb.threshold && (entry.openUntil.IsZero() || !now.Before(entry.openUntil)) {
		entry.openUntil = now.Add(cb.cooldown)
	}
}

func (cb *CircuitBreaker) RecordSuccess(name string) {
	if cb == nil {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if entry := cb.entries[name]; entry != nil {
		entry.failures = 0
		entry.openUntil = time.Time{}
	}
}

func (cb *CircuitBreaker) Snapshot(names []string, now time.Time) map[string]CircuitState {
	result := make(map[string]CircuitState, len(names))
	for _, name := range names {
		state := CircuitState{Name: name, Enabled: cb != nil}
		if cb != nil {
			cb.mu.Lock()
			entry := cb.entries[name]
			if entry != nil {
				state.ConsecutiveFailures = entry.failures
				state.OpenUntil = entry.openUntil
				state.Open = !entry.openUntil.IsZero() && now.Before(entry.openUntil)
			}
			cb.mu.Unlock()
		}
		result[name] = state
	}
	return result
}
