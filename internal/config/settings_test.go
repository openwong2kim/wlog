package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// readSettings decodes a settings file into a generic map for assertions.
func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v (data=%s)", path, err, data)
	}
	return m
}

// assertStatusLine checks the statusLine object equals the wlog command form.
func assertStatusLine(t *testing.T, m map[string]any) {
	t.Helper()
	sl, ok := m["statusLine"].(map[string]any)
	if !ok {
		t.Fatalf("statusLine missing or not an object: %#v", m["statusLine"])
	}
	if sl["type"] != "command" || sl["command"] != StatuslineCommand {
		t.Fatalf("statusLine value wrong: %#v", sl)
	}
}

func TestInstallStatusLine_FreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")

	changed, err := InstallStatusLine(path)
	if err != nil {
		t.Fatalf("InstallStatusLine(fresh): %v", err)
	}
	if !changed {
		t.Fatal("fresh install should report changed=true")
	}
	assertStatusLine(t, readSettings(t, path))

	// No backup is created for a fresh file (nothing to lose).
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("fresh install should not create a .bak (err=%v)", err)
	}
}

func TestInstallStatusLine_PreservesExistingKeysAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// Pre-existing settings with unrelated keys (must be preserved) and a nested
	// object (must survive round-tripping through RawMessage).
	original := []byte(`{
  "model": "claude-sonnet-4",
  "permissions": {"allow": ["Read"], "deny": []},
  "env": {"FOO": "bar"}
}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}

	changed, err := InstallStatusLine(path)
	if err != nil {
		t.Fatalf("InstallStatusLine: %v", err)
	}
	if !changed {
		t.Fatal("install into a file without statusLine should report changed=true")
	}

	m := readSettings(t, path)
	assertStatusLine(t, m)
	// Unrelated top-level keys preserved.
	if m["model"] != "claude-sonnet-4" {
		t.Errorf("model key not preserved: %#v", m["model"])
	}
	perms, ok := m["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions not preserved as object: %#v", m["permissions"])
	}
	if allow, _ := perms["allow"].([]any); len(allow) != 1 || allow[0] != "Read" {
		t.Errorf("nested permissions.allow not preserved: %#v", perms["allow"])
	}
	envObj, ok := m["env"].(map[string]any)
	if !ok || envObj["FOO"] != "bar" {
		t.Errorf("env not preserved: %#v", m["env"])
	}

	// Backup created with the exact original bytes.
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(bak) != string(original) {
		t.Errorf("backup is not the verbatim original:\n got=%s\nwant=%s", bak, original)
	}
}

func TestInstallStatusLine_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// First install.
	if _, err := InstallStatusLine(path); err != nil {
		t.Fatalf("first install: %v", err)
	}
	// Remove any backup so we can detect whether the second run writes one.
	_ = os.Remove(path + ".bak")
	first := readSettings(t, path)

	// Second install must be a no-op: changed=false, no new backup, identical file.
	changed, err := InstallStatusLine(path)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if changed {
		t.Error("second install should report changed=false (idempotent)")
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("idempotent run should not rewrite/backup (bak exists: err=%v)", err)
	}
	second := readSettings(t, path)
	assertStatusLine(t, second)
	if len(first) != len(second) {
		t.Errorf("idempotent run changed key set: %v -> %v", first, second)
	}
}

func TestInstallStatusLine_OverwritesDifferentStatusLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// A pre-existing, different statusLine (e.g. another tool) is replaced.
	if err := os.WriteFile(path, []byte(`{"statusLine":{"type":"command","command":"other-tool"}}`), 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	changed, err := InstallStatusLine(path)
	if err != nil {
		t.Fatalf("InstallStatusLine: %v", err)
	}
	if !changed {
		t.Error("replacing a different statusLine should report changed=true")
	}
	assertStatusLine(t, readSettings(t, path))
}

func TestInstallStatusLine_EmptyFileTreatedAsObject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	if _, err := InstallStatusLine(path); err != nil {
		t.Fatalf("InstallStatusLine(empty file): %v", err)
	}
	assertStatusLine(t, readSettings(t, path))
}

func TestInstallStatusLine_RejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{ this is not json`), 0o600); err != nil {
		t.Fatalf("write malformed: %v", err)
	}
	if _, err := InstallStatusLine(path); err == nil {
		t.Fatal("malformed settings.json should error rather than silently clobber the user's file")
	}
}

func TestClaudeSettingsPath(t *testing.T) {
	// Force HOME/USERPROFILE to a temp dir so we never touch the real file.
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	}
	t.Setenv("HOME", tmp)

	got, err := ClaudeSettingsPath()
	if err != nil {
		t.Fatalf("ClaudeSettingsPath: %v", err)
	}
	want := filepath.Join(tmp, ".claude", "settings.json")
	if got != want {
		t.Errorf("ClaudeSettingsPath = %q, want %q", got, want)
	}
	// It must not create the file.
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("ClaudeSettingsPath must not create the file (err=%v)", err)
	}
}
