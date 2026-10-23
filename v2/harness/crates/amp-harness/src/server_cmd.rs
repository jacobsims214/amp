//! `amp-harness server` and the chat's `/server` (#2164): manage the AMP server without editing
//! files or exporting variables.
//!
//! - `server` (or `server show`) prints the server in effect (MCP URL, web UI, CA file, protocol,
//!   and where each came from: flag, env, config or default) and the saved servers.
//! - `server set <url> [--name N] [--web-url U] [--ca-file F] [--protocol P]` checks the server
//!   first (it must answer, or answer that it needs a login), then saves it as a named server and
//!   makes it the active one. The web UI address is found the way the first run finds it (#2113):
//!   nothing is saved when the MCP origin serves the UI, docker compose's :5173 is found by a
//!   probe, and only an address no probe can find is asked for. A server that needs a login gets
//!   the offer to log in right away.
//! - `server use <name>` makes a saved server the active one (logins are kept per URL, so each
//!   server keeps its own); `server remove <name>` forgets one.
//!
//! In config.toml, `[server]` stays the server in effect (so older harness builds and every
//! command keep reading it), with `name` naming the saved server it came from; the saved servers
//! are `[servers.<name>]` tables with the same keys. Every write goes through
//! [`ConfigFile::edit`]: the old file is backed up to `config.toml.bak` and nothing is written
//! unless every other setting survives. A server that was configured before names existed is
//! kept as a saved server when another one becomes active. Secrets are never written.
//!
//! `/server` in the chat ([`Switcher`]) lists the same servers and switches the live session: it
//! connects to the new server first, then swaps the session's AMP connection (#2159), gives the
//! model the new server's AMP tools and server details ([`Runtime::retarget`]) and starts a new
//! metadata poller for the header. A switch that can't connect leaves everything as it was.

use std::{
    collections::BTreeSet,
    path::{Path, PathBuf},
    sync::{Arc, Mutex, PoisonError},
    time::Duration,
};

use amp_core::{agent::Runtime, policy::ToolPolicy, project::ProjectMarker, tools::call_json};
use amp_mcp::{
    AmpMcpClient, Auth, DEFAULT_MCP_URL, ENV_MCP_URL, ENV_WEB_URL, McpError, Overrides,
    ProtocolMode, Settings, Source,
};
use amp_tui::{ServerControl, ServerSwitched};
use clap::{Args, Subcommand};
use rpi_harness::agent_harness::AgentHarness;
use serde::Serialize;
use serde_json::json;

use crate::{
    conn::{self, CliError, ConnArgs},
    setup::{
        self, Change, ConfigFile, WebGuess,
        ui::{self, Ui},
    },
};

/// The table that holds the saved servers.
const SERVERS: &str = "servers";
/// How long checking a server may take (connect, then `amp_whoami`).
const CHECK_TIMEOUT: Duration = Duration::from_secs(45);
const WHOAMI_TIMEOUT: Duration = Duration::from_secs(10);

// ---------------------------------------------------------------------------------------------
// The saved servers in config.toml
// ---------------------------------------------------------------------------------------------

/// One server: a `[servers.<name>]` table, or the `[server]` table in effect.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct SavedServer {
    pub name: String,
    pub url: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub web_url: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ca_file: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub protocol: Option<String>,
}

impl SavedServer {
    fn from_table(name: &str, t: &toml::Table) -> Option<Self> {
        let get = |k: &str| {
            t.get(k)
                .and_then(|v| v.as_str())
                .map(str::trim)
                .filter(|v| !v.is_empty())
                .map(str::to_owned)
        };
        Some(Self {
            name: name.to_owned(),
            url: get("url")?,
            web_url: get("web_url"),
            ca_file: get("ca_file"),
            protocol: get("protocol"),
        })
    }

    /// The changes that write this server's keys to `[section]` (and drop the ones it lacks).
    fn changes(&self, section: &str) -> Vec<Change> {
        let mut out = vec![Change::set(section, "url", self.url.clone())];
        for (key, value) in [
            ("web_url", &self.web_url),
            ("ca_file", &self.ca_file),
            ("protocol", &self.protocol),
        ] {
            out.push(match value {
                Some(v) => Change::set(section, key, v.clone()),
                None => Change::remove(section, key),
            });
        }
        out
    }

    fn line(&self) -> String {
        let mut line = format!("{}  {}", self.name, self.url);
        if let Some(web) = &self.web_url {
            line.push_str(&format!("  (web UI {web})"));
        }
        if let Some(ca) = &self.ca_file {
            line.push_str(&format!("  (CA {ca})"));
        }
        line
    }
}

/// What config.toml says about servers.
#[derive(Debug, Clone, Default)]
pub struct Servers {
    /// `[servers.*]`, by name.
    pub saved: Vec<SavedServer>,
    /// `[server]` (its `name` key, or empty when it has none).
    pub active: Option<SavedServer>,
}

impl Servers {
    pub fn parse(text: &str) -> Result<Self, String> {
        let doc: toml::Table = toml::from_str(text).map_err(|e| e.to_string())?;
        let mut saved: Vec<SavedServer> = doc
            .get(SERVERS)
            .and_then(|v| v.as_table())
            .map(|t| {
                t.iter()
                    .filter_map(|(name, v)| SavedServer::from_table(name, v.as_table()?))
                    .collect()
            })
            .unwrap_or_default();
        saved.sort_by(|a, b| a.name.cmp(&b.name));
        let active = doc.get("server").and_then(|v| v.as_table()).and_then(|t| {
            let name = t
                .get("name")
                .and_then(|v| v.as_str())
                .unwrap_or("")
                .to_owned();
            SavedServer::from_table(&name, t)
        });
        Ok(Self { saved, active })
    }

    pub fn load(config: &ConfigFile) -> Result<Self, String> {
        match config.text()? {
            Some(text) => {
                Self::parse(&text).map_err(|e| format!("parse {}: {e}", config.path.display()))
            }
            None => Ok(Self::default()),
        }
    }

    pub fn get(&self, name: &str) -> Option<&SavedServer> {
        self.saved.iter().find(|s| s.name == name)
    }

    fn by_url(&self, url: &str) -> Option<&SavedServer> {
        self.saved.iter().find(|s| same_url(&s.url, url))
    }

