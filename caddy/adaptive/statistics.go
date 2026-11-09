package adaptive

import (
	"sort"
	"sync"
	"time"
)

const recentDurationLimit = 20

// BackendMetrics is a point-in-time metrics snapshot for one backend.
type BackendMetrics struct {
	Name                      string    `json:"name"`
	URL                       string    `json:"url"`
	Requests                  uint64    `json:"requests"`
	Successes                 uint64    `json:"successes"`
	Failures                  uint64    `json:"failures"`
	Retries                   uint64    `json:"retries"`
	ClientCancellations       uint64    `json:"client_cancellations"`
	SlowResponses             uint64    `json:"slow_responses"`
	BytesTransferred          uint64    `json:"bytes_transferred"`
	AverageDuration           string    `json:"average_duration"`
	RecentAverageDuration     string    `json:"recent_average_duration"`
	LastDuration              string    `json:"last_duration"`
	LastBytes                 uint64    `json:"last_bytes"`
	LastThroughputBytesSec    float64   `json:"last_throughput_bytes_sec"`
	AverageTTFB               string    `json:"average_ttfb"`
	RecentAverageTTFB         string    `json:"recent_average_ttfb"`
	LastTTFB                  string    `json:"last_ttfb"`
	LastTransferDuration      string    `json:"last_transfer_duration"`
	AverageThroughputBytesSec float64   `json:"average_throughput_bytes_sec"`
	LastSuccess               time.Time `json:"last_success,omitempty"`
	LastFailure               time.Time `json:"last_failure,omitempty"`
}

type backendMetricState struct {
	name, url                                                                                         string
	requests, completed, successes, failures, retries, cancellations, slowResponses, bytesTransferred uint64
	totalDuration, totalTTFB, totalTransfer                                                           time.Duration
	ttfbSamples                                                                                       uint64
	recentDurations, recentTTFB                                                                       []time.Duration
	lastDuration, lastTTFB, lastTransfer                                                              time.Duration
	lastBytes                                                                                         uint64
	lastThroughput                                                                                    float64
	lastSuccess, lastFailure                                                                          time.Time
}

// MetricsCollector is safe for concurrent request handlers. Logical request
// outcomes and upstream-attempt metrics are deliberately tracked separately.
type MetricsCollector struct {
	mu                                                  sync.RWMutex
	backends                                            map[string]*backendMetricState
	requests, attempts, retries, exhausted, slow, bytes uint64
	active                                              uint64
	outcomes                                            map[string]uint64
	accumulatedDuration                                 time.Duration
}

