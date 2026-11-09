package adaptive

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	caddyheaders "github.com/caddyserver/caddy/v2/modules/caddyhttp/headers"
)

// BackendConfig describes one upstream and its starting penalty.
type BackendConfig struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Penalty int    `json:"penalty"`
}

// CircuitBreakerConfig is optional. A nil config means the circuit breaker
// is disabled. If the block is present, its defaults are enabled=true,
// failure_threshold=3, and cooldown=60s.
type CircuitBreakerConfig struct {
	Enabled          *bool          `json:"enabled,omitempty"`
	FailureThreshold *int           `json:"failure_threshold,omitempty"`
	Cooldown         caddy.Duration `json:"cooldown,omitempty"`
}

// StatisticsConfig controls the private status endpoint.
type StatisticsConfig struct {
	Enabled    *bool    `json:"enabled,omitempty"`
	Path       string   `json:"path,omitempty"`
	AllowedIPs []string `json:"allowed_ips,omitempty"`
}

func (h *Handler) applyDefaults() {
	if h.Selection == "" {
		h.Selection = "adaptive_weighted_random"
	}
	if h.LatencyThreshold == 0 {
		h.LatencyThreshold = caddy.Duration(6 * time.Second)
	}
	if h.LatencyPenalty == nil {
		h.LatencyPenalty = intPtr(1)
	}
	if h.ErrorPenalty == nil {
		h.ErrorPenalty = intPtr(2)
	}
	if h.RecoveryInterval == 0 {
		h.RecoveryInterval = caddy.Duration(10 * time.Minute)
	}
	if h.RecoveryAmount == nil {
		h.RecoveryAmount = intPtr(1)
	}
	if h.MaxAttempts == nil {
		h.MaxAttempts = intPtr(3)
	}
	// A nil MaxPenalty intentionally means unlimited penalty.
	if h.RetryStatusCodes == nil {
		h.RetryStatusCodes = defaultRetryStatusCodes()
	}
	if h.Statistics == nil {
		h.Statistics = &StatisticsConfig{}
	}
	h.Statistics.applyDefaults()

	// CircuitBreaker remains nil unless its block is explicitly configured.
	if h.CircuitBreaker != nil {
		h.CircuitBreaker.applyDefaults()
	}
}

func (s *StatisticsConfig) applyDefaults() {
	if s.Enabled == nil {
		s.Enabled = boolPtr(false)
	}
	if s.Path == "" {
		s.Path = "/_adaptive/status"
	}
	if s.AllowedIPs == nil {
		s.AllowedIPs = []string{"127.0.0.1", "::1"}
	}
}

func (c *CircuitBreakerConfig) applyDefaults() {
	if c.Enabled == nil {
		c.Enabled = boolPtr(true)
	}
	if c.FailureThreshold == nil {
		c.FailureThreshold = intPtr(4)
	}
	if c.Cooldown == 0 {
		c.Cooldown = caddy.Duration(60 * time.Second)
	}
}