    /// The saved server `url` belongs to: `[server] name` when it matches, else any with that URL.
    pub fn name_of(&self, url: &str) -> Option<&str> {
        if let Some(active) = &self.active
            && same_url(&active.url, url)
            && let Some(s) = self.get(&active.name)
            && same_url(&s.url, url)
        {
            return Some(&s.name);
        }
        self.by_url(url).map(|s| s.name.as_str())
    }

    fn names(&self) -> String {
        if self.saved.is_empty() {
            return "none yet".to_owned();
        }
        self.saved
            .iter()
            .map(|s| s.name.as_str())
            .collect::<Vec<_>>()
            .join(", ")
    }
}

fn same_url(a: &str, b: &str) -> bool {
    a.trim_end_matches('/') == b.trim_end_matches('/')
}

/// A server name: letters, digits, `-` and `_` (a bare TOML key), at most 40.
pub fn valid_name(name: &str) -> Result<(), String> {
    let ok = !name.is_empty()
        && name.len() <= 40
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_');
    if ok {
        Ok(())
    } else {
        Err(format!(
            "`{name}` is not a server name: use letters, digits, - and _ (e.g. local, hosted)"
        ))
    }
}

/// A name for a server nobody named: `hosted` for hosted AMP, `local` for this machine, else the
/// host's first label; made unique among `taken` (another URL's name is never reused).
pub fn derive_name(url: &str, taken: &[SavedServer]) -> String {
    let host = setup::host_of(url);
    let bare = host.rsplit_once(':').map_or(host.as_str(), |(h, _)| h);
    let base = if same_url(url, DEFAULT_MCP_URL) {
        "hosted".to_owned()
    } else if matches!(bare, "localhost" | "127.0.0.1" | "[::1]") {
        "local".to_owned()
    } else {
        let label: String = bare
            .split('.')
            .next()
            .unwrap_or("")
            .chars()
            .map(|c| {
                if c.is_ascii_alphanumeric() || c == '-' {
                    c.to_ascii_lowercase()
                } else {
                    '-'
                }
            })
            .collect();
        let label = label.trim_matches('-').to_owned();
        if label.is_empty() {
            "server".to_owned()
        } else {
            label
        }
    };
    let free = |n: &str| taken.iter().all(|s| s.name != n || same_url(&s.url, url));
    if free(&base) {
        return base;
    }
    let port = host.rsplit_once(':').map(|(_, p)| p.to_owned());
    if let Some(port) = port.filter(|p| p.chars().all(|c| c.is_ascii_digit())) {
        let candidate = format!("{base}-{port}");
        if free(&candidate) {
            return candidate;
        }
    }
    (2..)
        .map(|i| format!("{base}-{i}"))
        .find(|n| free(n))
        .expect("an unused name")
}

/// What [`activate`] did.
#[derive(Debug, Default)]
pub struct Activated {
    pub backup: Option<PathBuf>,
    /// The server that was in effect without a name, now kept under this one.
    pub kept: Option<SavedServer>,
}

/// Saves `entry` as `[servers.<name>]` and makes it the server in effect (`[server]`), in one
/// edit. A `[server]` that matches no saved server is kept as one first, so it is never lost.
pub fn activate(config: &ConfigFile, entry: &SavedServer) -> Result<Activated, String> {
    valid_name(&entry.name)?;
    let servers = Servers::load(config)?;
    let mut changes = Vec::new();
    let mut kept = None;
    if let Some(active) = &servers.active
        && !same_url(&active.url, &entry.url)
        && servers.by_url(&active.url).is_none()
    {
        let mut taken = servers.saved.clone();
        taken.push(entry.clone());
        let name = if !active.name.is_empty()
            && valid_name(&active.name).is_ok()
            && taken.iter().all(|s| s.name != active.name)
        {
            active.name.clone()
        } else {
            derive_name(&active.url, &taken)
        };
        let old = SavedServer {
            name,
            ..active.clone()
        };
        changes.extend(old.changes(&format!("{SERVERS}.{}", old.name)));
        kept = Some(old);
    }
    changes.extend(entry.changes(&format!("{SERVERS}.{}", entry.name)));
    changes.extend(entry.changes("server"));
    changes.push(Change::set("server", "name", entry.name.clone()));
    let backup = config.edit(&changes)?;
    Ok(Activated { backup, kept })
}

/// The first run and `init` chose `url`: save it as a named server and make it active, the same
/// way `server set` does, so `amp-harness server` lists it and `server use` can come back to it
/// (#2212). A server already saved under that URL keeps its name and settings (`ca_file`
/// replaces its CA file); `[server]` with that URL but no name keeps its keys under a derived
/// name. Returns the entry when config.toml changed, `None` when it was already saved and active.
pub(crate) fn save_chosen(
    config: &ConfigFile,
    url: &str,
    ca_file: Option<String>,
) -> Result<Option<SavedServer>, String> {
    let servers = Servers::load(config)?;
    let previous = servers.name_of(url).and_then(|n| servers.get(n)).cloned();
    let active = servers.active.clone().filter(|a| same_url(&a.url, url));
    if let (Some(a), Some(p)) = (&active, &previous)
        && a.name == p.name
        && ca_file
            .as_ref()
            .is_none_or(|c| p.ca_file.as_ref() == Some(c))
    {
        return Ok(None);
    }
    let name = previous
        .as_ref()
        .map(|p| p.name.clone())
        .unwrap_or_else(|| derive_name(url, &servers.saved));
    let base = previous.or(active).unwrap_or_else(|| SavedServer {
        name: String::new(),
        url: url.to_owned(),
        web_url: None,
        ca_file: None,
        protocol: None,
    });
    let entry = SavedServer {
        name,
        url: url.to_owned(),
        ca_file: ca_file.or(base.ca_file),
        ..base
    };
    activate(config, &entry)?;
    Ok(Some(entry))
}

/// Saves the web UI address of the server in effect: `[server] web_url`, and the same key on its
/// `[servers.<name>]` entry so `server use` brings it back.
pub(crate) fn save_web_url(config: &ConfigFile, web_url: &str) -> Result<(), String> {
    let servers = Servers::load(config)?;
    let mut changes = vec![Change::set("server", "web_url", web_url)];
    if let Some(active) = &servers.active
        && let Some(saved) = servers.get(&active.name)
        && same_url(&saved.url, &active.url)
    {
        changes.push(Change::set(
            &format!("{SERVERS}.{}", saved.name),
            "web_url",
            web_url,
        ));
    }
    config.edit(&changes).map(|_| ())
}

