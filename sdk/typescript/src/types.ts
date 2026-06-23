// Layer: TypeScript SDK — wire types matching the Go API spec types.
// These interfaces are the contract between the TypeScript SDK and cmd/api.
// Keep them in sync with internal/spec/spec.go.

/** Specification for creating a new sandbox. */
export interface SandboxSpec {
  /** OCI image to run. Phase 1: only "base" is supported. */
  image: string;
  /** CPU allocation in thousandths of a CPU. 500 = 0.5 CPU. */
  cpu_millis: number;
  /** Memory limit in mebibytes. */
  memory_mib: number;
  /** Maximum sandbox lifetime in milliseconds. */
  timeout_ms: number;
  /** Optional list of allowed egress hostnames/CIDRs (Phase 2 enforcement). */
  egress_allowlist?: string[];
}

/** Full sandbox record returned by the API. */
export interface Sandbox {
  id: string;
  tenant_id: string;
  state: SandboxState;
  host_id?: string;
  created_at: string;
  updated_at: string;
  fail_reason?: string;
}

/** All valid sandbox lifecycle states. */
export type SandboxState =
  | "queued"
  | "provisioning"
  | "running"
  | "draining"
  | "terminated"
  | "failed";

/** Request body for executing a command inside a sandbox. */
export interface ExecRequest {
  /** Shell command to run (passed to /bin/sh -c inside the VM). */
  command: string;
  /** Optional stdin data. */
  stdin?: string;
  /** Command execution timeout in milliseconds. Defaults to 30000. */
  timeout_ms?: number;
}

/** Result of a completed exec command. */
export interface ExecResult {
  stdout: string;
  stderr: string;
  exit_code: number;
  duration_ms: number;
}

/** Response from GET /v1/sandboxes. */
export interface ListSandboxesResponse {
  sandboxes: Sandbox[];
  count: number;
}
