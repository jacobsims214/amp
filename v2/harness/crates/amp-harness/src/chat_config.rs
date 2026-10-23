//! The chat's configuration panels (#2245, H14 of decisions/amp-harness-configurability.md):
//! `/mcp`, `/agents`, `/skills`, `/model roles` / `/model <role> …` and `/config`.
//!
//! Each is a docked panel ([`amp_tui::InfoPanel`]) in the style of the AMP panels. Its keys run
//! slash commands, so `r` on a server and `/mcp restart <name>` are one path:
//! - **`/mcp`**: every MCP server with its health, live from the session's hub (`health()`, so a
//!   server turning ready shows at once). `/mcp <name>` shows one server in detail (what it runs or
//!   connects to, where it is configured, secrets as their source only, trust, roles, its tools).
//!   `/mcp restart|test|enable|disable|login|trust|untrust <name>` act on it; `/mcp add <name>
//!   <url> [--sse]` adds an HTTP (or SSE) server to the user config. Adding a stdio server,
//!   editing, removing, importing and secrets stay in the terminal (`amp-harness mcp …`): the
//!   panel names the command to copy.
//! - **`/agents`**, **`/agents <id>`**: the roster from the session's catalog: mode, the layer the
//!   file comes from (built-in, user, repo), model and where it was chosen, skills, dispatch.
//! - **`/skills`**, **`/skills <name>`**: the skills, their layers, files and who preloads them.
//! - **`/model roles`**: every role's provider/model, thinking and the setting that chose it.
//!   `/model <role>` picks a model for the role (Enter: this chat, `s`: save to config.toml);
//!   `/model <role> <provider/model> [thinking] [--save]` does it typed; `--reset` drops a
//!   chat-only choice.
//! - **`/config`**: the files in use, the AMP server, models, providers (keys as their source),
//!   roles, MCP servers, `[tui]`, `[tools]`, `[workers]` and `[secrets]`, each with where its value
//!   comes from (default, a config file, an env variable, a flag).
//!
//! Writes go through the CLI's own code ([`crate::mcp_crud`], [`crate::models_cmd`]): the same
//! checks, backups and trust rules. A change asks the shell to reload ([`ConfigReply::reload`]),
//! which runs the `/reload` path ([`crate::reload`]), so the session takes it exactly as it takes
//! a change made with the CLI in another terminal. A role's model set for this chat only
//! ([`Runtime::set_role_override`]) is kept over the files by every reload.
//!
//! Nothing here reads a secret: keychain secrets are shown as their account, env secrets as the
//! variable's name.

use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
    sync::Arc,
    time::{Duration, Instant},
};

use amp_core::{
    agent::Runtime,
    mcp_config::{AuthMode, McpConfig, McpServer, Scope as RunScope, Startup, Transport},
    mcp_hub::{McpHub, ServerHealth, ServerStatus},
    mcp_trust,
    models::{normalize_role, thinking_name},
    providers::SystemKeys,
    supervisor::SupervisorConfig,
};
use amp_mcp::oauth;
use amp_tui::{
    Cell, ConfigControl, ConfigFuture, ConfigReply, InfoPanel, InfoRow, Notifier, Tone,
    shell::short_role,
};
use serde_json::Value;

use crate::{
    agents_cmd::{self, Ctx, Scope},
    mcp_crud,
    models_cmd::{self, ScopeArgs},
};

/// How long `/mcp test` waits for a server's attempt (on top of its own startup timeout).
const TEST_GRACE: Duration = Duration::from_secs(2);
/// The label column of the detail panels.
const LABEL: usize = 10;

/// The configuration panels for one chat session.
pub struct ChatConfig(Arc<Inner>);

struct Inner {
    rt: Arc<Runtime>,
    /// The session's working directory (its repo layer comes from `rt.paths`).
    cwd: PathBuf,
    /// The chat's role (`amp-manager`).
    role: String,
    /// The AMP server settings the session started with (for `/config`).
    settings: Option<amp_mcp::Settings>,
    /// The worker limits the supervisor runs with (for `/config`).
    workers: Option<SupervisorConfig>,
    /// `--theme`, when given (for `/config`).
    theme_flag: Option<String>,
    /// Transcript lines from a command still running (`/mcp login`'s URL).
    notes: Notifier,
    /// `/mcp login` opens the browser (not in `--snapshot`).
    browser: bool,
}

/// What the chat hands [`ChatConfig::new`].
pub struct ChatConfigParts {
    pub rt: Arc<Runtime>,
    pub cwd: PathBuf,
    pub role: String,
    pub settings: Option<amp_mcp::Settings>,
    pub workers: Option<SupervisorConfig>,
    pub theme_flag: Option<String>,
    pub notes: Notifier,
    pub browser: bool,
}

impl ChatConfig {
    pub fn new(parts: ChatConfigParts) -> Self {
        Self(Arc::new(Inner {
            rt: parts.rt,
            cwd: parts.cwd,
            role: parts.role,
            settings: parts.settings,
            workers: parts.workers,
            theme_flag: parts.theme_flag,
            notes: parts.notes,
            browser: parts.browser,
        }))
    }
}

impl ConfigControl for ChatConfig {
    fn run(&self, name: &str, args: &str) -> ConfigFuture {
        let inner = Arc::clone(&self.0);
        let name = name.to_owned();
        let args = args.trim().to_owned();
        Box::pin(async move {
            match name.as_str() {
                "mcp" => inner.mcp(&args).await,
                "agents" => inner.agents(&args),
                "skills" => inner.skills(&args),
                "model" => inner.model(&args).await,
                "config" => inner.config().await,
                other => Err(format!("/{other}: not a configuration command")),
            }
        })
    }
}

fn words(args: &str) -> Vec<&str> {
    args.split_whitespace().collect()
}

impl Inner {
    fn ctx(&self) -> Ctx {
        Ctx {
            paths: self.rt.paths.clone(),
            cwd: self.cwd.clone(),
        }
    }

    fn hub(&self) -> &Arc<McpHub> {
        &self.rt.mcp_hub
    }

    // --------------------------------------------------------------------------------------------
    // /mcp
    // --------------------------------------------------------------------------------------------

    async fn mcp(&self, args: &str) -> Result<ConfigReply, String> {
        let w = words(args);
        let (verb, rest) = match w.split_first() {
            None => return Ok(self.mcp_list(None)),
            Some((verb, rest)) => (*verb, rest),
        };
        let one = |what: &str| -> Result<String, String> {
            match rest {
                [name] => Ok((*name).to_owned()),
                [] => Err(format!("/mcp {what} <name> (/mcp lists them)")),
                _ => Err(format!("/mcp {what} takes one server name")),
            }
        };
        match verb {
            "restart" | "start" => self.mcp_restart(&one(verb)?),
            "test" => self.mcp_test(&one(verb)?).await,
            "enable" => self.mcp_enable(&one(verb)?, true),
            "disable" => self.mcp_enable(&one(verb)?, false),
            "login" => self.mcp_login(&one(verb)?).await,
            "trust" => self.mcp_trust(&one(verb)?, true),
            "untrust" => self.mcp_trust(&one(verb)?, false),
            "add" => self.mcp_add(rest),
            "edit" | "remove" | "rm" | "import" | "secret" | "add-json" | "logout" => {
                Ok(terminal_only(verb, rest))
            }
            name if rest.is_empty() => self.mcp_detail(name),
            _ => Err(format!(
                "/mcp {args}: not a command (/mcp, /mcp <name>, /mcp \
                 restart|test|enable|disable|login|trust|untrust <name>, /mcp add <name> <url>)"
            )),
        }
    }

    /// The `/mcp` panel: every server with its health, read from the hub at every render.
    fn mcp_list(&self, select: Option<&str>) -> ConfigReply {
        let hub = Arc::clone(self.hub());
        let rows = Arc::new(move || mcp_rows(&hub.config(), &hub.health(), &untrusted(&hub)));
        let panel = InfoPanel::live("MCP servers", rows)
            .on_enter("details", "/mcp {}")
            .with_key('r', "restart", "/mcp restart {}")
            .with_key('t', "test", "/mcp test {}")
            .with_key('e', "enable", "/mcp enable {}")
            .with_key('d', "disable", "/mcp disable {}")
            .with_key('l', "login", "/mcp login {}")
            .with_key('T', "trust", "/mcp trust {}")
            .with_key('U', "untrust", "/mcp untrust {}");
        let mut reply = ConfigReply::panel(panel);
        reply.select = select.map(str::to_owned);
        reply
    }

