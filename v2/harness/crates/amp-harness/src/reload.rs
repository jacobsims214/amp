//! The chat's `/reload` and its configuration watcher (#2242, H13).
//!
//! `amp-harness agents|skills|models|mcp …` in another terminal writes the same files the chat
//! loaded from (config.toml, `agents/`, `skills/`, in the user layer and the repo's
//! `.amp/harness`). `/reload` puts them in place without restarting the chat
//! ([`amp_core::agent::Runtime::reload`]):
//! - **Agents (the roster):** workers the manager dispatches from now on are built from the new
//!   agent files and resolve their model from the new config; `amp_run_ticket` lists the new
//!   roster from the manager's next reply.
//! - **Skills:** listed (and preloaded) from the manager's next reply and in every new worker.
//! - **Models:** every role's model as configured now, for new workers; the chat itself switches
//!   when its own role's model changed (the transcript says so; `/model` still picks one for this
//!   chat only). New providers and keys are taken too.
//! - **MCP servers:** only the servers whose `[mcp.<name>]` table changed restart; new ones start,
//!   removed ones stop once no running worker holds their tools, and a stopped or needs-login one
//!   is tried again (`amp_core::mcp_hub::McpHub::reload`).
//!
//! Workers already running keep what they were built with. The chat's manager takes its role's
//! permissions as they are now (that is also what makes a new `dispatch: true` agent
//! dispatchable).
//!
//! The watcher ([`watch`]) looks at those files' sizes and modification times every
//! [`WATCH_EVERY`] and asks the shell to reload once a change has settled (two looks alike), so a
//! CLI change reaches the chat without typing `/reload`. `AMP_HARNESS_AUTO_RELOAD=0` turns it off;
//! `--snapshot` never starts it.

use std::{
    path::{Path, PathBuf},
    sync::Arc,
    time::{Duration, SystemTime},
};

use amp_core::{
    agent::{LiveSession, Runtime},
    policy::ToolPolicy,
    project::ProjectMarker,
    providers::SystemKeys,
    supervisor::Supervisor,
};
use amp_mcp::ConfigPaths;
use amp_tui::{ReloadControl, ReloadFuture, ReloadReply};
use rpi_harness::agent_harness::AgentHarness;

/// How often [`watch`] looks at the configuration files.
pub const WATCH_EVERY: Duration = Duration::from_secs(2);
/// `0`, `false`, `off` or `no` turns the watcher off (`/reload` still works).
pub const ENV_AUTO_RELOAD: &str = "AMP_HARNESS_AUTO_RELOAD";

/// `/reload` for one chat session.
pub struct ChatReload(Arc<Live>);

/// The session [`ChatReload`] keeps up to date.
struct Live {
    rt: Arc<Runtime>,
    harness: Arc<AgentHarness>,
    policy: Arc<ToolPolicy>,
    role: String,
    cwd: PathBuf,
    project: Option<ProjectMarker>,
    supervisor: Option<Arc<Supervisor>>,
}

impl ChatReload {
    pub fn new(session: &crate::session::Session) -> Self {
        Self(Arc::new(Live {
            rt: Arc::clone(&session.rt),
            harness: Arc::clone(&session.agent.harness),
            policy: Arc::clone(&session.agent.policy),
            role: session.agent.role.id.clone(),
            cwd: session.cwd.clone(),
            project: session.project.clone(),
            supervisor: session.supervisor.clone(),
        }))
    }
}

impl Live {
    async fn run(self: Arc<Self>) -> Result<ReloadReply, String> {
        let env = |k: &str| std::env::var(k).ok();
        let report = self
            .rt
            .reload(&SystemKeys::from_env(), &env)
            .await
            .map_err(|e| format!("{e}; nothing was changed"))?;
        let refreshed = self
            .rt
            .refresh_session(LiveSession {
                harness: &self.harness,
                role: &self.role,
                cwd: &self.cwd,
                project: self.project.as_ref(),
                extra_context: None,
                policy: &self.policy,
                // amp_run_ticket's description lists the roster: a fresh one lists the new one.
                replace_tools: self.supervisor.iter().map(|s| s.tool()).collect(),
            })
            .await
            .map_err(|e| e.to_string())?;
        let mut lines = report.lines();
        if lines.is_empty() && refreshed.prompt_changed {
            lines.push("the manager's prompt changed (AGENTS.md)".to_owned());
        }
        if refreshed.policy_changed {
            lines.push(format!(
                "{}'s permissions: the new ones apply now",
                self.role
            ));
        }
        let providers = self.rt.current_providers();
        let models = crate::chat::model_choices(&self.rt.current_models(), &providers);
        let switch_to = report
            .model_change(&self.role)
            .and(refreshed.resolved.as_ref())
            .map(|r| r.model_ref());
        Ok(ReloadReply {
            lines,
            providers: providers.all(),
            models,
            switch_to,
            warnings: Vec::new(),
        })
    }
}