/// The config.toml the server commands read and write: the global `--config` / `--config-dir`
/// (#2232), else the default path.
fn config_file(_conn: &ConnArgs) -> Result<ConfigFile, CliError> {
    conn::user_config_file().map(ConfigFile::new)
}

fn env_set(key: &str) -> Option<String> {
    std::env::var(key).ok().filter(|v| !v.trim().is_empty())
}

/// The settings for connecting to `entry`: its own URL, CA file, protocol and web UI, with the
/// person's `--config` / `--token-env`. Settings the old `[server]` holds never leak into it;
/// `AMP_CA_FILE`, `AMP_MCP_PROTOCOL` and `AMP_WEB_URL` still win, as they do everywhere.
pub fn entry_settings(entry: &SavedServer, base: &ConnArgs) -> Result<Settings, McpError> {
    let protocol = entry
        .protocol
        .as_deref()
        .map(str::parse::<ProtocolMode>)
        .transpose()
        .map_err(|e| McpError::Config(format!("server `{}` protocol: {e}", entry.name)))?;
    let mut settings = Settings::resolve(&Overrides {
        url: Some(entry.url.clone()),
        ca_file: entry.ca_file.as_ref().map(PathBuf::from),
        protocol,
        // The writer just created or changed the file; the default lookup finds it (#2232).
        config_file: None,
        token_env: base.token_env.clone(),
    })?;
    if matches!(settings.ca_file_source, Some(Source::ConfigFile(_))) {
        settings.ca_file = None;
        settings.ca_file_source = None;
    }
    if matches!(settings.protocol_source, Source::ConfigFile(_)) {
        settings.protocol = ProtocolMode::Auto;
        settings.protocol_source = Source::Default;
    }
    if env_set(ENV_WEB_URL).is_none() {
        settings.web_url = entry.web_url.clone();
    }
    Ok(settings)
}

/// How a server answered a check.
enum Check {
    /// Connected; `amp_whoami`'s email when it answered.
    Ok {
        client: AmpMcpClient,
        user: Option<String>,
    },
    /// It answered, but wants a login (or refused the static token).
    Login(McpError),
}

async fn check(settings: &Settings) -> Result<Check, McpError> {
    let connect = tokio::time::timeout(CHECK_TIMEOUT, conn::connect(settings))
        .await
        .map_err(|_| McpError::Config(format!("no answer within {CHECK_TIMEOUT:?}")))?;
    match connect {
        Ok(client) => {
            let user = whoami(&client).await;
            Ok(Check::Ok { client, user })
        }
        Err(e) if e.is_auth_required() => Ok(Check::Login(e)),
        Err(e) => Err(e),
    }
}

async fn whoami(client: &AmpMcpClient) -> Option<String> {
    let v = tokio::time::timeout(WHOAMI_TIMEOUT, call_json(client, "amp_whoami", json!({})))
        .await
        .ok()?
        .ok()?;
    v["email"].as_str().map(str::to_owned)
}

/// The web UI for `url` when nothing says: `None` when its own origin serves the UI (or no probe
/// finds one), else what the probe found (docker compose: `http://localhost:5173`).
async fn probe_web_url(url: &str, ca_file: Option<&Path>) -> WebGuess {
    setup::guess_web_url(url, ca_file).await
}

fn clean_web_url(raw: &str) -> Result<String, String> {
    let url = setup::origin_of(raw.trim())
        .trim_end_matches('/')
        .to_owned();
    if !(url.starts_with("http://") || url.starts_with("https://")) || url.ends_with("://") {
        return Err(format!("`{raw}` is not an http(s) address"));
    }
    Ok(url)
}

// ---------------------------------------------------------------------------------------------
// The CLI
// ---------------------------------------------------------------------------------------------

#[derive(Args, Debug)]
#[command(args_conflicts_with_subcommands = true)]
pub struct ServerArgs {
    #[command(subcommand)]
    action: Option<ServerAction>,
    #[command(flatten)]
    show: ShowArgs,
}

#[derive(Subcommand, Debug)]
enum ServerAction {
    /// Show the server in effect (and where each setting comes from) and the saved servers.
    Show(ShowArgs),
    /// Check a server, save it (named) and make it the active one. It must answer first.
    Set(SetArgs),
    /// Make a saved server the active one.
    Use(UseArgs),
    /// Forget a saved server.
    Remove(RemoveArgs),
}

#[derive(Args, Debug, Default)]
struct ShowArgs {
    #[command(flatten)]
    conn: ConnArgs,
    /// Print JSON.
    #[arg(long)]
    json: bool,
}

/// What `set` and `use` share: the token variable and the login offer (`--config` is global).
#[derive(Args, Debug)]
struct WriteArgs {
    /// Read the static AMP token from this environment variable instead of AMP_TOKEN.
    #[arg(long)]
    token_env: Option<String>,
    /// Never ask (no login offer, no web UI question).
    #[arg(long, short = 'y')]
    yes: bool,
    /// When logging in, print the login URL instead of opening a browser.
    #[arg(long)]
    no_browser: bool,
    /// Print JSON.
    #[arg(long)]
    json: bool,
}

impl WriteArgs {
    fn conn(&self) -> ConnArgs {
        ConnArgs {
            token_env: self.token_env.clone(),
            ..ConnArgs::default()
        }
    }
}

#[derive(Args, Debug)]
struct SetArgs {
    /// The server: `host`, `host:port` or a full URL (`/mcp` is added when there is no path;
    /// localhost gets http, anything else https).
    url: String,
    /// Save it under this name (default: hosted, local, or the host's first label).
    #[arg(long)]
    name: Option<String>,
    /// The web UI's address, when it is not the server's own (found by a probe otherwise).
    #[arg(long)]
    web_url: Option<String>,
    /// Extra root certificate (PEM) for this server, e.g. the docker-desktop cluster's CA.
    #[arg(long)]
    ca_file: Option<PathBuf>,
    /// MCP protocol for this server: auto (default), 2026-07-28 or 2025-11-25.
    #[arg(long)]
    protocol: Option<String>,
    #[command(flatten)]
    write: WriteArgs,
}

#[derive(Args, Debug)]
struct UseArgs {
    /// A saved server's name (`amp-harness server` lists them).
    name: String,
    #[command(flatten)]
    write: WriteArgs,
}

#[derive(Args, Debug)]
struct RemoveArgs {
    /// A saved server's name.
    name: String,
}