    fn server(&self, name: &str) -> Result<McpServer, String> {
        let config = self.hub().config();
        match config.servers.get(name) {
            Some(s) => Ok(s.clone()),
            None if config.broken.contains(name) => Err(format!(
                "MCP server `{name}` has configuration errors (`amp-harness config check` lists \
                 them)"
            )),
            None => Err(format!("no MCP server `{name}` (/mcp lists them)")),
        }
    }

    /// `/mcp <name>`: one server in detail, live.
    fn mcp_detail(&self, name: &str) -> Result<ConfigReply, String> {
        self.server(name)?;
        let hub = Arc::clone(self.hub());
        let repo_dir = self.rt.paths.repo_dir.clone();
        let name_owned = name.to_owned();
        let rows = Arc::new(move || {
            let config = hub.config();
            let Some(server) = config.servers.get(&name_owned) else {
                return vec![InfoRow::text(
                    format!("`{name_owned}` is no longer configured (/mcp lists the servers)"),
                    Tone::Dim,
                    0,
                )];
            };
            let health = hub.health();
            let tools = hub
                .connection(&name_owned)
                .map(|(_, tools)| {
                    let mut names: Vec<String> = tools.iter().map(|t| t.name.to_string()).collect();
                    names.sort();
                    names
                })
                .unwrap_or_default();
            mcp_detail_rows(
                server,
                shared_health(&health, &name_owned),
                &tools,
                &untrusted(&hub),
                repo_dir.as_deref(),
            )
        });
        let panel = InfoPanel::live(format!("MCP · {name}"), rows)
            .on_enter("back to the list", "/mcp")
            .with_key('r', "restart", "/mcp restart {}")
            .with_key('t', "test", "/mcp test {}")
            .with_key('e', "enable", "/mcp enable {}")
            .with_key('d', "disable", "/mcp disable {}")
            .with_key('l', "login", "/mcp login {}")
            .with_key('T', "trust", "/mcp trust {}")
            .with_key('U', "untrust", "/mcp untrust {}");
        Ok(ConfigReply::panel(panel))
    }

    fn mcp_restart(&self, name: &str) -> Result<ConfigReply, String> {
        let server = self.server(name)?;
        if !server.enabled {
            return Err(format!(
                "`{name}` is disabled; /mcp enable {name} turns it on"
            ));
        }
        self.hub().restart(name)?;
        Ok(ConfigReply::done(format!(
            "MCP server `{name}`: restarting"
        )))
    }

    /// `/mcp test <name>`: a ready server lists its tools; any other is (re)started and given its
    /// startup timeout to come up. Reports what happened, with the stderr log's last lines.
    async fn mcp_test(&self, name: &str) -> Result<ConfigReply, String> {
        let server = self.server(name)?;
        if !server.enabled {
            return Err(format!(
                "`{name}` is disabled; /mcp enable {name} turns it on"
            ));
        }
        if server.scope == RunScope::Worktree {
            return Err(format!(
                "`{name}` runs one instance per worker worktree; `amp-harness mcp test {name}` \
                 checks it in a terminal"
            ));
        }
        let hub = self.hub();
        let started = Instant::now();
        let status = |hub: &McpHub| {
            shared_health(&hub.health(), name)
                .map(|h| h.status.clone())
                .unwrap_or(ServerStatus::Idle)
        };
        if status(hub) != ServerStatus::Ready {
            hub.restart(name)?;
            let deadline = started + server.startup_timeout + TEST_GRACE;
            // The restart lands on the server's task first.
            tokio::time::sleep(Duration::from_millis(50)).await;
            while matches!(status(hub), ServerStatus::Starting | ServerStatus::Idle)
                && Instant::now() < deadline
            {
                tokio::time::sleep(Duration::from_millis(50)).await;
            }
        }
        let health = hub.health();
        let h = shared_health(&health, name);
        match hub.connection(name) {
            Ok((_, tools)) => {
                let mut names: Vec<String> = tools.iter().map(|t| t.name.to_string()).collect();
                names.sort();
                let mut line = format!(
                    "MCP server `{name}`: ready, {} tool{}",
                    names.len(),
                    if names.len() == 1 { "" } else { "s" }
                );
                if !names.is_empty() {
                    line.push_str(&format!(": {}", names.join(", ")));
                }
                if let Some(s) = h.and_then(|h| h.server.as_deref()) {
                    line.push_str(&format!(" (server {s})"));
                }
                Ok(ConfigReply {
                    note: Some((format!("`{name}` ready, {} tools", names.len()), false)),
                    lines: vec![line],
                    ..ConfigReply::default()
                })
            }
            Err(_) => {
                let status = h
                    .map(|h| h.status.to_string())
                    .unwrap_or_else(|| "not started".to_owned());
                let mut errors = vec![format!("MCP server `{name}`: {status}")];
                let tail = stderr_tail(h.and_then(|h| h.stderr_log.as_deref()), 5);
                if !tail.is_empty() {
                    errors.push(format!(
                        "last stderr lines ({}):\n  {}",
                        h.and_then(|h| h.stderr_log.as_deref())
                            .map(|p| p.display().to_string())
                            .unwrap_or_default(),
                        tail.join("\n  ")
                    ));
                }
                Ok(ConfigReply {
                    note: Some((format!("`{name}`: {status}"), true)),
                    errors,
                    ..ConfigReply::default()
                })
            }
        }
    }

    fn mcp_enable(&self, name: &str, on: bool) -> Result<ConfigReply, String> {
        let done = mcp_crud::set_enabled_in(&self.ctx(), name, on).map_err(|e| e.to_string())?;
        let word = if on { "enabled" } else { "disabled" };
        if !done.changed {
            return Ok(ConfigReply::done(format!("`{name}` is already {word}")));
        }
        let whose = if done.repo {
            "repo MCP server"
        } else {
            "MCP server"
        };
        let mut reply = ConfigReply::done(format!(
            "{whose} `{name}` {word} ({}{})",
            done.file.display(),
            if done.repo {
                "; the repo's file is unchanged"
            } else {
                ""
            }
        ));
        reply.reload = true;
        Ok(reply)
    }

    async fn mcp_login(&self, name: &str) -> Result<ConfigReply, String> {
        let notes = self.notes.clone();
        let browser = self.browser;
        let label = name.to_owned();
        let (server, outcome) = mcp_crud::login_in(
            &self.ctx(),
            name,
            oauth::DEFAULT_LOGIN_TIMEOUT,
            move |prompt: &oauth::LoginPrompt| {
                let opened = browser && oauth::open_browser(&prompt.authorization_url).is_ok();
                let how = if opened {
                    "Opened your browser to log in"
                } else {
                    "Open this URL in a browser to log in"
                };
                notes.notice(format!(
                    "MCP server `{label}`: {how} (waiting on {}):\n  {}",
                    prompt.redirect_uri, prompt.authorization_url
                ));
            },
        )
        .await
        .map_err(|e| e.to_string())?;
        // A server that needed a login is not retried on its own: start it now.
        let restarted = self.hub().restart(&server.name).is_ok();
        Ok(ConfigReply::done(format!(
            "logged in to `{}` ({}; stored in {}){}",
            server.name,
            outcome.key,
            outcome.stored_in,
            if restarted { "; connecting" } else { "" }
        )))
    }

    fn mcp_trust(&self, name: &str, on: bool) -> Result<ConfigReply, String> {
        let server = self.server(name)?;
        if !server.is_repo() {
            return Err(format!(
                "`{name}` is in your own config ({}); those are always trusted",
                server.origin.file.display()
            ));
        }
        let ctx = self.ctx();
        let mut reply = if on {
            let store = mcp_crud::trust_now(&ctx, name).map_err(|e| e.to_string())?;
            ConfigReply::done(format!(
                "trusted `{name}`: {} (recorded in {}; a change to its config asks again)",
                mcp_trust::summary(&server),
                store.display()
            ))
        } else {
            mcp_crud::untrust_now(&ctx, name);
            ConfigReply::done(format!(
                "untrusted `{name}`: it stops, and is asked about again next time"
            ))
        };
        reply.reload = true;
        Ok(reply)
    }

