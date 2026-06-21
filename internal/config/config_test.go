package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestDefaultDBPath verifies the platform-specific default DB path resolves to a
// "wlog/wlog.db" under the expected base directory and that the parent dir is
// created (best-effort). It manipulates the relevant env vars and restores them.
func TestDefaultDBPath(t *testing.T) {
	got := DefaultDBPath()
	if filepath.Base(got) != "wlog.db" {
		t.Errorf("DefaultDBPath base = %q, want wlog.db", filepath.Base(got))
	}
	if filepath.Base(filepath.Dir(got)) != "wlog" {
		t.Errorf("DefaultDBPath parent dir = %q, want wlog", filepath.Base(filepath.Dir(got)))
	}
	// The parent directory should now exist (MkdirAll best-effort).
	if fi, err := os.Stat(filepath.Dir(got)); err != nil || !fi.IsDir() {
		t.Errorf("DefaultDBPath parent dir not created: %v", err)
	}
}

// TestDefaultDBPathLinuxXDG checks the XDG branch on linux (the only platform
// where we can deterministically force the base via XDG_DATA_HOME).
func TestDefaultDBPathLinuxXDG(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("XDG branch only deterministic on linux (GOOS=%s)", runtime.GOOS)
	}
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	got := DefaultDBPath()
	want := filepath.Join(tmp, "wlog", "wlog.db")
	if got != want {
		t.Errorf("DefaultDBPath() = %q, want %q", got, want)
	}
}

// TestDefaultDBPathWindowsLocalAppData checks the LOCALAPPDATA branch on Windows.
func TestDefaultDBPathWindowsLocalAppData(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skipf("LOCALAPPDATA branch only on windows (GOOS=%s)", runtime.GOOS)
	}
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	got := DefaultDBPath()
	want := filepath.Join(tmp, "wlog", "wlog.db")
	if got != want {
		t.Errorf("DefaultDBPath() = %q, want %q", got, want)
	}
}

// TestValidate covers the bind policy and range invariants.
func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
		errSub  string
	}{
		{
			name:    "defaults ok (loopback)",
			mutate:  func(c *Config) {},
			wantErr: false,
		},
		{
			name:    "non-loopback without unsafe or token rejected",
			mutate:  func(c *Config) { c.Listen = "0.0.0.0" },
			wantErr: true,
			errSub:  "non-loopback",
		},
		{
			name:    "non-loopback with unsafe allowed",
			mutate:  func(c *Config) { c.Listen = "0.0.0.0"; c.Unsafe = true },
			wantErr: false,
		},
		{
			name:    "non-loopback with auth-token allowed",
			mutate:  func(c *Config) { c.Listen = "192.168.1.5"; c.AuthToken = "secret" },
			wantErr: false,
		},
		{
			name:    "localhost is loopback",
			mutate:  func(c *Config) { c.Listen = "localhost" },
			wantErr: false,
		},
		{
			name:    "ipv6 loopback",
			mutate:  func(c *Config) { c.Listen = "::1" },
			wantErr: false,
		},
		{
			name:    "empty listen rejected",
			mutate:  func(c *Config) { c.Listen = "" },
			wantErr: true,
			errSub:  "listen",
		},
		{
			name:    "grpc port out of range",
			mutate:  func(c *Config) { c.OTLPGRPCPort = 70000 },
			wantErr: true,
			errSub:  "otlp-grpc-port",
		},
		{
			name:    "http port out of range",
			mutate:  func(c *Config) { c.OTLPHTTPPort = -1 },
			wantErr: true,
			errSub:  "otlp-http-port",
		},
		{
			name:    "ui port out of range",
			mutate:  func(c *Config) { c.UIPort = 99999 },
			wantErr: true,
			errSub:  "ui-port",
		},
		{
			name:    "grpc and http port collision rejected",
			mutate:  func(c *Config) { c.OTLPHTTPPort = c.OTLPGRPCPort },
			wantErr: true,
			errSub:  "must differ",
		},
		{
			name:    "negative retention rejected",
			mutate:  func(c *Config) { c.Retention = -time.Hour },
			wantErr: true,
			errSub:  "retention",
		},
		{
			name:    "negative max-sessions rejected",
			mutate:  func(c *Config) { c.MaxSessions = -1 },
			wantErr: true,
			errSub:  "max-sessions",
		},
		{
			name:    "non-positive max-recv-bytes rejected",
			mutate:  func(c *Config) { c.MaxRecvBytes = 0 },
			wantErr: true,
			errSub:  "max-recv-bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.mutate(c)
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.errSub)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tt.wantErr && tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("Validate() error = %q, want substring %q", err.Error(), tt.errSub)
			}
		})
	}
}