pub async fn run(args: ServerArgs) -> Result<(), CliError> {
    match args.action {
        None => show(args.show),
        Some(ServerAction::Show(a)) => show(a),
        Some(ServerAction::Set(a)) => set(a).await,
        Some(ServerAction::Use(a)) => use_server(a).await,
        Some(ServerAction::Remove(a)) => remove(a),
    }
}

#[derive(Serialize)]
struct Setting {
    value: Option<String>,
    source: String,
}

#[derive(Serialize)]
struct ShowJson {
    config: Option<PathBuf>,
    /// The saved server in effect, if it is one.
    name: Option<String>,
    url: Setting,
    web_url: Setting,
    ca_file: Setting,
    protocol: Setting,
    auth: String,
    saved: Vec<SavedServer>,
}

fn show(args: ShowArgs) -> Result<(), CliError> {
    let config = config_file(&args.conn)?;
    let servers = Servers::load(&config).map_err(CliError::new)?;
    let settings = args.conn.settings()?;
    let name = servers.name_of(&settings.url).map(str::to_owned);
    let web_source = if env_set(ENV_WEB_URL).is_some() {
        Source::Env(ENV_WEB_URL.to_owned()).to_string()
    } else if settings.web_url.is_some() {
        Source::ConfigFile(config.path.clone()).to_string()
    } else {
        "the server's own origin".to_owned()
    };
    let out = ShowJson {
        config: settings.config_file.clone().or(Some(config.path.clone())),
        name: name.clone(),
        url: Setting {
            value: Some(settings.url.clone()),
            source: settings.url_source.to_string(),
        },
        web_url: Setting {
            value: Some(
                settings
                    .web_url
                    .clone()
                    .unwrap_or_else(|| setup::origin_of(&settings.url)),
            ),
            source: web_source,
        },
        ca_file: Setting {
            value: settings.ca_file.as_ref().map(|p| p.display().to_string()),
            source: settings
                .ca_file_source
                .as_ref()
                .map_or("none".to_owned(), ToString::to_string),
        },
        protocol: Setting {
            value: Some(settings.protocol.to_string()),
            source: settings.protocol_source.to_string(),
        },
        auth: match &settings.auth {
            Auth::None => "stored login (amp-harness login), if any".to_owned(),
            other => other.describe(),
        },
        saved: servers.saved.clone(),
    };
    if args.json {
        return conn::print_json(&out);
    }
    let title = match &name {
        Some(n) => format!("AMP server: {n}"),
        None => "AMP server".to_owned(),
    };
    println!("{title}");
    let row = |label: &str, s: &Setting| match &s.value {
        Some(value) => println!("  {label:<9} {value}  ({})", s.source),
        None => println!("  {label:<9} none"),
    };
    row("url", &out.url);
    row("web UI", &out.web_url);
    row("CA file", &out.ca_file);
    row("protocol", &out.protocol);
    println!("  {:<9} {}", "auth", out.auth);
    match &settings.url_source {
        Source::Flag => println!("  (--url wins over the saved servers for this command)"),
        Source::Env(var) => {
            println!("  ({var} is set and wins over the saved servers; unset it to use them)")
        }
        _ => {}
    }
    println!();
    if servers.saved.is_empty() {
        println!("No saved servers. Add one: amp-harness server set <url> [--name NAME]");
    } else {
        println!("Saved servers (amp-harness server use <name>):");
        for s in &servers.saved {
            let mark = if Some(&s.name) == name.as_ref() {
                "*"
            } else {
                " "
            };
            println!("{mark} {}", s.line());
        }
    }
    println!("Config: {}", config.path.display());
    Ok(())
}

#[derive(Serialize)]
struct SavedJson {
    server: SavedServer,
    config: PathBuf,
    backup: Option<PathBuf>,
    kept: Option<SavedServer>,
    /// `ok`, `login_needed`, `logged_in` or `unreachable`.
    status: &'static str,
    user: Option<String>,
    warnings: Vec<String>,
}

async fn set(args: SetArgs) -> Result<(), CliError> {
    let conn = args.write.conn();
    let config = config_file(&conn)?;
    let servers = Servers::load(&config).map_err(CliError::new)?;
    let url = setup::normalize_server_url(&args.url).map_err(CliError::usage)?;
    if let Some(name) = &args.name {
        valid_name(name).map_err(CliError::usage)?;
    }
    if let Some(p) = &args.protocol {
        p.parse::<ProtocolMode>()
            .map_err(|e| CliError::usage(format!("--protocol: {e}")))?;
    }
    let web_flag = args
        .web_url
        .as_deref()
        .map(clean_web_url)
        .transpose()
        .map_err(|e| CliError::usage(format!("--web-url: {e}")))?;
    let mut ui = ui::detect(!args.write.yes);

    // Same URL as a saved server: what the flags leave out keeps its saved value.
    let previous = match &args.name {
        Some(n) => servers.get(n).filter(|s| same_url(&s.url, &url)),
        None => servers.by_url(&url),
    };
    let name = match (&args.name, previous) {
        (Some(n), _) => n.clone(),
        (None, Some(p)) => p.name.clone(),
        (None, None) => derive_name(&url, &servers.saved),
    };
    let ca_file = match &args.ca_file {
        Some(p) => {
            let abs = std::path::absolute(p).unwrap_or_else(|_| p.clone());
            if !abs.is_file() {
                return Err(CliError::usage(format!(
                    "--ca-file {}: no such file",
                    p.display()
                )));
            }
            Some(abs.display().to_string())
        }
        None => previous.and_then(|p| p.ca_file.clone()),
    };
    let mut entry = SavedServer {
        name,
        url,
        web_url: None,
        ca_file,
        protocol: args
            .protocol
            .clone()
            .or_else(|| previous.and_then(|p| p.protocol.clone())),
    };
    let settings = entry_settings(&entry, &conn)?;

    // 1. It must answer before anything is saved.
    ui.say(&format!("Checking {} ...", entry.url));
    let checked = check(&settings).await.map_err(|e| {
        let mut message = format!(
            "Can't reach AMP at {} ({}), so nothing was saved. Check the address (and that AMP is \
             running there), then try again.",
            setup::host_of(&entry.url),
            e.reason()
        );
        if conn::verbose() {
            message.push_str(&format!("\n  cause: {e}"));
        }
        CliError::new(message)
    })?;

    // 2. Its web UI.
    entry.web_url = match web_flag {
        Some(url) => (url != setup::origin_of(&entry.url)).then_some(url),
        None => match probe_web_url(&entry.url, settings.ca_file.as_deref()).await {
            WebGuess::Same => None,
            WebGuess::Found(url) => Some(url),
            WebGuess::Unknown => {
                let saved = previous.and_then(|p| p.web_url.clone());
                if ui.interactive() {
                    let origin = setup::origin_of(&entry.url);
                    let default = saved.clone().unwrap_or_else(|| origin.clone());
                    loop {
                        let answer = ui.ask("Web UI address (for links)", Some(&default))?;
                        match clean_web_url(&answer) {
                            Ok(url) => break (url != origin).then_some(url),
                            Err(e) => ui.say(&format!("  {e}")),
                        }
                    }
                } else {
                    saved
                }
            }
        },
    };

    // 3. Save it and make it the active one.
    let activated = activate(&config, &entry).map_err(CliError::new)?;
    let mut out = SavedJson {
        server: entry.clone(),
        config: config.path.clone(),
        backup: activated.backup.clone(),
        kept: activated.kept.clone(),
        status: "ok",
        user: None,
        warnings: overrides_warnings(),
    };
    report_saved(&mut ui, &out, args.write.json, "Saved");

    // 4. A login, when it wants one.
    finish_login(&mut ui, &settings, checked, &args.write, &mut out).await?;
    if args.write.json {
        conn::print_json(&out)?;
    }
    Ok(())
}

