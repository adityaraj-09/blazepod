// Layer: vm-agent — Rust in-VM exec service.
// Runs inside every Firecracker microVM as PID 2 (after the init process).
// Listens on a vsock port (AF_VSOCK) for JSON ExecRequest messages from the host-agent.
// For each request it:
//   1. Spawns the command as a child process.
//   2. Collects stdout, stderr, exit code, and wall-clock duration.
//   3. Sends an ExecResponse JSON back over the same connection.
//
// Wire protocol: newline-delimited JSON (one ExecRequest per connection, one ExecResponse per connection).
// This keeps the protocol simple for Phase 1; Phase 2 adds streaming stdout/stderr chunks.
//
// vsock port: 8888 (matches vsock::ExecPort in the Go host-agent).
// vsock CID:  VMADDR_CID_ANY — listens for connections from any CID (host is CID 2).

use std::io::{Read, Write};
use std::os::unix::net::UnixListener;
use std::process::{Command, Stdio};
use std::time::Instant;

use serde::{Deserialize, Serialize};

// ExecRequest is the JSON command the host-agent sends over vsock.
#[derive(Debug, Deserialize)]
struct ExecRequest {
    /// Shell command to run inside the VM (passed to /bin/sh -c).
    command: String,
    /// Optional data written to the process stdin.
    #[serde(default)]
    stdin: String,
    /// Maximum execution time in milliseconds.
    #[serde(default = "default_timeout")]
    timeout_ms: u64,
}

fn default_timeout() -> u64 {
    30_000
}

// ExecResponse is the JSON result sent back to the host-agent.
#[derive(Debug, Serialize)]
struct ExecResponse {
    stdout: String,
    stderr: String,
    exit_code: i32,
    duration_ms: i64,
}

fn main() {
    // vsock listener setup.
    // On Linux with KVM, replace UnixListener with a proper AF_VSOCK listener.
    // For Phase 1 / development we bind a Unix socket at a known path so the
    // Go test harness can talk to the agent without KVM.
    //
    // Phase 2: use the vsock crate (https://crates.io/crates/vsock) to bind
    //   VsockListener::bind(VMADDR_CID_ANY, 8888) on a real Firecracker VM.
    let socket_path = std::env::var("VM_AGENT_SOCKET")
        .unwrap_or_else(|_| "/run/vm-agent.sock".to_string());

    // Remove stale socket from a previous run.
    let _ = std::fs::remove_file(&socket_path);

    let listener = UnixListener::bind(&socket_path).expect("vm-agent: failed to bind Unix socket");

    eprintln!("vm-agent: listening on {}", socket_path);

    for stream in listener.incoming() {
        match stream {
            Ok(conn) => {
                // Handle each connection synchronously in Phase 1.
                // Phase 2: spawn a thread per connection for concurrency.
                if let Err(e) = handle_connection(conn) {
                    eprintln!("vm-agent: connection error: {}", e);
                }
            }
            Err(e) => {
                eprintln!("vm-agent: accept error: {}", e);
            }
        }
    }
}

// handle_connection reads one ExecRequest, runs the command, and writes one ExecResponse.
// Takes ownership of the connection stream.
fn handle_connection<C: Read + Write>(mut conn: C) -> Result<(), Box<dyn std::error::Error>> {
    // Read the first newline-terminated line as the JSON ExecRequest.
    let line = read_line(&mut conn)?;
    if line.is_empty() {
        return Ok(());
    }

    let req: ExecRequest = serde_json::from_str(&line)
        .map_err(|e| format!("vm-agent: decode ExecRequest: {}", e))?;

    eprintln!("vm-agent: exec {:?} timeout_ms={}", req.command, req.timeout_ms);

    let start = Instant::now();

    // Spawn /bin/sh -c <command> to support full shell syntax.
    let mut child = Command::new("/bin/sh")
        .arg("-c")
        .arg(&req.command)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| format!("vm-agent: spawn: {}", e))?;

    // Write stdin if provided.
    if !req.stdin.is_empty() {
        if let Some(mut stdin) = child.stdin.take() {
            let _ = stdin.write_all(req.stdin.as_bytes());
        }
    }

    // Collect output (blocks until the child exits).
    let output = child
        .wait_with_output()
        .map_err(|e| format!("vm-agent: wait: {}", e))?;

    let duration_ms = start.elapsed().as_millis() as i64;
    let exit_code = output.status.code().unwrap_or(-1);

    let resp = ExecResponse {
        stdout: String::from_utf8_lossy(&output.stdout).to_string(),
        stderr: String::from_utf8_lossy(&output.stderr).to_string(),
        exit_code,
        duration_ms,
    };

    // Write the response as a single JSON line.
    let resp_json = serde_json::to_string(&resp)?;
    conn.write_all(resp_json.as_bytes())?;
    conn.write_all(b"\n")?;
    conn.flush()?;

    eprintln!(
        "vm-agent: done exit_code={} duration_ms={}",
        exit_code, duration_ms
    );
    Ok(())
}

// read_line reads bytes from the reader until '\n' and returns the line (without the newline).
fn read_line<R: Read>(reader: &mut R) -> Result<String, Box<dyn std::error::Error>> {
    let mut buf = Vec::new();
    let mut byte = [0u8; 1];
    loop {
        match reader.read(&mut byte) {
            Ok(0) => break, // EOF
            Ok(_) => {
                if byte[0] == b'\n' {
                    break;
                }
                buf.push(byte[0]);
            }
            Err(e) => return Err(Box::new(e)),
        }
    }
    Ok(String::from_utf8_lossy(&buf).to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;

    // MockConn wraps a Cursor for input and a Vec for output.
    struct MockConn {
        reader: Cursor<Vec<u8>>,
        writer: Vec<u8>,
    }

    impl MockConn {
        fn new(input: &str) -> Self {
            Self {
                reader: Cursor::new(input.as_bytes().to_vec()),
                writer: Vec::new(),
            }
        }

        fn output(&self) -> String {
            String::from_utf8_lossy(&self.writer).to_string()
        }
    }

    impl Read for MockConn {
        fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
            self.reader.read(buf)
        }
    }

    impl Write for MockConn {
        fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
            self.writer.write(buf)
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    fn run_exec(request_json: &str) -> serde_json::Value {
        let input = format!("{}\n", request_json);
        let mut conn = MockConn::new(&input);
        let _ = handle_connection(&mut conn);
        serde_json::from_str(&conn.output().trim_end()).unwrap()
    }

    #[test]
    fn test_echo_command() {
        let resp = run_exec(r#"{"command":"echo hello"}"#);
        assert_eq!(resp["stdout"].as_str().unwrap().trim(), "hello");
        assert_eq!(resp["exit_code"].as_i64().unwrap(), 0);
    }

    #[test]
    fn test_exit_code() {
        let resp = run_exec(r#"{"command":"exit 42"}"#);
        assert_eq!(resp["exit_code"].as_i64().unwrap(), 42);
    }

    #[test]
    fn test_stderr_capture() {
        let resp = run_exec(r#"{"command":"echo err >&2"}"#);
        assert_eq!(resp["stderr"].as_str().unwrap().trim(), "err");
    }

    #[test]
    fn test_duration_tracked() {
        let resp = run_exec(r#"{"command":"true"}"#);
        assert!(resp["duration_ms"].as_i64().unwrap() >= 0);
    }
}
