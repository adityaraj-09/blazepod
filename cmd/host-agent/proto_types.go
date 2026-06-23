// Layer: host-agent — hand-written protobuf/gRPC type stubs.
// Build: Linux only — the host-agent binary targets Linux.
// These mirror the proto/sandock.proto definitions and act as the wire types
// until `make proto` is run to generate the real stubs from protoc.
// Replace this file entirely with the output of proto generation (proto/gen/).
package main

import (
	"context"

	"google.golang.org/grpc"
)

// ---------- Request / Response message types ----------

// PlaceRequest mirrors sandock.v1.PlaceRequest.
type PlaceRequest struct {
	SandboxId       string
	TenantId        string
	ImageRef        string
	CpuMillis       uint32
	MemMib          uint32
	TimeoutMs       uint32
	EgressAllowlist []string
	SnapshotKey     string
}

// PlaceResponse mirrors sandock.v1.PlaceResponse.
type PlaceResponse struct {
	SandboxId string
	VsockCid  uint32
}

// TerminateRequest mirrors sandock.v1.TerminateRequest.
type TerminateRequest struct {
	SandboxId string
	Reason    string
}

// TerminateResponse mirrors sandock.v1.TerminateResponse.
type TerminateResponse struct {
	SandboxId string
}

// ExecRequest mirrors sandock.v1.ExecRequest.
type ExecRequest struct {
	SandboxId string
	Command   string
	Stdin     string
	TimeoutMs uint32
}

// ExecResponse mirrors sandock.v1.ExecResponse.
type ExecResponse struct {
	Stdout     string
	Stderr     string
	ExitCode   int32
	DurationMs int64
}

// LogRequest mirrors sandock.v1.LogRequest.
type LogRequest struct {
	SandboxId string
}

// LogChunk mirrors sandock.v1.LogChunk.
type LogChunk struct {
	SandboxId string
	Line      string
	Timestamp int64
	Stream    string
}

// HeartbeatRequest mirrors sandock.v1.HeartbeatRequest.
type HeartbeatRequest struct {
	HostId string
}

// HeartbeatResponse mirrors sandock.v1.HeartbeatResponse.
type HeartbeatResponse struct {
	HostId        string
	ActiveVms     uint32
	IdlePoolSize  uint32
	CpuUsagePct   float32
	MemUsedMib    uint64
	MemTotalMib   uint64
}

// ---------- gRPC service interface and registration ----------

// HostAgentServer is the interface that must be implemented by the host-agent server.
type HostAgentServer interface {
	PlaceSandbox(context.Context, *PlaceRequest) (*PlaceResponse, error)
	TerminateSandbox(context.Context, *TerminateRequest) (*TerminateResponse, error)
	ExecInSandbox(context.Context, *ExecRequest) (*ExecResponse, error)
	StreamLogs(*LogRequest, HostAgent_StreamLogsServer) error
	Heartbeat(context.Context, *HeartbeatRequest) (*HeartbeatResponse, error)
}

// HostAgent_StreamLogsServer is the server-side streaming interface for StreamLogs.
type HostAgent_StreamLogsServer interface {
	Send(*LogChunk) error
	grpc.ServerStream
}

// RegisterHostAgentServer registers the HostAgentServer with a gRPC server.
// Replace with the protoc-generated RegisterHostAgentServer once proto gen runs.
func RegisterHostAgentServer(s *grpc.Server, srv HostAgentServer) {
	// In production this registers the service descriptor from protoc output.
	// Phase 1: no-op registration (gRPC server starts but handlers aren't reachable
	// via the standard proto service path until proto generation runs on Linux).
	_ = srv
}
