package adaptive

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestUnmarshalCaddyfileParsesRepeatedHeaderDirectives(t *testing.T) {
	d := caddyfile.NewTestDispenser(`
adaptive_proxy {
    header_up -Via
    header_up -X-Forwarded-*
    header_up X-Proxy-Test configured
    header_down -Server
    header_down -Via
    header_down X-Response-Test configured
    backend local http://127.0.0.1:3000 {
        penalty 0
    }
}`)
	h := new(Handler)
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile: %v", err)
	}
	if h.Headers == nil || h.Headers.Request == nil || h.Headers.Response == nil {
		t.Fatal("header operations were not parsed")
	}
	if len(h.Headers.Request.Delete) != 2 {
		t.Fatalf("request delete operations=%v; want Via and X-Forwarded-*", h.Headers.Request.Delete)
	}
	if got := h.Headers.Request.Set.Get("X-Proxy-Test"); got != "configured" {
		t.Fatalf("header_up set=%q; want configured", got)
	}
	if len(h.Headers.Response.Delete) != 2 {
		t.Fatalf("response delete operations=%v; want Server and Via", h.Headers.Response.Delete)
	}
	if got := h.Headers.Response.Set.Get("X-Response-Test"); got != "configured" {
		t.Fatalf("header_down set=%q; want configured", got)
	}
	if !h.Headers.Response.Deferred {
		t.Fatal("header_down response operations should be deferred until upstream response exists")
	}
	if h.Headers.Request.Set == nil || h.Headers.Response.Set == nil {
		t.Fatal("expected populated header operation maps")
	}
}

func TestLatencyPenaltyModesAndDefaults(t *testing.T) {
	h := &Handler{Backends: []BackendConfig{{Name: "local", URL: "http://127.0.0.1:3000"}}}
	h.applyDefaults()
	if timeDuration := time.Duration(h.LatencyThreshold); timeDuration != 4*time.Second {
		t.Fatalf("default latency_threshold=%s; want 4s", timeDuration)
	}
	if h.LatencyPenalty == nil || !h.LatencyPenalty.Proportional || h.LatencyPenalty.Value != 1 {
		t.Fatalf("default latency_penalty=%+v; want 1x", h.LatencyPenalty)
	}

	tests := []struct {
		input        string
		wantValue    float64
		proportional bool
	}{
		{input: "1", wantValue: 1},
		{input: "0.5", wantValue: 0.5},
		{input: "1x", wantValue: 1, proportional: true},
		{input: "2.5x", wantValue: 2.5, proportional: true},
	}
	for _, test := range tests {
		d := caddyfile.NewTestDispenser("adaptive_proxy {\n latency_penalty " + test.input + "\n backend local http://127.0.0.1:3000\n}")
		parsed := new(Handler)
		if err := parsed.UnmarshalCaddyfile(d); err != nil {
			t.Fatalf("parse %q: %v", test.input, err)
		}
		if parsed.LatencyPenalty.Value != test.wantValue || parsed.LatencyPenalty.Proportional != test.proportional {
			t.Errorf("latency_penalty %q parsed as %+v", test.input, parsed.LatencyPenalty)
		}
	}
}

func TestLatencyPenaltyJSONRoundTrip(t *testing.T) {
	for _, input := range []string{`1`, `0.5`, `"1x"`, `"2.5x"`} {
		var got LatencyPenaltyConfig
		if err := json.Unmarshal([]byte(input), &got); err != nil {
			t.Fatalf("unmarshal %s: %v", input, err)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal %s: %v", input, err)
		}
		var roundTrip LatencyPenaltyConfig
		if err := json.Unmarshal(encoded, &roundTrip); err != nil {
			t.Fatalf("round-trip %s: %v", input, err)
		}
		if got != roundTrip {
			t.Errorf("round-trip %s: got %+v, want %+v", input, roundTrip, got)
		}
	}
}

func TestCaddyfileParsesFractionalPenaltySettings(t *testing.T) {
	d := caddyfile.NewTestDispenser(`
adaptive_proxy {
    error_penalty 1.5
    recovery_amount 0.25
    max_penalty 10.5
    backend remote https://remote.example {
        penalty 0.5
    }
}`)
	h := new(Handler)
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if *h.ErrorPenalty != 1.5 || *h.RecoveryAmount != 0.25 || *h.MaxPenalty != 10.5 {
		t.Fatalf("fractional settings parsed incorrectly: error=%v recovery=%v max=%v",
			*h.ErrorPenalty, *h.RecoveryAmount, *h.MaxPenalty)
	}
	if h.Backends[0].Penalty != 0.5 {
		t.Fatalf("backend penalty=%v; want 0.5", h.Backends[0].Penalty)
	}
}

func TestRetryDefaultsAndCaddyfileOptions(t *testing.T) {
	h := &Handler{Backends: []BackendConfig{{Name: "local", URL: "http://127.0.0.1:3000"}}}
	h.applyDefaults()
	if !h.MaxAttemptsAuto {
		t.Fatal("default max_attempts should be auto")
	}
	if h.PassThroughLastError == nil || !*h.PassThroughLastError {
		t.Fatal("pass_through_last_error should default to true")
	}

	d := caddyfile.NewTestDispenser(`adaptive_proxy {
		max_attempts 4
		pass_through_last_error false
		backend local http://127.0.0.1:3000
	}`)
	parsed := new(Handler)
	if err := parsed.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if parsed.MaxAttemptsAuto || *parsed.MaxAttempts != 4 {
		t.Fatalf("numeric max_attempts parsed incorrectly: auto=%v count=%d", parsed.MaxAttemptsAuto, *parsed.MaxAttempts)
	}
	if *parsed.PassThroughLastError {
		t.Fatal("pass_through_last_error false was not parsed")
	}

	d = caddyfile.NewTestDispenser(`adaptive_proxy {
		max_attempts auto
		backend local http://127.0.0.1:3000
	}`)
	parsed = new(Handler)
	if err := parsed.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if !parsed.MaxAttemptsAuto {
		t.Fatal("max_attempts auto was not parsed")
	}
}