// TestApplyEnv verifies WLOG_* overlay onto a fresh config, including the parsed
// duration and byte-size forms and the boolean flags.
func TestApplyEnv(t *testing.T) {
	t.Setenv("WLOG_OTLP_GRPC_PORT", "5317")
	t.Setenv("WLOG_OTLP_HTTP_PORT", "5318")
	t.Setenv("WLOG_UI_PORT", "9000")
	t.Setenv("WLOG_LISTEN", "127.0.0.1")
	t.Setenv("WLOG_UNSAFE", "true")
	t.Setenv("WLOG_AUTH_TOKEN", "tok123")
	t.Setenv("WLOG_DB", "/tmp/custom.db")
	t.Setenv("WLOG_MEMORY", "true")
	t.Setenv("WLOG_RETENTION", "30d")
	t.Setenv("WLOG_MAX_SESSIONS", "250")
	t.Setenv("WLOG_MAX_RECV_BYTES", "32MiB")
	t.Setenv("WLOG_NO_OPEN", "true")
	t.Setenv("WLOG_NO_STORE_PROMPTS", "true")

	c := Default()
	c.ApplyEnv()

	if c.OTLPGRPCPort != 5317 {
		t.Errorf("OTLPGRPCPort = %d, want 5317", c.OTLPGRPCPort)
	}
	if c.OTLPHTTPPort != 5318 {
		t.Errorf("OTLPHTTPPort = %d, want 5318", c.OTLPHTTPPort)
	}
	if c.UIPort != 9000 {
		t.Errorf("UIPort = %d, want 9000", c.UIPort)
	}
	if c.Listen != "127.0.0.1" {
		t.Errorf("Listen = %q, want 127.0.0.1", c.Listen)
	}
	if !c.Unsafe {
		t.Error("Unsafe = false, want true")
	}
	if c.AuthToken != "tok123" {
		t.Errorf("AuthToken = %q, want tok123", c.AuthToken)
	}
	if c.DBPath != "/tmp/custom.db" {
		t.Errorf("DBPath = %q, want /tmp/custom.db", c.DBPath)
	}
	if !c.Memory {
		t.Error("Memory = false, want true")
	}
	if c.Retention != 30*24*time.Hour {
		t.Errorf("Retention = %v, want 720h", c.Retention)
	}
	if c.MaxSessions != 250 {
		t.Errorf("MaxSessions = %d, want 250", c.MaxSessions)
	}
	if c.MaxRecvBytes != 32*1024*1024 {
		t.Errorf("MaxRecvBytes = %d, want %d", c.MaxRecvBytes, 32*1024*1024)
	}
	if !c.NoOpen {
		t.Error("NoOpen = false, want true")
	}
	if !c.NoStorePrompts {
		t.Error("NoStorePrompts = false, want true")
	}
}

