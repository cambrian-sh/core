package config

import (
	"strings"
	"testing"
)

// ENG-2003: mcp.handshake_timeout_ms / mcp.ping_timeout_ms are validated at
// load, and unset means the connector's built-in defaults (no DefaultConfig
// pin, so the zero value reaches the connector).
func TestLoad_MCPTimeouts(t *testing.T) {
	t.Run("unset stays zero (connector default)", func(t *testing.T) {
		cfg, _ := loadFrom(t, baseBundle(t), nil)
		if cfg.MCP.HandshakeTimeoutMs != 0 || cfg.MCP.PingTimeoutMs != 0 {
			t.Fatalf("unset bounds must load as 0, got %d / %d", cfg.MCP.HandshakeTimeoutMs, cfg.MCP.PingTimeoutMs)
		}
	})
	t.Run("configured values load", func(t *testing.T) {
		dir := baseBundle(t)
		writeCfg(t, dir, "config.local.json", `{"mcp": {"handshake_timeout_ms": 15000, "ping_timeout_ms": 3000}}`)
		cfg, _ := loadFrom(t, dir, nil)
		if cfg.MCP.HandshakeTimeoutMs != 15000 || cfg.MCP.PingTimeoutMs != 3000 {
			t.Fatalf("got %d / %d", cfg.MCP.HandshakeTimeoutMs, cfg.MCP.PingTimeoutMs)
		}
	})
	t.Run("env override loads", func(t *testing.T) {
		t.Setenv("CAMBRIAN_MCP__HANDSHAKE_TIMEOUT_MS", "20000")
		cfg, _ := loadFrom(t, baseBundle(t), nil)
		if cfg.MCP.HandshakeTimeoutMs != 20000 {
			t.Fatalf("env override not applied: %d", cfg.MCP.HandshakeTimeoutMs)
		}
	})
	for _, bad := range []struct{ body, key string }{
		{`{"mcp": {"handshake_timeout_ms": -1}}`, "mcp.handshake_timeout_ms"},
		{`{"mcp": {"handshake_timeout_ms": 600001}}`, "mcp.handshake_timeout_ms"},
		{`{"mcp": {"ping_timeout_ms": -5}}`, "mcp.ping_timeout_ms"},
	} {
		t.Run("rejects "+bad.body, func(t *testing.T) {
			dir := baseBundle(t)
			writeCfg(t, dir, "config.local.json", bad.body)
			if msg := loadErr(t, dir, nil); !strings.Contains(msg, bad.key) {
				t.Fatalf("error does not name %s: %s", bad.key, msg)
			}
		})
	}
	if errs := (MCPConfig{HandshakeTimeoutMs: MaxMCPTimeoutMs, PingTimeoutMs: 1}).validateTimeouts(); len(errs) != 0 {
		t.Fatalf("in-range bounds rejected: %v", errs)
	}
}
