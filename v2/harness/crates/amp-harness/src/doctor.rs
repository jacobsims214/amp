//! `amp-harness doctor [--json] [--offline]`: can this machine use AMP, and what is wrong if not?
//!
//! Checks, in order (later AMP checks are skipped when an earlier one makes them impossible):
//! - `config`: connection settings resolve.
//! - `credentials`: where logins live; D3 warns on the 0600 file fallback.
//! - `ca`: TLS trust: the CA file (readable, PEM, in date), and why a certificate was rejected.
//! - `connect`: reachability + negotiated protocol.
//! - `versions`: harness, rpi/rmcp pins, embedded asset commit, negotiated MCP protocol (D11).
//! - `tools` (tools/list), `contract` (D11 tools present), `whoami`, `project` (`.amp.json`, then
//!   `amp_get_project`).
//! - `roles`, `key:<provider>`, `models:<provider>`: see [`providers`]. These run even when AMP is
//!   unreachable.
//!
//! Every warn/fail carries a `fix`: the exact thing to run or change. Never prints a secret: auth
//! and provider keys are described by their source (`static token (env AMP_TOKEN)`, `env
//! OPENROUTER_API_KEY`), never by value. Exit code 1 when any check fails; warnings don't count.

mod ca;
mod providers;

use amp_core::{
    models::ModelsConfig,
    project::ProjectMarker,
    providers::SystemKeys,
    tools::{call_json, missing_contract_tools},
};
use amp_mcp::{Auth, CredentialBackend, FailureKind, McpError, Settings, oauth};
use clap::Args;
use serde::Serialize;
use serde_json::json;

use crate::{
    conn::{CliError, ConnArgs, connect, print_json},
    whoami,
};

#[derive(Args)]
pub struct DoctorArgs {
    #[command(flatten)]
    pub conn: ConnArgs,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
    /// Don't ask the model providers for their catalogs (no network besides AMP).
    #[arg(long)]
    pub offline: bool,
}

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
    /// TLS trust (`ca` check).
    pub ca: Option<ca::CaReport>,
    /// Harness, pins, assets and the negotiated protocol (`versions` check, D11).
    pub versions: VersionsReport,
    /// Roles, provider keys and pinned models (`roles`, `key:*`, `models:*` checks).
    pub models: providers::ModelsReport,
    pub checks: Vec<Check>,
}

#[derive(Debug, Serialize)]
pub struct VersionsReport {
    pub harness: &'static str,
    pub rpi_ai: &'static str,
    pub rpi_agent: &'static str,
    pub rpi_tools: &'static str,
    pub rpi_harness: &'static str,
    pub rpi_tui: &'static str,
    pub rmcp: &'static str,
    /// The commit that last changed the embedded agent/skill assets (`-dirty`, or `unknown`).
    pub assets_commit: &'static str,
    /// The assets manifest's hash of the `.opencode` sources they were generated from.
    pub assets_source_sha256: Option<String>,
    /// The MCP protocol negotiated with AMP (`null` when not connected).
    pub mcp_protocol: Option<String>,
}

impl VersionsReport {
    fn new() -> Self {
        Self {
            harness: env!("CARGO_PKG_VERSION"),
            rpi_ai: env!("AMP_PIN_RPI_AI"),
            rpi_agent: env!("AMP_PIN_RPI_AGENT"),
            rpi_tools: env!("AMP_PIN_RPI_TOOLS"),
            rpi_harness: env!("AMP_PIN_RPI_HARNESS"),
            rpi_tui: env!("AMP_PIN_RPI_TUI"),
            rmcp: env!("AMP_PIN_RMCP"),
            assets_commit: env!("AMP_ASSETS_COMMIT"),
            assets_source_sha256: amp_core::assets::Assets::embedded()
                .ok()
                .map(|a| a.manifest().source_sha256.clone()),
            mcp_protocol: None,
        }
    }

