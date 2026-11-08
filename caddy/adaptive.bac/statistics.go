package adaptive

import (
	"sort"
	"sync"
	"time"
)

const recentDurationLimit = 20

// BackendMetrics is a point-in-time metrics snapshot for one backend. Penalty
// and weight are joined from BackendPool when the status endpoint is rendered.
type BackendMetrics struct {
	Name                   string    `json:"name"`
	URL                    string    `json:"url"`
	Requests               uint64    `json:"requests"`
	Successes              uint64    `json:"successes"`
	Failures               uint64    `json:"failures"`
	Retries                uint64    `json:"retries"`
	SlowResponses          uint64    `json:"slow_responses"`
	BytesTransferred       uint64    `json:"bytes_transferred"`
	AverageDuration        string    `json:"average_duration"`
	RecentAverageDuration  string    `json:"recent_average_duration"`
	LastDuration           string    `json:"last_duration"`
	LastBytes              uint64    `json:"last_bytes"`
	LastThroughputBytesSec float64   `json:"last_throughput_bytes_sec"`
	LastSuccess            time.Time `json:"last_success,omitempty"`
	LastFailure            time.Time `json:"last_failure,omitempty"`
}

type backendMetricState struct {
	name             string
	url              string
	requests         uint64
	completed        uint64
	successes        uint64
	failures         uint64
	retries          uint64
	slowResponses    uint64
	bytesTransferred uint64
	totalDuration    time.Duration
	recentDurations  []time.Duration
	lastDuration     time.Duration
	lastBytes        uint64
	lastThroughput   float64
	lastSuccess      time.Time
	lastFailure      time.Time
}

// MetricsCollector is safe for concurrent request handlers.
type MetricsCollector struct {
	mu                  sync.RWMutex
	backends            map[string]*backendMetricState
	requests            uint64
	attempts            uint64
	retries             uint64
	exhausted           uint64
	slow                uint64
	bytes               uint64
	accumulatedDuration time.Duration
}

func newMetricsCollector(backends []BackendConfig) *MetricsCollector {
	m := &MetricsCollector{backends: make(map[string]*backendMetricState, len(backends))}
	for _, backend := range backends {
		m.backends[backend.Name] = &backendMetricState{
			name: backend.Name,
			url:  backend.URL,
		}
	}
	return m
}

func (m *MetricsCollector) recordRequest() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.requests++
	m.mu.Unlock()
}

func (m *MetricsCollector) recordExhausted() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.exhausted++
	m.mu.Unlock()
}

func (m *MetricsCollector) recordAttempt(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if backend := m.backends[name]; backend != nil {
		backend.requests++
		m.attempts++
	}
}

func (m *MetricsCollector) recordRetry(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if backend := m.backends[name]; backend != nil {
		backend.retries++
		m.retries++
	}
}

func (m *MetricsCollector) recordSuccess(name string, duration time.Duration, bytes uint64, slow bool) {
	m.recordCompletion(name, duration, bytes, slow, true)
}

func (m *MetricsCollector) recordFailure(name string, duration time.Duration, bytes uint64, slow bool) {
	m.recordCompletion(name, duration, bytes, slow, false)
}

func (m *MetricsCollector) recordCompletion(name string, duration time.Duration, bytes uint64, slow, success bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	backend := m.backends[name]
	if backend == nil {
		return
	}
	now := time.Now().UTC()
	backend.completed++
	if success {
		backend.successes++
		backend.lastSuccess = now
	} else {
		backend.failures++
		backend.lastFailure = now
	}
	if slow {
		backend.slowResponses++
		m.slow++
	}
	backend.bytesTransferred += bytes
	m.bytes += bytes
	backend.totalDuration += duration
	m.accumulatedDuration += duration
	backend.lastDuration = duration
	backend.lastBytes = bytes
	if duration > 0 {
		backend.lastThroughput = float64(bytes) / duration.Seconds()
	} else {
		backend.lastThroughput = 0
	}
	backend.recentDurations = append(backend.recentDurations, duration)
	if len(backend.recentDurations) > recentDurationLimit {
		backend.recentDurations = append([]time.Duration(nil), backend.recentDurations[len(backend.recentDurations)-recentDurationLimit:]...)
	}
}

// Snapshot returns immutable copies for rendering or diagnostics.
func (m *MetricsCollector) Snapshot() ([]BackendMetrics, map[string]uint64) {
	if m == nil {
		return nil, map[string]uint64{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	backends := make([]BackendMetrics, 0, len(m.backends))
	for _, state := range m.backends {
		var average, recentAverage time.Duration
		if state.completed > 0 {
			average = state.totalDuration / time.Duration(state.completed)
		}
		if len(state.recentDurations) > 0 {
			var total time.Duration
			for _, duration := range state.recentDurations {
				total += duration
			}
			recentAverage = total / time.Duration(len(state.recentDurations))
		}
		backends = append(backends, BackendMetrics{
			Name:                   state.name,
			URL:                    state.url,
			Requests:               state.requests,
			Successes:              state.successes,
			Failures:               state.failures,
			Retries:                state.retries,
			SlowResponses:          state.slowResponses,
			BytesTransferred:       state.bytesTransferred,
			AverageDuration:        average.String(),
			RecentAverageDuration:  recentAverage.String(),
			LastDuration:           state.lastDuration.String(),
			LastBytes:              state.lastBytes,
			LastThroughputBytesSec: state.lastThroughput,
			LastSuccess:            state.lastSuccess,
			LastFailure:            state.lastFailure,
		})
	}
	sort.Slice(backends, func(i, j int) bool { return backends[i].Name < backends[j].Name })
	global := map[string]uint64{
		"requests":           m.requests,
		"attempts":           m.attempts,
		"retries":            m.retries,
		"exhausted_requests": m.exhausted,
		"slow":               m.slow,
		"bytes":              m.bytes,
	}
	return backends, global
}

// totalDuration returns the accumulated duration of completed upstream bodies.
func (m *MetricsCollector) totalDuration() time.Duration {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.accumulatedDuration
}
