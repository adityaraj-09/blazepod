// Layer: TypeScript SDK — main SandockClient class.
// This is what developers import and use in their applications.
// Mirrors the developer experience shown in the guide:
//
//   const sb = await client.sandboxes.create({ image: "python:3.12", ... })
//   const result = await sb.exec("python3 -c 'print(42)'")
//   console.log(result.stdout)  // "42\n"
//   await sb.kill()

import {
  ExecRequest,
  ExecResult,
  ListSandboxesResponse,
  Sandbox,
  SandboxSpec,
} from "./types.js";

/** Options for constructing a SandockClient. */
export interface ClientOptions {
  /** Base URL of the sandock API gateway (e.g. "https://api.sandock.dev"). */
  baseURL: string;
  /** API key / Bearer token for authentication. */
  apiKey: string;
  /** Optional request timeout in milliseconds. Defaults to 60000. */
  timeoutMs?: number;
}

/**
 * SandboxHandle is returned by client.sandboxes.create().
 * It provides convenience methods for exec, kill, and status checks.
 */
export class SandboxHandle {
  constructor(
    private readonly client: SandockClient,
    public readonly id: string,
    public readonly data: Sandbox
  ) {}

  /**
   * exec runs a shell command inside this sandbox and returns the result.
   * Blocks until the command completes or the timeout is reached.
   */
  async exec(command: string, opts?: { stdin?: string; timeoutMs?: number }): Promise<ExecResult> {
    return this.client.sandboxes.exec(this.id, {
      command,
      stdin: opts?.stdin,
      timeout_ms: opts?.timeoutMs,
    });
  }

  /** kill requests immediate termination of this sandbox. */
  async kill(): Promise<void> {
    return this.client.sandboxes.kill(this.id);
  }

  /** status fetches the latest sandbox state from the API. */
  async status(): Promise<Sandbox> {
    return this.client.sandboxes.get(this.id);
  }

  /** waitUntilRunning polls until the sandbox reaches the "running" state or times out. */
  async waitUntilRunning(pollIntervalMs = 200, maxWaitMs = 30_000): Promise<void> {
    const deadline = Date.now() + maxWaitMs;
    while (Date.now() < deadline) {
      const sb = await this.status();
      if (sb.state === "running") return;
      if (sb.state === "failed" || sb.state === "terminated") {
        throw new Error(`Sandbox ${this.id} entered ${sb.state}: ${sb.fail_reason ?? ""}`);
      }
      await sleep(pollIntervalMs);
    }
    throw new Error(`Sandbox ${this.id} did not reach running state within ${maxWaitMs}ms`);
  }
}

/**
 * SandockClient is the main entry point for the Sandock TypeScript SDK.
 *
 * @example
 * ```ts
 * const client = new SandockClient({ baseURL: "http://localhost:8080", apiKey: "mytoken" });
 * const sb = await client.sandboxes.create({ image: "base", cpu_millis: 500, memory_mib: 256, timeout_ms: 60_000 });
 * await sb.waitUntilRunning();
 * const result = await sb.exec("echo hello");
 * console.log(result.stdout); // "hello\n"
 * await sb.kill();
 * ```
 */
export class SandockClient {
  private readonly baseURL: string;
  private readonly apiKey: string;
  private readonly timeoutMs: number;

  constructor(opts: ClientOptions) {
    this.baseURL = opts.baseURL.replace(/\/$/, "");
    this.apiKey = opts.apiKey;
    this.timeoutMs = opts.timeoutMs ?? 60_000;
  }

  /** sandboxes provides all sandbox lifecycle operations. */
  readonly sandboxes = {
    /**
     * create submits a new sandbox request and returns a SandboxHandle.
     * The sandbox starts in "queued" state; call waitUntilRunning() before exec.
     */
    create: async (spec: SandboxSpec): Promise<SandboxHandle> => {
      const sb = await this.post<Sandbox>("/v1/sandboxes", spec);
      return new SandboxHandle(this, sb.id, sb);
    },

    /** get fetches the current state of a sandbox by ID. */
    get: async (id: string): Promise<Sandbox> => {
      return this.request<Sandbox>("GET", `/v1/sandboxes/${id}`);
    },

    /** list returns all sandboxes owned by the authenticated tenant. */
    list: async (): Promise<ListSandboxesResponse> => {
      return this.request<ListSandboxesResponse>("GET", "/v1/sandboxes");
    },

    /** kill requests immediate sandbox termination. */
    kill: async (id: string): Promise<void> => {
      await this.request<void>("DELETE", `/v1/sandboxes/${id}`);
    },

    /** exec runs a command inside a running sandbox. */
    exec: async (id: string, req: ExecRequest): Promise<ExecResult> => {
      return this.post<ExecResult>(`/v1/sandboxes/${id}/exec`, req);
    },
  };

  // ---------- HTTP helpers ----------

  private async post<T>(path: string, body: unknown): Promise<T> {
    return this.request<T>("POST", path, body);
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const url = `${this.baseURL}${path}`;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);

    try {
      const resp = await fetch(url, {
        method,
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${this.apiKey}`,
        },
        body: body !== undefined ? JSON.stringify(body) : undefined,
        signal: controller.signal,
      });

      if (resp.status === 204) return undefined as unknown as T;

      const json = await resp.json() as Record<string, unknown>;

      if (!resp.ok) {
        const msg = typeof json["error"] === "string" ? json["error"] : JSON.stringify(json);
        throw new SandockAPIError(resp.status, msg);
      }

      return json as T;
    } finally {
      clearTimeout(timer);
    }
  }
}

/** SandockAPIError is thrown when the API returns a non-2xx response. */
export class SandockAPIError extends Error {
  constructor(
    public readonly statusCode: number,
    message: string
  ) {
    super(`Sandock API error ${statusCode}: ${message}`);
    this.name = "SandockAPIError";
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