    fn summary(&self) -> String {
        let rpi = [
            self.rpi_agent,
            self.rpi_tools,
            self.rpi_harness,
            self.rpi_tui,
        ];
        let rpi = if rpi.iter().all(|v| *v == self.rpi_ai) {
            format!("rpi {}", self.rpi_ai)
        } else {
            format!(
                "rpi-ai {} rpi-agent {} rpi-tools {} rpi-harness {} rpi-tui {}",
                self.rpi_ai, self.rpi_agent, self.rpi_tools, self.rpi_harness, self.rpi_tui
            )
        };
        format!(
            "harness {}, {rpi}, rmcp {}, assets {}, MCP {}",
            self.harness,
            self.rmcp,
            self.assets_commit,
            self.mcp_protocol.as_deref().unwrap_or("not connected")
        )
    }
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
    /// The marker was made for another server, or names another project than this server's
    /// (#2206). `null` when it agrees.
    pub warning: Option<String>,
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
    /// One of: config, credentials, ca, connect, versions, tools, contract, whoami, project, roles,
    /// `key:<provider>`, `models:<provider>`.
    pub name: String,
    pub status: CheckStatus,
    pub detail: String,
    /// For warn/fail: what to run or change to fix it.
    pub fix: Option<String>,
}

/// A check's outcome before it is named.
#[derive(Debug, Clone)]
pub struct Finding {
    pub status: CheckStatus,
    pub detail: String,
    pub fix: Option<String>,
}

impl Finding {
    pub fn new(status: CheckStatus, detail: impl Into<String>) -> Self {
        Self {
            status,
            detail: detail.into(),
            fix: None,
        }
    }

    pub fn fix(mut self, fix: impl Into<String>) -> Self {
        self.fix = Some(fix.into());
        self
    }
}

struct Checks(Vec<Check>);

impl Checks {
    fn add(&mut self, name: &str, status: CheckStatus, detail: impl Into<String>) {
        self.push(name, Finding::new(status, detail));
    }

    fn fail(&mut self, name: &str, detail: impl Into<String>, fix: impl Into<String>) {
        self.push(name, Finding::new(CheckStatus::Fail, detail).fix(fix));
    }

    fn push(&mut self, name: &str, finding: Finding) {
        self.0.push(Check {
            name: name.to_owned(),
            status: finding.status,
            detail: finding.detail,
            fix: finding.fix,
        });
    }

    fn skip(&mut self, names: &[&str], why: &str) {
        for name in names {
            self.add(name, CheckStatus::Skip, why);
        }
    }
}

