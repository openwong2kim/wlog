package config

// settings.go manages the Claude Code settings.json integration for
// `wlog statusline --install` (PLAN v3 §2). Claude Code reads a per-user
// settings file at ~/.claude/settings.json (Windows %USERPROFILE%\.claude\
// settings.json); a "statusLine" object of the form
//
//	{"type": "command", "command": "wlog statusline"}
//
// tells it to run our command for the status bar. InstallStatusLine merges that
// key in while preserving every other setting, backing up the existing file
// first, and is idempotent (a no-op when already configured).
//
// JSON is handled with encoding/json only (no third-party deps). Unknown keys
// are preserved by decoding the document into a map of raw messages and writing
// it back, so wlog never drops a user's other settings.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// StatuslineCommand is the command wlog registers as Claude Code's status line.
const StatuslineCommand = "wlog statusline"

// statusLineValue is the JSON object written under the "statusLine" key.
type statusLineValue struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// desiredStatusLine is the value InstallStatusLine writes (and compares against
// for idempotency).
var desiredStatusLine = statusLineValue{Type: "command", Command: StatuslineCommand}

// ClaudeSettingsPath returns the path to the Claude Code user settings file:
// ~/.claude/settings.json (Windows %USERPROFILE%\.claude\settings.json). It does
// not create the file or its directory; InstallStatusLine creates the directory
// when needed. An error is returned only when the home directory cannot be
// resolved.
func ClaudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot determine home directory for ~/.claude/settings.json: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// InstallStatusLine merges the wlog status-line command into the settings.json at
// path, preserving all other keys. It reports whether it changed anything:
//
//   - When the file is absent it is created (with just the statusLine key) and
//     changed=true.
//   - When the file exists but lacks the key (or has a different value) the key
//     is set, the original is backed up to "<path>.bak", and changed=true.
//   - When the key already equals the desired value nothing is written (no backup
//     churn) and changed=false.
//
// The directory is created as needed (0700). The file is written atomically via a
// temp file + rename so a crash mid-write cannot corrupt the user's settings.
func InstallStatusLine(path string) (bool, error) {
	settings, existed, raw, err := loadSettings(path)
	if err != nil {
		return false, err
	}

	// Idempotency: if statusLine already equals the desired value, do nothing.
	if cur, ok := settings["statusLine"]; ok {
		var have statusLineValue
		if json.Unmarshal(cur, &have) == nil && have == desiredStatusLine {
			return false, nil
		}
	}

	// Set / overwrite the statusLine key, leaving every other key untouched.
	desired, err := json.Marshal(desiredStatusLine)
	if err != nil {
		return false, fmt.Errorf("encode statusLine value: %w", err)
	}
	settings["statusLine"] = desired

	out, err := marshalSettings(settings)
	if err != nil {
		return false, err
	}

	// Back up the previous file before replacing it (only when it existed and had
	// content — there is nothing to lose for a fresh file).
	if existed && len(raw) > 0 {
		if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
			return false, fmt.Errorf("back up %s: %w", path, err)
		}
	}

	if err := writeFileAtomic(path, out, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// loadSettings reads and decodes the settings file into an ordered-insensitive
// map of raw messages. A missing file yields an empty map with existed=false and
// no error (a fresh install). The raw bytes are returned too so the caller can
// back them up verbatim.
func loadSettings(path string) (settings map[string]json.RawMessage, existed bool, raw []byte, err error) {
	raw, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, false, nil, nil
	}
	if err != nil {
		return nil, false, nil, fmt.Errorf("read %s: %w", path, err)
	}
	// An empty (but present) file is treated as an empty object.
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]json.RawMessage{}, true, raw, nil
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, true, raw, fmt.Errorf("parse %s (not a JSON object?): %w", path, err)
	}
	if settings == nil {
		settings = map[string]json.RawMessage{}
	}
	return settings, true, raw, nil
}

// marshalSettings encodes the settings map as indented JSON with a trailing
// newline (matching how editors / Claude Code tend to store the file). SetEscape
// HTML is disabled so values like commands are not escaped to < etc.
func marshalSettings(settings map[string]json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	return buf.Bytes(), nil
}

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by a rename, creating the directory when needed. The rename is atomic
// on the same filesystem, so a reader never sees a half-written settings file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp settings file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp settings file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		// Non-fatal on platforms that ignore chmod (e.g. Windows); continue.
		_ = err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp settings file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install settings file %s: %w", path, err)
	}
	return nil
}