async fn use_server(args: UseArgs) -> Result<(), CliError> {
    let conn = args.write.conn();
    let config = config_file(&conn)?;
    let servers = Servers::load(&config).map_err(CliError::new)?;
    let Some(entry) = servers.get(&args.name).cloned() else {
        return Err(CliError::usage(format!(
            "no saved server `{}` (saved: {}); add one with `amp-harness server set <url> --name {}`",
            args.name,
            servers.names(),
            args.name
        )));
    };
    let mut ui = ui::detect(!args.write.yes);
    let settings = entry_settings(&entry, &conn)?;
    let checked = check(&settings).await;
    let activated = activate(&config, &entry).map_err(CliError::new)?;
    let mut out = SavedJson {
        server: entry.clone(),
        config: config.path.clone(),
        backup: activated.backup.clone(),
        kept: activated.kept.clone(),
        status: "ok",
        user: None,
        warnings: overrides_warnings(),
    };
    report_saved(&mut ui, &out, args.write.json, "Now using");
    match checked {
        Ok(checked) => finish_login(&mut ui, &settings, checked, &args.write, &mut out).await?,
        Err(e) => {
            out.status = "unreachable";
            let w = format!(
                "{} did not answer ({}); it is the active server anyway. `amp-harness doctor` \
                 checks it again.",
                entry.url,
                e.reason()
            );
            if !args.write.json {
                ui.say(&format!("warning: {w}"));
            }
            out.warnings.push(w);
        }
    }
    if args.write.json {
        conn::print_json(&out)?;
    }
    Ok(())
}

fn remove(args: RemoveArgs) -> Result<(), CliError> {
    let config = config_file(&ConnArgs::default())?;
    let servers = Servers::load(&config).map_err(CliError::new)?;
    let Some(entry) = servers.get(&args.name) else {
        return Err(CliError::usage(format!(
            "no saved server `{}` (saved: {})",
            args.name,
            servers.names()
        )));
    };
    let mut changes = vec![Change::DropSection(format!("{SERVERS}.{}", entry.name))];
    let active = servers
        .active
        .as_ref()
        .is_some_and(|a| a.name == entry.name);
    if active {
        changes.push(Change::remove("server", "name"));
    }
    config.edit(&changes).map_err(CliError::new)?;
    println!("Removed the saved server `{}` ({}).", entry.name, entry.url);
    if active {
        println!(
            "It is still the server in effect until you `amp-harness server use` another one."
        );
    }
    Ok(())
}

/// `--url` / AMP_MCP_URL name a server in this shell and win over config.toml.
fn overrides_warnings() -> Vec<String> {
    match env_set(ENV_MCP_URL) {
        Some(url) => vec![format!(
            "{ENV_MCP_URL}={url} is set in this shell and wins over the saved server; unset it \
             (and remove it from your shell profile) to use the saved one"
        )],
        None => Vec::new(),
    }
}

fn report_saved(ui: &mut Box<dyn Ui>, out: &SavedJson, json: bool, verb: &str) {
    if json {
        return;
    }
    let s = &out.server;
    println!("{verb} AMP server `{}`: {}", s.name, s.url);
    match &s.web_url {
        Some(web) => println!("  web UI   {web}"),
        None => println!("  web UI   {} (the server's own)", setup::origin_of(&s.url)),
    }
    if let Some(ca) = &s.ca_file {
        println!("  CA file  {ca}");
    }
    println!("  config   {}", out.config.display());
    if let Some(backup) = &out.backup {
        println!("  backup   {}", backup.display());
    }
    if let Some(kept) = &out.kept {
        println!(
            "  kept your previous server as `{}` ({}): amp-harness server use {}",
            kept.name, kept.url, kept.name
        );
    }
    for w in &out.warnings {
        ui.say(&format!("warning: {w}"));
    }
}

/// After saving: say who we are, or offer the login the server wants.
async fn finish_login(
    ui: &mut Box<dyn Ui>,
    settings: &Settings,
    checked: Check,
    args: &WriteArgs,
    out: &mut SavedJson,
) -> Result<(), CliError> {
    let host = setup::host_of(&settings.url);
    match checked {
        Check::Ok { client, user } => {
            if let Some(user) = &user
                && !args.json
            {
                println!("Signed in to {host} as {user}.");
            }
            out.user = user;
            client.close().await;
            Ok(())
        }
        Check::Login(e) => {
            out.status = "login_needed";
            if let Auth::Static { env, .. } = &settings.auth {
                let w = format!("{host} refused the token in {env}: set a new one for it");
                if !args.json {
                    ui.say(&format!("warning: {w}"));
                }
                out.warnings.push(w);
                return Ok(());
            }
            let go = ui.interactive() && {
                ui.say(&format!(
                    "{host} needs you to log in ({e}). Your login is kept in the OS keychain."
                ));
                ui.confirm("Log in with your browser now?", true)?
            };
            if !go {
                let w = format!(
                    "{host} needs a login: run `amp-harness login` (it logs in to the active server)"
                );
                if !args.json {
                    ui.say(&w);
                }
                out.warnings.push(w);
                return Ok(());
            }
            crate::browser_login(
                settings,
                args.no_browser,
                amp_mcp::oauth::DEFAULT_LOGIN_TIMEOUT,
            )
            .await?;
            out.status = "logged_in";
            if let Ok(Check::Ok { client, user }) = check(settings).await {
                if let Some(user) = &user
                    && !args.json
                {
                    println!("Logged in to {host} as {user}.");
                }
                out.user = user;
                client.close().await;
            }
            Ok(())
        }
    }
}

