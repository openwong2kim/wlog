// Command replay is the wlog dogfood harness CLI: it synthesizes realistic
// Claude Code OTLP telemetry (via the synth package) and replays it at a running
// wlog receiver over gRPC or HTTP (PLAN §2 dogfood, §19 T9).
//
// Usage:
//
//	replay --endpoint grpc://127.0.0.1:4317   send synthetic OTLP over gRPC
//	replay --endpoint http://127.0.0.1:4318   send over OTLP/HTTP
//	replay --sessions 3 --temporality cumulative|delta --speed fast|real
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/openwong2kim/wlog/tools/replay/synth"
	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

func main() {
	var (
		endpoint    = flag.String("endpoint", "grpc://127.0.0.1:4317", "OTLP endpoint: grpc://host:port or http://host:port")
		sessions    = flag.Int("sessions", 1, "number of synthetic sessions to generate")
		temporality = flag.String("temporality", "cumulative", "metric temporality: cumulative|delta")
		speed       = flag.String("speed", "fast", "send speed: fast (no delay) | real (small inter-send delay)")
		prefix      = flag.String("prefix", "replay-sess-", "session id prefix")
		realtime    = flag.Bool("now", false, "anchor synthetic event times at the current wall clock (default: fixed epoch)")
		spreadDays  = flag.Int("spread-days", 0, "spread synthetic sessions across the last N local days (0 = single anchor). Produces a multi-day heatmap for demos.")
	)
	flag.Parse()

	temp := synth.Cumulative
	switch strings.ToLower(*temporality) {
	case "delta":
		temp = synth.Delta
	case "cumulative", "":
		temp = synth.Cumulative
	default:
		fatalf("invalid --temporality %q (want cumulative|delta)", *temporality)
	}

	scheme, addr, err := parseEndpoint(*endpoint)
	if err != nil {
		fatalf("%v", err)
	}

	delay := time.Duration(0)
	if strings.EqualFold(*speed, "real") {
		delay = 150 * time.Millisecond
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	send := func(b synth.Bundle) (int, error) {
		switch scheme {
		case "grpc":
			return sendGRPC(ctx, addr, b, delay)
		case "http":
			return sendHTTP(ctx, addr, b, delay)
		default:
			return 0, fmt.Errorf("unsupported endpoint scheme %q (want grpc:// or http://)", scheme)
		}
	}

	// Spread mode: synthesize sessions across the last N local days so the Daily
	// heatmap shows a realistic multi-day distribution (with gaps and varied
	// intensity) for demo screenshots. Single-anchor mode preserves the original
	// dogfood behavior.
	if *spreadDays > 0 {
		var totalSent, totalSessions int
		for d := 0; d < *spreadDays; d++ {
			// Deterministic 0..5 sessions/day: some days are empty (gaps), some
			// heavy (darker squares), giving the heatmap visual texture.
			n := (d*5 + 3) % 6
			if n == 0 {
				continue
			}
			y, mo, da := time.Now().AddDate(0, 0, -d).Date()
			dayBase := time.Date(y, mo, da, 12, 0, 0, 0, time.Now().Location())
			opts := synth.DefaultOptions()
			opts.Sessions = n
			opts.Temporality = temp
			opts.SessionIDPrefix = fmt.Sprintf("%sd%03d-", *prefix, d)
			opts.BaseTimeUnixNano = uint64(dayBase.UnixNano())
			s, err := send(synth.Generate(opts))
			if err != nil {
				fatalf("send (day -%d): %v", d, err)
			}
			totalSent += s
			totalSessions += n
		}
		fmt.Printf("replay: sent %d OTLP requests across %d days (%d sessions, %s) to %s://%s\n",
			totalSent, *spreadDays, totalSessions, temp, scheme, addr)
		return
	}

	opts := synth.DefaultOptions()
	opts.Sessions = *sessions
	opts.Temporality = temp
	opts.SessionIDPrefix = *prefix
	if *realtime {
		opts.BaseTimeUnixNano = uint64(time.Now().UnixNano())
	}

	bundle := synth.Generate(opts)
	sent, err := send(bundle)
	if err != nil {
		fatalf("send failed: %v", err)
	}

	fmt.Printf("replay: sent %d OTLP requests (%d sessions, %s) to %s://%s\n",
		sent, opts.Sessions, temp, scheme, addr)
	for _, t := range bundle.Totals {
		fmt.Printf("  session %s: cost=$%.4f tokens(in/out/cr/cc)=%d/%d/%d/%d accept=%d reject=%d active=%.0fs\n",
			t.SessionID, t.CostUSD, t.Input, t.Output, t.CacheRead, t.CacheCreation,
			t.ToolAccept, t.ToolReject, t.ActiveTimeSec)
	}
}

// parseEndpoint splits a grpc://host:port or http://host:port endpoint into its
// scheme and host:port.
func parseEndpoint(endpoint string) (scheme, addr string, err error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", fmt.Errorf("invalid --endpoint %q: %w", endpoint, err)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("invalid --endpoint %q: missing host:port", endpoint)
	}
	return strings.ToLower(u.Scheme), u.Host, nil
}

