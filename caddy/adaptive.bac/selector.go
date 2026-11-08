package adaptive

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

var (
	ErrNoEligibleBackend = errors.New("no eligible backend remains for this request")
	ErrUnknownBackend    = errors.New("unknown backend")
)

const maxUint64 = ^uint64(0)

// BackendSnapshot is a read-only view of a backend's current state.
// The statistics endpoint will use these snapshots later.
type BackendSnapshot struct {
	Name           string  `json:"name"`
	URL            string  `json:"url"`
	InitialPenalty uint64  `json:"initial_penalty"`
	Penalty        uint64  `json:"penalty"`
	Weight         float64 `json:"weight"`
}

type backendState struct {
	name           string
	url            string
	initialPenalty uint64
	penalty        uint64
}

// BackendPool owns the shared state for all configured upstreams.
type BackendPool struct {
	mu sync.RWMutex

	backends []*backendState
	byName   map[string]*backendState

	maxPenalty     *uint64
	recoveryAmount uint64

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newBackendPool(configs []BackendConfig, maxPenalty *int, recoveryInterval time.Duration, recoveryAmount int) (*BackendPool, error) {
	if len(configs) == 0 {
		return nil, errors.New("backend pool requires at least one backend")
	}
	if recoveryInterval <= 0 {
		return nil, errors.New("recovery interval must be greater than zero")
	}
	if recoveryAmount < 1 {
		return nil, errors.New("recovery amount must be at least one")
	}

	pool := &BackendPool{
		backends:       make([]*backendState, 0, len(configs)),
		byName:         make(map[string]*backendState, len(configs)),
		recoveryAmount: uint64(recoveryAmount),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}

	if maxPenalty != nil {
		if *maxPenalty < 0 {
			return nil, errors.New("maximum penalty cannot be negative")
		}
		capValue := uint64(*maxPenalty)
		pool.maxPenalty = &capValue
	}

	for _, cfg := range configs {
		if cfg.Name == "" {
			return nil, errors.New("backend name cannot be empty")
		}
		if cfg.Penalty < 0 {
			return nil, fmt.Errorf("backend %q has a negative initial penalty", cfg.Name)
		}
		if _, exists := pool.byName[cfg.Name]; exists {
			return nil, fmt.Errorf("duplicate backend name %q", cfg.Name)
		}

		initial := uint64(cfg.Penalty)

		if pool.maxPenalty != nil && initial > *pool.maxPenalty {
			return nil, fmt.Errorf(
				"backend %q initial penalty %d exceeds max_penalty %d",
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
// A nil max_penalty means unlimited growth, up to uint64 saturation.
func (p *BackendPool) AddPenalty(name string, amount int) error {
	if amount <= 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	backend, ok := p.byName[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownBackend, name)
	}

	increment := uint64(amount)

	if maxUint64-backend.penalty < increment {
		backend.penalty = maxUint64
	} else {
		backend.penalty += increment
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

func penaltyWeight(penalty uint64) float64 {
	return 1.0 / (1.0 + float64(penalty))
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
