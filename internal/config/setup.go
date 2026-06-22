package config

// setup.go implements the `wlog setup` integration with Claude Code's
// settings.json (~/.claude/settings.json). It builds on the same merge
// primitives as InstallStatusLine (settings.go): load into a map of raw
// messages (preserving unknown keys), mutate only wlog-owned keys, then write
// atomically with a .bak backup. Everything here is idempotent and reversible.
//
// `wlog setup` writes three things into settings.json, in one atomic write:
//   1. The OTel env block (under the nested "env" object) so Claude Code emits
//      telemetry to wlog — the headline gap that --print-claude-setup only
//      printed before.
//   2. The status line command (reusing the existing statusLine value).
//   3. A SessionEnd hook that runs `wlog hook session-end` for a passive
//      end-of-session summary.
//
// RemoveSetup is the value-matched inverse: it removes ONLY the keys whose
// current value still equals what setup wrote, leaving user customizations and
// unrelated keys untouched.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SessionEndHookCommand is the command wlog registers as a Claude Code
// SessionEnd hook (the passive end-of-session summary).
const SessionEndHookCommand = "wlog hook session-end"

// OTel env keys wlog owns in settings.json's "env" object. Kept here as the
// single source of truth; ClaudeSetupSnippet (config.go) mirrors these values
// for the legacy shell form.
const (
	envEnableTelemetry = "CLAUDE_CODE_ENABLE_TELEMETRY"
	envMetricsExporter = "OTEL_METRICS_EXPORTER"
	envLogsExporter    = "OTEL_LOGS_EXPORTER"
	envOTLPProtocol    = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envOTLPEndpoint    = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envLogUserPrompts  = "OTEL_LOG_USER_PROMPTS"
)

// OTelEnv returns the wlog-owned Claude Code OTel environment block, pointing
// telemetry at the given gRPC port on loopback. When prompts is true it also
// enables OTEL_LOG_USER_PROMPTS (prompt-text capture) — off by default for
// privacy.
func OTelEnv(grpcPort int, prompts bool) map[string]string {
	if grpcPort == 0 {
		grpcPort = DefaultOTLPGRPCPort
	}
	env := map[string]string{
		envEnableTelemetry: "1",
		envMetricsExporter: "otlp",
		envLogsExporter:    "otlp",
		envOTLPProtocol:    "grpc",
		envOTLPEndpoint:    fmt.Sprintf("http://127.0.0.1:%d", grpcPort),
	}
	if prompts {
		env[envLogUserPrompts] = "1"
	}
	return env
}

// ClaudeSetupSnippetJSON returns the settings.json-native form of the OTel
// setup: a pretty-printed {"env": {...}} block that works identically on every
// OS (unlike the shell `export` form, which silently does nothing on
// PowerShell/cmd/fish). This is the same block `wlog setup` merges in.
func ClaudeSetupSnippetJSON(grpcPort int, prompts bool) string {
	doc := map[string]any{"env": OTelEnv(grpcPort, prompts)}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

// SetupOptions controls which integrations ApplySetup / RemoveSetup touch.
type SetupOptions struct {
	GRPCPort   int  // OTLP gRPC port the endpoint points at (0 -> default)
	Prompts    bool // also enable OTEL_LOG_USER_PROMPTS
	StatusLine bool // manage the statusLine key
	Hooks      bool // manage the SessionEnd hook
}

// SetupChange reports which parts ApplySetup / RemoveSetup actually modified.
type SetupChange struct {
	Env        bool
	StatusLine bool
	Hooks      bool
}

// Any reports whether anything changed.
func (c SetupChange) Any() bool { return c.Env || c.StatusLine || c.Hooks }

// ApplySetup merges the selected wlog integrations into settings.json at path in
// a single atomic write (with a .bak backup of any prior content), preserving
// every other key. It is idempotent: a second run with the same options is a
// no-op (SetupChange.Any()==false, no backup churn).
func ApplySetup(path string, opts SetupOptions) (SetupChange, error) {
	settings, existed, raw, err := loadSettings(path)
	if err != nil {
		return SetupChange{}, err
	}

	var ch SetupChange
	if ch.Env, err = mergeOTelEnv(settings, opts.GRPCPort, opts.Prompts); err != nil {
		return SetupChange{}, err
	}
	if opts.StatusLine {
		if ch.StatusLine, err = setStatusLineKey(settings); err != nil {
			return SetupChange{}, err
		}
	}
	if opts.Hooks {
		if ch.Hooks, err = addSessionEndHook(settings); err != nil {
			return SetupChange{}, err
		}
	}
	if !ch.Any() {
		return ch, nil
	}
	return ch, writeSettings(path, settings, existed, raw)
}

// RemoveSetup removes the wlog-owned keys from settings.json, but only when
// their current value still equals what setup wrote (so user customizations are
// preserved). It is idempotent: a no-op when nothing wlog-owned is present.
func RemoveSetup(path string, opts SetupOptions) (SetupChange, error) {
	settings, existed, raw, err := loadSettings(path)
	if err != nil {
		return SetupChange{}, err
	}
	if !existed {
		return SetupChange{}, nil
	}

	var ch SetupChange
	if ch.Env, err = removeOTelEnv(settings, opts.GRPCPort); err != nil {
		return SetupChange{}, err
	}
	// Gate teardown on the same flags as ApplySetup so `--uninstall --no-statusline`
	// / `--no-hooks` actually scope the removal (symmetry with install).
	if opts.StatusLine {
		if ch.StatusLine, err = removeStatusLineKey(settings); err != nil {
			return SetupChange{}, err
		}
	}
	if opts.Hooks {
		if ch.Hooks, err = removeSessionEndHook(settings); err != nil {
			return SetupChange{}, err
		}
	}
	if !ch.Any() {
		return ch, nil
	}
	return ch, writeSettings(path, settings, existed, raw)
}

// writeSettings backs up the prior content (when present) and writes the new
// settings atomically. Shared by ApplySetup / RemoveSetup.
func writeSettings(path string, settings map[string]json.RawMessage, existed bool, raw []byte) error {
	out, err := marshalSettings(settings)
	if err != nil {
		return err
	}
	if existed && len(raw) > 0 {
		if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
			return fmt.Errorf("back up %s: %w", path, err)
		}
	}
	return writeFileAtomic(path, out, 0o600)
}