func defaultRetryStatusCodes() []int {
	codes := make([]int, 0, 198)
	for code := 401; code <= 599; code++ {
		if code != 404 {
			codes = append(codes, code)
		}
	}
	return codes
}

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
//
// Syntax:
//
//	adaptive_proxy {
//	    selection adaptive_weighted_random
//	    latency_threshold 6s
//	    latency_penalty 1
//	    error_penalty 2
//	    recovery_interval 10m
//	    recovery_amount 1
//	    max_attempts 3
//	    # max_penalty omitted means unlimited
//	    retry_status_codes 401-403 405-499 500-599
//	    header_up -Via
//	    header_up -X-Forwarded-*
//	    header_down -Server
//	    header_down -Via
//	    backend local http://main-be:1234 {
//	        penalty 0
//	    }
//	    circuit_breaker {
//	        enabled true
//	        failure_threshold 3
//	        cooldown 60s
//	    }
//	    statistics {
//	        enabled true
//	        path /_adaptive/status
//	        allowed_ips 127.0.0.1 ::1
//	    }
//	}
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	*h = Handler{}
	h.applyDefaults()

	if !d.Next() {
		return d.EOFErr()
	}
	if d.Val() != "adaptive_proxy" {
		return d.Errf("expected adaptive_proxy, got %q", d.Val())
	}
	if d.NextArg() {
		return d.ArgErr()
	}

	seen := make(map[string]bool)
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		key := d.Val()

		// Backend and header operations may be repeated; other top-level
		// directives may appear only once.
		if key != "backend" && key != "header_up" && key != "header_down" {
			if seen[key] {
				return d.Errf("directive %q may only be specified once", key)
			}
			seen[key] = true
		}

		switch key {
		case "selection":
			value, err := oneArg(d)
			if err != nil {
				return err
			}
			h.Selection = value

		case "latency_threshold":
			value, err := durationArg(d)
			if err != nil {
				return err
			}
			h.LatencyThreshold = caddy.Duration(value)

		case "latency_penalty":
			value, err := intArg(d)
			if err != nil {
				return err
			}
			h.LatencyPenalty = intPtr(value)

		case "error_penalty":
			value, err := intArg(d)
			if err != nil {
				return err
			}
			h.ErrorPenalty = intPtr(value)

		case "recovery_interval":
			value, err := durationArg(d)
			if err != nil {
				return err
			}
			h.RecoveryInterval = caddy.Duration(value)

		case "recovery_amount":
			value, err := intArg(d)
			if err != nil {
				return err
			}
			h.RecoveryAmount = intPtr(value)

		case "max_attempts":
			value, err := intArg(d)
			if err != nil {
				return err
			}
			h.MaxAttempts = intPtr(value)

		case "max_penalty":
			value, err := intArg(d)
			if err != nil {
				return err
			}
			h.MaxPenalty = intPtr(value)

		case "retry_status_codes":
			args := d.RemainingArgs()
			if len(args) == 0 {
				return d.ArgErr()
			}
			codes, err := parseStatusCodes(args)
			if err != nil {
				return d.WrapErr(err)
			}
			h.RetryStatusCodes = codes

		case "header_up":
			if h.Headers == nil {
				h.Headers = new(caddyheaders.Handler)
			}
			if h.Headers.Request == nil {
				h.Headers.Request = new(caddyheaders.HeaderOps)
			}
			if err := parseHeaderOperation(d, h.Headers.Request); err != nil {
				return err
			}

		case "header_down":
			if h.Headers == nil {
				h.Headers = new(caddyheaders.Handler)
			}
			if h.Headers.Response == nil {
				h.Headers.Response = &caddyheaders.RespHeaderOps{
					HeaderOps: new(caddyheaders.HeaderOps),
					Deferred:  true,
				}
			}
			if err := parseHeaderOperation(d, h.Headers.Response.HeaderOps); err != nil {
				return err
			}

		case "backend":
			backend, err := parseBackend(d)
			if err != nil {
				return err
			}
			h.Backends = append(h.Backends, backend)

		case "circuit_breaker":
			if d.NextArg() {
				return d.ArgErr()
			}
			cfg, err := parseCircuitBreaker(d)
			if err != nil {
				return err
			}
			h.CircuitBreaker = cfg

		case "statistics":
			if d.NextArg() {
				return d.ArgErr()
			}
			cfg, err := parseStatistics(d, h.Statistics)
			if err != nil {
				return err
			}
			h.Statistics = cfg

		default:
			return d.Errf("unknown adaptive_proxy option %q", key)
		}
	}

	h.applyDefaults()
	return h.validate()
}

func parseHeaderOperation(d *caddyfile.Dispenser, ops *caddyheaders.HeaderOps) error {
	args := d.RemainingArgs()
	var field, value string
	var replacement *string

	switch len(args) {
	case 1:
		field = args[0]
	case 2:
		field, value = args[0], args[1]
	case 3:
		field, value = args[0], args[1]
		replacement = &args[2]
	default:
		return d.ArgErr()
	}

	if err := caddyheaders.CaddyfileHeaderOp(ops, field, value, replacement); err != nil {
		return d.Err(err.Error())
	}
	return nil
}