// sendGRPC dials the OTLP/gRPC endpoint and exports every request in the bundle.
// On backpressure (Unavailable) it retries so no data is lost.
func sendGRPC(ctx context.Context, addr string, b synth.Bundle, delay time.Duration) (int, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return 0, fmt.Errorf("dial grpc %s: %w", addr, err)
	}
	defer conn.Close()

	mc := collectormetricspb.NewMetricsServiceClient(conn)
	lc := collectorlogspb.NewLogsServiceClient(conn)
	tc := collectortracepb.NewTraceServiceClient(conn)

	sent := 0
	for _, req := range b.Metrics {
		if err := retry(ctx, func() error { _, e := mc.Export(ctx, req); return e }); err != nil {
			return sent, fmt.Errorf("export metrics: %w", err)
		}
		sent++
		sleep(ctx, delay)
	}
	for _, req := range b.Logs {
		if err := retry(ctx, func() error { _, e := lc.Export(ctx, req); return e }); err != nil {
			return sent, fmt.Errorf("export logs: %w", err)
		}
		sent++
		sleep(ctx, delay)
	}
	for _, req := range b.Traces {
		if err := retry(ctx, func() error { _, e := tc.Export(ctx, req); return e }); err != nil {
			return sent, fmt.Errorf("export traces: %w", err)
		}
		sent++
		sleep(ctx, delay)
	}
	return sent, nil
}

// sendHTTP posts every request to the OTLP/HTTP endpoints as x-protobuf,
// retrying on 503 (backpressure).
func sendHTTP(ctx context.Context, addr string, b synth.Bundle, delay time.Duration) (int, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	post := func(path string, msg proto.Message) error {
		body, err := proto.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		return retry(ctx, func() error {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				"http://"+addr+path, bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/x-protobuf")
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusServiceUnavailable {
				return errRetryable
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("%s: status %d", path, resp.StatusCode)
			}
			return nil
		})
	}

	sent := 0
	for _, req := range b.Metrics {
		if err := post("/v1/metrics", req); err != nil {
			return sent, fmt.Errorf("post metrics: %w", err)
		}
		sent++
		sleep(ctx, delay)
	}
	for _, req := range b.Logs {
		if err := post("/v1/logs", req); err != nil {
			return sent, fmt.Errorf("post logs: %w", err)
		}
		sent++
		sleep(ctx, delay)
	}
	for _, req := range b.Traces {
		if err := post("/v1/traces", req); err != nil {
			return sent, fmt.Errorf("post traces: %w", err)
		}
		sent++
		sleep(ctx, delay)
	}
	return sent, nil
}

// errRetryable signals transient backpressure that retry re-attempts.
var errRetryable = fmt.Errorf("retryable")

// retry runs fn up to a fixed number of attempts with backoff, treating gRPC
// Unavailable and errRetryable as retryable so the client never loses data.
func retry(ctx context.Context, fn func() error) error {
	const maxAttempts = 20
	backoff := 50 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !isRetryable(err) {
			return err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 500*time.Millisecond {
			backoff *= 2
		}
	}
	return fmt.Errorf("exhausted retries: %w", lastErr)
}

func isRetryable(err error) bool {
	if err == errRetryable {
		return true
	}
	return strings.Contains(err.Error(), "Unavailable") || strings.Contains(err.Error(), "queue full")
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "replay: error: "+format+"\n", args...)
	os.Exit(1)
}
