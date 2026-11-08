Adaptive Playback Caddy module — phase 1 (configuration parser)

Files:
- adaptive/go.mod
- adaptive/handler.go
- adaptive/config.go
- adaptive/selector.go (placeholder)
- adaptive/statistics.go (placeholder)
- Dockerfile (Caddy 2.11.9 builder/runtime)
- test.Caddyfile (configuration-adaptation test)

Important:
This phase implements configuration parsing/defaults/validation only.
The HTTP handler intentionally passes requests to the next handler. It does
not proxy or select backends yet.

Defaults:
- selection: adaptive_weighted_random
- latency_threshold: 6s
- latency_penalty: 1
- error_penalty: 2
- recovery_interval: 5m
- recovery_amount: 1
- max_attempts: 3
- max_penalty: unlimited (omit the directive)
- retry_status_codes: 401-599 excluding 404
- circuit_breaker: disabled unless its block is present; if present:
  enabled=true, failure_threshold=3, cooldown=60s
- statistics: enabled=true, path=/_adaptive/status,
  allowed_ips=127.0.0.1 and ::1
