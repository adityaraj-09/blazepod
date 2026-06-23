// Layer: vm-agent — Rust in-VM exec service.
// Listens on AF_VSOCK port 8888 (production) or a Unix socket (VM_AGENT_DEV=1).
//
// Wire protocol: one newline-delimited JSON ExecRequest per connection,
// one ExecResponse JSON line back.

use std::io::{Read, Write};
use std::process::{Command, Stdio};
use std::time::Instant;

use serde::{Deserialize, Serialize};

const EXEC_PORT: u32 = 8888;

#[derive(Debug, Deserialize)]
struct ExecRequest {
    command: String,
    #[serde(default)]
    stdin: String,
    #[serde(default = "default_timeout")]
    timeout_ms: u64,
}

fn default_timeout() -> u64 {
    30_000
}

#[derive(Debug, Serialize)]
struct ExecResponse {
    stdout: String,
    stderr: String,
    exit_code: i32,
    duration_ms: i64,
}

fn main() {
    if std::env::var("VM_AGENT_DEV").is_ok() {
        run_unix_listener();
    } else {
        run_vsock_listener();
    }
}

#[cfg(target_os = "linux")]
fn run_vsock_listener() {
    use vsock::{VsockAddr, VsockListener, VMADDR_CID_ANY};

    let listener = VsockListener::bind(&VsockAddr::new(VMADDR_CID_ANY, EXEC_PORT))
        .expect("vm-agent: failed to bind AF_VSOCK");

    eprintln!("vm-agent: listening on vsock port {}", EXEC_PORT);

    for stream in listener.incoming() {
        match stream {
            Ok(conn) => {
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

#[cfg(not(target_os = "linux"))]
fn run_vsock_listener() {
    eprintln!("vm-agent: AF_VSOCK requires Linux; set VM_AGENT_DEV=1 for Unix socket mode");
    std::process::exit(1);
}

fn run_unix_listener() {
    use std::os::unix::net::UnixListener;

    let socket_path = std::env::var("VM_AGENT_SOCKET")
        .unwrap_or_else(|_| "/run/vm-agent.sock".to_string());

    let _ = std::fs::remove_file(&socket_path);

    let listener =
        UnixListener::bind(&socket_path).expect("vm-agent: failed to bind Unix socket");

    eprintln!("vm-agent: listening on {}", socket_path);

    for stream in listener.incoming() {
        match stream {
            Ok(conn) => {
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

fn handle_connection<C: Read + Write>(mut conn: C) -> Result<(), Box<dyn std::error::Error>> {
    let line = read_line(&mut conn)?;
    if line.is_empty() {
        return Ok(());
    }

    let req: ExecRequest = serde_json::from_str(&line)
        .map_err(|e| format!("vm-agent: decode ExecRequest: {}", e))?;

    eprintln!("vm-agent: exec {:?} timeout_ms={}", req.command, req.timeout_ms);

    let start = Instant::now();

    let mut child = Command::new("/bin/sh")
        .arg("-c")
        .arg(&req.command)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| format!("vm-agent: spawn: {}", e))?;

    if !req.stdin.is_empty() {
        if let Some(mut stdin) = child.stdin.take() {
            let _ = stdin.write_all(req.stdin.as_bytes());
        }
    }

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

fn read_line<R: Read>(reader: &mut R) -> Result<String, Box<dyn std::error::Error>> {
    let mut buf = Vec::new();
    let mut byte = [0u8; 1];
    loop {
        match reader.read(&mut byte) {
            Ok(0) => break,
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