pub async fn run(args: DoctorArgs) -> Result<(), CliError> {
    let report = diagnose(&args.conn, args.offline).await;
    if args.json {
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

/// What the AMP half of the checks found (the report fields it fills).
#[derive(Default)]
struct AmpFindings {
    settings: Option<Settings>,
    auth: AuthReport,
    connection: ConnectionReport,
    tools: Option<ToolsReport>,
    whoami: Option<WhoamiReport>,
    project: Option<ProjectReport>,
    ca: Option<ca::CaReport>,
}

async fn diagnose(conn: &ConnArgs, offline: bool) -> Report {
    let mut checks = Checks(Vec::new());
    let mut versions = VersionsReport::new();
    let amp = amp_checks(conn, &mut checks, &mut versions).await;

    // The model side needs no AMP connection: a brand-new machine sees both halves at once.
    let env = |k: &str| std::env::var(k).ok();
    // #2232: the user file `--config` / `--config-dir` name, plus the repo layer of this folder.
    let paths = std::env::current_dir()
        .map(|cwd| crate::conn::config_paths_in(&cwd))
        .unwrap_or_else(|_| crate::conn::config_paths());
    let config_path = paths.user_file.as_ref().map(|p| p.display().to_string());
    let models_config = ModelsConfig::load_paths(&env, &paths).map_err(|e| e.to_string());
    let (models, findings) =
        providers::check(models_config, config_path, &SystemKeys::from_env(), offline).await;
    for (name, finding) in findings {
        checks.push(&name, finding);
    }

    Report {
        ok: checks.0.iter().all(|c| c.status != CheckStatus::Fail),
        harness_version: env!("CARGO_PKG_VERSION"),
        server: amp.settings.as_ref().map(server_report),
        auth: amp.auth,
        connection: amp.connection,
        tools: amp.tools,
        whoami: amp.whoami,
        project: amp.project,
        ca: amp.ca,
        versions,
        models,
        checks: checks.0,
    }
}

const AFTER_CONNECT: [&str; 4] = ["tools", "contract", "whoami", "project"];

async fn amp_checks(
    conn: &ConnArgs,
    checks: &mut Checks,
    versions: &mut VersionsReport,
) -> AmpFindings {
    let mut found = AmpFindings::default();

    let settings = match conn.settings() {
        Ok(s) => {
            checks.add("config", CheckStatus::Ok, format!("server {}", s.url));
            s
        }
        Err(e) => {
            checks.fail(
                "config",
                e.to_string(),
                "fix the setting the error names (--url / AMP_MCP_URL, --ca-file / AMP_CA_FILE, \
                 --protocol / AMP_MCP_PROTOCOL); for the saved server run `amp-harness server` to \
                 see where each setting comes from and `amp-harness server set <url>` to replace it",
            );
            checks.skip(&["credentials", "ca", "connect"], "no settings");
            checks.push(
                "versions",
                Finding::new(CheckStatus::Ok, versions.summary()),
            );
            checks.skip(&AFTER_CONNECT, "no settings");
            return found;
        }
    };

    credentials_check(&settings, checks, &mut found.auth).await;

    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs() as i64);
    let (ca_report, ca_file_finding) = ca::check_file(&settings, now);
    found.ca = Some(ca_report);

    let client = match connect(&settings).await {
        Ok(client) => {
            checks.push("ca", ca_file_finding);
            client
        }
        Err(e) => {
            found.connection.reachable = e.is_auth_required();
            found.connection.error = Some(e.to_string());
            let tls = ca::classify_tls(&e.to_string(), &settings);
            let ca_broken = tls.is_some() || ca_file_finding.status == CheckStatus::Fail;
            if let (Some(t), Some(report)) = (&tls, found.ca.as_mut()) {
                report.tls_error = Some(t.detail.clone());
            }
            // A broken CA file (unreadable, expired) is the more specific diagnosis.
            let ca_finding = match tls {
                Some(t) if ca_file_finding.status != CheckStatus::Fail => t,
                _ => ca_file_finding,
            };
            checks.push("ca", ca_finding);
            let (detail, fix) = connect_failure(&e, &settings, ca_broken);
            checks.fail("connect", detail, fix);
            checks.push(
                "versions",
                Finding::new(CheckStatus::Ok, versions.summary()),
            );
            checks.skip(&AFTER_CONNECT, "not connected");
            found.settings = Some(settings);
            return found;
        }
    };

    let session = client.session().clone();
    found.auth.method = Some(session.auth.clone());
    let connection = &mut found.connection;
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
    versions.mcp_protocol = Some(session.protocol_version.to_string());
    checks.push(
        "versions",
        Finding::new(CheckStatus::Ok, versions.summary()),
    );

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
                checks.fail(
                    "contract",
                    format!("missing: {}", missing.join(", ")),
                    format!(
                        "this AMP server ({}) lacks tools amp-harness {} needs: upgrade the AMP \
                         server, or use an amp-harness release that matches it",
                        session
                            .server_version
                            .as_deref()
                            .unwrap_or("unknown version"),
                        env!("CARGO_PKG_VERSION")
                    ),
                );
            }
            found.tools = Some(ToolsReport {
                count: catalog.len(),
                missing_contract: missing,
            });
        }
        Err(e) => {
            checks.fail(
                "tools",
                e.to_string(),
                "run `amp-harness doctor` again; if it keeps failing, AMP's MCP server is \
                 unhealthy (check its logs)",
            );
            checks.skip(&["contract"], "no tool list");
        }
    }

    match whoami::fetch(&client).await {
        Ok(me) => {
            checks.add("whoami", CheckStatus::Ok, me.short());
            found.whoami = Some(WhoamiReport {
                user_id: me.user_id,
                email: me.email,
                account: me.account,
            });
        }
        Err(e) => checks.fail("whoami", e.to_string(), login_fix(&settings)),
    }

    match ProjectMarker::find_from_cwd() {
        Ok(None) => checks.add(
            "project",
            CheckStatus::Skip,
            "no .amp.json here (not a project checkout)",
        ),
        Err(e) => checks.fail(
            "project",
            format!(".amp.json: {e}"),
            "make .amp.json valid JSON with a numeric \"project_id\" (the id in the AMP web UI's \
             project URL)",
        ),
        Ok(Some(marker)) => {
            let fetched = call_json(
                &client,
                "amp_get_project",
                json!({"project_id": marker.project_id}),
            )
            .await;
            let (ok, name) = match &fetched {
                Ok(v) => (
                    true,
                    v.pointer("/project/name")
                        .and_then(|n| n.as_str())
                        .map(str::to_owned),
                ),
                Err(_) => (false, None),
            };
            let warning = match &fetched {
                Ok(_) => marker.server_warning(&settings.url, name.as_deref()),
                Err(_) => marker.server_warning(&settings.url, None),
            };
            match (&fetched, &warning) {
                (Ok(_), None) => checks.add(
                    "project",
                    CheckStatus::Ok,
                    format!(
                        "#{} {} (from {})",
                        marker.project_id,
                        name.as_deref().unwrap_or("?"),
                        marker.path.display()
                    ),
                ),
                (Ok(_), Some(w)) => checks.push(
                    "project",
                    Finding::new(CheckStatus::Warn, w.clone()).fix(format!(
                        "if #{} is not this folder's project on {}, run `amp-harness init` here \
                         to pick this server's project",
                        marker.project_id,
                        crate::setup::host_of(&settings.url)
                    )),
                ),
                (Err(e), _) => checks.fail(
                    "project",
                    match &warning {
                        Some(w) => format!(
                            "#{} from {}: {e} ({w})",
                            marker.project_id,
                            marker.path.display()
                        ),
                        None => {
                            format!("#{} from {}: {e}", marker.project_id, marker.path.display())
                        }
                    },
                    format!(
                        "check you can open project #{} in the AMP web UI as {}; if it is the \
                         wrong project, fix \"project_id\" in {}",
                        marker.project_id,
                        found
                            .whoami
                            .as_ref()
                            .and_then(|w| w.email.as_deref())
                            .unwrap_or("this user"),
                        marker.path.display()
                    ),
                ),
            }
            found.project = Some(ProjectReport {
                marker: marker.path.display().to_string(),
                id: marker.project_id,
                name,
                marker_name: marker.project_name.clone(),
                found: ok,
                warning,
            });
        }
    }
    client.close().await;
    found.settings = Some(settings);
    found
}

