//! `amp-harness doctor [--json]`: can this machine use AMP, and how is it connected?
//!
//! Checks, in order (later ones are skipped when an earlier one makes them impossible):
//! `config` (settings resolve), `credentials` (where logins live; D3 warns on the file fallback),
//! `connect` (reachability + negotiated protocol), `tools` (tools/list), `contract` (D11 tools
//! present), `whoami`, `project` (`.amp.json`, then `amp_get_project`).
//!
//! Never prints a secret: auth is described by its source (`static token (env AMP_TOKEN)`,
//! `oauth (macOS Keychain ...)`), never by its value. Exit code 1 when any check fails.

use amp_core::{
    project::ProjectMarker,
    tools::{call_json, missing_contract_tools},
};
use amp_mcp::{Auth, CredentialBackend, Settings, oauth};
use serde::Serialize;
use serde_json::json;

use crate::{
    conn::{CliError, ConnArgs, connect, print_json},
    whoami,
};

/// `doctor --json`. Add fields, never rename or remove them.
#[derive(Debug, Serialize)]
pub struct Report {
    /// No check failed (warnings allowed).
    pub ok: bool,
    pub harness_version: &'static str,
    pub server: Option<ServerReport>,
    pub auth: AuthReport,
    pub connection: ConnectionReport,
    pub tools: Option<ToolsReport>,
    pub whoami: Option<WhoamiReport>,
    pub project: Option<ProjectReport>,
    pub checks: Vec<Check>,
}

#[derive(Debug, Serialize)]
pub struct ServerReport {
    pub url: String,
    /// `flag`, `env AMP_MCP_URL`, `config <path>` or `default`.
    pub url_source: String,
    pub ca_file: Option<String>,
    pub ca_file_source: Option<String>,
    /// The requested mode: `auto`, `2026-07-28` or `2025-11-25`.
    pub protocol_mode: String,
    pub protocol_source: String,
    pub config_file: Option<String>,
}

#[derive(Debug, Default, Serialize)]
pub struct AuthReport {
    /// How the connection authenticated (from the session), or would have.
    pub method: Option<String>,
    /// Where logins are kept; `null` when a static token makes it irrelevant.
    pub credential_store: Option<String>,
    /// Whether a login is stored for this server URL.
    pub stored_login: Option<bool>,
}

