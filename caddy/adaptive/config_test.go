package adaptive

import (
	"testing"

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
