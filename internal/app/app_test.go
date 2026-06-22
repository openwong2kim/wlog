package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/model"
	"github.com/openwong2kim/wlog/internal/store"
)

// captureStdout redirects os.Stdout to a pipe for the duration of fn and returns
// everything written. A background scanner forwards lines to lineCh so callers
// can react to the startup banner before fn returns (Run blocks until ctx ends).
func captureStdout(t *testing.T, lineCh chan<- string, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	var collected strings.Builder
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text()
			collected.WriteString(line)
			collected.WriteByte('\n')
			if lineCh != nil {
				select {
				case lineCh <- line:
				default:
				}
			}
		}
		close(done)
	}()

	fn()

	_ = w.Close()
	os.Stdout = orig
	<-done
	return collected.String()
}

// uiPortRe extracts the UI port from the DESIGN §6 startup line:
//
//	wlog v0.1.0  otlp=grpc://127.0.0.1:4317,http://127.0.0.1:4318  ui=http://127.0.0.1:7890
var uiPortRe = regexp.MustCompile(`ui=http://[^:]+:(\d+)`)

func TestRun_HealthThenGracefulShutdown(t *testing.T) {
	cfg := config.Default()
	cfg.Memory = true
	cfg.NoOpen = true
	cfg.UIPort = 0           // auto-select UI port
	cfg.OTLPGRPCPort = 0     // auto-select to avoid conflicts with a real wlog / parallel test
	cfg.OTLPHTTPPort = 0     // auto-select
	cfg.Retention = time.Hour

	ctx, cancel := context.WithCancel(context.Background())

	lineCh := make(chan string, 8)
	runErrCh := make(chan error, 1)
	captureDone := make(chan struct{})

	go func() {
		_ = captureStdout(t, lineCh, func() {
			runErrCh <- Run(ctx, cfg)
		})
		close(captureDone)
	}()

	// Wait for the startup banner and pull the UI port from it.
	var uiPort string
	deadline := time.After(10 * time.Second)
	for uiPort == "" {
		select {
		case line := <-lineCh:
			if m := uiPortRe.FindStringSubmatch(line); m != nil {
				uiPort = m[1]
			}
		case <-deadline:
			t.Fatal("timed out waiting for startup banner")
		}
	}

	// Hit /api/health and assert a 200 with a sane status field.
	url := "http://127.0.0.1:" + uiPort + "/api/health"
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", url, resp.StatusCode)
	}
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("decode health: %v (body=%s)", err, body)
	}
	if health.Status == "" {
		t.Fatalf("health.status empty (body=%s)", body)
	}

	// Trigger graceful shutdown and assert Run returned without error.
	cancel()
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("Run returned error on graceful shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after context cancel (graceful shutdown stuck)")
	}

	// Wait for captureStdout to finish restoring the global os.Stdout before
	// returning. Run() sends on runErrCh from inside captureStdout's fn, i.e.
	// BEFORE captureStdout restores os.Stdout — so without this join the capture
	// goroutine is still writing os.Stdout when the next test reads it (-race).
	<-captureDone
}

func TestRun_UIPortConflictHint(t *testing.T) {
	// Occupy a UI port, then ask Run to bind it: expect a hint-bearing error.
	occupied := mustListen(t)
	defer occupied.Close()
	_, portStr, _ := splitHostPort(occupied.Addr().String())

	cfg := config.Default()
	cfg.Memory = true
	cfg.NoOpen = true
	cfg.OTLPGRPCPort = 0
	cfg.OTLPHTTPPort = 0
	cfg.UIPort = atoi(t, portStr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := captureStdoutErr(t, func() error { return Run(ctx, cfg) })
	if err == nil {
		t.Fatal("expected a bind error for an occupied UI port, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "hint:") || !strings.Contains(msg, "--ui-port") {
		t.Fatalf("error missing actionable hint: %q", msg)
	}
}

func TestRun_GRPCPortConflictHint(t *testing.T) {
	// Occupy the gRPC OTLP port; the receiver must surface a hint.
	occupied := mustListen(t)
	defer occupied.Close()
	_, portStr, _ := splitHostPort(occupied.Addr().String())

	cfg := config.Default()
	cfg.Memory = true
	cfg.NoOpen = true
	cfg.UIPort = 0
	cfg.OTLPHTTPPort = 0
	cfg.OTLPGRPCPort = atoi(t, portStr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := captureStdoutErr(t, func() error { return Run(ctx, cfg) })
	if err == nil {
		t.Fatal("expected a bind error for an occupied gRPC port, got nil")
	}
	if !strings.Contains(err.Error(), "hint:") {
		t.Fatalf("gRPC bind error missing hint: %q", err.Error())
	}
}

func TestExport_JSONStructureAndMissingSession(t *testing.T) {
	// Seed an in-memory DB via a store, then export it through app.Export by
	// pointing the config at the same shared in-memory database.
	st, err := store.Open("", true, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	const sid = "sess-export-1"
	batch := model.Batch{
		Sessions: []model.Session{{
			ID: sid, FirstSeen: 1000, LastSeen: 2000,
			AppVersion: "1.2.3", ModelSet: "claude-3", TokenSource: "events", HasEvents: true,
		}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 1500, Model: "claude-3",
			Tokens:   model.Tokens{Input: 100, Output: 50},
			CostUSD:  0.0031, DurationMS: 2100,
			DedupKey: model.DedupKey(sid, "api_request", "1500"),
		}},
		ToolResults: []model.ToolResult{{
			SessionID: sid, TS: 1600, ToolName: "Bash", Success: true,
			DurationMS: 42, BashCommand: "ls -la",
			DedupKey: model.DedupKey(sid, "tool_result", "1600"),
		}},
	}
	if err := st.WriteBatch(ctx, batch); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	// Use ExportSession directly against the same DB (Export opens its own store;
	// the shared in-memory cache is per-DSN, so we exercise the same code path the
	// CLI uses via the api package the binary links).
	out := exportToString(t, st, sid)

	var doc struct {
		Session struct {
			ID         string `json:"id"`
			AppVersion string `json:"app_version"`
			HasEvents  bool   `json:"has_events"`
		} `json:"session"`
		APIRequests   []map[string]any `json:"api_requests"`
		ToolResults   []map[string]any `json:"tool_results"`
		ToolDecisions []map[string]any `json:"tool_decisions"`
		Events        []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode export: %v (out=%s)", err, out)
	}
	if doc.Session.ID != sid {
		t.Fatalf("export session id = %q, want %q", doc.Session.ID, sid)
	}
	if doc.Session.AppVersion != "1.2.3" || !doc.Session.HasEvents {
		t.Fatalf("export session meta wrong: %+v", doc.Session)
	}
	if len(doc.APIRequests) != 1 || len(doc.ToolResults) != 1 {
		t.Fatalf("export rows wrong: api=%d tools=%d", len(doc.APIRequests), len(doc.ToolResults))
	}
	// Slices must be non-nil (serialize as [] not null).
	if doc.ToolDecisions == nil || doc.Events == nil {
		t.Fatal("empty slices must serialize as [] not null")
	}

	// Missing session must error clearly.
	cfg := config.Default()
	cfg.Memory = true
	if err := Export(ctx, cfg, "no-such-session", "json", ""); err == nil {
		t.Fatal("expected error exporting a missing session")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing-session error should mention not found: %q", err.Error())
	}

	// Unsupported format must error.
	if err := Export(ctx, cfg, sid, "csv", ""); err == nil {
		t.Fatal("expected error for unsupported format")
	} else if !strings.Contains(err.Error(), "json") {
		t.Fatalf("format error should mention json: %q", err.Error())
	}
}