    /// `/mcp add <name> <url> [--sse]`: an HTTP (or SSE) server in the user config.
    fn mcp_add(&self, rest: &[&str]) -> Result<ConfigReply, String> {
        let sse = rest.contains(&"--sse");
        let plain: Vec<&str> = rest.iter().copied().filter(|w| *w != "--sse").collect();
        let looks_url = |s: &str| s.starts_with("http://") || s.starts_with("https://");
        match plain.as_slice() {
            [name, url] if looks_url(url) => {
                let spec = mcp_crud::http_spec(name, url, sse).map_err(|e| e.to_string())?;
                let added = mcp_crud::add_server_in(
                    &self.ctx(),
                    name,
                    Scope::User,
                    spec,
                    &mcp_crud::AddOpts {
                        force: false,
                        no_test: true,
                        secret_stdin: false,
                        json: false,
                        note: "Added from the chat (`/mcp add`).",
                        quiet: true,
                    },
                )
                .map_err(|e| e.to_string())?;
                let kind = added.server.transport.kind().as_str();
                let mut lines = vec![format!(
                    "added {kind} MCP server `{name}` to {}; connecting (/mcp shows its health)",
                    added.file.display()
                )];
                lines.extend(added.warnings.iter().map(|w| format!("warning: {w}")));
                lines.push(format!(
                    "headers, secrets and OAuth settings: `amp-harness mcp edit {name}` or \
                     `amp-harness mcp secret {name}` in a terminal"
                ));
                let mut reply = self.mcp_list(Some(name));
                reply.note = Some((format!("added `{name}`"), false));
                reply.lines = lines;
                reply.reload = true;
                Ok(reply)
            }
            [name, ..] if plain.len() >= 2 => Ok(ConfigReply {
                lines: vec![format!(
                    "stdio MCP servers are added in a terminal (the command runs on your \
                     machine, and secrets are asked hidden):\n  amp-harness mcp add {name} -- {}",
                    plain[1..]
                        .iter()
                        .skip_while(|w| **w == "--")
                        .copied()
                        .collect::<Vec<_>>()
                        .join(" ")
                )],
                ..ConfigReply::default()
            }),
            _ => Err(
                "/mcp add <name> <url> [--sse] (stdio servers: `amp-harness mcp add <name> -- \
                 <command>` in a terminal)"
                    .to_owned(),
            ),
        }
    }

    // --------------------------------------------------------------------------------------------
    // /agents
    // --------------------------------------------------------------------------------------------

    fn agents(&self, args: &str) -> Result<ConfigReply, String> {
        let catalog = self.rt.catalog();
        let models = self.rt.current_models();
        let ctx = self.ctx();
        let reports: Vec<agents_cmd::AgentReport> = catalog
            .roles
            .roles()
            .map(|r| {
                agents_cmd::agent_report_from(&ctx, &catalog.assets, &catalog.roles, &models, r)
            })
            .collect();
        if args.is_empty() {
            let id_w = reports
                .iter()
                .map(|a| short_role(&a.id).len())
                .max()
                .unwrap_or(0)
                .max(5);
            let mut rows = vec![header(&[
                ("agent", id_w),
                ("mode", 8),
                ("from", 8),
                ("model", 0),
            ])];
            rows.extend(reports.iter().map(|a| {
                let (model, tone) = match (&a.model, &a.model_error) {
                    (Some(m), _) => (m.clone(), Tone::Muted),
                    (None, Some(e)) => (format!("no model: {e}"), Tone::Error),
                    _ => ("no model".to_owned(), Tone::Error),
                };
                // primary: the manager; worker: the manager dispatches it; helper: it can't.
                let mode = if a.mode == "primary" {
                    "primary"
                } else if a.dispatchable {
                    "worker"
                } else {
                    "helper"
                };
                InfoRow::new(vec![
                    Cell::new(short_role(&a.id), Tone::Text).pad(id_w),
                    Cell::new(mode, Tone::Dim).pad(8),
                    Cell::new(a.origin, origin_tone(a.origin)).pad(8),
                    Cell::flex(model, tone),
                ])
                .key(short_role(&a.id))
            }));
            rows.push(InfoRow::text(
                "new or changed agents: `amp-harness agents new|edit` in a terminal (/config shows \
                 the folders)",
                Tone::Dim,
                0,
            ));
            let panel = InfoPanel::fixed("agents", rows).on_enter("details", "/agents {}");
            return Ok(ConfigReply::panel(panel));
        }
        let Some(a) = catalog
            .roles
            .get(args)
            .and_then(|role| reports.iter().find(|a| a.id == role.id))
        else {
            return Err(format!("no agent `{args}` (/agents lists them)"));
        };
        let asset = catalog.assets.agent(&a.id);
        let mut rows = vec![
            InfoRow::new(vec![
                Cell::new(short_role(&a.id), Tone::Text),
                Cell::new(format!("({})", a.origin), origin_tone(a.origin)),
            ])
            .key(short_role(&a.id)),
            InfoRow::field(
                "file",
                LABEL,
                a.path
                    .as_deref()
                    .map(|p| show_path(p, self.rt.paths.repo_dir.as_deref()))
                    .unwrap_or_else(|| format!("built-in agents/{}.md", a.id)),
                Tone::Muted,
            ),
        ];
        if !a.overrides.is_empty() {
            rows.push(InfoRow::field(
                "overrides",
                LABEL,
                a.overrides.join(", "),
                Tone::Muted,
            ));
        }
        rows.push(InfoRow::field("mode", LABEL, a.mode.clone(), Tone::Text));
        rows.push(match (&a.model, &a.model_error) {
            (Some(m), _) => InfoRow::field(
                "model",
                LABEL,
                format!(
                    "{m} · thinking {} · from {}",
                    a.thinking.unwrap_or("off"),
                    short_source(a.model_source.as_deref().unwrap_or("?"))
                ),
                Tone::Text,
            ),
            (None, e) => InfoRow::field(
                "model",
                LABEL,
                format!("none: {}", e.as_deref().unwrap_or("unresolved")),
                Tone::Error,
            ),
        });
        if let Some(base) = &a.based_on {
            rows.push(InfoRow::field("based on", LABEL, base.clone(), Tone::Muted));
        }
        rows.push(InfoRow::field(
            "skills",
            LABEL,
            if a.skills.is_empty() {
                "(none preloaded)".to_owned()
            } else {
                a.skills.join(", ")
            },
            Tone::Muted,
        ));
        if a.mode != "primary" {
            rows.push(InfoRow::field(
                "dispatch",
                LABEL,
                if !a.dispatchable {
                    "no: the manager's task rules deny it (add `dispatch: true`)"
                } else if a.dispatch {
                    "yes (dispatch: true)"
                } else {
                    "yes (the manager's task rules allow it)"
                },
                Tone::Muted,
            ));
        }
        rows.push(InfoRow::new(vec![Cell::new("about", Tone::Dim)]));
        rows.push(InfoRow::text(
            agents_cmd::one_line(&a.description),
            Tone::Text,
            2,
        ));
        rows.push(InfoRow::new(vec![Cell::new("permissions", Tone::Dim)]));
        for line in permission_lines(asset.map(|x| &x.frontmatter)) {
            rows.push(InfoRow::text(line, Tone::Muted, 2));
        }
        rows.push(InfoRow::text(
            format!(
                "change it: `amp-harness agents edit {}` in a terminal; its model: /model {}",
                short_role(&a.id),
                short_role(&a.id)
            ),
            Tone::Dim,
            0,
        ));
        let panel = InfoPanel::fixed(format!("agent · {}", short_role(&a.id)), rows)
            .on_enter("back to the list", "/agents")
            .with_key('m', "model", "/model {}");
        Ok(ConfigReply::panel(panel))
    }

    // --------------------------------------------------------------------------------------------
    // /skills
    // --------------------------------------------------------------------------------------------

