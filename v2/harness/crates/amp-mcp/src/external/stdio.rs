//! Local MCP servers over stdio (#2236, H3; D6/D7 of decisions/amp-harness-configurability.md).
//!
//! - The child gets exactly [`StdioSpec::env`]: the inherited environment is cleared first, so
//!   whatever amp-core left out (AMP_TOKEN, provider keys, host plumbing) never reaches it.
//! - stdin/stdout are the JSON-RPC transport. stderr is pumped into [`StdioSpec::stderr_log`]
//!   (rotated at [`STDERR_LOG_MAX_BYTES`]); it never reaches the TTY. rmcp's own builder would
//!   inherit it, which scribbles over the chat.
//! - On Unix the child leads a new session (`setsid`), so it has no controlling terminal (no
//!   `/dev/tty` prompts, no SIGINT from the person's Ctrl-C) and its process group is its own.
//!   Shutdown and drop kill that whole group, so `npx`/`uvx` wrappers and their children go too.
//! - If the harness itself is killed (SIGKILL), nothing can run: the server sees EOF on stdin,
//!   which ends every conforming stdio server (the MCP spec's shutdown for stdio).

use std::{
    collections::BTreeMap,
    fmt,
    io::Write as _,
    path::{Path, PathBuf},
    process::Stdio,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use process_wrap::tokio::{ChildWrapper, CommandWrap};
use tokio::{io::AsyncReadExt as _, process::ChildStderr, task::JoinHandle};

use super::{ExternalClient, ExternalOptions};
use crate::McpError;

/// A stderr log is rotated (to `<name>.log.1`) when it reaches this size (D7: 5 MB).
pub const STDERR_LOG_MAX_BYTES: u64 = 5 * 1024 * 1024;
/// How long a stdio server gets to exit after stdin closes before its group is killed.
pub const SHUTDOWN_GRACE: Duration = Duration::from_secs(3);

/// `<state>/mcp-logs/<server>.log` (D7). Worktree-scoped instances pass their own file name.
pub fn stderr_log_path(state_dir: &Path, server: &str) -> PathBuf {
    state_dir.join("mcp-logs").join(format!("{server}.log"))
}

/// Everything needed to start one stdio server. amp-core builds it from `[mcp.<name>]`.
#[derive(Clone)]
pub struct StdioSpec {
    /// The configured server name, for errors, logs and [`ExternalClient::name`].
    pub name: String,
    /// The program: a name looked up on `env["PATH"]`, or a path.
    pub command: String,
    pub args: Vec<String>,
    /// The child's complete environment (nothing else is inherited). May hold secret values.
    pub env: BTreeMap<String, String>,
    pub cwd: PathBuf,
    pub stderr_log: PathBuf,
    pub options: ExternalOptions,
}

// Args and env values may hold expanded `${env:..}` or keychain values: names only.
impl fmt::Debug for StdioSpec {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("StdioSpec")
            .field("name", &self.name)
            .field("command", &self.command)
            .field("args", &format_args!("[{} args]", self.args.len()))
            .field("env", &self.env.keys().collect::<Vec<_>>())
            .field("cwd", &self.cwd)
            .field("stderr_log", &self.stderr_log)
            .field("options", &self.options)
            .finish()
    }
}

/// Renames a full log to `<file>.1` (replacing an older one). Returns whether it rotated.
pub fn rotate_log_if_full(path: &Path, max: u64) -> std::io::Result<bool> {
    match std::fs::metadata(path) {
        Ok(m) if m.len() >= max => {
            std::fs::rename(path, rotated(path))?;
            Ok(true)
        }
        Ok(_) => Ok(false),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(e) => Err(e),
    }
}

fn rotated(path: &Path) -> PathBuf {
    let mut name = path.file_name().unwrap_or_default().to_os_string();
    name.push(".1");
    path.with_file_name(name)
}

fn open_log(path: &Path) -> std::io::Result<(std::fs::File, u64)> {
    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir)?;
    }
    rotate_log_if_full(path, STDERR_LOG_MAX_BYTES)?;
    let file = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)?;
    let len = file.metadata()?.len();
    Ok((file, len))
}

/// Copies the child's stderr into the log until EOF, rotating at [`STDERR_LOG_MAX_BYTES`].
async fn pump_stderr(
    mut stderr: ChildStderr,
    path: PathBuf,
    mut file: std::fs::File,
    mut len: u64,
) {
    let mut buf = vec![0u8; 8192];
    loop {
        let n = match stderr.read(&mut buf).await {
            Ok(0) | Err(_) => return,
            Ok(n) => n,
        };
        if file.write_all(&buf[..n]).is_err() {
            // The log went away (disk full, deleted dir): keep draining so the child never blocks.
            continue;
        }
        len += n as u64;
        if len >= STDERR_LOG_MAX_BYTES {
            drop(file);
            match rotate_log_if_full(&path, STDERR_LOG_MAX_BYTES).and_then(|_| open_log(&path)) {
                Ok((f, l)) => (file, len) = (f, l),
                Err(_) => {
                    // Drain without logging rather than stall the server on a full pipe.
                    while matches!(stderr.read(&mut buf).await, Ok(n) if n > 0) {}
                    return;
                }
            }
        }
    }
}

/// The running child: its process (group) and stderr pump.
pub(crate) struct ChildGuard {
    pub(crate) pid: u32,
    pub(crate) log: PathBuf,
    child: Option<Box<dyn ChildWrapper>>,
    pump: Option<JoinHandle<()>>,
}

