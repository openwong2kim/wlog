// Package config holds runtime configuration for wlog: ports, bind policy,
// data path resolution (XDG / LOCALAPPDATA / macOS), retention, recv limits,
// and WLOG_* environment overrides. It is dependency-free beyond stdlib.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Default values for configuration knobs. Exported so callers (CLI flag
// defaults, tests) can reference a single source of truth.
const (
	DefaultOTLPGRPCPort = 4317
	DefaultOTLPHTTPPort = 4318
	DefaultUIPort       = 0 // 0 = pick a free port automatically
	DefaultListen       = "127.0.0.1"
	DefaultRetention    = 90 * 24 * time.Hour
	DefaultMaxSessions  = 1000
	DefaultMaxRecvBytes = 16 * 1024 * 1024 // 16 MiB
)

// Config is the resolved runtime configuration for a wlog process.
type Config struct {
	// OTLPGRPCPort is the OTLP/gRPC listener port (default 4317).
	OTLPGRPCPort int
	// OTLPHTTPPort is the OTLP/HTTP listener port (default 4318).
	OTLPHTTPPort int
	// UIPort is the embedded web UI port; 0 selects a free port automatically.
	UIPort int
	// Listen is the bind address (default 127.0.0.1, loopback).
	Listen string
	// Unsafe permits binding to a non-loopback address without auth.
	Unsafe bool
	// AuthToken, when set, gates access on non-loopback binds.
	AuthToken string
	// DBPath is the SQLite database file path (ignored when Memory is true).
	DBPath string
	// Memory uses an in-memory database instead of a file.
	Memory bool
	// Retention is the max age of retained data (default 90 days).
	Retention time.Duration
	// MaxSessions is the max number of retained sessions (oldest pruned first).
	MaxSessions int
	// MaxRecvBytes bounds a single received OTLP message (default 16 MiB).
	MaxRecvBytes int64
	// NoOpen disables auto-opening the browser on startup.
	NoOpen bool
	// NoStorePrompts refuses to persist prompt / tool-argument payloads.
	NoStorePrompts bool
	// PrintClaudeSetup prints the Claude Code OTel env snippet and exits.
	PrintClaudeSetup bool
	// NoJSONL disables the zero-config Claude Code transcript JSONL ingestion
	// (PLAN v3 feature 0). When false (default) wlog scans + watches ClaudeDir
	// alongside the OTLP receiver.
	NoJSONL bool
	// ClaudeDir overrides the directory scanned for Claude Code transcript JSONL
	// files. Empty selects the platform default (~/.claude/projects).
	ClaudeDir string
}

// Default returns a Config populated with the documented default values.
func Default() *Config {
	return &Config{
		OTLPGRPCPort: DefaultOTLPGRPCPort,
		OTLPHTTPPort: DefaultOTLPHTTPPort,
		UIPort:       DefaultUIPort,
		Listen:       DefaultListen,
		Retention:    DefaultRetention,
		MaxSessions:  DefaultMaxSessions,
		MaxRecvBytes: DefaultMaxRecvBytes,
	}
}

// DefaultDBPath returns the platform-specific default database file path and
// ensures its parent directory exists. Windows: %LOCALAPPDATA%\wlog\wlog.db,
// macOS: ~/Library/Application Support/wlog/wlog.db, Linux/other:
// ${XDG_DATA_HOME:-~/.local/share}/wlog/wlog.db.
func DefaultDBPath() string {
	dir := defaultDataDir()
	// Best-effort directory creation; file open will surface real errors.
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "wlog.db")
}

func defaultDataDir() string {
	switch runtime.GOOS {
	case "windows":
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, "wlog")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "AppData", "Local", "wlog")
		}
		return filepath.Join(".", "wlog")
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "wlog")
		}
		return filepath.Join(".", "wlog")
	default: // linux and others: XDG
		if base := os.Getenv("XDG_DATA_HOME"); base != "" {
			return filepath.Join(base, "wlog")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "wlog")
		}
		return filepath.Join(".", "wlog")
	}
}