    fn skills(&self, args: &str) -> Result<ConfigReply, String> {
        let catalog = self.rt.catalog();
        let assets = &catalog.assets;
        if args.is_empty() {
            let reports: Vec<_> = assets
                .skills()
                .map(|s| crate::skills_cmd::report(assets, Some(&catalog.roles), &s.name, false))
                .collect();
            let name_w = reports
                .iter()
                .map(|s| s.name.len())
                .max()
                .unwrap_or(0)
                .max(5);
            let mut rows = vec![header(&[
                ("skill", name_w),
                ("from", 9),
                ("what it does", 0),
            ])];
            rows.extend(reports.iter().map(|s| {
                InfoRow::new(vec![
                    Cell::new(s.name.clone(), Tone::Text).pad(name_w),
                    Cell::new(s.layers.join("+"), origin_tone(s.origin)).pad(9),
                    Cell::flex(agents_cmd::one_line(&s.description), Tone::Muted),
                ])
                .key(s.name.clone())
            }));
            if rows.len() == 1 {
                rows.push(InfoRow::text("no skills", Tone::Dim, 0));
            }
            rows.push(InfoRow::text(
                "new or changed skills: `amp-harness skills new|edit|import` in a terminal \
                 (/config shows the folders)",
                Tone::Dim,
                0,
            ));
            let panel = InfoPanel::fixed("skills", rows).on_enter("details", "/skills {}");
            return Ok(ConfigReply::panel(panel));
        }
        if assets.skill(args).is_none() {
            return Err(format!("no skill `{args}` (/skills lists them)"));
        }
        let r = crate::skills_cmd::report(assets, Some(&catalog.roles), args, false);
        let mut rows = vec![
            InfoRow::new(vec![
                Cell::new(r.name.clone(), Tone::Text),
                Cell::new(format!("({})", r.layers.join(" + ")), origin_tone(r.origin)),
            ])
            .key(r.name.clone()),
            InfoRow::field(
                "file",
                LABEL,
                r.path
                    .as_deref()
                    .map(|p| show_path(p, self.rt.paths.repo_dir.as_deref()))
                    .unwrap_or_else(|| format!("built-in skills/{}/SKILL.md", r.name)),
                Tone::Muted,
            ),
            InfoRow::field(
                "preloaded",
                LABEL,
                if r.preloaded_by.is_empty() {
                    "by no agent (any agent can load it when it needs it)".to_owned()
                } else {
                    format!(
                        "by {}",
                        r.preloaded_by
                            .iter()
                            .map(|id| short_role(id))
                            .collect::<Vec<_>>()
                            .join(", ")
                    )
                },
                Tone::Muted,
            ),
            InfoRow::new(vec![Cell::new("about", Tone::Dim)]),
            InfoRow::text(agents_cmd::one_line(&r.description), Tone::Text, 2),
            InfoRow::new(vec![Cell::new(
                format!("files ({})", r.files.len()),
                Tone::Dim,
            )]),
        ];
        for f in &r.files {
            rows.push(
                InfoRow::new(vec![
                    Cell::flex(f.path.clone(), Tone::Text),
                    Cell::new(f.origin, origin_tone(f.origin)),
                ])
                .indent(2),
            );
        }
        rows.push(InfoRow::text(
            format!(
                "change it: `amp-harness skills edit {}` in a terminal",
                r.name
            ),
            Tone::Dim,
            0,
        ));
        let panel = InfoPanel::fixed(format!("skill · {}", r.name), rows)
            .on_enter("back to the list", "/skills");
        Ok(ConfigReply::panel(panel))
    }

    // --------------------------------------------------------------------------------------------
    // /model roles, /model <role> …
    // --------------------------------------------------------------------------------------------

    async fn model(&self, args: &str) -> Result<ConfigReply, String> {
        let w = words(args);
        let save = w.contains(&"--save");
        let reset = w.contains(&"--reset");
        let plain: Vec<&str> = w
            .iter()
            .copied()
            .filter(|x| !matches!(*x, "--save" | "--reset"))
            .collect();
        match plain.as_slice() {
            ["roles"] if !save && !reset => Ok(self.roles_panel(None)),
            [role] if reset => self.reset_role(role),
            [role] if !save => self.role_picker(role),
            [role, model] => self.set_role(role, model, None, save).await,
            [role, model, thinking] => self.set_role(role, model, Some(thinking), save).await,
            _ => Err(
                "/model roles, /model <role> (a picker), /model <role> <provider/model> \
                 [thinking] [--save], /model <role> --reset"
                    .to_owned(),
            ),
        }
    }

    /// The role as the catalog knows it (`amp-worker-backend`), or why not.
    fn role_id(&self, role: &str) -> Result<String, String> {
        self.rt
            .catalog()
            .roles
            .get(role)
            .map(|r| r.id.clone())
            .ok_or_else(|| format!("no role `{role}` (/model roles lists them)"))
    }

    /// `/model roles`: every role's model and where it was chosen.
    fn roles_panel(&self, select: Option<&str>) -> ConfigReply {
        let catalog = self.rt.catalog();
        let models = self.rt.current_models();
        let overrides = self.rt.role_overrides();
        let roles: Vec<_> = catalog.roles.roles().collect();
        let id_w = roles
            .iter()
            .map(|r| short_role(&r.id).len())
            .max()
            .unwrap_or(0)
            .max(4);
        let resolved: Vec<_> = roles.iter().map(|r| r.resolve_model(&models)).collect();
        let model_w = resolved
            .iter()
            .filter_map(|r| r.as_ref().ok().map(|r| r.model_ref().len()))
            .max()
            .unwrap_or(0)
            .max(14);
        let mut rows = vec![header(&[
            ("", 1),
            ("role", id_w),
            ("provider/model", model_w),
            ("thinking", 8),
            ("chosen by", 0),
        ])];
        for (role, resolved) in roles.into_iter().zip(resolved) {
            let short = short_role(&role.id);
            let mine = role.id == self.role;
            let mut cells = vec![Cell::new(if mine { "●" } else { "" }, Tone::Accent).pad(1)];
            cells.push(Cell::new(short, Tone::Text).pad(id_w));
            match resolved {
                Ok(r) => {
                    cells.push(Cell::flex(r.model_ref(), Tone::Text).pad(model_w));
                    cells.push(Cell::new(thinking_name(r.thinking), Tone::Muted).pad(8));
                    let source = if overrides.contains_key(normalize_role(&role.id)) {
                        Cell::new("this chat only", Tone::Accent)
                    } else if let Some(how) = r.fell_back() {
                        // #2246: no model of its own; say which fallback it runs on.
                        Cell::new(fallback_label(&how), Tone::Warning)
                    } else {
                        Cell::new(short_source(&r.source), Tone::Dim)
                    };
                    cells.push(source);
                }
                Err(e) => cells.push(Cell::flex(format!("no model: {e}"), Tone::Error)),
            }
            let mut row = InfoRow::new(cells).key(short);
            if !overrides.contains_key(normalize_role(&role.id)) {
                row = row.keys(&[]);
            }
            rows.push(row);
        }
        // #2246: what a role with no model of its own runs on.
        let fallback = models.fallback_model();
        rows.push(InfoRow::text(
            match &fallback.resolved {
                Ok(r) => format!(
                    "fallback: {} ({}) for roles with no model of their own",
                    r.model_ref(),
                    fallback_setting(&fallback)
                ),
                Err(e) => format!(
                    "fallback: {} does not resolve ({}); set [models] fallback",
                    fallback_setting(&fallback),
                    e.0
                ),
            },
            if fallback.resolved.is_ok() {
                Tone::Muted
            } else {
                Tone::Warning
            },
            0,
        ));
        rows.push(InfoRow::text(
            "● this chat's role · Enter picks a model (for this chat, or s to save it to \
             config.toml) · `amp-harness models set` in a terminal",
            Tone::Dim,
            0,
        ));
        let panel = InfoPanel::fixed("models by role", rows)
            .on_enter("pick a model", "/model {}")
            .with_key('x', "drop this chat's choice", "/model {} --reset");
        let mut reply = ConfigReply::panel(panel);
        reply.select = select.map(str::to_owned);
        reply
    }