// --- env merge / remove ----------------------------------------------------

// decodeObject decodes settings[key] into a map of raw messages, treating
// absent/null/empty as an empty object. A present-but-non-object value is an
// error (we refuse to silently clobber it).
func decodeObject(settings map[string]json.RawMessage, key string) (map[string]json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	raw, ok := settings[key]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return obj, nil
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("settings.json %q is not a JSON object; refusing to modify it: %w", key, err)
	}
	return obj, nil
}

func mergeOTelEnv(settings map[string]json.RawMessage, grpcPort int, prompts bool) (bool, error) {
	env, err := decodeObject(settings, "env")
	if err != nil {
		return false, err
	}
	changed := false
	for k, v := range OTelEnv(grpcPort, prompts) {
		want, _ := json.Marshal(v)
		if cur, ok := env[k]; !ok || !bytes.Equal(cur, want) {
			env[k] = want
			changed = true
		}
	}
	// Prompts is a toggle: when off, actively clear a value-matched
	// OTEL_LOG_USER_PROMPTS="1" so plain `wlog setup` turns prompt capture back
	// OFF (idempotent in both directions). A user-set non-"1" value is left alone.
	if !prompts {
		want, _ := json.Marshal("1")
		if cur, ok := env[envLogUserPrompts]; ok && bytes.Equal(cur, want) {
			delete(env, envLogUserPrompts)
			changed = true
		}
	}
	if changed {
		if len(env) == 0 {
			delete(settings, "env")
		} else {
			enc, err := json.Marshal(env)
			if err != nil {
				return false, fmt.Errorf("encode env: %w", err)
			}
			settings["env"] = enc
		}
	}
	return changed, nil
}

func removeOTelEnv(settings map[string]json.RawMessage, grpcPort int) (bool, error) {
	if _, ok := settings["env"]; !ok {
		return false, nil
	}
	env, err := decodeObject(settings, "env")
	if err != nil {
		return false, err
	}
	changed := false
	// Include the prompts key so uninstall cleans it whether or not it was set.
	for k, v := range OTelEnv(grpcPort, true) {
		want, _ := json.Marshal(v)
		if cur, ok := env[k]; ok && bytes.Equal(cur, want) {
			delete(env, k)
			changed = true
		}
	}
	if changed {
		if len(env) == 0 {
			delete(settings, "env")
		} else {
			enc, err := json.Marshal(env)
			if err != nil {
				return false, fmt.Errorf("encode env: %w", err)
			}
			settings["env"] = enc
		}
	}
	return changed, nil
}

// --- statusLine set / remove (value-matched) -------------------------------

func setStatusLineKey(settings map[string]json.RawMessage) (bool, error) {
	if cur, ok := settings["statusLine"]; ok {
		var have statusLineValue
		if json.Unmarshal(cur, &have) == nil && have == desiredStatusLine {
			return false, nil
		}
	}
	enc, err := json.Marshal(desiredStatusLine)
	if err != nil {
		return false, fmt.Errorf("encode statusLine: %w", err)
	}
	settings["statusLine"] = enc
	return true, nil
}