/// D3: a static token wins and is never stored; otherwise the store holds logins (warn on the file).
async fn credentials_check(settings: &Settings, checks: &mut Checks, auth: &mut AuthReport) {
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
                let finding = match (&stored, backend.is_keychain()) {
                    (Err(e), _) => Finding::new(
                        CheckStatus::Warn,
                        format!("cannot read logins: {e}"),
                    )
                    .fix(format!(
                        "run `amp-harness logout --url {0}` and then `amp-harness login --url {0}`",
                        settings.url
                    )),
                    (Ok(_), false) => Finding::new(
                        CheckStatus::Warn,
                        format!(
                            "no OS keychain; logins are kept in {} (mode 0600)",
                            backend.describe()
                        ),
                    )
                    .fix(file_store_fix()),
                    (Ok(true), true) => Finding::new(
                        CheckStatus::Ok,
                        format!("login stored in {}", backend.describe()),
                    ),
                    (Ok(false), true) => Finding::new(
                        CheckStatus::Ok,
                        format!("no login stored in {}", backend.describe()),
                    ),
                };
                checks.push("credentials", finding);
            }
            Err(e) => checks.push(
                "credentials",
                Finding::new(CheckStatus::Warn, e.to_string())
                    .fix("set HOME (or XDG_CONFIG_HOME), or use AMP_TOKEN"),
            ),
        },
    }
}

fn file_store_fix() -> String {
    let forced = std::env::var(amp_mcp::ENV_CREDENTIAL_STORE)
        .is_ok_and(|v| v.trim().eq_ignore_ascii_case("file"));
    if forced {
        format!(
            "unset {} to keep logins in the OS keychain instead",
            amp_mcp::ENV_CREDENTIAL_STORE
        )
    } else if cfg!(target_os = "linux") {
        format!(
            "start a Secret Service keyring (e.g. gnome-keyring-daemon) and log in again; to keep \
             the file on purpose, set {}=file",
            amp_mcp::ENV_CREDENTIAL_STORE
        )
    } else {
        "unlock the OS keychain and run `amp-harness login` again".to_owned()
    }
}

fn login_fix(settings: &Settings) -> String {
    match &settings.auth {
        Auth::Static { env, .. } => format!(
            "{env} was not accepted: create a new token in the AMP web UI and export {env}=<token>, \
             or unset {env} and run `amp-harness login --url {}`",
            settings.url
        ),
        Auth::None => format!(
            "run `amp-harness login --url {}` (or export AMP_TOKEN=<token from the AMP web UI>)",
            settings.url
        ),
    }
}