// TestApplyEnvIgnoresUnparseable verifies that malformed env values are silently
// ignored (best-effort overlay) and leave the defaults intact.
func TestApplyEnvIgnoresUnparseable(t *testing.T) {
	t.Setenv("WLOG_OTLP_GRPC_PORT", "not-a-number")
	t.Setenv("WLOG_MAX_SESSIONS", "xyz")
	t.Setenv("WLOG_RETENTION", "garbage")
	t.Setenv("WLOG_MAX_RECV_BYTES", "??")
	t.Setenv("WLOG_UNSAFE", "maybe")

	c := Default()
	c.ApplyEnv()

	if c.OTLPGRPCPort != DefaultOTLPGRPCPort {
		t.Errorf("OTLPGRPCPort = %d, want default %d", c.OTLPGRPCPort, DefaultOTLPGRPCPort)
	}
	if c.MaxSessions != DefaultMaxSessions {
		t.Errorf("MaxSessions = %d, want default %d", c.MaxSessions, DefaultMaxSessions)
	}
	if c.Retention != DefaultRetention {
		t.Errorf("Retention = %v, want default %v", c.Retention, DefaultRetention)
	}
	if c.MaxRecvBytes != DefaultMaxRecvBytes {
		t.Errorf("MaxRecvBytes = %d, want default %d", c.MaxRecvBytes, DefaultMaxRecvBytes)
	}
	if c.Unsafe {
		t.Error("Unsafe = true on unparseable, want default false")
	}
}

// TestParseRetention covers the day suffix, Go durations, fractional days, and
// error cases.
func TestParseRetention(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "90d", want: 90 * 24 * time.Hour},
		{in: "30d", want: 30 * 24 * time.Hour},
		{in: "1d", want: 24 * time.Hour},
		{in: "0.5d", want: 12 * time.Hour},
		{in: "30m", want: 30 * time.Minute},
		{in: "72h", want: 72 * time.Hour},
		{in: "1h30m", want: 90 * time.Minute},
		{in: " 7d ", want: 7 * 24 * time.Hour}, // trimmed
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "10x", wantErr: true},
		{in: "dd", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseRetention(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRetention(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRetention(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseRetention(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestParseBytes covers decimal (KB/MB/GB), binary (KiB/MiB/GiB), bare bytes,
// fractional values, case-insensitivity, and error cases.
func TestParseBytes(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "16MB", want: 16 * 1024 * 1024},
		{in: "16MiB", want: 16 * 1024 * 1024},
		{in: "32MiB", want: 32 * 1024 * 1024},
		{in: "1GB", want: 1 << 30},
		{in: "1GiB", want: 1 << 30},
		{in: "512KB", want: 512 * 1024},
		{in: "512KiB", want: 512 * 1024},
		{in: "1024", want: 1024}, // bare bytes
		{in: "2048B", want: 2048},
		{in: "1.5MB", want: int64(1.5 * float64(1<<20))},
		{in: "16mb", want: 16 * 1024 * 1024}, // case-insensitive
		{in: " 8MB ", want: 8 * 1024 * 1024}, // trimmed
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "MB", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseBytes(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseBytes(%q) = %d, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBytes(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseBytes(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestDefault sanity-checks the documented default values so a regression in the
// constants is caught.
func TestDefault(t *testing.T) {
	c := Default()
	if c.OTLPGRPCPort != DefaultOTLPGRPCPort || c.OTLPHTTPPort != DefaultOTLPHTTPPort {
		t.Errorf("default OTLP ports = %d/%d, want %d/%d",
			c.OTLPGRPCPort, c.OTLPHTTPPort, DefaultOTLPGRPCPort, DefaultOTLPHTTPPort)
	}
	if c.Listen != DefaultListen {
		t.Errorf("default Listen = %q, want %q", c.Listen, DefaultListen)
	}
	if c.Retention != DefaultRetention {
		t.Errorf("default Retention = %v, want %v", c.Retention, DefaultRetention)
	}
	if c.MaxSessions != DefaultMaxSessions {
		t.Errorf("default MaxSessions = %d, want %d", c.MaxSessions, DefaultMaxSessions)
	}
	if c.MaxRecvBytes != DefaultMaxRecvBytes {
		t.Errorf("default MaxRecvBytes = %d, want %d", c.MaxRecvBytes, DefaultMaxRecvBytes)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Default().Validate() = %v, want nil", err)
	}
}
