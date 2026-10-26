//! doctor's MCP server checks (#2246, H16): `mcp` (how many of the configured servers are ready)
//! and one `mcp:<name>` per server.
//!
//! Each enabled, trusted server is connected through a short-lived hub, the same way `mcp list`
//! and sessions start it (env stripping, trust store, keychain), so doctor sees what a session
//! would: ready with its tools, needs-login, a missing secret, a command that is not there.
//! A server's report has its transport, layer and file, trust (repo servers), how it
//! authenticates (secrets by their source only), its health, and which roles may use it (the
//! per-server `roles` filter, then each role's `mcp` permission over its tools).
//!
//! A table with configuration errors fails (the person's own file is wrong); a server that
//! is not trusted, needs a login or does not start warns, as a broken server does not stop a
//! session either. `--offline` reports the configuration without connecting.

use std::collections::BTreeMap;

use amp_core::{
    mcp_config::{AuthMode, McpConfig, McpServer, Severity, TOOL_PREFIX},
    mcp_hub::{ServerHealth, ServerStatus},
    mcp_trust::TrustState,
    roles::RoleRegistry,
};
use amp_mcp::{CredentialBackend, oauth};
use serde::Serialize;
use serde_json::Value;

use super::{CheckStatus, Finding, providers::Named};
use crate::{agents_cmd::Ctx, mcp_crud, setup};

/// `doctor --json` `.mcp`. Add fields, never rename or remove them.
#[derive(Debug, Default, Serialize)]
pub struct McpReport {
    pub servers: Vec<ServerReport>,
    /// Tables that did not load, with their errors.
    pub broken: Vec<BrokenReport>,
    /// Repo servers ignored because one of the person's own has the same name (#2331).
    pub shadowed: Vec<ShadowReport>,
    /// Ready servers out of the enabled, trusted ones that were connected.
    pub ready: usize,
    pub checked: usize,
}

#[derive(Debug, Serialize)]
pub struct ServerReport {
    pub name: String,
    /// stdio, http or sse.
    #[serde(rename = "type")]
    pub kind: &'static str,
    /// user or repo.
    pub layer: &'static str,
    pub file: String,
    pub enabled: bool,
    /// Repo servers only: trusted, override, untrusted, changed.
    pub trust: Option<TrustState>,
    /// The command it runs, or the URL it connects to.
    pub target: String,
    /// How it authenticates, secrets by their source only (`bearer token: env GH_TOKEN (set)`).
    pub auth: Vec<String>,
    /// The health as the hub reports it (`{"state": "ready"}`, `{"state": "needs-login", ...}`);
    /// `null` when it was not connected (disabled, untrusted, `--offline`).
    pub status: Option<Value>,
    pub tools: Option<Vec<String>>,
    pub server: Option<String>,
    pub protocol: Option<String>,
    pub stderr_log: Option<String>,
    /// Which roles may use it, and how many of its tools each one sees.
    pub roles: Vec<RoleAccess>,
}

#[derive(Debug, Clone, Serialize)]
pub struct RoleAccess {
    pub role: String,
    /// `all` (every tool), `some` (the `mcp` permission denies some), `none`.
    pub access: &'static str,
    /// Tools this role sees (`null` when the tool list is unknown).
    pub tools: Option<usize>,
    /// Why not all: `roles filter` or `mcp policy`.
    pub why: Option<&'static str>,
}

/// A repo `[mcp.<name>]` left out for the person's own `<name>`: theirs runs, the repo's never
/// starts.
#[derive(Debug, Serialize)]
pub struct ShadowReport {
    pub name: String,
    pub repo_file: String,
    pub user_file: String,
    pub warning: String,
}

#[derive(Debug, Serialize)]
pub struct BrokenReport {
    pub name: String,
    pub problems: Vec<String>,
}

