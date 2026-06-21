// Package deps pins the verified third-party dependencies that downstream
// batches (otel parser, store, receiver) will import, so that `go mod tidy`
// retains them in go.mod and teammates working in parallel worktrees do not
// hit module-graph conflicts.
//
// This file imports the packages solely for their build-graph effect. It can
// be deleted once every dependency below has a real importer in the tree.
package deps

import (
	// SQLite driver (pure Go, CGO-free) — store package.
	_ "modernc.org/sqlite"

	// OTLP wire types and collector service definitions — otel/receiver.
	_ "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	_ "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	_ "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	_ "go.opentelemetry.io/proto/otlp/common/v1"
	_ "go.opentelemetry.io/proto/otlp/logs/v1"
	_ "go.opentelemetry.io/proto/otlp/metrics/v1"
	_ "go.opentelemetry.io/proto/otlp/resource/v1"
	_ "go.opentelemetry.io/proto/otlp/trace/v1"

	// gRPC server runtime and codes — receiver package.
	_ "google.golang.org/grpc"
	_ "google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/status"

	// Protobuf runtime + protojson for OTLP/HTTP JSON — receiver package.
	_ "google.golang.org/protobuf/encoding/protojson"
	_ "google.golang.org/protobuf/proto"
)