impl ChildGuard {
    /// stdin is already closed. Waits up to `grace` for the server to exit, then kills its group
    /// (stragglers such as an `npx` wrapper's children included) and reaps it.
    pub(crate) async fn shutdown(&mut self, grace: Duration) {
        let Some(mut child) = self.child.take() else {
            return;
        };
        let exited = matches!(tokio::time::timeout(grace, child.wait()).await, Ok(Ok(_)));
        // Before the leader is reaped its pid (= the group id) cannot be reused, so the kill is
        // safe. After a reap the group id stays reserved while any member lives; an empty group
        // makes this a harmless ESRCH.
        let _ = child.start_kill();
        if !exited {
            let _ = tokio::time::timeout(grace, child.wait()).await;
        }
        if let Some(pump) = self.pump.take() {
            let _ = tokio::time::timeout(Duration::from_secs(1), pump).await;
        }
    }
}

impl Drop for ChildGuard {
    /// Dropped without [`ChildGuard::shutdown`] (the client dropped, a failed handshake, a panic):
    /// kill the whole group now, synchronously, so nothing outlives the harness's handle on it.
    /// The leader was never reaped, so its group id is still ours. tokio reaps the zombie.
    fn drop(&mut self) {
        if let Some(child) = self.child.as_mut() {
            let _ = child.start_kill();
        }
    }
}

fn spawn_error(spec: &StdioSpec, reason: impl Into<String>) -> McpError {
    McpError::Spawn {
        server: spec.name.clone(),
        command: spec.command.clone(),
        reason: reason.into(),
    }
}

impl ExternalClient {
    /// Starts a stdio server and runs the MCP handshake, all within
    /// `spec.options.connect_timeout` (the server's `startup_timeout_secs`). On any failure the
    /// child's group is killed before this returns.
    pub async fn connect_stdio(spec: StdioSpec) -> Result<Self, McpError> {
        if !spec.cwd.is_dir() {
            return Err(spawn_error(
                &spec,
                format!("working directory {} does not exist", spec.cwd.display()),
            ));
        }
        let (mut log, len) = open_log(&spec.stderr_log).map_err(|e| {
            spawn_error(
                &spec,
                format!("stderr log {}: {e}", spec.stderr_log.display()),
            )
        })?;

        let mut command = tokio::process::Command::new(&spec.command);
        command
            .args(&spec.args)
            .env_clear()
            .envs(&spec.env)
            .current_dir(&spec.cwd)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        let mut wrap = CommandWrap::from(command);
        #[cfg(unix)]
        wrap.wrap(process_wrap::tokio::ProcessSession);
        #[cfg(not(unix))]
        wrap.wrap(process_wrap::tokio::KillOnDrop);

        let mut child = wrap.spawn().map_err(|e| {
            let reason = match e.kind() {
                std::io::ErrorKind::NotFound => {
                    "command not found (not on the server's PATH, or no such file)".to_owned()
                }
                std::io::ErrorKind::PermissionDenied => "permission denied".to_owned(),
                _ => e.to_string(),
            };
            spawn_error(&spec, reason)
        })?;
        let pid = child.id().unwrap_or_default();
        let stdin = child.inner_mut().stdin().take();
        let stdout = child.inner_mut().stdout().take();
        let stderr = child.inner_mut().stderr().take();
        let mut guard = ChildGuard {
            pid,
            log: spec.stderr_log.clone(),
            child: Some(child),
            pump: None,
        };
        let (Some(stdin), Some(stdout)) = (stdin, stdout) else {
            return Err(spawn_error(&spec, "no stdin/stdout pipe"));
        };
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_or(0, |d| d.as_secs());
        // The command name only: args may hold expanded secrets.
        let header = format!(
            "--- amp-harness: started `{}` (pid {pid}) at unix {now} ---\n",
            spec.command
        );
        let _ = log.write_all(header.as_bytes());
        if let Some(stderr) = stderr {
            guard.pump = Some(tokio::spawn(pump_stderr(
                stderr,
                spec.stderr_log.clone(),
                log,
                len + header.len() as u64,
            )));
        }

        let target = format!(
            "stdio server `{}` (stderr: {})",
            spec.name,
            spec.stderr_log.display()
        );
        // On error `guard` drops here and kills the group.
        let mut client =
            Self::handshake(&spec.name, &target, "stdio", (stdout, stdin), &spec.options).await?;
        client.child = Some(guard);
        Ok(client)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(name: &str) -> PathBuf {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-scratch/amp-mcp-stdio-unit")
            .join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn log_path_is_per_server_under_state() {
        assert_eq!(
            stderr_log_path(Path::new("/s"), "fs"),
            PathBuf::from("/s/mcp-logs/fs.log")
        );
    }

    #[test]
    fn rotates_only_a_full_log() {
        let dir = scratch("rotate");
        let log = dir.join("x.log");
        assert!(!rotate_log_if_full(&log, 10).unwrap(), "missing file");
        std::fs::write(&log, b"12345").unwrap();
        assert!(!rotate_log_if_full(&log, 10).unwrap());
        std::fs::write(&log, b"1234567890").unwrap();
        assert!(rotate_log_if_full(&log, 10).unwrap());
        assert!(!log.exists());
        assert_eq!(std::fs::read(dir.join("x.log.1")).unwrap(), b"1234567890");
    }

    #[test]
    fn debug_prints_names_not_values() {
        let spec = StdioSpec {
            name: "fs".into(),
            command: "npx".into(),
            args: vec!["--token=sekrit-arg".into()],
            env: BTreeMap::from([("API_KEY".to_owned(), "sekrit-env".to_owned())]),
            cwd: PathBuf::from("/"),
            stderr_log: PathBuf::from("/l"),
            options: ExternalOptions::default(),
        };
        let shown = format!("{spec:?}");
        assert!(shown.contains("API_KEY"));
        assert!(!shown.contains("sekrit"), "{shown}");
    }
}