/// The `mcp` and `mcp:<name>` checks.
pub async fn check(
    ctx: &Ctx,
    roles: Option<&RoleRegistry>,
    offline: bool,
) -> (McpReport, Vec<Named>) {
    let config = McpConfig::load(&ctx.paths);
    let mut report = McpReport::default();
    let mut out: Vec<Named> = Vec::new();
    if config.servers.is_empty() && config.broken.is_empty() {
        out.push((
            "mcp".to_owned(),
            Finding::new(
                CheckStatus::Skip,
                "no MCP servers configured (`amp-harness mcp add` adds one)",
            ),
        ));
        return (report, out);
    }

    let store = mcp_crud::trust_store();
    let trust = |s: &McpServer| s.is_repo().then(|| mcp_crud::trust_of(s, store.as_ref()));
    let runnable: Vec<McpServer> = config
        .servers
        .values()
        .filter(|s| s.enabled && trust(s).is_none_or(|t| t.allows()))
        .cloned()
        .collect();
    let probe = if offline || runnable.is_empty() {
        None
    } else {
        match mcp_crud::probe(ctx, runnable.clone()).await {
            Ok(p) => Some(p),
            Err(e) => {
                out.push((
                    "mcp".to_owned(),
                    Finding::new(
                        CheckStatus::Warn,
                        format!("could not start the servers: {}", e.message),
                    )
                    .fix("set HOME or XDG_STATE_HOME (MCP servers keep logs and trust there)"),
                ));
                None
            }
        }
    };
    let backend = CredentialBackend::resolve().ok();
    let service = setup::keychain_for_keys().ok();

    for s in config.servers.values() {
        let t = trust(s);
        let health = probe.as_ref().and_then(|p| p.health.get(&s.name));
        let tools = probe.as_ref().and_then(|p| p.tools.get(&s.name)).cloned();
        let auth = auth_lines(ctx, s, service.as_deref(), backend.as_ref()).await;
        let access = role_access(s, roles, tools.as_deref());
        let finding = server_finding(s, t, health, tools.as_deref(), &auth, &access, offline);
        if health.is_some() && s.enabled {
            report.checked += 1;
            if matches!(health.map(|h| &h.status), Some(ServerStatus::Ready)) {
                report.ready += 1;
            }
        }
        report.servers.push(ServerReport {
            name: s.name.clone(),
            kind: s.transport.kind().as_str(),
            layer: s.origin.layer.as_str(),
            file: s.origin.file.display().to_string(),
            enabled: s.enabled,
            trust: t,
            target: mcp_crud::target(s),
            auth,
            status: health.and_then(|h| serde_json::to_value(&h.status).ok()),
            tools,
            server: health.and_then(|h| h.server.clone()),
            protocol: health.and_then(|h| h.protocol.clone()),
            stderr_log: health
                .and_then(|h| h.stderr_log.as_ref())
                .map(|p| p.display().to_string()),
            roles: access,
        });
        out.push((format!("mcp:{}", s.name), finding));
    }
    for name in &config.broken {
        let problems: Vec<String> = config
            .problems
            .iter()
            .filter(|p| {
                p.severity == Severity::Error
                    && (p.key == format!("mcp.{name}")
                        || p.key.starts_with(&format!("mcp.{name}.")))
            })
            .map(|p| p.to_string())
            .collect();
        out.push((
            format!("mcp:{name}"),
            Finding::new(
                CheckStatus::Fail,
                format!("configuration error: {}", problems.join("; ")),
            )
            .fix(format!(
                "fix [mcp.{name}] as the error says (`amp-harness mcp edit {name}`), or remove it \
                 with `amp-harness mcp remove {name}`; `amp-harness config check` checks again"
            )),
        ));
        report.broken.push(BrokenReport {
            name: name.clone(),
            problems,
        });
    }
    // #2331: a repo server ignored because one of the person's own has its name.
    for sh in &config.shadowed {
        let name = &sh.name;
        out.push((
            format!("mcp:{name}:repo"),
            Finding::new(CheckStatus::Warn, sh.warning()).fix(format!(
                "rename [mcp.{name}] in {} (e.g. [mcp.{name}-repo]) to use both, or remove \
                 [mcp.{name}] from {} to use the repo's",
                sh.repo_file.display(),
                sh.user_file.display()
            )),
        ));
        report.shadowed.push(ShadowReport {
            name: name.clone(),
            repo_file: sh.repo_file.display().to_string(),
            user_file: sh.user_file.display().to_string(),
            warning: sh.warning(),
        });
    }
    let enabled = config.servers.values().filter(|s| s.enabled).count();
    let summary = if offline {
        Finding::new(
            CheckStatus::Skip,
            format!(
                "--offline: {} configured ({enabled} enabled), not connected",
                config.servers.len() + config.broken.len()
            ),
        )
    } else {
        let mut detail = format!("{} of {} ready", report.ready, enabled);
        let disabled = config.servers.len() - enabled;
        if disabled > 0 {
            detail.push_str(&format!(", {disabled} disabled"));
        }
        if !config.broken.is_empty() {
            detail.push_str(&format!(", {} with errors", config.broken.len()));
        }
        if !config.shadowed.is_empty() {
            detail.push_str(&format!(
                ", {} repo server{} ignored (same name as yours)",
                config.shadowed.len(),
                if config.shadowed.len() == 1 { "" } else { "s" }
            ));
        }
        if let Some(p) = &probe {
            detail.push_str(&format!(" (connected in {} ms)", p.elapsed.as_millis()));
        }
        Finding::new(CheckStatus::Ok, detail)
    };
    if !out.iter().any(|(n, _)| n == "mcp") {
        out.insert(0, ("mcp".to_owned(), summary));
    }
    (report, out)
}