/// `connect`'s detail and fix for a failed connection. The detail is the same one sentence the
/// other commands print ([`crate::conn::connect_headline`], #2208); the full cause chain is in
/// the JSON's `connection.error` and, with `--verbose`, on a `cause:` line. The fix never assumes
/// the AMP repo (no `make up`): a user of the released binary has neither it nor its ports.
fn connect_failure(e: &McpError, settings: &Settings, ca_broken: bool) -> (String, String) {
    let detail = crate::conn::connect_headline(&settings.url, Some(&settings.auth), e);
    if e.is_auth_required() {
        return (detail, login_fix(settings));
    }
    if ca_broken || matches!(e, McpError::CaFile { .. }) {
        return (
            detail,
            "fix the `ca` check above, then run `amp-harness doctor` again".to_owned(),
        );
    }
    let fix = if matches!(e, McpError::Config(_)) {
        "fix the server URL: `amp-harness server set <url>` checks and saves it (or pass --url / \
         AMP_MCP_URL for one command)"
            .to_owned()
    } else {
        let switch = match &settings.url_source {
            amp_mcp::Source::Env(var) => format!(
                "or unset {var} (it overrides the saved server), or switch with `amp-harness \
                 server use <name>` / `amp-harness server set <your AMP's address>`"
            ),
            _ => "or switch to your AMP with `amp-harness server set <its address>` (or \
                  `amp-harness server use <name>` for a saved one)"
                .to_owned(),
        };
        let (url, source) = (&settings.url, &settings.url_source);
        match e.kind() {
            FailureKind::HostNotFound | FailureKind::NotFound | FailureKind::NotMcp => format!(
                "check the address {url} (from {source}; AMP's usually ends in /mcp), {switch}"
            ),
            _ => format!("is AMP running at {url} (from {source})? Start it, {switch}"),
        }
    };
    (detail, fix)
}

/// #2275: which provider and endpoint the roles use, and each role's model.
fn print_models(m: &providers::ModelsReport) {
    if let (Some(mode), Some(selection)) = (m.mode, &m.selection) {
        println!("  models:   {mode} {selection}");
    }
    for p in &m.providers {
        let name = if p.name == p.id {
            String::new()
        } else {
            format!(" ({})", p.name)
        };
        println!(
            "  provider: {}{name} {} (key: {})",
            p.id, p.base_url, p.key_source
        );
    }
    let width = m.roles.iter().map(|r| r.role.len()).max().unwrap_or(0);
    for (i, r) in m.roles.iter().enumerate() {
        println!(
            "  {}{:<width$}  {}/{}",
            if i == 0 { "roles:    " } else { "          " },
            r.role,
            r.provider,
            r.model
        );
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
    print_models(&r.models);
    println!();
    let width = r
        .checks
        .iter()
        .map(|c| c.name.len())
        .max()
        .unwrap_or(0)
        .max(11);
    for c in &r.checks {
        let mark = match c.status {
            CheckStatus::Ok => "ok  ",
            CheckStatus::Warn => "warn",
            CheckStatus::Fail => "FAIL",
            CheckStatus::Skip => "skip",
        };
        println!("  [{mark}] {:<width$} {}", c.name, c.detail);
        // #2208: the cause chain behind the connect sentence, only with --verbose / RUST_LOG.
        if c.name == "connect"
            && c.status == CheckStatus::Fail
            && crate::conn::verbose()
            && let Some(error) = &r.connection.error
        {
            println!("         {:<width$} cause: {error}", "");
        }
        if let Some(fix) = &c.fix
            && matches!(c.status, CheckStatus::Warn | CheckStatus::Fail)
        {
            println!("         {:<width$} fix: {fix}", "");
        }
    }
    println!();
    if r.ok {
        let warned = r
            .checks
            .iter()
            .filter(|c| c.status == CheckStatus::Warn)
            .count();
        if warned == 0 {
            println!("All checks passed.");
        } else {
            println!(
                "No failures ({warned} warning{}).",
                if warned == 1 { "" } else { "s" }
            );
        }
    } else {
        let failed: Vec<&str> = r
            .checks
            .iter()
            .filter(|c| c.status == CheckStatus::Fail)
            .map(|c| c.name.as_str())
            .collect();
        println!("Failed: {}", failed.join(", "));
        if r.connection.reachable && !r.connection.connected {
            println!("Hint: run `amp-harness login` (or set AMP_TOKEN).");
        }
    }
}