func removeStatusLineKey(settings map[string]json.RawMessage) (bool, error) {
	cur, ok := settings["statusLine"]
	if !ok {
		return false, nil
	}
	// Only remove OUR statusLine, and only when it has EXACTLY {type,command}
	// with our values. A user who added extra fields (e.g. Claude Code's
	// "padding") is treated as having customized it, so we leave it untouched —
	// deleting it would silently drop their customization.
	var obj map[string]json.RawMessage
	if json.Unmarshal(cur, &obj) != nil || len(obj) != 2 {
		return false, nil
	}
	var have statusLineValue
	if json.Unmarshal(cur, &have) == nil && have == desiredStatusLine {
		delete(settings, "statusLine")
		return true, nil
	}
	return false, nil
}

// --- SessionEnd hook add / remove ------------------------------------------

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type hookEntry struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []hookCommand `json:"hooks"`
}

// sessionEndEntries decodes hooks.SessionEnd as raw entries (preserving unknown
// fields verbatim) plus a typed view for detection. Returns the hooks object so
// callers can write back.
func sessionEndEntries(settings map[string]json.RawMessage) (hooks map[string]json.RawMessage, entries []json.RawMessage, err error) {
	hooks, err = decodeObject(settings, "hooks")
	if err != nil {
		return nil, nil, err
	}
	raw, ok := hooks["SessionEnd"]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return hooks, nil, nil
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, nil, fmt.Errorf("settings.json hooks.SessionEnd is not an array; refusing to modify it: %w", err)
	}
	return hooks, entries, nil
}

// entryHasWlog reports whether a raw SessionEnd entry contains our command.
func entryHasWlog(raw json.RawMessage) bool {
	var e hookEntry
	if json.Unmarshal(raw, &e) != nil {
		return false
	}
	for _, h := range e.Hooks {
		if h.Type == "command" && h.Command == SessionEndHookCommand {
			return true
		}
	}
	return false
}

func addSessionEndHook(settings map[string]json.RawMessage) (bool, error) {
	hooks, entries, err := sessionEndEntries(settings)
	if err != nil {
		return false, err
	}
	for _, re := range entries {
		if entryHasWlog(re) {
			return false, nil // already installed
		}
	}
	newEntry, err := json.Marshal(hookEntry{Hooks: []hookCommand{{Type: "command", Command: SessionEndHookCommand}}})
	if err != nil {
		return false, fmt.Errorf("encode SessionEnd hook: %w", err)
	}
	entries = append(entries, newEntry)
	encArr, err := json.Marshal(entries)
	if err != nil {
		return false, fmt.Errorf("encode SessionEnd entries: %w", err)
	}
	hooks["SessionEnd"] = encArr
	encHooks, err := json.Marshal(hooks)
	if err != nil {
		return false, fmt.Errorf("encode hooks: %w", err)
	}
	settings["hooks"] = encHooks
	return true, nil
}

// filterWlogFromEntry removes only wlog's command from a single SessionEnd entry,
// preserving the entry's other commands and any unknown fields (e.g. "matcher").
// It returns the rewritten entry, whether anything changed, and whether the entry
// became empty (drop=true) and should be removed. A non-object / malformed entry
// is left verbatim.
func filterWlogFromEntry(raw json.RawMessage) (out json.RawMessage, changed bool, drop bool) {
	var entry map[string]json.RawMessage
	if json.Unmarshal(raw, &entry) != nil {
		return raw, false, false
	}
	hraw, ok := entry["hooks"]
	if !ok {
		return raw, false, false
	}
	var cmds []json.RawMessage
	if json.Unmarshal(hraw, &cmds) != nil {
		return raw, false, false
	}
	kept := make([]json.RawMessage, 0, len(cmds))
	for _, c := range cmds {
		var hc hookCommand
		if json.Unmarshal(c, &hc) == nil && hc.Type == "command" && hc.Command == SessionEndHookCommand {
			changed = true
			continue
		}
		kept = append(kept, c)
	}
	if !changed {
		return raw, false, false
	}
	if len(kept) == 0 {
		return nil, true, true
	}
	enc, err := json.Marshal(kept)
	if err != nil {
		return raw, false, false
	}
	entry["hooks"] = enc
	newRaw, err := json.Marshal(entry)
	if err != nil {
		return raw, false, false
	}
	return newRaw, true, false
}

func removeSessionEndHook(settings map[string]json.RawMessage) (bool, error) {
	if _, ok := settings["hooks"]; !ok {
		return false, nil
	}
	hooks, entries, err := sessionEndEntries(settings)
	if err != nil {
		return false, err
	}
	kept := make([]json.RawMessage, 0, len(entries))
	changed := false
	for _, re := range entries {
		newRaw, ch, drop := filterWlogFromEntry(re)
		if ch {
			changed = true
		}
		if drop {
			continue // wlog was this entry's only command -> drop the now-empty entry
		}
		kept = append(kept, newRaw)
	}
	if !changed {
		return false, nil
	}
	if len(kept) == 0 {
		delete(hooks, "SessionEnd")
	} else {
		encArr, err := json.Marshal(kept)
		if err != nil {
			return false, fmt.Errorf("encode SessionEnd entries: %w", err)
		}
		hooks["SessionEnd"] = encArr
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		encHooks, err := json.Marshal(hooks)
		if err != nil {
			return false, fmt.Errorf("encode hooks: %w", err)
		}
		settings["hooks"] = encHooks
	}
	return true, nil
}