/// One server's finding.
fn server_finding(
    s: &McpServer,
    trust: Option<TrustState>,
    health: Option<&ServerHealth>,
    tools: Option<&[String]>,
    auth: &[String],
    access: &[RoleAccess],
    offline: bool,
) -> Finding {
    let name = &s.name;
    let what = format!(
        "{} ({}, {})",
        s.transport.kind().as_str(),
        s.origin.layer.as_str(),
        mcp_crud::target(s)
    );
    if !s.enabled {
        return Finding::new(
            CheckStatus::Skip,
            format!("{what}: disabled in {}", s.enabled_origin.file.display()),
        );
    }
    if let Some(t) = trust
        && !t.allows()
    {
        let changed = if t == TrustState::Changed {
            "its configuration changed since you trusted it"
        } else {
            "not trusted"
        };
        return Finding::new(
            CheckStatus::Warn,
            format!("{what}: a repo server, {changed}, so it never starts"),
        )
        .fix(format!(
            "if you trust what it runs, run with `--trust-repo` to start it for one run, or \
             `amp-harness mcp trust {name}` to trust it as it is now"
        ));
    }
    if offline {
        return Finding::new(
            CheckStatus::Skip,
            format!(
                "{what}: --offline, not connected; auth: {}",
                auth.join("; ")
            ),
        );
    }
    let Some(h) = health else {
        return Finding::new(CheckStatus::Warn, format!("{what}: no health reported"))
            .fix(format!("`amp-harness mcp test {name}`"));
    };
    let mut tail = format!("auth: {}", auth.join("; "));
    tail.push_str(&format!("; roles: {}", access_summary(access)));
    match &h.status {
        ServerStatus::Ready => {
            let n = tools.map_or(h.tools, <[String]>::len);
            Finding::new(
                CheckStatus::Ok,
                format!(
                    "{what}: ready, {n} tool{}{}; {tail}",
                    if n == 1 { "" } else { "s" },
                    h.protocol
                        .as_deref()
                        .map(|p| format!(", MCP {p}"))
                        .unwrap_or_default()
                ),
            )
        }
        ServerStatus::NeedsLogin { reason } => Finding::new(
            CheckStatus::Warn,
            format!("{what}: needs a login ({reason}); {tail}"),
        )
        .fix(format!(
            "`amp-harness mcp login {name}` (or check the token it sends with `amp-harness \
                     mcp get {name}`)"
        )),
        ServerStatus::Untrusted { reason } => {
            Finding::new(CheckStatus::Warn, format!("{what}: {reason}"))
                .fix(format!("`amp-harness mcp trust {name}`"))
        }
        ServerStatus::Failed { reason, .. } | ServerStatus::Stopped { reason } => {
            let mut detail = format!("{what}: {} ({reason}); {tail}", h.status.word());

            // A stdio server that never got as far as a client has no log in its health yet.
            let log = h.stderr_log.clone().or_else(|| {
                (s.transport.stdio().is_some())
                    .then(|| mcp_crud::state().ok())
                    .flatten()
                    .map(|st| st.join("mcp-logs").join(format!("{name}.log")))
            });
            let log_tail: Vec<String> = mcp_crud::stderr_tail(log.as_deref(), 6)
                .into_iter()
                .filter(|l| !l.starts_with("--- amp-harness:"))
                .collect();
            let log_tail = &log_tail[log_tail.len().saturating_sub(2)..];
            if !log_tail.is_empty() {
                detail.push_str(&format!("; stderr: {}", log_tail.join(" | ")));
            }
            Finding::new(CheckStatus::Warn, detail).fix(format!(
                "fix what the reason names (a missing secret: `amp-harness mcp secret {name}`; a \
                 missing variable: export it), then `amp-harness mcp test {name}` shows the \
                 server's stderr"
            ))
        }
        other => Finding::new(
            CheckStatus::Warn,
            format!(
                "{what}: still {} after its startup timeout ({} s)",
                other.word(),
                s.startup_timeout.as_secs()
            ),
        )
        .fix(format!(
            "`amp-harness mcp test {name}`; raise startup_timeout_secs with `amp-harness mcp edit \
             {name}` if it is just slow"
        )),
    }
}