    /// `/model <role>`: the models the role can run on; Enter uses one for this chat, `s` saves.
    fn role_picker(&self, role: &str) -> Result<ConfigReply, String> {
        let id = self.role_id(role)?;
        let short = short_role(&id).to_owned();
        let models = self.rt.current_models();
        let providers = self.rt.current_providers();
        let current = self
            .rt
            .catalog()
            .roles
            .get(&id)
            .and_then(|r| r.resolve_model(&models).ok())
            .map(|r| r.model_ref());
        let choices = crate::chat::model_choices(&models, &providers);
        if choices.is_empty() {
            return Err("no provider has a key yet: add one with /connect".to_owned());
        }
        let mut rows: Vec<InfoRow> = choices
            .iter()
            .map(|m| {
                let now = current.as_deref() == Some(m.reference.as_str());
                let mut cells = vec![
                    Cell::new(
                        if now { "●" } else { "○" },
                        if now { Tone::Success } else { Tone::Dim },
                    ),
                    Cell::flex(m.reference.clone(), Tone::Text),
                ];
                if now {
                    cells.push(Cell::new("current", Tone::Success));
                }
                if m.model.reasoning {
                    cells.push(Cell::new("thinking", Tone::Dim));
                }
                InfoRow::new(cells).key(m.reference.clone())
            })
            .collect();
        rows.push(InfoRow::text(
            format!(
                "Enter: {short} runs on it in this chat (workers started from now) · s: also save \
                 [roles.{short}] in config.toml"
            ),
            Tone::Dim,
            0,
        ));
        let mut reply = ConfigReply::panel(
            InfoPanel::fixed(format!("model for {short}"), rows)
                .on_enter("use", &format!("/model {short} {{}}"))
                .with_key('s', "save", &format!("/model {short} {{}} --save")),
        );
        reply.select = current;
        Ok(reply)
    }

    /// `/model <role> <provider/model> [thinking] [--save]`.
    async fn set_role(
        &self,
        role: &str,
        model: &str,
        thinking: Option<&str>,
        save: bool,
    ) -> Result<ConfigReply, String> {
        let id = self.role_id(role)?;
        let short = short_role(&id).to_owned();
        let models = self.rt.current_models();
        if !models.knows_model(model) {
            return Err(format!(
                "`{model}` is not a configured model (/model {short} lists them; /connect adds \
                 a provider)"
            ));
        }
        let provider = model.split_once('/').map(|(p, _)| p).unwrap_or(model);
        if self.rt.current_providers().get(provider).is_err() {
            return Err(format!(
                "provider `{provider}` has no key yet: /connect {provider} adds it"
            ));
        }
        let mut lines = Vec::new();
        if save {
            let outcome = models_cmd::set_role_in(
                &self.rt.paths,
                &short,
                model,
                thinking,
                ScopeArgs::default(),
                true,
            )
            .await
            .map_err(|e| e.to_string())?;
            self.rt.clear_role_override(&short);
            lines.push(format!(
                "{short}: {model} saved in {} ([roles.{short}]){}",
                outcome.file,
                if outcome.changed {
                    ""
                } else {
                    "; the file already said so"
                }
            ));
            lines.extend(outcome.warnings.iter().map(|w| format!("warning: {w}")));
        } else {
            let resolved = self
                .rt
                .set_role_override(&short, model, thinking)
                .map_err(|e| e.to_string())?;
            lines.push(format!(
                "{short}: {} • thinking {} for this chat (workers started from now; /model {short} \
                 {model} --save keeps it)",
                resolved.model_ref(),
                thinking_name(resolved.thinking)
            ));
        }
        let mut reply = self.roles_panel(Some(&short));
        reply.note = lines.first().cloned().map(|l| (l, false));
        reply.lines = lines;
        reply.reload = true;
        Ok(reply)
    }

    fn reset_role(&self, role: &str) -> Result<ConfigReply, String> {
        let id = self.role_id(role)?;
        let short = short_role(&id).to_owned();
        if !self.rt.clear_role_override(&short) {
            return Err(format!(
                "{short} has no model chosen in this chat; `amp-harness models reset {short}` \
                 drops a saved one"
            ));
        }
        let mut reply = self.roles_panel(Some(&short));
        let line = format!("{short}: back to its configured model");
        reply.note = Some((line.clone(), false));
        reply.lines = vec![line];
        reply.reload = true;
        Ok(reply)
    }

    // --------------------------------------------------------------------------------------------
    // /config
    // --------------------------------------------------------------------------------------------