// ---------------------------------------------------------------------------------------------
// `/server` in the chat
// ---------------------------------------------------------------------------------------------

/// The chat's [`ServerControl`]: lists the servers from config.toml and switches the live session.
pub struct Switcher {
    conn: ConnArgs,
    rt: Arc<Runtime>,
    harness: Arc<AgentHarness>,
    policy: Arc<ToolPolicy>,
    /// The session's `.amp.json`: its project stays selected after a switch, and a marker made
    /// for another server than the new one is called out (#2228, like the start's banner, #2206).
    marker: Option<ProjectMarker>,
    /// Start a metadata poller for the new server (the chat has one).
    with_meta: bool,
    state: Mutex<SwitchState>,
    /// One connection change at a time: a `/server` switch, or the offline retry (#2178).
    busy: tokio::sync::Mutex<()>,
}

struct SwitchState {
    settings: Settings,
    /// The AMP tools the model has now (from the catalog its tools were built from).
    amp_tools: BTreeSet<String>,
}

impl Switcher {
    /// For a session whose AMP tools were just built from `rt.mcp`'s catalog.
    pub async fn new(
        conn: ConnArgs,
        settings: Settings,
        rt: Arc<Runtime>,
        harness: Arc<AgentHarness>,
        policy: Arc<ToolPolicy>,
        marker: Option<ProjectMarker>,
        with_meta: bool,
    ) -> Self {
        // Offline (#2178): no AMP tools yet, and no connect attempt here (it would hold up the
        // chat's start); [`Switcher::connect_offline`] makes it.
        let amp_tools = match rt.mcp.is_online() {
            true => match rt.mcp.catalog().await {
                Ok(c) => c.names().map(str::to_owned).collect(),
                Err(_) => BTreeSet::new(),
            },
            false => BTreeSet::new(),
        };
        Self {
            conn,
            rt,
            harness,
            policy,
            marker,
            with_meta,
            state: Mutex::new(SwitchState {
                settings,
                amp_tools,
            }),
            busy: tokio::sync::Mutex::new(()),
        }
    }

    /// The chat started offline (#2178): one attempt to connect to the current server. On success
    /// the session's connection, the model's AMP tools and its prompt move onto it, as a
    /// `/server` switch does, and the result is the line for the transcript. `Ok(None)` when
    /// the session is already online (a `/server` got there first).
    pub async fn connect_offline(&self) -> Result<Option<String>, McpError> {
        let _busy = self.busy.lock().await;
        if self.rt.mcp.is_online() {
            return Ok(None);
        }
        let settings = self.current();
        let client = conn::connect(&settings).await?;
        let new_tools: BTreeSet<String> =
            client.catalog().await?.names().map(str::to_owned).collect();
        let old_tools = self
            .state
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .amp_tools
            .clone();
        self.rt.mcp.replace(client);
        self.rt.mcp.set_reconnector(conn::reconnector(&settings));
        let retargeted = self
            .rt
            .retarget(&self.harness, &self.policy, &old_tools)
            .await;
        self.state
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .amp_tools = new_tools;
        let host = setup::host_of(&settings.url);
        Ok(Some(match retargeted {
            Ok(r) => format!(
                "Connected to AMP at {host}: the manager has its {} AMP tools now.",
                r.amp_tools
            ),
            Err(e) => format!(
                "Connected to AMP at {host}, but the manager's tools were not updated ({e}); \
                 restart the chat to use AMP."
            ),
        }))
    }

    fn current(&self) -> Settings {
        self.state
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .settings
            .clone()
    }

    fn servers(&self) -> Result<(ConfigFile, Servers), String> {
        let config = config_file(&self.conn).map_err(|e| e.message)?;
        let servers = Servers::load(&config)?;
        Ok((config, servers))
    }

    /// Which server `target` names: a saved one, or a new address (saved once it works).
    async fn resolve(&self, target: &str, servers: &Servers) -> Result<SavedServer, String> {
        if let Some(s) = servers.get(target) {
            return Ok(s.clone());
        }
        let address = target.contains(['.', ':', '/']) || target == "localhost";
        if !address {
            return Err(format!(
                "no saved server `{target}` (saved: {}). `/server <address>` adds one, as does \
                 `amp-harness server set <url> --name {target}`",
                servers.names()
            ));
        }
        let url = setup::normalize_server_url(target)?;
        if let Some(s) = servers.by_url(&url) {
            return Ok(s.clone());
        }
        let web_url = match probe_web_url(&url, None).await {
            WebGuess::Found(u) => Some(u),
            WebGuess::Same | WebGuess::Unknown => None,
        };
        Ok(SavedServer {
            name: derive_name(&url, &servers.saved),
            url,
            web_url,
            ca_file: None,
            protocol: None,
        })
    }