func parseBackend(d *caddyfile.Dispenser) (BackendConfig, error) {
	var backend BackendConfig
	if !d.Args(&backend.Name, &backend.URL) || d.NextArg() {
		return backend, d.ArgErr()
	}

	// penalty defaults to zero when the nested block is omitted.
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "penalty":
			value, err := intArg(d)
			if err != nil {
				return backend, err
			}
			backend.Penalty = value
		default:
			return backend, d.Errf("unknown backend option %q", d.Val())
		}
	}
	return backend, nil
}

func parseCircuitBreaker(d *caddyfile.Dispenser) (*CircuitBreakerConfig, error) {
	cfg := &CircuitBreakerConfig{}
	cfg.applyDefaults()

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "enabled":
			value, err := boolArg(d)
			if err != nil {
				return nil, err
			}
			cfg.Enabled = boolPtr(value)
		case "failure_threshold":
			value, err := intArg(d)
			if err != nil {
				return nil, err
			}
			cfg.FailureThreshold = intPtr(value)
		case "cooldown":
			value, err := durationArg(d)
			if err != nil {
				return nil, err
			}
			cfg.Cooldown = caddy.Duration(value)
		default:
			return nil, d.Errf("unknown circuit_breaker option %q", d.Val())
		}
	}
	return cfg, nil
}

func parseStatistics(d *caddyfile.Dispenser, defaults *StatisticsConfig) (*StatisticsConfig, error) {
	cfg := &StatisticsConfig{}
	if defaults != nil {
		*cfg = *defaults
		cfg.AllowedIPs = append([]string(nil), defaults.AllowedIPs...)
	}
	cfg.applyDefaults()

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "enabled":
			value, err := boolArg(d)
			if err != nil {
				return nil, err
			}
			cfg.Enabled = boolPtr(value)
		case "path":
			value, err := oneArg(d)
			if err != nil {
				return nil, err
			}
			cfg.Path = value
		case "allowed_ips":
			values := d.RemainingArgs()
			if len(values) == 0 {
				return nil, d.ArgErr()
			}
			cfg.AllowedIPs = values
		default:
			return nil, d.Errf("unknown statistics option %q", d.Val())
		}
	}
	return cfg, nil
}

func oneArg(d *caddyfile.Dispenser) (string, error) {
	if !d.NextArg() {
		return "", d.ArgErr()
	}
	value := d.Val()
	if d.NextArg() {
		return "", d.ArgErr()
	}
	return value, nil
}

func intArg(d *caddyfile.Dispenser) (int, error) {
	raw, err := oneArg(d)
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, d.Errf("expected an integer, got %q", raw)
	}
	return value, nil
}

func boolArg(d *caddyfile.Dispenser) (bool, error) {
	raw, err := oneArg(d)
	if err != nil {
		return false, err
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, d.Errf("expected true or false, got %q", raw)
	}
	return value, nil
}

func durationArg(d *caddyfile.Dispenser) (time.Duration, error) {
	raw, err := oneArg(d)
	if err != nil {
		return 0, err
	}
	value, err := caddy.ParseDuration(raw)
	if err != nil {
		return 0, d.Errf("invalid duration %q: %v", raw, err)
	}
	return value, nil
}

