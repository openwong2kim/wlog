package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// envOf extracts the settings "env" object as a string map for assertions.
func envOf(t *testing.T, m map[string]any) map[string]string {
	t.Helper()
	raw, ok := m["env"].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func defaultSetupOpts() SetupOptions {
	return SetupOptions{GRPCPort: DefaultOTLPGRPCPort, StatusLine: true, Hooks: true}
}

func TestOTelEnv(t *testing.T) {
	env := OTelEnv(4317, false)
	if env[envEnableTelemetry] != "1" || env[envMetricsExporter] != "otlp" ||
		env[envLogsExporter] != "otlp" || env[envOTLPProtocol] != "grpc" {
		t.Fatalf("unexpected base env: %#v", env)
	}
	if env[envOTLPEndpoint] != "http://127.0.0.1:4317" {
		t.Errorf("endpoint = %q", env[envOTLPEndpoint])
	}
	if _, ok := env[envLogUserPrompts]; ok {
		t.Error("prompts key should be absent by default")
	}
	if OTelEnv(0, false)[envOTLPEndpoint] != "http://127.0.0.1:4317" {
		t.Error("port 0 should fall back to default")
	}
	if OTelEnv(9999, true)[envLogUserPrompts] != "1" {
		t.Error("prompts=true should set OTEL_LOG_USER_PROMPTS=1")
	}
	if OTelEnv(9999, false)[envOTLPEndpoint] != "http://127.0.0.1:9999" {
		t.Error("custom port not reflected")
	}
}

func TestClaudeSetupSnippetJSON(t *testing.T) {
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(ClaudeSetupSnippetJSON(4317, false)), &doc); err != nil {
		t.Fatalf("snippet is not valid JSON: %v", err)
	}
	if doc.Env[envOTLPEndpoint] != "http://127.0.0.1:4317" {
		t.Errorf("snippet env wrong: %#v", doc.Env)
	}
}

func TestApplySetup_FreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	ch, err := ApplySetup(path, defaultSetupOpts())
	if err != nil {
		t.Fatalf("ApplySetup(fresh): %v", err)
	}
	if !ch.Env || !ch.StatusLine || !ch.Hooks {
		t.Fatalf("fresh install should change all three: %#v", ch)
	}
	m := readSettings(t, path)
	assertStatusLine(t, m)
	env := envOf(t, m)
	if env[envEnableTelemetry] != "1" || env[envOTLPEndpoint] != "http://127.0.0.1:4317" {
		t.Errorf("env not written: %#v", env)
	}
	// SessionEnd hook present with our command.
	ins, err := InspectSetup(path, DefaultOTLPGRPCPort)
	if err != nil {
		t.Fatalf("InspectSetup: %v", err)
	}
	if !ins.OTelConfigured || !ins.PortMatches || !ins.StatusLine || !ins.SessionEndHook {
		t.Errorf("inspection after fresh setup: %#v", ins)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("fresh install should not create .bak (err=%v)", err)
	}
}

