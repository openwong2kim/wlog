package api

import (
	"net/http"

	"github.com/openwong2kim/wlog/internal/config"
)

// setupPorts is the {grpc, http} block of the /api/setup response.
type setupPorts struct {
	GRPC int `json:"grpc"`
	HTTP int `json:"http"`
}

// setupResponse is the /api/setup body (PLAN §10): the Claude Code OTel env
// snippet plus the configured OTLP ports for the Onboarding screen. Env is the
// cross-platform settings.json-native form (additive; works on every OS, unlike
// the bash-only Snippet which is kept for back-compat).
type setupResponse struct {
	Snippet string            `json:"snippet"`
	Env     map[string]string `json:"env"`
	Ports   setupPorts        `json:"ports"`
}

// handleSetup returns the Claude Code OTel setup snippet (equivalent to
// --print-claude-setup), the settings.json-native env block, and the live OTLP
// ports.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	grpcPort := config.DefaultOTLPGRPCPort
	httpPort := config.DefaultOTLPHTTPPort
	if s.cfg != nil {
		grpcPort = s.cfg.OTLPGRPCPort
		httpPort = s.cfg.OTLPHTTPPort
	}
	writeJSON(w, http.StatusOK, setupResponse{
		Snippet: config.ClaudeSetupSnippet(grpcPort),
		Env:     config.OTelEnv(grpcPort, false),
		Ports:   setupPorts{GRPC: grpcPort, HTTP: httpPort},
	})
}