func parseStatusCodes(args []string) ([]int, error) {
	set := make(map[int]struct{})
	for _, arg := range args {
		if strings.Contains(arg, "-") {
			parts := strings.Split(arg, "-")
			if len(parts) != 2 {
				return nil, fmt.Errorf("invalid HTTP status range %q", arg)
			}
			first, err1 := strconv.Atoi(parts[0])
			last, err2 := strconv.Atoi(parts[1])
			if err1 != nil || err2 != nil || first < 100 || last > 599 || first > last {
				return nil, fmt.Errorf("invalid HTTP status range %q (expected 100-599)", arg)
			}
			for code := first; code <= last; code++ {
				set[code] = struct{}{}
			}
			continue
		}
		code, err := strconv.Atoi(arg)
		if err != nil || code < 100 || code > 599 {
			return nil, fmt.Errorf("invalid HTTP status code %q (expected 100-599)", arg)
		}
		set[code] = struct{}{}
	}

	codes := make([]int, 0, len(set))
	for code := range set {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	if len(codes) == 0 {
		return nil, fmt.Errorf("at least one retry status code is required")
	}
	return codes, nil
}

func (h *Handler) validate() error {
	if h.Selection != "adaptive_weighted_random" {
		return fmt.Errorf("unsupported selection strategy %q; currently supported: adaptive_weighted_random", h.Selection)
	}
	if h.LatencyThreshold <= 0 {
		return fmt.Errorf("latency_threshold must be greater than zero")
	}
	if h.LatencyPenalty == nil || *h.LatencyPenalty < 0 {
		return fmt.Errorf("latency_penalty must be zero or greater")
	}
	if h.ErrorPenalty == nil || *h.ErrorPenalty < 0 {
		return fmt.Errorf("error_penalty must be zero or greater")
	}
	if h.RecoveryInterval <= 0 {
		return fmt.Errorf("recovery_interval must be greater than zero")
	}
	if h.RecoveryAmount == nil || *h.RecoveryAmount < 1 {
		return fmt.Errorf("recovery_amount must be at least 1")
	}
	if h.MaxAttempts == nil || *h.MaxAttempts < 1 {
		return fmt.Errorf("max_attempts must be at least 1")
	}
	if h.MaxPenalty != nil && *h.MaxPenalty < 0 {
		return fmt.Errorf("max_penalty must be zero or greater")
	}
	if len(h.Backends) == 0 {
		return fmt.Errorf("at least one backend must be configured")
	}

	backendNames := make(map[string]struct{}, len(h.Backends))
	for _, backend := range h.Backends {
		if strings.TrimSpace(backend.Name) == "" {
			return fmt.Errorf("backend name cannot be empty")
		}
		if _, exists := backendNames[backend.Name]; exists {
			return fmt.Errorf("duplicate backend name %q", backend.Name)
		}
		backendNames[backend.Name] = struct{}{}
		if backend.Penalty < 0 {
			return fmt.Errorf("backend %q penalty must be zero or greater", backend.Name)
		}
		u, err := url.Parse(backend.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("backend %q has invalid URL %q; expected an http:// or https:// URL", backend.Name, backend.URL)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("backend %q URL must not contain credentials, query parameters, or a fragment", backend.Name)
		}
	}

	seenCodes := make(map[int]struct{}, len(h.RetryStatusCodes))
	for _, code := range h.RetryStatusCodes {
		if code < 100 || code > 599 {
			return fmt.Errorf("retry status code %d is outside the valid HTTP range 100-599", code)
		}
		if _, exists := seenCodes[code]; exists {
			return fmt.Errorf("duplicate retry status code %d", code)
		}
		seenCodes[code] = struct{}{}
	}

	if h.CircuitBreaker != nil {
		h.CircuitBreaker.applyDefaults()
		if h.CircuitBreaker.FailureThreshold == nil || *h.CircuitBreaker.FailureThreshold < 1 {
			return fmt.Errorf("circuit_breaker failure_threshold must be at least 1")
		}
		if h.CircuitBreaker.Cooldown <= 0 {
			return fmt.Errorf("circuit_breaker cooldown must be greater than zero")
		}
	}

	if h.Statistics != nil {
		h.Statistics.applyDefaults()
		if !strings.HasPrefix(h.Statistics.Path, "/") ||
			strings.ContainsAny(h.Statistics.Path, "?#") {
			return fmt.Errorf("statistics path must be an absolute URL path without query or fragment")
		}
		if len(h.Statistics.AllowedIPs) == 0 {
			return fmt.Errorf("statistics allowed_ips must contain at least one IP address")
		}
		for _, rawIP := range h.Statistics.AllowedIPs {
			if net.ParseIP(rawIP) == nil {
				return fmt.Errorf("statistics allowed_ips contains invalid IP address %q", rawIP)
			}
		}
	}
	return nil
}

var _ caddyfile.Unmarshaler = (*Handler)(nil)