func TestApplySetup_PreservesAndBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(`{
  "model": "claude-sonnet-4",
  "env": {"FOO": "bar"},
  "hooks": {"PreToolUse": [{"matcher":"Bash","hooks":[{"type":"command","command":"echo hi"}]}]}
}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	if _, err := ApplySetup(path, defaultSetupOpts()); err != nil {
		t.Fatalf("ApplySetup: %v", err)
	}
	m := readSettings(t, path)
	if m["model"] != "claude-sonnet-4" {
		t.Errorf("model not preserved: %#v", m["model"])
	}
	env := envOf(t, m)
	if env["FOO"] != "bar" {
		t.Errorf("existing env key not preserved: %#v", env)
	}
	if env[envEnableTelemetry] != "1" {
		t.Errorf("wlog env not added: %#v", env)
	}
	// Existing PreToolUse hook preserved; SessionEnd added.
	hooks, _ := m["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Errorf("existing PreToolUse hook dropped: %#v", hooks)
	}
	if _, ok := hooks["SessionEnd"]; !ok {
		t.Errorf("SessionEnd hook not added: %#v", hooks)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil || string(bak) != string(original) {
		t.Errorf("backup not verbatim original (err=%v)", err)
	}
}

func TestApplySetup_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, err := ApplySetup(path, defaultSetupOpts()); err != nil {
		t.Fatalf("first: %v", err)
	}
	_ = os.Remove(path + ".bak")
	ch, err := ApplySetup(path, defaultSetupOpts())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if ch.Any() {
		t.Errorf("second run should be a no-op: %#v", ch)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("idempotent run should not write .bak")
	}
}

func TestApplySetup_EnvNotObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"env":"oops"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ApplySetup(path, defaultSetupOpts()); err == nil {
		t.Fatal("env as a non-object should error, not silently overwrite")
	}
}

func TestApplySetup_FlagsAndPrompts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	opts := SetupOptions{GRPCPort: 9000, Prompts: true, StatusLine: false, Hooks: false}
	ch, err := ApplySetup(path, opts)
	if err != nil {
		t.Fatalf("ApplySetup: %v", err)
	}
	if !ch.Env || ch.StatusLine || ch.Hooks {
		t.Fatalf("only env should change: %#v", ch)
	}
	env := envOf(t, readSettings(t, path))
	if env[envOTLPEndpoint] != "http://127.0.0.1:9000" {
		t.Errorf("custom port not written: %#v", env)
	}
	if env[envLogUserPrompts] != "1" {
		t.Errorf("prompts not written: %#v", env)
	}
	ins, _ := InspectSetup(path, 9000)
	if ins.StatusLine || ins.SessionEndHook {
		t.Errorf("statusLine/hook should be absent: %#v", ins)
	}
	if !ins.PromptsEnabled {
		t.Errorf("prompts should be detected on")
	}
}

func TestRemoveSetup_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(`{"model":"x","env":{"FOO":"bar"}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ApplySetup(path, defaultSetupOpts()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ch, err := RemoveSetup(path, defaultSetupOpts())
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !ch.Env || !ch.StatusLine || !ch.Hooks {
		t.Fatalf("remove should report all three: %#v", ch)
	}
	m := readSettings(t, path)
	if m["model"] != "x" {
		t.Errorf("user key dropped: %#v", m)
	}
	env := envOf(t, m)
	if env["FOO"] != "bar" {
		t.Errorf("user env key dropped: %#v", env)
	}
	if _, ok := env[envEnableTelemetry]; ok {
		t.Errorf("wlog env not removed: %#v", env)
	}
	if _, ok := m["statusLine"]; ok {
		t.Errorf("statusLine not removed")
	}
	if _, ok := m["hooks"]; ok {
		t.Errorf("empty hooks object should be removed")
	}
}

func TestRemoveSetup_PreservesUserModified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, err := ApplySetup(path, defaultSetupOpts()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// User changes the endpoint value + the statusLine.
	m := readSettings(t, path)
	env := m["env"].(map[string]any)
	env[envOTLPEndpoint] = "http://example.com:9999"
	m["statusLine"] = map[string]any{"type": "command", "command": "other-tool"}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if _, err := RemoveSetup(path, defaultSetupOpts()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	m = readSettings(t, path)
	env = m["env"].(map[string]any)
	if env[envOTLPEndpoint] != "http://example.com:9999" {
		t.Errorf("user-modified endpoint should be preserved: %#v", env[envOTLPEndpoint])
	}
	sl, _ := m["statusLine"].(map[string]any)
	if sl["command"] != "other-tool" {
		t.Errorf("user-modified statusLine should be preserved: %#v", sl)
	}
	// Constant wlog keys (value-matched) were still removed.
	if _, ok := env[envEnableTelemetry]; ok {
		t.Errorf("value-matched wlog key should be removed: %#v", env)
	}
}

func TestRemoveSetup_NoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"model":"x"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ch, err := RemoveSetup(path, defaultSetupOpts())
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if ch.Any() {
		t.Errorf("remove with nothing wlog-owned should be a no-op: %#v", ch)
	}
}

func TestInspectSetup_MissingAndMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// Missing file: not an error.
	ins, err := InspectSetup(path, DefaultOTLPGRPCPort)
	if err != nil {
		t.Fatalf("InspectSetup(missing): %v", err)
	}
	if ins.SettingsExist || ins.OTelConfigured {
		t.Errorf("missing file should report nothing configured: %#v", ins)
	}

	// Configured for port 4317, inspected against 5000 -> mismatch.
	if _, err := ApplySetup(path, defaultSetupOpts()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ins, err = InspectSetup(path, 5000)
	if err != nil {
		t.Fatalf("InspectSetup: %v", err)
	}
	if !ins.OTelConfigured {
		t.Error("should detect OTel configured")
	}
	if ins.PortMatches {
		t.Error("port 4317 vs 5000 should not match")
	}
	if ins.EndpointPort != 4317 {
		t.Errorf("EndpointPort = %d, want 4317", ins.EndpointPort)
	}
}

func TestApplySetup_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, err := ApplySetup(path, defaultSetupOpts()); err != nil {
		t.Fatalf("ApplySetup: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("settings.json mode = %o, want 600", perm)
	}
}

// TestApplySetup_HooksShape pins the exact Claude Code SessionEnd shape:
// hooks.SessionEnd is an array of {hooks: [{type:"command", command:...}]}.
func TestApplySetup_HooksShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, err := ApplySetup(path, defaultSetupOpts()); err != nil {
		t.Fatalf("ApplySetup: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc struct {
		Hooks struct {
			SessionEnd []struct {
				Hooks []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"SessionEnd"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Hooks.SessionEnd) != 1 || len(doc.Hooks.SessionEnd[0].Hooks) != 1 {
		t.Fatalf("unexpected SessionEnd shape: %+v", doc.Hooks.SessionEnd)
	}
	h := doc.Hooks.SessionEnd[0].Hooks[0]
	if h.Type != "command" || h.Command != SessionEndHookCommand {
		t.Errorf("hook = %+v, want {command, %q}", h, SessionEndHookCommand)
	}
}

// TestApplySetup_PromptsToggleOff verifies plain `wlog setup` turns prompt
// capture back OFF after a prior `--prompts` run (idempotent both directions).
func TestApplySetup_PromptsToggleOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, err := ApplySetup(path, SetupOptions{GRPCPort: DefaultOTLPGRPCPort, Prompts: true, StatusLine: true, Hooks: true}); err != nil {
		t.Fatalf("setup --prompts: %v", err)
	}
	if envOf(t, readSettings(t, path))[envLogUserPrompts] != "1" {
		t.Fatal("prompts should be ON after --prompts")
	}
	ch, err := ApplySetup(path, defaultSetupOpts())
	if err != nil {
		t.Fatalf("setup (prompts off): %v", err)
	}
	if !ch.Env {
		t.Error("turning prompts off should report an env change")
	}
	if _, ok := envOf(t, readSettings(t, path))[envLogUserPrompts]; ok {
		t.Error("OTEL_LOG_USER_PROMPTS should be removed when prompts is off")
	}
}

// TestRemoveSetup_PreservesSiblingHookCommand: uninstall removes only wlog's
// command from a SessionEnd entry, keeping a co-located user command.
func TestRemoveSetup_PreservesSiblingHookCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := `{"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":"user-tool"},{"type":"command","command":"wlog hook session-end"}]}]}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := RemoveSetup(path, defaultSetupOpts()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	s := string(mustRead(t, path))
	if !strings.Contains(s, "user-tool") {
		t.Errorf("sibling user command was dropped: %s", s)
	}
	if strings.Contains(s, SessionEndHookCommand) {
		t.Errorf("wlog command was not removed: %s", s)
	}
}

// TestRemoveSetup_PreservesCustomizedStatusLine: a statusLine with extra fields
// (e.g. Claude Code's "padding") is user-customized and must NOT be removed.
func TestRemoveSetup_PreservesCustomizedStatusLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := `{"statusLine":{"type":"command","command":"wlog statusline","padding":0}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ch, err := RemoveSetup(path, defaultSetupOpts())
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if ch.StatusLine {
		t.Error("a customized statusLine (extra keys) should not be removed")
	}
	if _, ok := readSettings(t, path)["statusLine"]; !ok {
		t.Error("customized statusLine should be preserved")
	}
}

// TestRemoveSetup_RespectsFlags: --uninstall --no-statusline --no-hooks scopes
// the teardown to env only (symmetry with ApplySetup).
func TestRemoveSetup_RespectsFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, err := ApplySetup(path, defaultSetupOpts()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ch, err := RemoveSetup(path, SetupOptions{GRPCPort: DefaultOTLPGRPCPort, StatusLine: false, Hooks: false})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if ch.StatusLine || ch.Hooks {
		t.Errorf("--no-statusline/--no-hooks should scope teardown: %#v", ch)
	}
	m := readSettings(t, path)
	if _, ok := m["statusLine"]; !ok {
		t.Error("statusLine should be preserved with --no-statusline")
	}
	if _, ok := m["hooks"]; !ok {
		t.Error("hooks should be preserved with --no-hooks")
	}
	if _, ok := envOf(t, m)[envEnableTelemetry]; ok {
		t.Error("env should still be removed")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