#[derive(Debug, Default, Serialize)]
pub struct ConnectionReport {
    /// The server answered (a 401 counts: it is there, we are just not logged in).
    pub reachable: bool,
    pub connected: bool,
    pub protocol_version: Option<String>,
    /// `stateless` (2026-07-28+) or `legacy` (initialize).
    pub era: Option<String>,
    /// Auto mode found a legacy-only server.
    pub fell_back: Option<bool>,
    pub server_name: Option<String>,
    pub server_version: Option<String>,
    /// The server sent instructions.
    pub instructions: bool,
    pub error: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct ToolsReport {
    pub count: usize,
    pub missing_contract: Vec<&'static str>,
}

#[derive(Debug, Serialize)]
pub struct WhoamiReport {
    pub user_id: Option<i64>,
    pub email: Option<String>,
    pub account: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct ProjectReport {
    /// The `.amp.json` that named it.
    pub marker: String,
    pub id: i64,
    /// The name AMP has for it (`null` if it could not be read).
    pub name: Option<String>,
    /// The name in `.amp.json`.
    pub marker_name: Option<String>,
    pub found: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum CheckStatus {
    Ok,
    Warn,
    Fail,
    Skip,
}

#[derive(Debug, Serialize)]
pub struct Check {
    /// One of: config, credentials, connect, tools, contract, whoami, project.
    pub name: &'static str,
    pub status: CheckStatus,
    pub detail: String,
}

struct Checks(Vec<Check>);

impl Checks {
    fn add(&mut self, name: &'static str, status: CheckStatus, detail: impl Into<String>) {
        self.0.push(Check {
            name,
            status,
            detail: detail.into(),
        });
    }

    fn skip(&mut self, names: &[&'static str], why: &str) {
        for name in names {
            self.add(name, CheckStatus::Skip, why);
        }
    }
}

pub async fn run(conn: ConnArgs, json: bool) -> Result<(), CliError> {
    let report = diagnose(&conn).await;
    if json {
        print_json(&report)?;
    } else {
        print_human(&report);
    }
    if report.ok {
        Ok(())
    } else {
        Err(CliError::silent(1))
    }
}

fn server_report(s: &Settings) -> ServerReport {
    ServerReport {
        url: s.url.clone(),
        url_source: s.url_source.to_string(),
        ca_file: s.ca_file.as_ref().map(|p| p.display().to_string()),
        ca_file_source: s.ca_file_source.as_ref().map(ToString::to_string),
        protocol_mode: s.protocol.to_string(),
        protocol_source: s.protocol_source.to_string(),
        config_file: s.config_file.as_ref().map(|p| p.display().to_string()),
    }
}

async fn diagnose(conn: &ConnArgs) -> Report {
    let mut checks = Checks(Vec::new());
    let mut auth = AuthReport::default();
    let mut connection = ConnectionReport::default();
    let mut tools = None;
    let mut whoami_report = None;
    let mut project = None;
    let after_connect = ["tools", "contract", "whoami", "project"];

    let settings = match conn.settings() {
        Ok(s) => {
            checks.add("config", CheckStatus::Ok, format!("server {}", s.url));
            Some(s)
        }
        Err(e) => {
            checks.add("config", CheckStatus::Fail, e.to_string());
            None
        }
    };
    let Some(settings) = settings else {
        checks.skip(&["credentials", "connect"], "no settings");
        checks.skip(&after_connect, "no settings");
        return finish(None, auth, connection, tools, None, None, checks);
    };

    // Credentials: a static token wins and is never stored; otherwise the store holds logins.
    match &settings.auth {
        Auth::Static { env, .. } => {
            auth.method = Some(settings.auth.describe());
            checks.add(
                "credentials",
                CheckStatus::Ok,
                format!("static token from {env} (not stored)"),
            );
        }
        Auth::None => match CredentialBackend::resolve() {
            Ok(backend) => {
                auth.credential_store = Some(backend.describe());
                let stored = oauth::has_login(&settings.url, &backend).await;
                auth.stored_login = stored.as_ref().ok().copied();
                let (status, detail) = match (&stored, backend.is_keychain()) {
                    (Err(e), _) => (CheckStatus::Warn, format!("cannot read logins: {e}")),
                    (Ok(_), false) => (
                        CheckStatus::Warn,
                        format!(
                            "no OS keychain; logins are kept in {} (mode 0600)",
                            backend.describe()
                        ),
                    ),
                    (Ok(true), true) => (
                        CheckStatus::Ok,
                        format!("login stored in {}", backend.describe()),
                    ),
                    (Ok(false), true) => (
                        CheckStatus::Ok,
                        format!("no login stored in {}", backend.describe()),
                    ),
                };
                checks.add("credentials", status, detail);
            }
            Err(e) => checks.add("credentials", CheckStatus::Warn, e.to_string()),
        },
    }

    let client = match connect(&settings).await {
        Ok(client) => client,
        Err(e) => {
            connection.reachable = e.is_auth_required();
            connection.error = Some(e.to_string());
            let detail = if e.is_auth_required() {
                format!("{} answered, but needs a login: {e}", settings.url)
            } else {
                e.to_string()
            };
            checks.add("connect", CheckStatus::Fail, detail);
            checks.skip(&after_connect, "not connected");
            return finish(Some(&settings), auth, connection, tools, None, None, checks);
        }
    };

    let session = client.session().clone();
    auth.method = Some(session.auth.clone());
    connection.reachable = true;
    connection.connected = true;
    connection.protocol_version = Some(session.protocol_version.to_string());
    connection.era = Some(session.era.to_string());
    connection.fell_back = Some(session.fell_back);
    connection.server_name = session.server_name.clone();
    connection.server_version = session.server_version.clone();
    connection.instructions = session
        .instructions
        .as_deref()
        .is_some_and(|i| !i.trim().is_empty());
    let mut detail = format!(
        "MCP {} ({}{})",
        session.protocol_version,
        session.era,
        if session.fell_back {
            ", fell back from 2026-07-28"
        } else {
            ""
        }
    );
    if let Some(name) = &session.server_name {
        detail.push_str(&format!(
            ", server {name} {}",
            session.server_version.as_deref().unwrap_or("")
        ));
    }
    checks.add("connect", CheckStatus::Ok, detail.trim_end().to_owned());

    match client.catalog().await {
        Ok(catalog) => {
            let missing = missing_contract_tools(&catalog);
            checks.add("tools", CheckStatus::Ok, format!("{} tools", catalog.len()));
            if missing.is_empty() {
                checks.add(
                    "contract",
                    CheckStatus::Ok,
                    format!(
                        "all {} contract tools present",
                        amp_core::tools::CONTRACT_TOOLS.len()
                    ),
                );
            } else {
                checks.add(
                    "contract",
                    CheckStatus::Fail,
                    format!("missing: {}", missing.join(", ")),
                );
            }
            tools = Some(ToolsReport {
                count: catalog.len(),
                missing_contract: missing,
            });
        }
        Err(e) => {
            checks.add("tools", CheckStatus::Fail, e.to_string());
            checks.skip(&["contract"], "no tool list");
        }
    }

    match whoami::fetch(&client).await {
        Ok(me) => {
            checks.add("whoami", CheckStatus::Ok, me.short());
            whoami_report = Some(WhoamiReport {
                user_id: me.user_id,
                email: me.email,
                account: me.account,
            });
        }
        Err(e) => checks.add("whoami", CheckStatus::Fail, e.to_string()),
    }

    match ProjectMarker::find_from_cwd() {
        Ok(None) => checks.add(
            "project",
            CheckStatus::Skip,
            "no .amp.json here (not a project checkout)",
        ),
        Err(e) => checks.add("project", CheckStatus::Fail, format!(".amp.json: {e}")),
        Ok(Some(marker)) => {
            let fetched = call_json(
                &client,
                "amp_get_project",
                json!({"project_id": marker.project_id}),
            )
            .await;
            let (found, name) = match &fetched {
                Ok(v) => (
                    true,
                    v.pointer("/project/name")
                        .and_then(|n| n.as_str())
                        .map(str::to_owned),
                ),
                Err(_) => (false, None),
            };
            match &fetched {
                Ok(_) => checks.add(
                    "project",
                    CheckStatus::Ok,
                    format!(
                        "#{} {} (from {})",
                        marker.project_id,
                        name.as_deref().unwrap_or("?"),
                        marker.path.display()
                    ),
                ),
                Err(e) => checks.add(
                    "project",
                    CheckStatus::Fail,
                    format!("#{} from {}: {e}", marker.project_id, marker.path.display()),
                ),
            }
            project = Some(ProjectReport {
                marker: marker.path.display().to_string(),
                id: marker.project_id,
                name,
                marker_name: marker.project_name.clone(),
                found,
            });
        }
    }
    client.close().await;
    finish(
        Some(&settings),
        auth,
        connection,
        tools,
        whoami_report,
        project,
        checks,
    )
}

fn finish(
    settings: Option<&Settings>,
    auth: AuthReport,
    connection: ConnectionReport,
    tools: Option<ToolsReport>,
    whoami: Option<WhoamiReport>,
    project: Option<ProjectReport>,
    checks: Checks,
) -> Report {
    Report {
        ok: checks.0.iter().all(|c| c.status != CheckStatus::Fail),
        harness_version: env!("CARGO_PKG_VERSION"),
        server: settings.map(server_report),
        auth,
        connection,
        tools,
        whoami,
        project,
        checks: checks.0,
    }
}

fn print_human(r: &Report) {
    println!("amp-harness {} doctor", r.harness_version);
    if let Some(s) = &r.server {
        println!("  server:   {} ({})", s.url, s.url_source);
        if let Some(ca) = &s.ca_file {
            println!(
                "  ca file:  {ca} ({})",
                s.ca_file_source.as_deref().unwrap_or("?")
            );
        }
        println!("  protocol: {} ({})", s.protocol_mode, s.protocol_source);
    }
    if let Some(method) = &r.auth.method {
        println!("  auth:     {method}");
    }
    println!();
    for c in &r.checks {
        let mark = match c.status {
            CheckStatus::Ok => "ok  ",
            CheckStatus::Warn => "warn",
            CheckStatus::Fail => "FAIL",
            CheckStatus::Skip => "skip",
        };
        println!("  [{mark}] {:<11} {}", c.name, c.detail);
    }
    println!();
    if r.ok {
        println!("All checks passed.");
    } else {
        let failed: Vec<&str> = r
            .checks
            .iter()
            .filter(|c| c.status == CheckStatus::Fail)
            .map(|c| c.name)
            .collect();
        println!("Failed: {}", failed.join(", "));
        if r.connection.reachable && !r.connection.connected {
            println!("Hint: run `amp-harness login` (or set AMP_TOKEN).");
        }
    }
}