impl ReloadControl for ChatReload {
    fn reload(&self) -> ReloadFuture {
        Box::pin(Arc::clone(&self.0).run())
    }
}

/// Whether the watcher runs: on unless [`ENV_AUTO_RELOAD`] says no.
pub fn auto_reload(env: &dyn Fn(&str) -> Option<String>) -> bool {
    !env(ENV_AUTO_RELOAD).is_some_and(|v| {
        matches!(
            v.trim().to_ascii_lowercase().as_str(),
            "0" | "false" | "off" | "no"
        )
    })
}

/// One file's size and modification time.
type Stamp = (PathBuf, u64, Option<SystemTime>);

/// What [`watch`] compares: every file a reload reads (config.toml, the repo's config.toml, the
/// global `AGENTS.md`, everything under `agents/` and `skills/` in both layers). Names starting
/// with `.` are skipped (the CLI's `.edit/` scratch and backups). Missing files are left out, so
/// creating one is a change.
#[cfg(test)]
pub fn fingerprint(paths: &ConfigPaths) -> Vec<Stamp> {
    fingerprint_with(paths, &[])
}

/// [`fingerprint`] plus `extra` files: the MCP secret versions and each server's env_file
/// ([`amp_core::mcp_hub::McpHub::watched_files`], #2311), so `mcp secret` in another terminal or
/// an edited env_file reloads too.
pub fn fingerprint_with(paths: &ConfigPaths, extra: &[PathBuf]) -> Vec<Stamp> {
    let mut out = Vec::new();
    let mut files: Vec<PathBuf> = paths.user_file.iter().cloned().collect();
    files.extend(extra.iter().cloned());
    files.extend(paths.repo_file());
    if let Some(dir) = &paths.user_dir {
        files.push(dir.join("AGENTS.md"));
    }
    for file in files {
        stamp(&file, &mut out);
    }
    for layer in [&paths.user_dir, &paths.repo_dir].into_iter().flatten() {
        for sub in ["agents", "skills"] {
            walk(&layer.join(sub), &mut out, 0);
        }
    }
    out.sort();
    out
}

fn stamp(path: &Path, out: &mut Vec<Stamp>) {
    if let Ok(meta) = std::fs::metadata(path)
        && meta.is_file()
    {
        out.push((path.to_path_buf(), meta.len(), meta.modified().ok()));
    }
}

fn walk(dir: &Path, out: &mut Vec<Stamp>, depth: usize) {
    // Skills are small trees; the bound only guards against a symlink loop.
    if depth > 8 {
        return;
    }
    let Ok(entries) = std::fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        if entry.file_name().to_string_lossy().starts_with('.') {
            continue;
        }
        let path = entry.path();
        if path.is_dir() {
            // A directory's own entry: an empty new skill dir, or one removed, is a change too.
            out.push((path.clone(), 0, None));
            walk(&path, out, depth + 1);
        } else {
            stamp(&path, out);
        }
    }
}