    async fn switch_to(self: Arc<Self>, target: String) -> Result<ServerSwitched, String> {
        let _busy = self.busy.lock().await;
        let target = target.trim().to_owned();
        let (config, servers) = self.servers()?;
        let entry = self.resolve(&target, &servers).await?;
        let current = self.current();
        let here = setup::host_of(&current.url);
        // Offline (#2178), `/server <the same one>` connects it now instead of waiting for the retry.
        if same_url(&entry.url, &current.url) && self.rt.mcp.is_online() {
            return Ok(ServerSwitched {
                notice: format!("already on `{}` ({})", entry.name, entry.url),
                meta: None,
            });
        }
        let settings = entry_settings(&entry, &self.conn).map_err(|e| e.to_string())?;
        let host = setup::host_of(&settings.url);

        // Connect first: on any failure the session stays where it is.
        let (client, user) = match check(&settings).await {
            Ok(Check::Ok { client, user }) => (client, user),
            Ok(Check::Login(_)) => {
                return Err(match &settings.auth {
                    Auth::Static { env, .. } => {
                        format!("{host} refused the token in {env}; still on {here}")
                    }
                    Auth::None => format!(
                        "{host} needs a login: run `amp-harness login --url {}` in a shell, then \
                         `/server {target}` again. Still on {here}.",
                        settings.url
                    ),
                });
            }
            Err(e) => {
                return Err(format!(
                    "can't reach {} ({}). Still on {here}.",
                    entry.url,
                    e.reason()
                ));
            }
        };
        // Its tool list, fetched before the swap so a server without one changes nothing.
        let catalog = client.catalog().await.map_err(|e| {
            format!(
                "{host} did not list its tools ({}). Still on {here}.",
                e.reason()
            )
        })?;
        let new_tools: BTreeSet<String> = catalog.names().map(str::to_owned).collect();

        let old_tools = self
            .state
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .amp_tools
            .clone();
        self.rt.mcp.replace(client);
        self.rt.mcp.set_reconnector(conn::reconnector(&settings));
        let retargeted = self
            .rt
            .retarget(&self.harness, &self.policy, &old_tools)
            .await;
        {
            let mut state = self.state.lock().unwrap_or_else(PoisonError::into_inner);
            state.settings = settings.clone();
            state.amp_tools = new_tools;
        }

        let mut lines = vec![format!(
            "now on `{}` · {}{}",
            entry.name,
            settings.url,
            user.map(|u| format!(" · signed in as {u}"))
                .unwrap_or_default()
        )];
        match retargeted {
            Ok(r) => lines.push(format!(
                "the manager's AMP tools: {}; its prompt names the new server",
                tool_change(r.amp_tools, &r.added, &r.removed, conn::verbose())
            )),
            Err(e) => lines.push(format!(
                "warning: the manager's tools were not updated ({e}); restart the chat"
            )),
        }
        match activate(&config, &entry) {
            Ok(a) => {
                let mut line = format!("saved as the active server in {}", config.path.display());
                if let Some(kept) = a.kept {
                    line.push_str(&format!(
                        " (your previous server is kept as `{}`)",
                        kept.name
                    ));
                }
                lines.push(line);
            }
            Err(e) => lines.push(format!(
                "warning: not saved ({e}); the next start uses the old server"
            )),
        }
        if self.conn.url.is_some() {
            lines.push("note: --url still names the old server for the next start".to_owned());
        } else if env_set(ENV_MCP_URL).is_some() {
            lines.push(format!(
                "note: {ENV_MCP_URL} still names the old server for the next start"
            ));
        }
        if let Some(line) = self.marker.as_ref().map(|m| marker_line(m, &settings.url)) {
            lines.push(line);
        }
        let project_id = self.marker.as_ref().map(|m| m.project_id);
        let meta = self
            .with_meta
            .then(|| crate::chat::start_meta_with(settings, project_id));
        Ok(ServerSwitched {
            notice: lines.join("\n"),
            meta,
        })
    }
}

/// The switch notice's line about `.amp.json` (#2228): its project stays selected, or, when the
/// marker was made for another server than `url` (its `amp_api`, compared by origin as at the
/// start, #2206), one warning naming both servers and `/project`. Never a prompt: the chat goes on.
fn marker_line(marker: &ProjectMarker, url: &str) -> String {
    let pid = marker.project_id;
    // With no server name to compare, only a marker made for another server warns.
    match marker.server_warning(url, None) {
        Some(w) => format!("warning: {w}; `/project <id>` picks this server's"),
        None => format!("project #{pid} (.amp.json) stays selected; `/project <id>` picks another"),
    }
}

impl ServerControl for Switcher {
    fn describe(&self) -> String {
        let current = self.current();
        let mut lines = Vec::new();
        let servers = self.servers();
        let name = servers
            .as_ref()
            .ok()
            .and_then(|(_, s)| s.name_of(&current.url).map(str::to_owned));
        lines.push(format!(
            "AMP server: {}{}",
            name.map(|n| format!("`{n}` · ")).unwrap_or_default(),
            current.url
        ));
        if let Some(web) = &current.web_url {
            lines.push(format!("web UI: {web}"));
        }
        match &servers {
            Ok((config, s)) if !s.saved.is_empty() => {
                lines.push(format!("saved ({}):", config.path.display()));
                for saved in &s.saved {
                    let mark = if same_url(&saved.url, &current.url) {
                        "*"
                    } else {
                        " "
                    };
                    lines.push(format!("{mark} {}", saved.line()));
                }
            }
            Ok(_) => lines.push("no saved servers yet".to_owned()),
            Err(e) => lines.push(format!("saved servers: {e}")),
        }
        lines.push(
            "/server <name> switches · /server <address> adds one and switches · \
             `amp-harness server` in a shell for more"
                .to_owned(),
        );
        lines.join("\n")
    }

    fn switch(
        self: Arc<Self>,
        target: String,
    ) -> std::pin::Pin<Box<dyn std::future::Future<Output = Result<ServerSwitched, String>> + Send>>
    {
        Box::pin(self.switch_to(target))
    }
}

/// Most tool names a `/server` notice lists; a bigger change (e.g. 0 to 75 tools, #2211) is
/// summarised by counts unless `--verbose`.
const LIST_TOOL_NAMES_UP_TO: usize = 5;

/// How the manager's AMP tool set changed with a switch: `75 (the same set)`,
/// `74 (-amp_kb_write)`, or `75 (was 0; 75 added)`.
fn tool_change(now: usize, added: &[String], removed: &[String], verbose: bool) -> String {
    if added.is_empty() && removed.is_empty() {
        return format!("{now} (the same set)");
    }
    if verbose || added.len() + removed.len() <= LIST_TOOL_NAMES_UP_TO {
        let mut diff = Vec::new();
        if !added.is_empty() {
            diff.push(format!("+{}", added.join(" +")));
        }
        if !removed.is_empty() {
            diff.push(format!("-{}", removed.join(" -")));
        }
        return format!("{now} ({})", diff.join(" "));
    }
    let was = (now + removed.len()).saturating_sub(added.len());
    let mut counts = Vec::new();
    if !added.is_empty() {
        counts.push(format!("{} added", added.len()));
    }
    if !removed.is_empty() {
        counts.push(format!("{} removed", removed.len()));
    }
    format!("{now} (was {was}; {})", counts.join(", "))
}

#[cfg(test)]
mod tests {

