package adaptive

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"
)

var (
	ErrNoEligibleBackend = errors.New("no eligible backend remains for this request")
	ErrUnknownBackend    = errors.New("unknown backend")
)

// BackendSnapshot is a read-only view of a backend's current state.
// The statistics endpoint will use these snapshots later.
type BackendSnapshot struct {
	Name           string  `json:"name"`
	URL            string  `json:"url"`
	InitialPenalty float64 `json:"initial_penalty"`
	Penalty        float64 `json:"penalty"`
	Weight         float64 `json:"weight"`
}

type backendState struct {
	name           string
	url            string
	initialPenalty float64
	penalty        float64
}

// BackendPool owns the shared state for all configured upstreams.
type BackendPool struct {
	mu sync.RWMutex

	backends []*backendState
	byName   map[string]*backendState

	maxPenalty     *float64
	recoveryAmount float64

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newBackendPool(configs []BackendConfig, maxPenalty *float64, recoveryInterval time.Duration, recoveryAmount float64) (*BackendPool, error) {
	if len(configs) == 0 {
		return nil, errors.New("backend pool requires at least one backend")
	}
	if recoveryInterval <= 0 {
		return nil, errors.New("recovery interval must be greater than zero")
	}
	if !validPenalty(recoveryAmount) || recoveryAmount == 0 {
		return nil, errors.New("recovery amount must be greater than zero")
	}

	pool := &BackendPool{
		backends:       make([]*backendState, 0, len(configs)),
		byName:         make(map[string]*backendState, len(configs)),
		recoveryAmount: recoveryAmount,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}

	if maxPenalty != nil {
		if !validPenalty(*maxPenalty) {
			return nil, errors.New("maximum penalty must be a finite non-negative number")
		}
		capValue := *maxPenalty
		pool.maxPenalty = &capValue
	}

	for _, cfg := range configs {
		if cfg.Name == "" {
			return nil, errors.New("backend name cannot be empty")
		}
		if !validPenalty(cfg.Penalty) {
			return nil, fmt.Errorf("backend %q initial penalty must be finite and non-negative", cfg.Name)
		}
		if _, exists := pool.byName[cfg.Name]; exists {
			return nil, fmt.Errorf("duplicate backend name %q", cfg.Name)
		}

		initial := cfg.Penalty

		if pool.maxPenalty != nil && initial > *pool.maxPenalty {
			return nil, fmt.Errorf(
				"backend %q initial penalty %v exceeds max_penalty %v",
				cfg.Name, initial, *pool.maxPenalty,
			)
		}

		backend := &backendState{
			name:           cfg.Name,
			url:            cfg.URL,
			initialPenalty: initial,
			penalty:        initial,
		}

		pool.backends = append(pool.backends, backend)
		pool.byName[cfg.Name] = backend
	}

	go pool.recoveryLoop(recoveryInterval)

	return pool, nil
}

// Select chooses one backend using weighted random selection.
//
// The excluded map contains backend names already attempted for the current
// request. Pass nil to select from the complete pool.
func (p *BackendPool) Select(excluded map[string]struct{}) (BackendSnapshot, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var totalWeight float64

	for _, backend := range p.backends {
		if _, skip := excluded[backend.name]; skip {
			continue
		}

		totalWeight += penaltyWeight(backend.penalty)
	}

	if totalWeight <= 0 {
		return BackendSnapshot{}, ErrNoEligibleBackend
	}

	draw := rand.Float64() * totalWeight

	for _, backend := range p.backends {
		if _, skip := excluded[backend.name]; skip {
			continue
		}

		weight := penaltyWeight(backend.penalty)

		if draw < weight {
			return snapshotOf(backend), nil
		}

		draw -= weight
	}

	// Floating-point rounding fallback.
	for i := len(p.backends) - 1; i >= 0; i-- {
		backend := p.backends[i]

		if _, skip := excluded[backend.name]; !skip {
			return snapshotOf(backend), nil
		}
	}

	return BackendSnapshot{}, ErrNoEligibleBackend
}

// AddPenalty increases a backend's penalty.
// A nil max_penalty means unlimited growth.
func (p *BackendPool) AddPenalty(name string, amount float64) error {
	if !validPenalty(amount) {
		return fmt.Errorf("penalty increment must be a finite non-negative number")
	}
	if amount == 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	backend, ok := p.byName[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownBackend, name)
	}

	backend.penalty += amount
	if math.IsInf(backend.penalty, 0) || math.IsNaN(backend.penalty) {
		backend.penalty = math.MaxFloat64
	}

	if p.maxPenalty != nil && backend.penalty > *p.maxPenalty {
		backend.penalty = *p.maxPenalty
	}

	return nil
}

// Snapshot returns a consistent copy of all backend states.
func (p *BackendPool) Snapshot() []BackendSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]BackendSnapshot, 0, len(p.backends))

	for _, backend := range p.backends {
		result = append(result, snapshotOf(backend))
	}

	return result
}

// RecoverNow applies one recovery step immediately.
func (p *BackendPool) RecoverNow() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.recoverLocked()
}

func (p *BackendPool) recoverLocked() {
	for _, backend := range p.backends {
		// Never recover below the configured starting penalty.
		if backend.penalty <= backend.initialPenalty {
			continue
		}

		excess := backend.penalty - backend.initialPenalty

		if excess <= p.recoveryAmount {
			backend.penalty = backend.initialPenalty
		} else {
			backend.penalty -= p.recoveryAmount
		}
	}
}

func (p *BackendPool) recoveryLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(p.done)

	for {
		select {
		case <-ticker.C:
			p.RecoverNow()

		case <-p.stop:
			return
		}
	}
}

// Close stops the recovery worker. Safe to call more than once.
func (p *BackendPool) Close() {
	if p == nil {
		return
	}

	p.closeOnce.Do(func() {
		close(p.stop)
		<-p.done
	})
}

func penaltyWeight(penalty float64) float64 {
	return 1.0 / (1.0 + penalty)
}

func snapshotOf(backend *backendState) BackendSnapshot {
	return BackendSnapshot{
		Name:           backend.name,
		URL:            backend.url,
		InitialPenalty: backend.initialPenalty,
		Penalty:        backend.penalty,
		Weight:         penaltyWeight(backend.penalty),
	}
}