    async fn config(&self) -> Result<ConfigReply, String> {
        let models = self.rt.current_models();
        let keys = SystemKeys::from_env();
        // #2246: with every agent of the session's roster, so roles that fall back are listed.
        let agents: Vec<_> = self
            .rt
            .catalog()
            .roles
            .roles()
            .map(|r| (r.model_role.clone(), r.model_hints()))
            .collect();
        let report = crate::config_cmd::build_report_with(&models, &keys, false, &agents).await;
        let env = |k: &str| std::env::var(k).ok().filter(|v| !v.trim().is_empty());
        let paths = &self.rt.paths;
        let user_text = paths
            .user_file
            .as_ref()
            .and_then(|p| std::fs::read_to_string(p).ok());
        let user_doc: Option<toml::Table> =
            user_text.as_deref().and_then(|t| toml::from_str(t).ok());
        let user_file = paths
            .user_file
            .as_ref()
            .map(|p| p.display().to_string())
            .unwrap_or_default();
        let from_file = |section: &str, key: &str| -> Option<String> {
            user_doc.as_ref()?.get(section)?.get(key).map(|v| match v {
                toml::Value::String(s) => s.clone(),
                other => other.to_string(),
            })
        };
        let setting = |section: &str, key: &str, var: &str, default: &str| -> (String, String) {
            if let Some(v) = env(var) {
                (v, format!("env {var}"))
            } else if let Some(v) = from_file(section, key) {
                (v, format!("config {user_file}"))
            } else {
                (default.to_owned(), "default".to_owned())
            }
        };

        let mut rows = Vec::new();
        let head = |rows: &mut Vec<InfoRow>, title: &str| {
            rows.push(InfoRow::new(vec![Cell::new(title, Tone::Accent)]));
        };
        let line = |rows: &mut Vec<InfoRow>, label: &str, value: String, source: String| {
            let mut cells = vec![
                Cell::new(label, Tone::Dim).pad(18),
                Cell::flex(tilde(&value), Tone::Text),
            ];
            if !source.is_empty() {
                cells.push(Cell::new(format!("({})", tilde(&source)), Tone::Muted));
            }
            rows.push(InfoRow::new(cells).indent(2));
        };

        head(&mut rows, "files");
        let exists = |p: &Option<PathBuf>| match p {
            Some(p) if p.exists() => p.display().to_string(),
            Some(p) => format!("{} (not there yet)", p.display()),
            None => "(none)".to_owned(),
        };
        line(
            &mut rows,
            "user config",
            exists(&paths.user_file),
            config_flag_source(&env),
        );
        line(
            &mut rows,
            "user dir",
            exists(&paths.user_dir),
            String::new(),
        );
        for sub in ["agents", "skills"] {
            line(
                &mut rows,
                sub,
                format!(
                    "{} · {}",
                    layer_dir(&paths.user_dir, sub),
                    layer_dir(&paths.repo_dir, sub)
                ),
                "user · repo".to_owned(),
            );
        }
        line(
            &mut rows,
            "repo config",
            match paths.repo_file() {
                Some(p) => exists(&Some(p)),
                None => "(not in an AMP project)".to_owned(),
            },
            String::new(),
        );
        line(
            &mut rows,
            "state",
            self.rt.state_dir.display().to_string(),
            String::new(),
        );

        if let Some(s) = &self.settings {
            head(&mut rows, "AMP server");
            line(&mut rows, "url", s.url.clone(), s.url_source.to_string());
            line(&mut rows, "auth", s.auth.describe(), String::new());
            line(
                &mut rows,
                "protocol",
                format!("{:?}", s.protocol).to_lowercase(),
                s.protocol_source.to_string(),
            );
            if let (Some(ca), Some(src)) = (&s.ca_file, &s.ca_file_source) {
                line(
                    &mut rows,
                    "ca file",
                    ca.display().to_string(),
                    src.to_string(),
                );
            }
        }

        head(&mut rows, "models");
        line(
            &mut rows,
            report.mode,
            report.selection.clone(),
            report.selection_source.clone(),
        );
        head(&mut rows, "providers (keys: where they come from)");
        for p in &report.providers {
            line(
                &mut rows,
                &p.id,
                format!("{} · {}", p.api, p.base_url),
                format!("key: {}", p.key_source),
            );
        }
        head(&mut rows, "roles (/model roles changes them)");
        let overrides = self.rt.role_overrides();
        for r in &report.roles {
            let source = if overrides.contains_key(&r.role) {
                amp_core::agent::ROLE_OVERRIDE_SOURCE.to_owned()
            } else {
                short_source(r.source.as_deref().unwrap_or(""))
            };
            match (&r.provider, &r.model, &r.error) {
                (Some(p), Some(m), _) => line(
                    &mut rows,
                    &r.role,
                    format!("{p}/{m} · {}", r.thinking.unwrap_or("-")),
                    source,
                ),
                (_, _, Some(e)) => line(&mut rows, &r.role, format!("error: {e}"), String::new()),
                _ => {}
            }
        }

        head(&mut rows, "MCP servers (/mcp)");
        let mcp = self.hub().config();
        if mcp.servers.is_empty() && mcp.broken.is_empty() {
            line(
                &mut rows,
                "(none)",
                "/mcp add <name> <url>".to_owned(),
                String::new(),
            );
        }
        for s in mcp.servers.values() {
            line(
                &mut rows,
                &s.name,
                format!(
                    "{} · {}{}",
                    s.transport.kind().as_str(),
                    mcp_crud::target(s),
                    if s.enabled { "" } else { " · disabled" }
                ),
                format!("{} {}", s.origin.layer.as_str(), s.origin.file.display()),
            );
        }
        for name in &mcp.broken {
            line(
                &mut rows,
                name,
                "configuration error".to_owned(),
                String::new(),
            );
        }
        let trust_all = mcp_trust::trust_all_from(&|k| std::env::var(k).ok());
        if trust_all {
            line(
                &mut rows,
                "repo trust",
                "every repo server allowed".to_owned(),
                "--trust-repo-mcp / env AMP_HARNESS_TRUST_REPO_MCP".to_owned(),
            );
        }

        head(&mut rows, "chat [tui]");
        let (theme, theme_src) = match &self.theme_flag {
            Some(t) => (t.clone(), "flag --theme".to_owned()),
            None => setting("tui", "theme", "AMP_HARNESS_THEME", "auto"),
        };
        line(&mut rows, "theme", theme, theme_src);
        let (v, src) = setting("tui", "color", "AMP_HARNESS_COLOR", "from COLORTERM");
        line(&mut rows, "color", v, src);
        let (v, src) = setting(
            "tui",
            "show_thinking",
            "AMP_HARNESS_SHOW_THINKING",
            "collapsed",
        );
        line(&mut rows, "show_thinking", v, src);
        let (v, src) = setting("tui", "mouse", "AMP_HARNESS_MOUSE", "true");
        line(&mut rows, "mouse", v, src);
        let (v, src) = setting(
            "tui",
            "padding",
            "AMP_HARNESS_PADDING",
            &amp_tui::DEFAULT_PADDING.to_string(),
        );
        line(&mut rows, "padding", v, src);
        let auto = if crate::reload::auto_reload(&|k| std::env::var(k).ok()) {
            "on"
        } else {
            "off"
        };
        line(
            &mut rows,
            "auto reload",
            auto.to_owned(),
            if env(crate::reload::ENV_AUTO_RELOAD).is_some() {
                format!("env {}", crate::reload::ENV_AUTO_RELOAD)
            } else {
                "default".to_owned()
            },
        );

        head(&mut rows, "tools, workers, secrets");
        let (bash, bash_src) = match (
            env(amp_core::agent::ENV_BASH_TIMEOUT_SECS),
            from_file("tools", "bash_timeout_secs"),
        ) {
            (Some(_), _) => (
                self.rt.bash_timeout,
                format!("env {}", amp_core::agent::ENV_BASH_TIMEOUT_SECS),
            ),
            (None, Some(_)) => (self.rt.bash_timeout, format!("config {user_file}")),
            (None, None) => (self.rt.bash_timeout, "default".to_owned()),
        };
        line(
            &mut rows,
            "bash timeout",
            bash.map_or("none".to_owned(), |s| format!("{s} s")),
            bash_src,
        );
        let (v, src) = setting(
            "tools",
            "mcp_max_output_tokens",
            amp_core::mcp_hub::ENV_MAX_OUTPUT_TOKENS,
            "25000",
        );
        line(&mut rows, "mcp output cap", format!("{v} tokens"), src);
        if let Some(w) = &self.workers {
            let (_, src) = setting(
                "workers",
                "max_parallel",
                amp_core::supervisor::ENV_MAX_WORKERS,
                "",
            );
            line(&mut rows, "max workers", w.max_parallel.to_string(), src);
            line(
                &mut rows,
                "worker limits",
                format!(
                    "{} min · {} turns",
                    w.timeout.as_secs() / 60,
                    if w.max_turns == 0 {
                        "no turn limit".to_owned()
                    } else {
                        w.max_turns.to_string()
                    }
                ),
                String::new(),
            );
        }
        let pass: Vec<&String> = self.rt.pass_env.iter().collect();
        line(
            &mut rows,
            "pass to bash",
            if pass.is_empty() {
                "(none)".to_owned()
            } else {
                pass.iter()
                    .map(|s| s.as_str())
                    .collect::<Vec<_>>()
                    .join(", ")
            },
            if pass.is_empty() {
                "default".to_owned()
            } else {
                format!("config {user_file}")
            },
        );
        line(
            &mut rows,
            "hidden from bash",
            format!("{} variables (names only)", self.rt.secret_env().len()),
            String::new(),
        );
        rows.push(InfoRow::text(
            "secrets show their source only · `amp-harness config check` checks every file",
            Tone::Dim,
            0,
        ));
        Ok(ConfigReply::panel(InfoPanel::fixed("config", rows)))
    }
}

// ------------------------------------------------------------------------------------------------
// Rows
// ------------------------------------------------------------------------------------------------

/// A dim column-header row: (title, width); width 0 is the last, flexible column.
fn header(columns: &[(&str, usize)]) -> InfoRow {
    InfoRow::new(
        columns
            .iter()
            .map(|(title, width)| {
                let cell = Cell::new(*title, Tone::Dim);
                if *width > 0 { cell.pad(*width) } else { cell }
            })
            .collect(),
    )
}

/// The repo servers the hub keeps off until trusted, by name.
fn untrusted(hub: &McpHub) -> BTreeMap<String, String> {
    hub.untrusted()
        .into_iter()
        .map(|u| (u.name.clone(), u.reason()))
        .collect()
}

/// A server's shared (not per-worktree) health.
fn shared_health<'a>(health: &'a [ServerHealth], name: &str) -> Option<&'a ServerHealth> {
    health
        .iter()
        .find(|h| h.name == name && h.instance.is_none())
        .or_else(|| health.iter().find(|h| h.name == name))
}

/// The tone and one-line text of a status.
fn status_view(status: &ServerStatus, tools: usize) -> (Tone, String) {
    match status {
        ServerStatus::Ready => (
            Tone::Success,
            format!("ready · {tools} tool{}", if tools == 1 { "" } else { "s" }),
        ),
        ServerStatus::Starting => (Tone::Muted, "starting…".to_owned()),
        ServerStatus::Idle => (Tone::Muted, "idle (lazy: r starts it)".to_owned()),
        ServerStatus::Disabled => (Tone::Dim, "disabled".to_owned()),
        ServerStatus::NeedsLogin { .. } => (Tone::Warning, status.to_string()),
        ServerStatus::Untrusted { .. } => (Tone::Warning, "not trusted (T trusts it)".to_owned()),
        ServerStatus::Failed { .. } | ServerStatus::Stopped { .. } => {
            (Tone::Error, status.to_string())
        }
    }
}

fn mark(status: &ServerStatus) -> Cell {
    match status {
        ServerStatus::Ready => Cell::new("●", Tone::Success),
        ServerStatus::Disabled | ServerStatus::Idle => Cell::new("○", Tone::Dim),
        ServerStatus::Starting => Cell::new("◌", Tone::Muted),
        ServerStatus::NeedsLogin { .. } | ServerStatus::Untrusted { .. } => {
            Cell::new("●", Tone::Warning)
        }
        ServerStatus::Failed { .. } | ServerStatus::Stopped { .. } => Cell::new("●", Tone::Error),
    }
}

