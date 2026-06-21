package api

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/openwong2kim/wlog/internal/version"
)

// healthResponse is the /api/health body (PLAN §10).
type healthResponse struct {
	Status    string          `json:"status"`
	Version   string          `json:"version"`
	UptimeSec int64           `json:"uptime_sec"`
	DB        healthDB        `json:"db"`
	Ingest    healthIngest    `json:"ingest"`
	Retention healthRetention `json:"retention"`
}

type healthDB struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

type healthIngest struct {
	Received          int64 `json:"received"`
	Accepted          int64 `json:"accepted"`
	RejectedPermanent int64 `json:"rejected_permanent"`
	RetrySignaled     int64 `json:"retry_signaled"`
	BusDropped        int64 `json:"bus_dropped"`
}

type healthRetention struct {
	Policy string `json:"policy"`
}

// handleHealth reports liveness, build version, uptime, DB path+size, ingest
// counters, and the retention policy.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	c := s.counters()

	resp := healthResponse{
		Status:    "ok",
		Version:   version.String(),
		UptimeSec: int64(time.Since(s.started).Seconds()),
		DB: healthDB{
			Path:      s.dbPath,
			SizeBytes: s.dbSizeBytes(),
		},
		Ingest: healthIngest{
			Received:          c.Received,
			Accepted:          c.Accepted,
			RejectedPermanent: c.RejectedPermanent,
			RetrySignaled:     c.RetrySignaled,
			BusDropped:        c.BusDropped,
		},
		Retention: healthRetention{Policy: s.retentionPolicy()},
	}
	writeJSON(w, http.StatusOK, resp)
}

// dbSizeBytes returns the on-disk database file size, or 0 for an in-memory /
// unknown path (memory DBs and a stat failure both report 0 rather than error).
func (s *Server) dbSizeBytes() int64 {
	if s.dbPath == "" || s.dbPath == ":memory:" {
		return 0
	}
	fi, err := os.Stat(s.dbPath)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// retentionPolicy formats the configured retention as a human-readable policy
// string, e.g. "90d or 1000 sessions". A memory DB has no retention.
func (s *Server) retentionPolicy() string {
	if s.cfg == nil {
		return ""
	}
	days := int(s.cfg.Retention.Hours() / 24)
	if s.cfg.Retention > 0 && s.cfg.MaxSessions > 0 {
		return fmt.Sprintf("%dd or %d sessions", days, s.cfg.MaxSessions)
	}
	if s.cfg.Retention > 0 {
		return fmt.Sprintf("%dd", days)
	}
	if s.cfg.MaxSessions > 0 {
		return fmt.Sprintf("%d sessions", s.cfg.MaxSessions)
	}
	return "none"
}