/// Calls `on_change` each time the configuration files ([`fingerprint`]) changed and then stayed
/// the same for one more look, `every` apart. Runs until the task is dropped or aborted.
/// `extra` names more files to watch, asked again on every look (the MCP servers' env_files
/// change with the configuration).
pub async fn watch(
    paths: ConfigPaths,
    every: Duration,
    extra: impl Fn() -> Vec<PathBuf> + Send + Sync + 'static,
    on_change: impl Fn() + Send + 'static,
) {
    let extra = std::sync::Arc::new(extra);
    let look = move || {
        let paths = paths.clone();
        let extra = std::sync::Arc::clone(&extra);
        async move {
            tokio::task::spawn_blocking(move || fingerprint_with(&paths, &extra()))
                .await
                .unwrap_or_default()
        }
    };
    let mut seen = look().await;
    let mut pending: Option<Vec<Stamp>> = None;
    loop {
        tokio::time::sleep(every).await;
        let now = look().await;
        if now == seen {
            pending = None;
            continue;
        }
        if pending.as_ref() == Some(&now) {
            seen = now;
            pending = None;
            on_change();
        } else {
            pending = Some(now);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn scratch(name: &str) -> PathBuf {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-scratch/reload")
            .join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn paths(dir: &Path) -> ConfigPaths {
        ConfigPaths {
            user_file: Some(dir.join("user/config.toml")),
            user_dir: Some(dir.join("user")),
            repo_dir: Some(dir.join("repo/.amp/harness")),
            ..ConfigPaths::default()
        }
    }

    #[test]
    fn the_fingerprint_covers_every_file_a_reload_reads() {
        let dir = scratch("fingerprint");
        let p = paths(&dir);
        assert!(fingerprint(&p).is_empty(), "nothing exists yet");
        let user = dir.join("user");
        let repo = dir.join("repo/.amp/harness");
        std::fs::create_dir_all(user.join("agents")).unwrap();
        std::fs::create_dir_all(repo.join("skills/pdf/scripts")).unwrap();
        std::fs::write(user.join("config.toml"), "[roles.manager]\n").unwrap();
        std::fs::write(repo.join("config.toml"), "[mcp]\n").unwrap();
        std::fs::write(user.join("AGENTS.md"), "be nice\n").unwrap();
        std::fs::write(user.join("agents/sql.md"), "---\n---\n").unwrap();
        std::fs::write(repo.join("skills/pdf/scripts/run.sh"), "echo\n").unwrap();
        let fp = fingerprint(&p);
        for want in [
            "user/config.toml",
            "repo/.amp/harness/config.toml",
            "user/AGENTS.md",
            "user/agents/sql.md",
            "repo/.amp/harness/skills/pdf/scripts/run.sh",
        ] {
            assert!(
                fp.iter().any(|(path, ..)| path.ends_with(want)),
                "{want} missing from {fp:?}"
            );
        }
        // The CLI's scratch and dotfiles are not configuration.
        std::fs::create_dir_all(user.join("agents/.edit")).unwrap();
        std::fs::write(user.join("agents/.edit/x.md"), "x").unwrap();
        std::fs::write(user.join("agents/.sql.md.swp"), "x").unwrap();
        assert_eq!(fingerprint(&p), fp);
        // A changed size is a change; so is a removed file.
        std::fs::write(user.join("agents/sql.md"), "---\nmode: subagent\n---\n").unwrap();
        assert_ne!(fingerprint(&p), fp);
        let fp = fingerprint(&p);
        std::fs::remove_file(repo.join("skills/pdf/scripts/run.sh")).unwrap();
        assert_ne!(fingerprint(&p), fp);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[tokio::test]
    async fn the_watcher_fires_once_per_settled_change() {
        let dir = scratch("watch");
        let p = paths(&dir);
        std::fs::create_dir_all(dir.join("user")).unwrap();
        let fired = Arc::new(AtomicUsize::new(0));
        let counter = Arc::clone(&fired);
        let env_file = dir.join("srv.env");
        let extra = vec![env_file.clone()];
        let task = tokio::spawn(watch(
            p,
            Duration::from_millis(40),
            move || extra.clone(),
            move || {
                counter.fetch_add(1, Ordering::SeqCst);
            },
        ));
        let wait_for = |n: usize| {
            let fired = Arc::clone(&fired);
            async move {
                let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
                while fired.load(Ordering::SeqCst) < n && tokio::time::Instant::now() < deadline {
                    tokio::time::sleep(Duration::from_millis(10)).await;
                }
                fired.load(Ordering::SeqCst)
            }
        };
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert_eq!(fired.load(Ordering::SeqCst), 0, "no change, no reload");
        std::fs::write(dir.join("user/config.toml"), "[roles.manager]\n").unwrap();
        assert_eq!(wait_for(1).await, 1);
        // Quiet again: no more reloads.
        tokio::time::sleep(Duration::from_millis(200)).await;
        assert_eq!(fired.load(Ordering::SeqCst), 1);
        std::fs::create_dir_all(dir.join("user/skills/new")).unwrap();
        assert_eq!(wait_for(2).await, 2);
        // #2311: an extra file (an MCP server's env_file, the secret versions) counts too.
        std::fs::write(&env_file, "A=1\n").unwrap();
        assert_eq!(wait_for(3).await, 3);
        task.abort();
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn auto_reload_is_on_unless_turned_off() {
        assert!(auto_reload(&|_| None));
        assert!(auto_reload(&|_| Some("1".into())));
        for off in ["0", "false", "OFF", " no "] {
            assert!(!auto_reload(&|_| Some(off.into())), "{off}");
        }
    }
}