/// The letter keys that make sense for a server in `status`.
fn server_keys(server: &McpServer, status: &ServerStatus, trusted_repo: bool) -> Vec<char> {
    let mut keys: Vec<char> = match status {
        ServerStatus::Disabled => vec!['e'],
        ServerStatus::Untrusted { .. } => vec!['T', 'd'],
        ServerStatus::NeedsLogin { .. } => vec!['l', 'r', 'd'],
        ServerStatus::Starting => vec!['t', 'd'],
        _ => vec!['r', 't', 'd'],
    };
    if server.transport.remote().is_some()
        && !matches!(
            status,
            ServerStatus::Disabled
                | ServerStatus::Untrusted { .. }
                | ServerStatus::NeedsLogin { .. }
        )
        && server
            .transport
            .remote()
            .is_some_and(|r| r.auth != AuthMode::None)
    {
        keys.push('l');
    }
    if trusted_repo && !matches!(status, ServerStatus::Untrusted { .. }) {
        keys.push('U');
    }
    keys
}

/// The `/mcp` list: a summary line, one row per server (broken ones too), and how to add one.
pub(crate) fn mcp_rows(
    config: &McpConfig,
    health: &[ServerHealth],
    untrusted: &BTreeMap<String, String>,
) -> Vec<InfoRow> {
    let mut rows = Vec::new();
    let names: Vec<&String> = config.servers.keys().chain(config.broken.iter()).collect();
    if names.is_empty() {
        rows.push(InfoRow::text("no MCP servers configured yet", Tone::Dim, 0));
    } else {
        let ready = health
            .iter()
            .filter(|h| h.instance.is_none() && h.status == ServerStatus::Ready)
            .count();
        let enabled = config.servers.values().filter(|s| s.enabled);
        let waiting = enabled
            .clone()
            .filter(|s| untrusted.contains_key(&s.name))
            .count();
        let should_run = enabled.count() - waiting;
        let disabled = config.servers.values().filter(|s| !s.enabled).count();
        let mut summary = format!("{ready} of {should_run} ready");
        if waiting > 0 {
            summary.push_str(&format!(" · {waiting} not trusted"));
        }
        if disabled > 0 {
            summary.push_str(&format!(" · {disabled} disabled"));
        }
        if !config.broken.is_empty() {
            summary.push_str(&format!(" · {} with errors", config.broken.len()));
        }
        rows.push(InfoRow::new(vec![Cell::new(summary, Tone::Dim)]));
    }
    let name_w = names.iter().map(|n| n.len()).max().unwrap_or(0);
    for name in names {
        let Some(server) = config.servers.get(name) else {
            rows.push(
                InfoRow::new(vec![
                    Cell::new("●", Tone::Error),
                    Cell::new(name.clone(), Tone::Text).pad(name_w),
                    Cell::flex(
                        "configuration error (`amp-harness config check` lists it)",
                        Tone::Error,
                    ),
                ])
                .key(name.clone())
                .keys(&[]),
            );
            continue;
        };
        let h = shared_health(health, name);
        let status = match (h, server.enabled) {
            (_, false) => ServerStatus::Disabled,
            (Some(h), _) => h.status.clone(),
            (None, _) if untrusted.contains_key(name) => ServerStatus::Untrusted {
                reason: untrusted[name].clone(),
            },
            (None, _) => ServerStatus::Idle,
        };
        let (tone, text) = status_view(&status, h.map_or(0, |h| h.tools));
        let trusted_repo = server.is_repo() && !untrusted.contains_key(name);
        rows.push(
            InfoRow::new(vec![
                mark(&status),
                Cell::new(name.clone(), Tone::Text).pad(name_w),
                Cell::new(
                    format!(
                        "{} · {}",
                        server.transport.kind().as_str(),
                        server.origin.layer.as_str()
                    ),
                    Tone::Dim,
                )
                .pad(13),
                Cell::flex(text, tone),
            ])
            .key(name.clone())
            .keys(&server_keys(server, &status, trusted_repo)),
        );
    }
    rows.push(InfoRow::text(
        "add one: /mcp add <name> <url> · stdio, edit, import, secrets: `amp-harness mcp …` in a \
         terminal",
        Tone::Dim,
        0,
    ));
    rows
}

/// `/mcp <name>`'s rows.
pub(crate) fn mcp_detail_rows(
    server: &McpServer,
    health: Option<&ServerHealth>,
    tools: &[String],
    untrusted: &BTreeMap<String, String>,
    repo_dir: Option<&Path>,
) -> Vec<InfoRow> {
    let name = &server.name;
    let status = match (health, server.enabled) {
        (_, false) => ServerStatus::Disabled,
        (Some(h), _) => h.status.clone(),
        (None, _) => match untrusted.get(name) {
            Some(reason) => ServerStatus::Untrusted {
                reason: reason.clone(),
            },
            None => ServerStatus::Idle,
        },
    };
    let (tone, text) = status_view(&status, health.map_or(0, |h| h.tools));
    let trusted_repo = server.is_repo() && !untrusted.contains_key(name);
    let mut rows = vec![
        InfoRow::new(vec![
            mark(&status),
            Cell::new(name.clone(), Tone::Text),
            Cell::flex(text, tone),
        ])
        .key(name.clone())
        .keys(&server_keys(server, &status, trusted_repo)),
    ];
    let field = |label: &str, value: String, tone: Tone| InfoRow::field(label, LABEL, value, tone);
    rows.push(field(
        "type",
        format!(
            "{}{}",
            server.transport.kind().as_str(),
            if server.scope == RunScope::Worktree {
                " · one instance per worker worktree"
            } else {
                ""
            }
        ),
        Tone::Text,
    ));
    rows.push(field(
        "from",
        format!(
            "{} config · {}",
            server.origin.layer.as_str(),
            show_path(&server.origin.file.display().to_string(), repo_dir)
        ),
        Tone::Muted,
    ));
    match &server.transport {
        Transport::Stdio(s) => {
            rows.push(field("runs", mcp_crud::target(server), Tone::Text));
            if let Some(cwd) = &s.cwd {
                rows.push(field("cwd", cwd.clone(), Tone::Muted));
            }
            for (k, v) in &s.env {
                rows.push(field("env", format!("{k}={v}"), Tone::Muted));
            }
            if let Some(f) = &s.env_file {
                rows.push(field("env file", f.clone(), Tone::Muted));
            }
        }
        Transport::Http(r) | Transport::Sse(r) => {
            rows.push(field("url", r.url.clone(), Tone::Text));
            for (k, v) in &r.headers {
                rows.push(field("header", format!("{k}: {v}"), Tone::Muted));
            }
            rows.push(field(
                "auth",
                match r.auth {
                    AuthMode::Auto => "auto (OAuth when the server asks; l logs in)",
                    AuthMode::Oauth => "OAuth (l logs in)",
                    AuthMode::None => "none",
                }
                .to_owned(),
                Tone::Muted,
            ));
            if let Some(var) = &r.bearer_env {
                rows.push(field(
                    "secret",
                    format!("bearer token: env {var}"),
                    Tone::Muted,
                ));
            }
        }
    }
    // Secrets by their source only; nothing is read from the keychain here.
    for item in mcp_crud::keychain_items(server) {
        rows.push(field(
            "secret",
            format!("{}: keychain {}", item.label, item.account),
            Tone::Muted,
        ));
    }
    rows.push(field(
        "enabled",
        if server.enabled {
            "yes".to_owned()
        } else {
            format!(
                "no (set in {})",
                show_path(&server.enabled_origin.file.display().to_string(), repo_dir)
            )
        },
        Tone::Muted,
    ));
    if server.is_repo() {
        rows.push(field(
            "trust",
            match untrusted.get(name) {
                Some(reason) => format!("not trusted: {reason}"),
                None => "trusted (U forgets it)".to_owned(),
            },
            if untrusted.contains_key(name) {
                Tone::Warning
            } else {
                Tone::Muted
            },
        ));
    }
    rows.push(field("roles", server.roles.join(", "), Tone::Muted));
    rows.push(field(
        "startup",
        format!(
            "{} · waits {} s · calls time out after {} s",
            match server.startup {
                Startup::Background => "background",
                Startup::Eager => "eager",
                Startup::Lazy => "lazy",
            },
            server.startup_timeout.as_secs(),
            server.call_timeout.as_secs()
        ),
        Tone::Muted,
    ));
    if let Some(h) = health {
        let mut about = Vec::new();
        if let Some(s) = &h.server {
            about.push(format!("server {s}"));
        }
        if let Some(p) = &h.protocol {
            about.push(format!("protocol {p}"));
        }
        if let Some(pid) = h.pid {
            about.push(format!("pid {pid}"));
        }
        if h.restarts > 0 {
            about.push(format!("{} restarts", h.restarts));
        }
        if !about.is_empty() {
            rows.push(field("process", about.join(" · "), Tone::Muted));
        }
        if let Some(log) = &h.stderr_log {
            rows.push(field(
                "stderr",
                tilde(&log.display().to_string()),
                Tone::Muted,
            ));
        }
    }
    rows.push(InfoRow::new(vec![Cell::new(
        format!("tools ({})", tools.len()),
        Tone::Dim,
    )]));
    if tools.is_empty() {
        rows.push(InfoRow::text("(none listed: not connected)", Tone::Dim, 2));
    } else {
        rows.push(InfoRow::text(tools.join(", "), Tone::Text, 2));
    }
    let scope_flag = if server.is_repo() {
        " --scope repo"
    } else {
        ""
    };
    rows.push(InfoRow::text(
        format!(
            "change it in a terminal: `amp-harness mcp edit {name}{scope_flag}` · `amp-harness \
             mcp remove {name}{scope_flag}`"
        ),
        Tone::Dim,
        0,
    ));
    rows
}