/// How a server authenticates, secrets by their source and state only (never a value).
async fn auth_lines(
    ctx: &Ctx,
    s: &McpServer,
    service: Option<&str>,
    backend: Option<&CredentialBackend>,
) -> Vec<String> {
    let mut lines: Vec<String> = mcp_crud::secret_rows(s, service)
        .iter()
        .map(|row| {
            format!(
                "{}: {} ({})",
                row["secret"].as_str().unwrap_or_default(),
                row["source"].as_str().unwrap_or_default(),
                row["state"].as_str().unwrap_or_default()
            )
        })
        .collect();
    if let Some(r) = s.transport.remote() {
        match r.auth {
            AuthMode::None => lines.push("OAuth off".to_owned()),
            mode => {
                let login = match (backend, mcp_crud::login_key(ctx, s)) {
                    (Some(b), Some(key)) => oauth::has_login_key(&key, b).await.ok(),
                    _ => None,
                };
                let word = if mode == AuthMode::Oauth {
                    "OAuth"
                } else {
                    "OAuth if asked"
                };
                lines.push(match login {
                    Some(true) => format!("{word}: logged in"),
                    Some(false) => format!("{word}: no login stored"),
                    None => format!("{word}: login unknown"),
                });
            }
        }
    }
    if lines.is_empty() {
        lines.push("none".to_owned());
    }
    lines
}

/// Which roles see the server and how many of its tools (the `roles` filter, then each role's
/// `mcp` permission, as a session applies them).
fn role_access(
    s: &McpServer,
    roles: Option<&RoleRegistry>,
    tools: Option<&[String]>,
) -> Vec<RoleAccess> {
    let Some(roles) = roles else {
        return Vec::new();
    };
    let cwd = std::env::current_dir().unwrap_or_default();
    roles
        .roles()
        .map(|role| {
            let id = role.id.strip_prefix("amp-").unwrap_or(&role.id).to_owned();
            if !s.allows_role(&role.id) {
                return RoleAccess {
                    role: id,
                    access: "none",
                    tools: Some(0),
                    why: Some("roles filter"),
                };
            }
            let Some(tools) = tools else {
                return RoleAccess {
                    role: id,
                    access: "all",
                    tools: None,
                    why: None,
                };
            };
            let policy = role.tool_policy(cwd.clone());
            let names: Vec<(String, String)> = tools
                .iter()
                .map(|t| {
                    (
                        format!("{TOOL_PREFIX}{}__{t}", s.name),
                        format!("{}/{t}", s.name),
                    )
                })
                .collect();
            policy.learn_mcp_subjects(names.iter().map(|(n, sub)| (n.as_str(), sub.as_str())));
            let seen = names.iter().filter(|(n, _)| policy.exposes(n)).count();
            let (access, why) = match seen {
                n if n == tools.len() => ("all", None),
                0 => ("none", Some("mcp policy")),
                _ => ("some", Some("mcp policy")),
            };
            RoleAccess {
                role: id,
                access,
                tools: Some(seen),
                why,
            }
        })
        .collect()
}

/// `all roles` or `12 roles: all tools; reviewer: none (mcp policy)`.
fn access_summary(access: &[RoleAccess]) -> String {
    if access.is_empty() {
        return "unknown (the agents did not load)".to_owned();
    }
    let mut groups: BTreeMap<String, Vec<&str>> = BTreeMap::new();
    for a in access {
        let key = match (a.access, a.tools, a.why) {
            ("all", _, _) => "all tools".to_owned(),
            ("none", _, Some(why)) => format!("none ({why})"),
            (_, Some(n), Some(why)) => {
                format!("{n} tool{} ({why})", if n == 1 { "" } else { "s" })
            }
            (other, _, _) => other.to_owned(),
        };
        groups.entry(key).or_default().push(&a.role);
    }
    if groups.len() == 1 && groups.contains_key("all tools") {
        return format!("all {} roles", access.len());
    }
    groups
        .iter()
        .map(|(what, who)| {
            if who.len() > 4 {
                format!("{} roles: {what}", who.len())
            } else {
                format!("{}: {what}", who.join(", "))
            }
        })
        .collect::<Vec<_>>()
        .join(", ")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn a(
        role: &str,
        access: &'static str,
        tools: Option<usize>,
        why: Option<&'static str>,
    ) -> RoleAccess {
        RoleAccess {
            role: role.into(),
            access,
            tools,
            why,
        }
    }

    #[test]
    fn access_is_summarised_by_group() {
        assert_eq!(
            access_summary(&[
                a("manager", "all", Some(2), None),
                a("x", "all", None, None)
            ]),
            "all 2 roles"
        );
        let many: Vec<RoleAccess> = (0..6)
            .map(|i| a(&format!("r{i}"), "all", Some(2), None))
            .chain([
                a("reviewer", "none", Some(0), Some("mcp policy")),
                a("manager", "none", Some(0), Some("roles filter")),
                a("worker", "some", Some(1), Some("mcp policy")),
            ])
            .collect();
        assert_eq!(
            access_summary(&many),
            "worker: 1 tool (mcp policy), 6 roles: all tools, reviewer: none (mcp policy), \
             manager: none (roles filter)"
        );
        assert_eq!(access_summary(&[]), "unknown (the agents did not load)");
    }
}