// DefaultClaudeDir returns the platform default directory that holds Claude Code
// transcript JSONL files: ~/.claude/projects on every platform (Windows
// %USERPROFILE%\.claude\projects), matching where Claude Code writes them. It
// does NOT create the directory — absence is normal (OTLP-only operation) and is
// handled by silently skipping the JSONL scan. An empty string is returned when
// the home directory cannot be determined.
func DefaultClaudeDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// Validate checks invariants and the bind policy. A non-loopback Listen
// without --unsafe and without an --auth-token is rejected.
func (c *Config) Validate() error {
	if c.OTLPGRPCPort < 0 || c.OTLPGRPCPort > 65535 {
		return fmt.Errorf("invalid --otlp-grpc-port %d", c.OTLPGRPCPort)
	}
	if c.OTLPHTTPPort < 0 || c.OTLPHTTPPort > 65535 {
		return fmt.Errorf("invalid --otlp-http-port %d", c.OTLPHTTPPort)
	}
	if c.UIPort < 0 || c.UIPort > 65535 {
		return fmt.Errorf("invalid --ui-port %d", c.UIPort)
	}
	if c.OTLPGRPCPort != 0 && c.OTLPGRPCPort == c.OTLPHTTPPort {
		return fmt.Errorf("--otlp-grpc-port and --otlp-http-port must differ (both %d)", c.OTLPGRPCPort)
	}
	if c.Listen == "" {
		return fmt.Errorf("--listen must not be empty")
	}
	if !isLoopback(c.Listen) && !c.Unsafe && c.AuthToken == "" {
		return fmt.Errorf("non-loopback bind requires --unsafe or --auth-token")
	}
	if c.Retention < 0 {
		return fmt.Errorf("--retention must not be negative")
	}
	if c.MaxSessions < 0 {
		return fmt.Errorf("--max-sessions must not be negative")
	}
	if c.MaxRecvBytes <= 0 {
		return fmt.Errorf("--max-recv-bytes must be positive")
	}
	return nil
}

// isLoopback reports whether the given host (IP literal or "localhost")
// refers to the loopback interface.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ApplyEnv overlays WLOG_* environment variables onto the config. Explicit
// flags should be applied after this so they take precedence; callers decide
// ordering. Unparseable values are ignored (best-effort overlay).
func (c *Config) ApplyEnv() {
	if v, ok := envInt("WLOG_OTLP_GRPC_PORT"); ok {
		c.OTLPGRPCPort = v
	}
	if v, ok := envInt("WLOG_OTLP_HTTP_PORT"); ok {
		c.OTLPHTTPPort = v
	}
	if v, ok := envInt("WLOG_UI_PORT"); ok {
		c.UIPort = v
	}
	if v := os.Getenv("WLOG_LISTEN"); v != "" {
		c.Listen = v
	}
	if v, ok := envBool("WLOG_UNSAFE"); ok {
		c.Unsafe = v
	}
	if v := os.Getenv("WLOG_AUTH_TOKEN"); v != "" {
		c.AuthToken = v
	}
	if v := os.Getenv("WLOG_DB"); v != "" {
		c.DBPath = v
	}
	if v, ok := envBool("WLOG_MEMORY"); ok {
		c.Memory = v
	}
	if v := os.Getenv("WLOG_RETENTION"); v != "" {
		if d, err := ParseRetention(v); err == nil {
			c.Retention = d
		}
	}
	if v, ok := envInt("WLOG_MAX_SESSIONS"); ok {
		c.MaxSessions = v
	}
	if v := os.Getenv("WLOG_MAX_RECV_BYTES"); v != "" {
		if b, err := ParseBytes(v); err == nil {
			c.MaxRecvBytes = b
		}
	}
	if v, ok := envBool("WLOG_NO_OPEN"); ok {
		c.NoOpen = v
	}
	if v, ok := envBool("WLOG_NO_STORE_PROMPTS"); ok {
		c.NoStorePrompts = v
	}
	if v, ok := envBool("WLOG_NO_JSONL"); ok {
		c.NoJSONL = v
	}
	if v := os.Getenv("WLOG_CLAUDE_DIR"); v != "" {
		c.ClaudeDir = v
	}
}

func envInt(key string) (int, bool) {
	v := os.Getenv(key)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, false
	}
	return n, true
}

func envBool(key string) (bool, bool) {
	v := os.Getenv(key)
	if v == "" {
		return false, false
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, false
	}
	return b, true
}

// ParseRetention parses a retention duration. It accepts Go durations
// (e.g. "72h") and a day suffix (e.g. "90d").
func ParseRetention(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty retention")
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid retention %q: %w", s, err)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid retention %q: %w", s, err)
	}
	return d, nil
}

// ParseBytes parses a byte size with optional KB/MB/GB (decimal) or
// KiB/MiB/GiB (binary) suffix; a bare number is bytes.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	upper := strings.ToUpper(s)
	type unit struct {
		suffix string
		mult   int64
	}
	// Order matters: longer suffixes first.
	units := []unit{
		{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(upper, u.suffix) {
			num := strings.TrimSpace(upper[:len(upper)-len(u.suffix)])
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid size %q: %w", s, err)
			}
			return int64(f * float64(u.mult)), nil
		}
	}
	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	return n, nil
}

// ClaudeSetupSnippet returns the Claude Code OTel environment snippet
// (DESIGN §6 format) pointing telemetry at the given gRPC port on localhost.
func ClaudeSetupSnippet(grpcPort int) string {
	return fmt.Sprintf(`# Claude Code OTel setup for wlog
export CLAUDE_CODE_ENABLE_TELEMETRY=1
export OTEL_METRICS_EXPORTER=otlp
export OTEL_LOGS_EXPORTER=otlp
export OTEL_EXPORTER_OTLP_PROTOCOL=grpc
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:%d
# then restart your shell and run Claude Code`, grpcPort)
}