// --- inspection (the `wlog setup --status` doctor + first-run nudge) --------

// SetupInspection is the read-only view of how wlog is wired into settings.json,
// used by `wlog setup --status` and the first-run nudge. It never mutates.
type SetupInspection struct {
	SettingsExist  bool // settings.json present and parseable
	OTelConfigured bool // all wlog OTel keys present in env
	EndpointPort   int  // port parsed from OTEL_EXPORTER_OTLP_ENDPOINT (0 if absent)
	PortMatches    bool // EndpointPort == the wlog gRPC port
	ProtocolGRPC   bool // OTEL_EXPORTER_OTLP_PROTOCOL == "grpc"
	PromptsEnabled bool // OTEL_LOG_USER_PROMPTS == "1"
	StatusLine     bool // wlog statusLine registered
	SessionEndHook bool // wlog SessionEnd hook registered
}

// InspectSetup reports how settings.json at path is configured relative to a
// wlog gRPC port. A missing file is not an error (SettingsExist=false). A
// malformed file IS an error so the doctor can flag it.
func InspectSetup(path string, grpcPort int) (SetupInspection, error) {
	if grpcPort == 0 {
		grpcPort = DefaultOTLPGRPCPort
	}
	var ins SetupInspection
	settings, existed, _, err := loadSettings(path)
	if err != nil {
		return ins, err
	}
	ins.SettingsExist = existed
	if !existed {
		return ins, nil
	}

	env, err := decodeObject(settings, "env")
	if err != nil {
		return ins, err
	}
	got := func(k string) (string, bool) {
		raw, ok := env[k]
		if !ok {
			return "", false
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return s, true
	}
	tele, _ := got(envEnableTelemetry)
	_, hasMetrics := got(envMetricsExporter)
	_, hasLogs := got(envLogsExporter)
	endpoint, hasEndpoint := got(envOTLPEndpoint)
	ins.OTelConfigured = tele == "1" && hasMetrics && hasLogs && hasEndpoint
	if p, ok := got(envOTLPProtocol); ok && p == "grpc" {
		ins.ProtocolGRPC = true
	}
	if p, ok := got(envLogUserPrompts); ok && p == "1" {
		ins.PromptsEnabled = true
	}
	if hasEndpoint {
		ins.EndpointPort = portFromEndpoint(endpoint)
		ins.PortMatches = ins.EndpointPort == grpcPort
	}

	if cur, ok := settings["statusLine"]; ok {
		var have statusLineValue
		if json.Unmarshal(cur, &have) == nil && have == desiredStatusLine {
			ins.StatusLine = true
		}
	}
	if _, entries, err := sessionEndEntries(settings); err == nil {
		for _, re := range entries {
			if entryHasWlog(re) {
				ins.SessionEndHook = true
				break
			}
		}
	}
	return ins, nil
}

// portFromEndpoint extracts the port from an OTLP endpoint like
// "http://127.0.0.1:4317" (0 when it cannot be parsed).
func portFromEndpoint(endpoint string) int {
	i := strings.LastIndex(endpoint, ":")
	if i < 0 {
		return 0
	}
	tail := endpoint[i+1:]
	if j := strings.IndexAny(tail, "/?"); j >= 0 {
		tail = tail[:j]
	}
	n, err := strconv.Atoi(strings.TrimSpace(tail))
	if err != nil {
		return 0
	}
	return n
}

// OTelConfiguredForPort reports whether telemetry is wired to a wlog gRPC port,
// via EITHER the process environment (a live OTEL_EXPORTER_OTLP_ENDPOINT
// pointing at the port) OR settings.json. Used by the first-run nudge so it does
// not pester users who already configured wlog by any means. Best-effort: any
// error means "treat as not configured" (the nudge is harmless).
func OTelConfiguredForPort(grpcPort int) bool {
	if grpcPort == 0 {
		grpcPort = DefaultOTLPGRPCPort
	}
	if os.Getenv(envEnableTelemetry) == "1" {
		if portFromEndpoint(os.Getenv(envOTLPEndpoint)) == grpcPort {
			return true
		}
	}
	path, err := ClaudeSettingsPath()
	if err != nil {
		return false
	}
	ins, err := InspectSetup(path, grpcPort)
	if err != nil {
		return false
	}
	return ins.OTelConfigured && ins.PortMatches
}