func newMetricsCollector(backends []BackendConfig) *MetricsCollector {
	m := &MetricsCollector{backends: make(map[string]*backendMetricState, len(backends)), outcomes: make(map[string]uint64)}
	for _, b := range backends {
		m.backends[b.Name] = &backendMetricState{name: b.Name, url: b.URL}
	}
	return m
}
func (m *MetricsCollector) recordRequest() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.requests++
	m.active++
	m.mu.Unlock()
}
func (m *MetricsCollector) recordOutcome(outcome string) {
	if m == nil || outcome == "" {
		return
	}
	m.mu.Lock()
	m.outcomes[outcome]++
	if m.active > 0 {
		m.active--
	}
	if outcome == "exhausted_retries" {
		m.exhausted++
	}
	m.mu.Unlock()
}
func (m *MetricsCollector) recordAttempt(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if b := m.backends[name]; b != nil {
		b.requests++
		m.attempts++
	}
	m.mu.Unlock()
}
func (m *MetricsCollector) recordCancellation(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if b := m.backends[name]; b != nil {
		b.cancellations++
	}
	m.mu.Unlock()
}
func (m *MetricsCollector) recordRetry(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if b := m.backends[name]; b != nil {
		b.retries++
		m.retries++
	}
	m.mu.Unlock()
}
func (m *MetricsCollector) recordSuccess(name string, duration time.Duration, bytes uint64, slow bool) {
	m.recordCompletion(name, duration, 0, 0, bytes, slow, true)
}
func (m *MetricsCollector) recordFailure(name string, duration time.Duration, bytes uint64, slow bool) {
	m.recordCompletion(name, duration, 0, 0, bytes, slow, false)
}
func (m *MetricsCollector) recordCompletion(name string, duration, ttfb, transfer time.Duration, bytes uint64, slow, success bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.backends[name]
	if b == nil {
		return
	}
	now := time.Now().UTC()
	b.completed++
	if success {
		b.successes++
		b.lastSuccess = now
	} else {
		b.failures++
		b.lastFailure = now
	}
	if slow {
		b.slowResponses++
		m.slow++
	}
	b.bytesTransferred += bytes
	m.bytes += bytes
	b.totalDuration += duration
	if ttfb > 0 {
		b.totalTTFB += ttfb
		b.ttfbSamples++
	}
	b.totalTransfer += transfer
	m.accumulatedDuration += transfer
	b.lastDuration = duration
	b.lastTTFB = ttfb
	b.lastTransfer = transfer
	b.lastBytes = bytes
	if transfer > 0 {
		b.lastThroughput = float64(bytes) / transfer.Seconds()
	} else {
		b.lastThroughput = 0
	}
	b.recentDurations = append(b.recentDurations, duration)
	if len(b.recentDurations) > recentDurationLimit {
		b.recentDurations = append([]time.Duration(nil), b.recentDurations[len(b.recentDurations)-recentDurationLimit:]...)
	}
	if ttfb > 0 {
		b.recentTTFB = append(b.recentTTFB, ttfb)
	}
	if len(b.recentTTFB) > recentDurationLimit {
		b.recentTTFB = append([]time.Duration(nil), b.recentTTFB[len(b.recentTTFB)-recentDurationLimit:]...)
	}
}
func avgDuration(ds []time.Duration) time.Duration {
	var total time.Duration
	for _, d := range ds {
		total += d
	}
	if len(ds) == 0 {
		return 0
	}
	return total / time.Duration(len(ds))
}
func (m *MetricsCollector) Snapshot() ([]BackendMetrics, map[string]uint64) {
	if m == nil {
		return nil, map[string]uint64{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]BackendMetrics, 0, len(m.backends))
	for _, b := range m.backends {
		var avg, avgTTFB time.Duration
		if b.completed > 0 {
			avg = b.totalDuration / time.Duration(b.completed)
		}
		if b.ttfbSamples > 0 {
			avgTTFB = b.totalTTFB / time.Duration(b.ttfbSamples)
		}
		throughput := float64(0)
		if b.totalTransfer > 0 {
			throughput = float64(b.bytesTransferred) / b.totalTransfer.Seconds()
		}
		out = append(out, BackendMetrics{Name: b.name, URL: b.url, Requests: b.requests, Successes: b.successes, Failures: b.failures, Retries: b.retries, ClientCancellations: b.cancellations, SlowResponses: b.slowResponses, BytesTransferred: b.bytesTransferred, AverageDuration: avg.String(), RecentAverageDuration: avgDuration(b.recentDurations).String(), LastDuration: b.lastDuration.String(), LastBytes: b.lastBytes, LastThroughputBytesSec: b.lastThroughput, AverageTTFB: avgTTFB.String(), RecentAverageTTFB: avgDuration(b.recentTTFB).String(), LastTTFB: b.lastTTFB.String(), LastTransferDuration: b.lastTransfer.String(), AverageThroughputBytesSec: throughput, LastSuccess: b.lastSuccess, LastFailure: b.lastFailure})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	g := map[string]uint64{"requests": m.requests, "attempts": m.attempts, "retries": m.retries, "exhausted_requests": m.exhausted, "slow": m.slow, "bytes": m.bytes, "in_progress": m.active}
	for k, v := range m.outcomes {
		g["outcome_"+k] = v
	}
	return out, g
}
func (m *MetricsCollector) totalDuration() time.Duration {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.accumulatedDuration
}