    /// #2228: after `/server`, a marker made for another server is one warning naming both
    /// servers and `/project`; a marker for the new server (any loopback spelling) is not.
    #[test]
    fn the_switch_notice_warns_about_a_marker_made_for_another_server() {
        let marker = ProjectMarker::parse(
            std::path::Path::new("/w/.amp.json"),
            r#"{"project_id": 71, "project_name": "AMP v2", "amp_api": "http://localhost:18000"}"#,
        )
        .unwrap();
        let line = marker_line(&marker, "http://localhost:18001/mcp");
        assert_eq!(
            line,
            "warning: .amp.json at /w/.amp.json was made for http://localhost:18000; #71 on this \
             server (http://localhost:18001) may be another project; `/project <id>` picks this \
             server's"
        );
        assert!(!line.contains('\n'));
        for same in ["http://localhost:18000/mcp", "http://127.0.0.1:18000/mcp"] {
            assert_eq!(
                marker_line(&marker, same),
                "project #71 (.amp.json) stays selected; `/project <id>` picks another"
            );
        }
    }

    fn tools(n: usize) -> Vec<String> {
        (0..n).map(|i| format!("amp_t{i}")).collect()
    }

    #[test]
    fn tool_change_summarises_big_diffs_and_lists_small_ones() {
        assert_eq!(tool_change(75, &[], &[], false), "75 (the same set)");
        // #2211: 0 -> 75 is one short line, no names.
        let line = tool_change(75, &tools(75), &[], false);
        assert_eq!(line, "75 (was 0; 75 added)");
        assert!(!line.contains("amp_t"));
        assert_eq!(
            tool_change(70, &tools(2), &tools(7), false),
            "70 (was 75; 2 added, 7 removed)"
        );
        // Small diffs still name the tools.
        assert_eq!(
            tool_change(74, &[], &["amp_kb_write".to_owned()], false),
            "74 (-amp_kb_write)"
        );
        assert_eq!(
            tool_change(75, &tools(3), &tools(2), false),
            "75 (+amp_t0 +amp_t1 +amp_t2 -amp_t0 -amp_t1)"
        );
        // --verbose lists every name.
        let verbose = tool_change(75, &tools(75), &[], true);
        assert!(verbose.starts_with("75 (+amp_t0 +amp_t1"));
        assert!(verbose.contains("+amp_t74"));
    }
    use super::*;

    fn saved(name: &str, url: &str) -> SavedServer {
        SavedServer {
            name: name.to_owned(),
            url: url.to_owned(),
            web_url: None,
            ca_file: None,
            protocol: None,
        }
    }

    #[test]
    fn names() {
        assert_eq!(derive_name(DEFAULT_MCP_URL, &[]), "hosted");
        assert_eq!(derive_name("http://localhost:18000/mcp", &[]), "local");
        assert_eq!(derive_name("https://amp.local/mcp", &[]), "amp");
        assert_eq!(derive_name("https://My_Box.example.com/mcp", &[]), "my-box");
        // Taken by another URL: the port, then a number.
        let taken = [saved("local", "http://localhost:8000/mcp")];
        assert_eq!(
            derive_name("http://localhost:18000/mcp", &taken),
            "local-18000"
        );
        let taken = [
            saved("amp", "https://amp.one/mcp"),
            saved("amp-2", "https://amp.two/mcp"),
        ];
        assert_eq!(derive_name("https://amp.three/mcp", &taken), "amp-3");
        // Its own name again.
        assert_eq!(derive_name("https://amp.one/mcp", &taken), "amp");
        assert!(valid_name("local-2_x").is_ok());
        for bad in ["", "a b", "a.b", "x\"", &"n".repeat(41)] {
            assert!(valid_name(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn parses_saved_and_active() {
        let s = Servers::parse(
            "[server]\nurl = \"http://localhost:18000/mcp\"\nname = \"local\"\n\n\
             [servers.local]\nurl = \"http://localhost:18000/mcp\"\nweb_url = \"http://localhost:5173\"\n\n\
             [servers.hosted]\nurl = \"https://amp.simstech.cloud/mcp\"\n\n[servers.broken]\nweb_url = \"x\"\n",
        )
        .unwrap();
        assert_eq!(
            s.saved.iter().map(|s| s.name.as_str()).collect::<Vec<_>>(),
            ["hosted", "local"]
        );
        assert_eq!(s.name_of("http://localhost:18000/mcp"), Some("local"));
        assert_eq!(s.name_of("https://amp.simstech.cloud/mcp/"), Some("hosted"));
        assert_eq!(s.name_of("https://other/mcp"), None);
        assert_eq!(
            s.get("local").unwrap().web_url.as_deref(),
            Some("http://localhost:5173")
        );
    }

    #[test]
    fn activate_keeps_an_unnamed_server_and_round_trips() {
        let dir =
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../../target/test-scratch/server-activate");
        let _ = std::fs::remove_dir_all(&dir);
        let config = ConfigFile::new(dir.join("amp-harness/config.toml"));
        std::fs::create_dir_all(dir.join("amp-harness")).unwrap();
        let before = "# my settings\n[server]\nurl = \"https://amp.local/mcp\"\nca_file = \"/certs/ca.pem\" # cluster\n\n[tui]\ntheme = \"light\"\n";
        std::fs::write(&config.path, before).unwrap();

        let local = SavedServer {
            web_url: Some("http://localhost:5173".into()),
            ..saved("local", "http://localhost:18000/mcp")
        };
        let a = activate(&config, &local).unwrap();
        assert_eq!(std::fs::read_to_string(a.backup.unwrap()).unwrap(), before);
        let kept = a.kept.unwrap();
        assert_eq!(kept.name, "amp");
        assert_eq!(kept.ca_file.as_deref(), Some("/certs/ca.pem"));
        let s = Servers::load(&config).unwrap();
        let active = s.active.clone().unwrap();
        assert_eq!(active.name, "local");
        assert_eq!(active.web_url.as_deref(), Some("http://localhost:5173"));
        // The old CA belongs to the old server: gone from [server].
        assert_eq!(active.ca_file, None);
        assert_eq!(config.get("tui", "theme").as_deref(), Some("light"));
        let text = std::fs::read_to_string(&config.path).unwrap();
        assert!(text.starts_with("# my settings\n"), "{text}");

        // And back: [server] is the kept one again, with its CA, and nothing new is kept.
        let a = activate(&config, s.get("amp").unwrap()).unwrap();
        assert!(a.kept.is_none());
        let s = Servers::load(&config).unwrap();
        let active = s.active.unwrap();
        assert_eq!(active.name, "amp");
        assert_eq!(active.ca_file.as_deref(), Some("/certs/ca.pem"));
        assert_eq!(active.web_url, None);
        assert_eq!(s.saved.len(), 2);
        let _ = std::fs::remove_dir_all(&dir);
    }
}