/// What the person runs in a terminal for a `/mcp` verb the chat leaves to the CLI.
fn terminal_only(verb: &str, rest: &[&str]) -> ConfigReply {
    let why = match verb {
        "edit" => "opens $EDITOR",
        "import" => "asks about each server and its secrets",
        "secret" => "asks for the value hidden",
        "add-json" => "may hold secrets",
        _ => "changes files and the keychain",
    };
    ConfigReply {
        lines: vec![format!(
            "/mcp {verb} runs in a terminal ({why}):\n  amp-harness mcp {verb}{}{}",
            if rest.is_empty() { "" } else { " " },
            rest.join(" ")
        )],
        ..ConfigReply::default()
    }
}

/// The last `n` non-empty lines of a stderr log.
fn stderr_tail(path: Option<&Path>, n: usize) -> Vec<String> {
    let Some(text) = path.and_then(|p| std::fs::read_to_string(p).ok()) else {
        return Vec::new();
    };
    let lines: Vec<&str> = text.lines().filter(|l| !l.trim().is_empty()).collect();
    lines[lines.len().saturating_sub(n)..]
        .iter()
        .map(|l| (*l).to_owned())
        .collect()
}

fn origin_tone(origin: &str) -> Tone {
    match origin {
        "repo" => Tone::Warning,
        "user" => Tone::Accent,
        _ => Tone::Muted,
    }
}

fn layer_dir(dir: &Option<PathBuf>, sub: &str) -> String {
    match dir {
        Some(d) => d.join(sub).display().to_string(),
        None => "no repo layer".to_owned(),
    }
}

/// The "chosen by" cell of a role that fell back (#2246): `fallback: worker-backend's model`,
/// `fallback: [models] fallback`, `based on reviewer` or `agent file model`. Short, so the model
/// column keeps its width; the footer and /config name where the fallback was set.
fn fallback_label(how: &str) -> String {
    if let Some(role) = how.strip_prefix("fallback ") {
        format!("fallback: {role}'s model")
    } else if let Some(role) = how.strip_prefix("based_on ") {
        format!("based on {role}")
    } else if how == "[models] fallback" {
        "fallback: [models] fallback".to_owned()
    } else {
        how.to_owned()
    }
}

/// `worker-backend's model, default` / `[models] fallback, config.toml`.
fn fallback_setting(f: &amp_core::models::FallbackModel) -> String {
    if f.source.starts_with("inherit") {
        return short_source(&f.source);
    }
    let what = if f.setting.contains('/') {
        "[models] fallback".to_owned()
    } else {
        format!("{}'s model", f.setting)
    };
    format!("{what}, {}", short_source(&f.source))
}

/// `config /Users/x/.config/amp-harness/config.toml` -> `config config.toml`; the panels show
/// the full paths under /config.
fn short_source(source: &str) -> String {
    let mut out = String::new();
    for word in source.split(' ') {
        if !out.is_empty() {
            out.push(' ');
        }
        if word.contains('/') {
            let p = Path::new(word.trim_end_matches([')', ',']));
            let tail = word.len() - word.trim_end_matches([')', ',']).len();
            let parent = p
                .parent()
                .and_then(|x| x.file_name())
                .map(|x| x.to_string_lossy().into_owned());
            let file = p
                .file_name()
                .map(|x| x.to_string_lossy().into_owned())
                .unwrap_or_default();
            match parent {
                Some(parent) if parent == "harness" => out.push_str(&format!("repo/{file}")),
                _ => out.push_str(&file),
            }
            out.push_str(&word[word.len() - tail..]);
        } else {
            out.push_str(word);
        }
    }
    out
}

/// A file for a detail panel: inside the repo as `<repo>/…`, under `$HOME` as `~/…`.
fn show_path(path: &str, repo_dir: Option<&Path>) -> String {
    if let Some(repo) = repo_dir.and_then(|d| d.ancestors().nth(2))
        && let Ok(rest) = Path::new(path).strip_prefix(repo)
    {
        return format!("<repo>/{}", rest.display());
    }
    tilde(path)
}

/// `path` with a leading `$HOME` written as `~` (anywhere in a value or a source).
fn tilde(text: &str) -> String {
    match std::env::var("HOME") {
        Ok(home) if home.len() > 1 => text.replace(&format!("{home}/"), "~/"),
        _ => text.to_owned(),
    }
}

/// Where `--config` came from, for `/config`.
fn config_flag_source(env: &dyn Fn(&str) -> Option<String>) -> String {
    if env("AMP_HARNESS_CONFIG").is_some() {
        "env AMP_HARNESS_CONFIG".to_owned()
    } else if env("AMP_HARNESS_CONFIG_DIR").is_some() {
        "env AMP_HARNESS_CONFIG_DIR".to_owned()
    } else {
        String::new()
    }
}

/// An agent's `permission` frontmatter, one line per permission.
fn permission_lines(frontmatter: Option<&Value>) -> Vec<String> {
    match frontmatter.and_then(|f| f.get("permission")) {
        Some(Value::Object(map)) if !map.is_empty() => map
            .iter()
            .map(|(perm, rule)| {
                let text = match rule {
                    Value::String(a) => a.clone(),
                    Value::Object(patterns) => patterns
                        .iter()
                        .map(|(p, a)| format!("{p:?} {}", a.as_str().unwrap_or("?")))
                        .collect::<Vec<_>>()
                        .join(", "),
                    other => other.to_string(),
                };
                format!("{perm}: {text}")
            })
            .collect(),
        Some(Value::String(a)) => vec![format!("everything: {a}")],
        _ => vec!["(none: every tool allowed)".to_owned()],
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn detail_paths_are_repo_relative_or_under_home() {
        let repo = Path::new("/work/proj/.amp/harness");
        assert_eq!(
            show_path("/work/proj/.amp/harness/agents/x.md", Some(repo)),
            "<repo>/.amp/harness/agents/x.md"
        );
        assert_eq!(show_path("/elsewhere/x.md", Some(repo)), "/elsewhere/x.md");
        assert_eq!(show_path("/elsewhere/x.md", None), "/elsewhere/x.md");
    }

    #[test]
    fn fallback_roles_say_which_fallback() {
        assert_eq!(
            fallback_label("fallback worker-backend"),
            "fallback: worker-backend's model"
        );
        assert_eq!(fallback_label("based_on reviewer"), "based on reviewer");
        assert_eq!(
            fallback_label("[models] fallback"),
            "fallback: [models] fallback"
        );
        assert_eq!(fallback_label("agent file model"), "agent file model");
    }

    #[test]
    fn sources_name_the_file_not_the_whole_path() {
        assert_eq!(
            short_source("config /Users/ada/.config/amp-harness/config.toml"),
            "config config.toml"
        );
        assert_eq!(
            short_source("config /a/b/config.toml + repo /r/.amp/harness/config.toml"),
            "config config.toml + repo repo/config.toml"
        );
        assert_eq!(short_source("profile opencode"), "profile opencode");
        assert_eq!(
            short_source("based_on worker-backend (config /x/config.toml)"),
            "based_on worker-backend (config config.toml)"
        );
    }
}
