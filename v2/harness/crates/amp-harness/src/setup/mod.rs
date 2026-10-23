//! First run and `amp-harness init` (D12, M4-1): everything between downloading the binary and
//! chatting with the manager.
//!
//! One engine ([`run`]) serves both:
//! - **first run**: `amp-harness` in a terminal asks only for what is missing: the server (when
//!   nothing names one), a browser login (when the server wants one), a project for this folder
//!   (when there is no `.amp.json`), and a provider key (when the roles' providers have none).
//!   Then the chat opens.
//! - **`init`**: the same steps on purpose, with every answer available as a flag. In a terminal
//!   it asks for what the flags leave open (defaulting to the current setting); with `--yes` or
//!   without a terminal it never asks (`init --url … --project … --token-env AMP_TOKEN`).
//!
//! Steps, in order:
//! 1. **Server** (D4): `--url` / the answer is tried first and saved to config.toml `[server] url`
//!    only once it answers. When the server's own origin doesn't serve the web UI (docker compose
//!    serves the MCP on :18000 and the UI on :5173), the UI address is found by probing and saved
//!    as `web_url`, so links in the chat open the UI.
//! 2. **Login** (D3): a server that answers 401 gets the browser OAuth login, stored in the OS
//!    keychain. A static token (`AMP_TOKEN` / `--token-env`) is used as is and never written.
//! 3. **Who**: `amp_whoami`.
//! 4. **Project**: the nearest `.amp.json` is used. Otherwise the person picks one of their
//!    projects (`amp_list_projects`) or creates one (`amp_create_project`, org from
//!    `amp_list_orgs`), and setup writes `.amp.json` at the repository root (or the folder) with
//!    the real AMP origin in `amp_api`.
//! 5. **Models**: if the selected profile's providers have no key, setup switches to a profile
//!    whose keys exist (an Anthropic key alone selects `profile = "anthropic"`), or asks which
//!    provider the person has a key for and reads the key **hidden**, checks it with the provider,
//!    and stores it **only in the OS keychain** (service `amp-harness`, account
//!    `provider:<id>`). Keys never go to a file or to the output. With no keychain, setup says to
//!    set the provider's environment variable instead.
//! 6. **PATH**: a hint when the binary's directory is not on PATH.
//!
//! The chat then opens as the manager. A project created here, or a person with no projects yet,
//! gets a suggested first prompt in the editor (seed the KB with amp-init / the `amp_guide`
//! onboarding), sent only when they press Enter.

pub(crate) mod config_file;
pub mod ui;

use std::{
    collections::BTreeSet,
    path::{Path, PathBuf},
    time::Duration,
};

use amp_core::{
    models::{ENV_PROFILE, ModelsConfig, ProviderApi, ProviderConfig, Selection},
    project::{ENV_CEILING_DIR, MARKER_FILE, ProjectMarker},
    providers::{KeyLookup, SystemKeys, keychain_account, resolve_key},
    tools::call_json,
};
use amp_mcp::{AmpMcpClient, Auth, DEFAULT_MCP_URL, McpError, Settings, Source};
use clap::Args;
use serde::Serialize;
use serde_json::{Value, json};

pub use config_file::{Change, ConfigFile};
use ui::Ui;

use crate::conn::{self, CliError, ConnArgs};

/// How long the web UI probe waits for one answer.
const WEB_PROBE_TIMEOUT: Duration = Duration::from_secs(3);
/// How long a provider key check waits.
const KEY_CHECK_TIMEOUT: Duration = Duration::from_secs(15);
/// The web UI port of the docker compose stack (`v2/docker-compose.yml`).
const COMPOSE_UI_PORT: u16 = 5173;
/// How many projects the picker lists before asking for `#id`.
const PICKER_MAX: usize = 20;
/// The shipped default profile's description (it has none in models.toml).
const DEFAULT_PROFILE_TEXT: &str = "the opencode roster on OpenRouter (MiMo, DeepSeek, GLM, Kimi)";

#[derive(Args, Debug, Default)]
pub struct InitArgs {
    #[command(flatten)]
    pub conn: ConnArgs,
    /// The web UI address, when it is not the MCP URL's origin (saved as [server] web_url).
    #[arg(long, value_name = "URL")]
    pub web_url: Option<String>,
    /// Link this folder to an existing AMP project (writes .amp.json).
    #[arg(long, value_name = "ID", conflicts_with_all = ["create", "no_project"])]
    pub project: Option<i64>,
    /// Create a new AMP project with this name and link this folder to it.
    #[arg(long, value_name = "NAME", conflicts_with = "no_project")]
    pub create: Option<String>,
    /// With --create: the project code (2-6 letters; default: from the name).
    #[arg(long, requires = "create")]
    pub code: Option<String>,
    /// With --create: the org (default: your only org).
    #[arg(long, value_name = "ID", requires = "create")]
    pub org: Option<i64>,
    /// With --create: the owning team (needed only if you are on several teams in the org).
    #[arg(long, value_name = "ID", requires = "create")]
    pub team: Option<i64>,
    /// Leave the project alone (no .amp.json).
    #[arg(long)]
    pub no_project: bool,
    /// Where .amp.json goes (default: the repository root, else the current directory).
    #[arg(long, value_name = "DIR")]
    pub dir: Option<PathBuf>,
    /// Store the provider key found in this environment variable in the OS keychain
    /// (e.g. --key-env OPENROUTER_API_KEY). The key is never printed or written to a file.
    #[arg(long, value_name = "VAR")]
    pub key_env: Option<String>,
    /// With --key-env: the provider the key is for (default: the provider whose key_env is VAR).
    #[arg(long, value_name = "ID", requires = "key_env")]
    pub provider: Option<String>,
    /// The model profile: opencode, anthropic, inherit, or one of yours (saved as [models] profile).
    #[arg(long, value_name = "NAME")]
    pub profile: Option<String>,
    /// Allow the browser login with --yes (without --yes, init asks).
    #[arg(long)]
    pub login: bool,
    /// Print the login URL instead of opening a browser.
    #[arg(long)]
    pub no_browser: bool,
    /// Never ask: use the flags and what is already set up.
    #[arg(long, short = 'y')]
    pub yes: bool,
    /// Print a JSON summary on stdout.
    #[arg(long)]
    pub json: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    /// Before the chat: only what is missing, quietly.
    FirstRun,
    /// `amp-harness init`.
    Init,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ProjectArg {
    /// Use `.amp.json`, else ask (interactive) or leave it.
    Auto,
    Id(i64),
    Create {
        name: String,
        code: Option<String>,
        org: Option<i64>,
        team: Option<i64>,
    },
    Skip,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LoginPolicy {
    Ask,
    Allow,
    Never,
}

/// What setup was asked to do.
pub struct Plan {
    pub mode: Mode,
    pub conn: ConnArgs,
    pub web_url: Option<String>,
    pub project: ProjectArg,
    /// Where `.amp.json` goes; default: the repository root of `cwd`, else `cwd`.
    pub dir: Option<PathBuf>,
    pub cwd: PathBuf,
    pub key_env: Option<String>,
    pub provider: Option<String>,
    pub profile: Option<String>,
    pub login: LoginPolicy,
    pub no_browser: bool,
}

/// What setup did (`init --json`; never contains a secret).
#[derive(Debug, Default, Serialize)]
pub struct Summary {
    pub server: String,
    pub web_url: Option<String>,
    pub config: Option<PathBuf>,
    pub auth: String,
    pub user: Option<String>,
    pub project: Option<ProjectSummary>,
    pub profile: Option<String>,
    /// Provider id -> where its key comes from (`env VAR`, `keychain`, `missing`, `not needed`).
    pub providers: std::collections::BTreeMap<String, String>,
    pub warnings: Vec<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ProjectSummary {
    pub id: i64,
    pub name: Option<String>,
    pub code: Option<String>,
    pub marker: PathBuf,
    pub created: bool,
    /// `.amp.json` was written by this run.
    #[serde(skip)]
    pub linked_now: bool,
}

/// The result for the chat.
#[derive(Debug, Default)]
pub struct Outcome {
    /// A suggested first prompt for the editor (sent only when the person presses Enter).
    pub prefill: Option<String>,
    pub summary: Summary,
}

// ---------------------------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------------------------

/// `amp-harness init`.
pub async fn init(args: InitArgs) -> Result<(), CliError> {
    let cwd =
        std::env::current_dir().map_err(|e| CliError::new(format!("current directory: {e}")))?;
    let project = if args.no_project {
        ProjectArg::Skip
    } else if let Some(id) = args.project {
        ProjectArg::Id(id)
    } else if let Some(name) = args.create.clone() {
        ProjectArg::Create {
            name,
            code: args.code.clone(),
            org: args.org,
            team: args.team,
        }
    } else {
        ProjectArg::Auto
    };
    let mut ui = ui::detect(!args.yes);
    let login = match (ui.interactive(), args.login) {
        (true, _) => LoginPolicy::Ask,
        (false, true) => LoginPolicy::Allow,
        (false, false) => LoginPolicy::Never,
    };
    let plan = Plan {
        mode: Mode::Init,
        conn: args.conn.clone(),
        web_url: args.web_url.clone(),
        project,
        dir: args.dir.clone(),
        cwd,
        key_env: args.key_env.clone(),
        provider: args.provider.clone(),
        profile: args.profile.clone(),
        login,
        no_browser: args.no_browser,
    };
    let outcome = run(&plan, ui.as_mut()).await?;
    if args.json {
        return conn::print_json(&outcome.summary);
    }
    let s = &outcome.summary;
    for w in &s.warnings {
        ui.say(&format!("warning: {w}"));
    }
    println!("Ready. Run `amp-harness` in this folder to chat with the manager.");
    println!("  server:  {}", s.server);
    if let Some(user) = &s.user {
        println!("  user:    {user} (auth: {})", s.auth);
    }
    match &s.project {
        Some(p) => println!(
            "  project: #{} {} ({})",
            p.id,
            p.name.as_deref().unwrap_or(""),
            p.marker.display()
        ),
        None => println!("  project: none linked"),
    }
    if let Some(profile) = &s.profile {
        println!("  models:  profile {profile}");
    }
    for (id, source) in &s.providers {
        println!("  key:     {id}: {source}");
    }
    Ok(())
}

/// The chat's first run: asks only for what is missing. `None` when there is no one to ask
/// (no terminal), so the chat starts as before.
pub async fn first_run(conn: &ConnArgs, cwd: Option<&Path>) -> Result<Option<Outcome>, CliError> {
    let mut ui = ui::detect(true);
    if !ui.interactive() {
        return Ok(None);
    }
    let cwd = match cwd {
        Some(dir) => dir.to_path_buf(),
        None => {
            std::env::current_dir().map_err(|e| CliError::new(format!("current directory: {e}")))?
        }
    };
    let plan = Plan {
        mode: Mode::FirstRun,
        conn: conn.clone(),
        web_url: None,
        project: ProjectArg::Auto,
        dir: None,
        cwd,
        key_env: None,
        provider: None,
        profile: None,
        login: LoginPolicy::Ask,
        no_browser: false,
    };
    match run(&plan, ui.as_mut()).await {
        // AMP is unreachable: the chat opens without it and says so (#2178).
        Err(e) if e.offline => Ok(None),
        other => other.map(Some),
    }
}

// ---------------------------------------------------------------------------------------------
// The engine
// ---------------------------------------------------------------------------------------------

fn env_var(key: &str) -> Option<String> {
    std::env::var(key).ok().filter(|v| !v.trim().is_empty())
}

/// The config.toml setup reads and writes: the global `--config` / `--config-dir` (#2232), else
/// the default path.
pub(crate) fn config_path(_conn: &ConnArgs) -> Result<PathBuf, CliError> {
    conn::user_config_file()
}

pub async fn run(plan: &Plan, ui: &mut dyn Ui) -> Result<Outcome, CliError> {
    let path = config_path(&plan.conn)?;
    let config = ConfigFile::new(path.clone());
    let config_existed = config.exists();
    let mut summary = Summary {
        config: Some(path.clone()),
        ..Summary::default()
    };
    let init = plan.mode == Mode::Init;
    if init && ui.interactive() {
        ui.say("Setting up amp-harness. Press Enter to keep the value in [brackets].");
    }

    // 1. Server.
    let (client, settings, server_chosen) = server_step(plan, ui, &config).await?;
    summary.server = settings.url.clone();
    summary.auth = client.session().auth.clone();

    // Web UI address.
    summary.web_url = web_step(plan, ui, &config, &settings, server_chosen).await?;

    // 3. Who.
    let whoami = call_json(&client, "amp_whoami", json!({})).await?;
    let user = whoami["email"]
        .as_str()
        .map(str::to_owned)
        .unwrap_or_else(|| "(unknown user)".to_owned());
    if init || server_chosen {
        ui.say(&format!(
            "Signed in to {} as {user}.",
            host_of(&settings.url)
        ));
    }
    summary.user = Some(user);

    // 4. Project.
    let (project, new_user) =
        project_step(plan, ui, &client, &settings, &mut summary.warnings).await?;
    summary.project = project.clone();

    // 5. Models and keys.
    models_step(plan, ui, &config, &mut summary).await?;

    // 6. PATH: when this run set something up (not on every chat start).
    let linked_now = project
        .as_ref()
        .is_some_and(|p| plan.mode == Mode::Init || p.linked_now);
    if (init || server_chosen || linked_now || !config_existed && config.exists())
        && let Some(hint) = path_hint()
    {
        ui.say(&hint);
    }

    let created = project.as_ref().is_some_and(|p| p.created);
    let prefill = match (new_user, created) {
        (true, true) => Some(
            "I'm new to AMP: show me around (amp_guide onboarding), then set up this project: \
             look through this folder and seed the knowledge base with \
             architecture/project-overview.md (amp-init)."
                .to_owned(),
        ),
        (false, true) => Some(
            "Set up this new project: look through this folder and seed the knowledge base with \
             architecture/project-overview.md (amp-init)."
                .to_owned(),
        ),
        (true, false) => Some("I'm new to AMP: show me around (amp_guide onboarding).".to_owned()),
        (false, false) => None,
    };
    Ok(Outcome { prefill, summary })
}

/// Picks the server, connects, and logs in when the server wants it. Returns whether the server
/// was chosen (asked or given) in this run.
async fn server_step(
    plan: &Plan,
    ui: &mut dyn Ui,
    config: &ConfigFile,
) -> Result<(AmpMcpClient, Settings, bool), CliError> {
    let current = plan.conn.settings()?;
    let init = plan.mode == Mode::Init;
    // Does this run choose the server (and save it)?
    let ask = ui.interactive()
        && plan.conn.url.is_none()
        && (current.url_source == Source::Default
            || (init && current.url_source != Source::Env(amp_mcp::ENV_MCP_URL.to_owned())));
    let save_flag = init && plan.conn.url.is_some();
    if ask && plan.mode == Mode::FirstRun {
        ui.say(
            "Welcome to amp-harness. A few questions and you're chatting with your AMP manager.",
        );
        ui.say("Which AMP server do you use? Press Enter for hosted AMP.");
    }
    let default_answer = if current.url_source == Source::Default {
        DEFAULT_MCP_URL.trim_end_matches("/mcp").to_owned()
    } else {
        current.url.clone()
    };

    let mut attempts = 0;
    loop {
        attempts += 1;
        let mut conn = plan.conn.clone();
        if ask {
            let answer = ui.ask("AMP server", Some(&default_answer))?;
            let url = normalize_server_url(&answer).map_err(CliError::usage);
            match url {
                Ok(url) => conn.url = Some(url),
                Err(e) => {
                    ui.say(&format!("  {}", e.message));
                    continue;
                }
            }
        }
        let settings = conn.settings()?;
        match connect_with_login(plan, ui, &settings).await {
            Ok(client) => {
                if ask || save_flag {
                    // Saved as a named server, as `server set` does (#2212).
                    let ca_text = plan
                        .conn
                        .ca_file
                        .as_ref()
                        .filter(|_| init)
                        .map(|p| std::path::absolute(p).unwrap_or_else(|_| p.clone()))
                        .map(|p| p.display().to_string());
                    let saved = crate::server_cmd::save_chosen(config, &settings.url, ca_text)
                        .map_err(CliError::new)?;
                    if let Some(entry) = saved {
                        ui.say(&format!(
                            "Saved the server {} as `{}` in {}.",
                            settings.url,
                            entry.name,
                            config.path.display()
                        ));
                    }
                }
                return Ok((client, settings, ask || save_flag));
            }
            Err(e) if ask && attempts < 4 && !e.is_auth_required() => {
                ui.say(&format!(
                    "  Can't reach AMP at {} ({}).",
                    host_of(&settings.url),
                    e.reason()
                ));
                ui.say("  Check the address (or that AMP is running) and try again.");
            }
            // The chat's first run with a server already chosen (config, AMP_MCP_URL, --url):
            // the chat opens offline and retries (#2178) instead of stopping here.
            Err(e)
                if plan.mode == Mode::FirstRun
                    && !ask
                    && !e.is_auth_required()
                    && e.kind() != amp_mcp::FailureKind::Config =>
            {
                return Err(CliError {
                    offline: true,
                    ..connect_error(&settings, e)
                });
            }
            Err(e) => return Err(connect_error(&settings, e)),
        }
    }
}

fn connect_error(settings: &Settings, e: McpError) -> CliError {
    if e.is_auth_required() {
        let how = match &settings.auth {
            Auth::Static { env, .. } => format!("the token in {env} was refused; set a new one"),
            Auth::None => "run `amp-harness login` (or set AMP_TOKEN), then try again".to_owned(),
        };
        return CliError::new(format!("{} needs a login: {how}", settings.url));
    }
    CliError::new(crate::conn::connect_message_verbose(
        &settings.url,
        Some(&settings.auth),
        &e,
    ))
}

async fn connect_with_login(
    plan: &Plan,
    ui: &mut dyn Ui,
    settings: &Settings,
) -> Result<AmpMcpClient, McpError> {
    match conn::connect(settings).await {
        Err(e) if e.is_auth_required() && matches!(settings.auth, Auth::None) => {
            let go = match plan.login {
                LoginPolicy::Never => false,
                LoginPolicy::Allow => true,
                LoginPolicy::Ask => {
                    ui.say(&format!(
                        "{} needs you to log in. Your login is kept in the OS keychain.",
                        host_of(&settings.url)
                    ));
                    ui.confirm("Log in with your browser now?", true)
                        .map_err(|e| McpError::Config(e.message))?
                }
            };
            if !go {
                return Err(e);
            }
            crate::browser_login(
                settings,
                plan.no_browser,
                amp_mcp::oauth::DEFAULT_LOGIN_TIMEOUT,
            )
            .await?;
            ui.say("Logged in.");
            conn::connect(settings).await
        }
        other => other,
    }
}

/// `https://host`, `host`, `host:port`, `…/mcp`: the MCP endpoint URL. Hosts without a scheme get
/// https, except localhost and 127.0.0.1 (http); a URL without a path gets `/mcp`.
pub fn normalize_server_url(input: &str) -> Result<String, String> {
    let raw = input.trim();
    if raw.is_empty() || raw.contains(char::is_whitespace) {
        return Err(format!("`{raw}` is not a server address"));
    }
    let with_scheme = if raw.contains("://") {
        raw.to_owned()
    } else {
        let host = raw.split(['/', ':']).next().unwrap_or("");
        let local = matches!(host, "localhost" | "127.0.0.1" | "[::1]");
        format!("{}://{raw}", if local { "http" } else { "https" })
    };
    let (scheme, rest) = with_scheme.split_once("://").unwrap_or(("", ""));
    if !matches!(scheme, "http" | "https") || rest.is_empty() {
        return Err(format!("`{raw}` is not an http(s) address"));
    }
    let (authority, path) = match rest.find('/') {
        Some(i) => (&rest[..i], rest[i..].trim_end_matches('/')),
        None => (rest, ""),
    };
    if authority.is_empty() {
        return Err(format!("`{raw}` has no host"));
    }
    let path = if path.is_empty() { "/mcp" } else { path };
    Ok(format!("{scheme}://{authority}{path}"))
}

/// `scheme://host[:port]` of a URL.
pub fn origin_of(url: &str) -> String {
    match url.split_once("://") {
        Some((scheme, rest)) => {
            let authority = rest.split(['/', '?', '#']).next().unwrap_or(rest);
            format!("{scheme}://{authority}")
        }
        None => url.to_owned(),
    }
}

pub(crate) fn host_of(url: &str) -> String {
    origin_of(url)
        .split_once("://")
        .map(|(_, h)| h.to_owned())
        .unwrap_or_else(|| url.to_owned())
}

/// Where the web UI is, as far as a probe can tell.
#[derive(Debug, PartialEq, Eq)]
pub(crate) enum WebGuess {
    /// The MCP URL's origin serves it: nothing to save.
    Same,
    Found(String),
    Unknown,
}

async fn looks_like_ui(url: &str, ca_file: Option<&Path>) -> bool {
    let Ok(http) = amp_mcp::build_http_client(ca_file) else {
        return false;
    };
    let Ok(Ok(resp)) = tokio::time::timeout(
        WEB_PROBE_TIMEOUT,
        http.get(url).timeout(WEB_PROBE_TIMEOUT).send(),
    )
    .await
    else {
        return false;
    };
    resp.status().is_success()
        && resp
            .headers()
            .get("content-type")
            .and_then(|v| v.to_str().ok())
            .is_some_and(|v| v.contains("text/html"))
}

pub(crate) async fn guess_web_url(mcp_url: &str, ca_file: Option<&Path>) -> WebGuess {
    let origin = origin_of(mcp_url);
    if looks_like_ui(&format!("{origin}/"), ca_file).await {
        return WebGuess::Same;
    }
    let host = host_of(mcp_url);
    let name = host.rsplit_once(':').map(|(h, _)| h).unwrap_or(&host);
    if matches!(name, "localhost" | "127.0.0.1") {
        let candidate = format!("http://{name}:{COMPOSE_UI_PORT}");
        if candidate != origin && looks_like_ui(&format!("{candidate}/"), None).await {
            return WebGuess::Found(candidate);
        }
    }
    WebGuess::Unknown
}

/// Saves `--web-url`, or finds the web UI when this run chose the server. Returns the web UI
/// address in effect (`None` = the MCP URL's origin).
async fn web_step(
    plan: &Plan,
    ui: &mut dyn Ui,
    config: &ConfigFile,
    settings: &Settings,
    server_chosen: bool,
) -> Result<Option<String>, CliError> {
    let save = |ui: &mut dyn Ui, url: &str| -> Result<(), CliError> {
        crate::server_cmd::save_web_url(config, url).map_err(CliError::new)?;
        ui.say(&format!("Web UI: {url} (saved as web_url)."));
        Ok(())
    };
    if let Some(raw) = &plan.web_url {
        let url = origin_of(raw.trim()).trim_end_matches('/').to_owned();
        if !url.starts_with("http://") && !url.starts_with("https://") {
            return Err(CliError::usage(format!(
                "--web-url {raw}: not an http(s) address"
            )));
        }
        save(ui, &url)?;
        return Ok(Some(url));
    }
    if !server_chosen || env_var(amp_mcp::ENV_WEB_URL).is_some() {
        return Ok(settings.web_url.clone());
    }
    // The server was (re)chosen: an old web_url may belong to another server.
    match guess_web_url(&settings.url, settings.ca_file.as_deref()).await {
        WebGuess::Same => {
            if config.get("server", "web_url").is_some() {
                let origin = origin_of(&settings.url);
                save(ui, &origin)?;
            }
            Ok(None)
        }
        WebGuess::Found(url) => {
            save(ui, &url)?;
            Ok(Some(url))
        }
        WebGuess::Unknown if plan.mode == Mode::Init && ui.interactive() => {
            let origin = origin_of(&settings.url);
            let current = config.get("server", "web_url").unwrap_or(origin.clone());
            let answer = ui.ask("Web UI address (for links)", Some(&current))?;
            let url = origin_of(answer.trim()).trim_end_matches('/').to_owned();
            if url != origin || config.get("server", "web_url").is_some() {
                save(ui, &url)?;
            }
            Ok(Some(url))
        }
        WebGuess::Unknown => Ok(settings.web_url.clone()),
    }
}

// ---------------------------------------------------------------------------------------------
// Project
// ---------------------------------------------------------------------------------------------

#[derive(Debug, Clone)]
struct ProjectInfo {
    id: i64,
    name: Option<String>,
    code: Option<String>,
    state: Option<String>,
}

impl ProjectInfo {
    fn from_value(v: &Value) -> Option<Self> {
        let p = if v.get("project").is_some_and(Value::is_object) {
            &v["project"]
        } else {
            v
        };
        Some(Self {
            id: p["id"].as_i64().filter(|id| *id > 0)?,
            name: p["name"].as_str().map(str::to_owned),
            code: p["code"].as_str().map(str::to_owned),
            state: p["state"].as_str().map(str::to_owned),
        })
    }

    fn label(&self) -> String {
        let mut s = format!(
            "#{} {}",
            self.id,
            self.name.as_deref().unwrap_or("(no name)")
        );
        if let Some(code) = &self.code {
            s.push_str(&format!(" ({code})"));
        }
        s
    }
}

fn list_of<'a>(v: &'a Value, key: &str) -> Vec<&'a Value> {
    match v.get(key).or(Some(v)) {
        Some(Value::Array(items)) => items.iter().collect(),
        _ => Vec::new(),
    }
}

/// The repository root of `cwd` (git), else `cwd`.
fn marker_dir(plan: &Plan) -> PathBuf {
    if let Some(dir) = &plan.dir {
        return dir.clone();
    }
    std::process::Command::new("git")
        .args(["rev-parse", "--show-toplevel"])
        .current_dir(&plan.cwd)
        .stderr(std::process::Stdio::null())
        .output()
        .ok()
        .filter(|o| o.status.success())
        .and_then(|o| String::from_utf8(o.stdout).ok())
        .map(|s| PathBuf::from(s.trim()))
        .filter(|p| !p.as_os_str().is_empty() && p.is_dir())
        // Never above AMP_HARNESS_CEILING_DIR, where the marker search would not find it.
        .filter(|root| {
            std::env::var_os(ENV_CEILING_DIR)
                .filter(|v| !v.is_empty())
                .map(PathBuf::from)
                .is_none_or(|ceiling| {
                    let canon =
                        |p: &Path| std::fs::canonicalize(p).unwrap_or_else(|_| p.to_path_buf());
                    canon(root).starts_with(canon(&ceiling))
                })
        })
        .unwrap_or_else(|| plan.cwd.clone())
}

/// Writes `.amp.json` in `dir`, keeping any other fields an existing file has. `amp_api` is the
/// real AMP origin (D4), so older tooling that reads it gets the right server.
fn write_marker(dir: &Path, p: &ProjectInfo, mcp_url: &str) -> Result<PathBuf, CliError> {
    let path = dir.join(MARKER_FILE);
    let mut obj = std::fs::read_to_string(&path)
        .ok()
        .and_then(|t| serde_json::from_str::<Value>(&t).ok())
        .and_then(|v| v.as_object().cloned())
        .unwrap_or_default();
    obj.insert("project_id".into(), json!(p.id));
    for (key, value) in [("project_name", &p.name), ("project_code", &p.code)] {
        match value {
            Some(v) => obj.insert(key.into(), json!(v)),
            None => obj.remove(key),
        };
    }
    obj.insert("amp_api".into(), json!(origin_of(mcp_url)));
    let text = serde_json::to_string_pretty(&Value::Object(obj))
        .map_err(|e| CliError::new(format!("encode .amp.json: {e}")))?;
    std::fs::write(&path, format!("{text}\n"))
        .map_err(|e| CliError::new(format!("write {}: {e}", path.display())))?;
    Ok(path)
}

/// A project code from a name: initials of several words, else the first letters (2-6).
pub fn derive_code(name: &str) -> String {
    // Letters only: AMP takes 2-6 letters.
    let words: Vec<String> = name
        .split(|c: char| !c.is_ascii_alphabetic())
        .filter(|w| !w.is_empty())
        .map(|w| w.to_ascii_uppercase())
        .collect();
    let mut code: String = if words.len() >= 2 {
        words
            .iter()
            .filter_map(|w| w.chars().next())
            .take(6)
            .collect()
    } else {
        words
            .first()
            .map(|w| w.chars().take(4).collect())
            .unwrap_or_default()
    };
    if code.len() < 2 {
        code = format!("{code}PRJ").chars().take(3).collect();
    }
    code
}

/// A code for `name` that is not in `taken` (upper case), for the default the first run offers
/// (#2229). Codes are unique across AMP, archived projects included, so [`derive_code`]'s
/// initials are often gone. Tries, in order: the initials; the initials plus the last word's next
/// letters (QT -> QTH); the name's letters (QATH); the initials plus one letter (QTA..QTZ). Falls
/// back to [`derive_code`] when everything is taken.
pub fn free_code(name: &str, taken: &BTreeSet<String>) -> String {
    let base = derive_code(name);
    let words: Vec<String> = name
        .split(|c: char| !c.is_ascii_alphabetic())
        .filter(|w| !w.is_empty())
        .map(|w| w.to_ascii_uppercase())
        .collect();
    let mut candidates = vec![base.clone()];
    if words.len() >= 2 && base.len() < 6 {
        let rest: String = words.last().map(|w| w[1..].to_owned()).unwrap_or_default();
        for n in 1..=rest.len().min(6 - base.len()) {
            candidates.push(format!("{base}{}", &rest[..n]));
        }
    }
    let letters: String = words.concat();
    for n in 2..=letters.len().min(6) {
        candidates.push(letters[..n].to_owned());
    }
    let stem: String = base.chars().take(5).collect();
    candidates.extend(('A'..='Z').map(|c| format!("{stem}{c}")));
    candidates
        .into_iter()
        .find(|c| (2..=6).contains(&c.len()) && !taken.contains(c))
        .unwrap_or(base)
}

/// AMP's own words in a tool error, without the transport's (`tools/call amp_x: Mcp error:
/// -32603: ` from a JSON-RPC error; a tool result error has none).
fn amp_message(message: &str) -> &str {
    let m = message.trim();
    let m = match m.strip_prefix("tools/call ") {
        Some(rest) => rest.split_once(": ").map_or(rest, |(_, r)| r),
        None => m,
    };
    match m.strip_prefix("Mcp error: ") {
        Some(rest) => rest.split_once(": ").map_or(rest, |(_, r)| r),
        None => m,
    }
}

/// Whether AMP refused a project because its code is taken (`project code "QT" already exists`).
fn code_taken(e: &CliError) -> bool {
    e.message.contains("code") && e.message.contains("already exists")
}

/// The codes of every project `amp_list_projects` returned, archived ones included, upper case.
fn listed_codes(projects: &[ProjectInfo]) -> BTreeSet<String> {
    projects
        .iter()
        .filter_map(|p| p.code.as_deref())
        .map(|c| c.trim().to_ascii_uppercase())
        .filter(|c| !c.is_empty())
        .collect()
}

/// Where a new `.amp.json` goes when it replaces `old`, a marker made for another server (#2206):
/// the usual place (`--dir`, else the repository root) when the new file shadows `old` there or
/// overwrites it, else the working directory. So a parent folder's marker for another server is
/// shadowed, not rewritten.
fn replacement_dir(plan: &Plan, old: &ProjectMarker) -> PathBuf {
    let usual = marker_dir(plan);
    if plan.dir.is_some() {
        return usual;
    }
    let canon = |p: &Path| std::fs::canonicalize(p).unwrap_or_else(|_| p.to_path_buf());
    let old_dir = old.path.parent().map(canon).unwrap_or_default();
    let usual_c = canon(&usual);
    // `old` sits below the usual place: a file there would not be found first.
    if old_dir != usual_c && old_dir.starts_with(&usual_c) {
        plan.cwd.clone()
    } else {
        usual
    }
}

/// Returns the linked project and whether the person had no projects before this run. Warnings
/// about the marker (#2206) go to `warnings`.
async fn project_step(
    plan: &Plan,
    ui: &mut dyn Ui,
    client: &AmpMcpClient,
    settings: &Settings,
    warnings: &mut Vec<String>,
) -> Result<(Option<ProjectSummary>, bool), CliError> {
    let ceiling = std::env::var_os(ENV_CEILING_DIR)
        .filter(|v| !v.is_empty())
        .map(PathBuf::from);
    let existing = ProjectMarker::find_within(&plan.cwd, ceiling.as_deref());
    let summary = |p: &ProjectInfo, marker: PathBuf, created: bool| ProjectSummary {
        id: p.id,
        name: p.name.clone(),
        code: p.code.clone(),
        marker,
        created,
        linked_now: true,
    };
    // A marker made for another server, which the picker below may replace (#2206).
    let mut replacing: Option<ProjectMarker> = None;
    let link_dir = |replacing: &Option<ProjectMarker>| match replacing {
        Some(old) => replacement_dir(plan, old),
        None => marker_dir(plan),
    };
    let link = |ui: &mut dyn Ui,
                p: &ProjectInfo,
                created: bool,
                dir: PathBuf|
     -> Result<ProjectSummary, CliError> {
        let path = write_marker(&dir, p, &settings.url)?;
        ui.say(&format!(
            "{} {} and linked this folder ({}).",
            if created { "Created" } else { "Using" },
            p.label(),
            path.display()
        ));
        Ok(summary(p, path, created))
    };

    match &plan.project {
        ProjectArg::Skip => return Ok((None, false)),
        ProjectArg::Id(id) => {
            let v = call_json(client, "amp_get_project", json!({"project_id": id})).await?;
            let p = ProjectInfo::from_value(&v).ok_or_else(|| {
                CliError::new(format!("amp_get_project {id}: no project in the answer"))
            })?;
            return Ok((Some(link(ui, &p, false, marker_dir(plan))?), false));
        }
        ProjectArg::Create {
            name,
            code,
            org,
            team,
        } => {
            // #2229: without --code, a code no listed project has; a taken one is one sentence.
            let code = match code {
                Some(c) => c.clone(),
                None => {
                    let listed = call_json(client, "amp_list_projects", json!({})).await.ok();
                    let all: Vec<ProjectInfo> = listed
                        .as_ref()
                        .map(|v| {
                            list_of(v, "projects")
                                .into_iter()
                                .filter_map(ProjectInfo::from_value)
                                .collect()
                        })
                        .unwrap_or_default();
                    free_code(name, &listed_codes(&all))
                }
            };
            let p = create_project(client, name, Some(code.clone()), *org, *team)
                .await
                .map_err(|e| {
                    if code_taken(&e) {
                        CliError::usage(format!(
                            "Code {} is taken; pick another with --code.",
                            code.trim().to_ascii_uppercase()
                        ))
                    } else {
                        e
                    }
                })?;
            return Ok((Some(link(ui, &p, true, marker_dir(plan))?), false));
        }
        ProjectArg::Auto => {}
    }

    match existing {
        Ok(Some(marker)) => {
            let label = format!(
                "#{} {}",
                marker.project_id,
                marker.project_name.as_deref().unwrap_or("")
            );
            // #2206: project ids are per server. A marker made for another server, or naming
            // another project than this server's, is said; the former is not used silently.
            let foreign = marker.other_server(&settings.url).is_some();
            let server_name = if foreign || marker.project_name.is_some() {
                call_json(
                    client,
                    "amp_get_project",
                    json!({"project_id": marker.project_id}),
                )
                .await
                .ok()
                .and_then(|v| ProjectInfo::from_value(&v))
                .and_then(|p| p.name)
            } else {
                None
            };
            if let Some(w) = marker.server_warning(&settings.url, server_name.as_deref()) {
                if foreign && ui.interactive() {
                    ui.say(&format!("Warning: {w}."));
                }
                warnings.push(w);
            }
            let keep = (foreign && !ui.interactive())
                || (!foreign && plan.mode == Mode::FirstRun)
                || (!foreign && !ui.interactive())
                || (!foreign
                    && ui.confirm(
                        &format!(
                            "This folder is linked to project {} ({}). Keep it?",
                            label.trim_end(),
                            marker.path.display()
                        ),
                        true,
                    )?);
            let kept = ProjectSummary {
                id: marker.project_id,
                name: marker.project_name.clone(),
                code: marker.project_code.clone(),
                marker: marker.path.clone(),
                created: false,
                linked_now: false,
            };
            if keep {
                return Ok((Some(kept), false));
            }
            if foreign {
                replacing = Some(marker);
            }
        }
        Ok(None) => {}
        // A broken marker: the session would fail on it too. Say so; init can replace it.
        Err(e) if plan.mode == Mode::Init && ui.interactive() => {
            ui.say(&format!(
                "The project marker can't be read ({e}); pick a project to replace it."
            ));
        }
        Err(e) => return Err(e.into()),
    }
    if !ui.interactive() {
        return Ok((None, false));
    }

    let listed = call_json(client, "amp_list_projects", json!({})).await?;
    let all: Vec<ProjectInfo> = list_of(&listed, "projects")
        .into_iter()
        .filter_map(ProjectInfo::from_value)
        .collect();
    // #2229: a new project's default code avoids every listed code, archived ones included.
    let taken = listed_codes(&all);
    let projects: Vec<ProjectInfo> = all
        .into_iter()
        .filter(|p| p.state.as_deref().is_none_or(|s| s == "active"))
        .collect();
    let default_name = marker_dir(plan)
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_else(|| "My project".to_owned());

    // Skipping keeps the other server's marker, which the session would find anyway: say so.
    let kept = replacing.as_ref().map(|m| ProjectSummary {
        id: m.project_id,
        name: m.project_name.clone(),
        code: m.project_code.clone(),
        marker: m.path.clone(),
        created: false,
        linked_now: false,
    });
    let skip =
        |ui: &mut dyn Ui, new_user: bool| -> Result<(Option<ProjectSummary>, bool), CliError> {
            match &kept {
            Some(k) => ui.say(&format!(
                "Keeping #{} from {} for now: run `amp-harness init` here to link this server's \
                 project.",
                k.id,
                k.marker.display()
            )),
            None => ui.say("No project for now: run `amp-harness init` later to link one."),
        }
            Ok((kept.clone(), new_user))
        };
    let dir = link_dir(&replacing);

    if projects.is_empty() {
        ui.say("You have no AMP projects yet. Projects hold the plan, the tickets and the knowledge base.");
        if !ui.confirm("Create a project for this folder now?", true)? {
            return skip(ui, true);
        }
        let p = create_interactive(ui, client, &default_name, taken.clone()).await?;
        return Ok((Some(link(ui, &p, true, dir.clone())?), true));
    }

    ui.say(if replacing.is_some() {
        "Pick this server's project for this folder, or create a new one:"
    } else {
        "This folder isn't linked to an AMP project yet. Pick one, or create a new one:"
    });
    for (i, p) in projects.iter().take(PICKER_MAX).enumerate() {
        ui.say(&format!("  {:>2}) {}", i + 1, p.label()));
    }
    if projects.len() > PICKER_MAX {
        ui.say(&format!(
            "      … {} more: type #<id>",
            projects.len() - PICKER_MAX
        ));
    }
    ui.say("   n) a new project");
    let question = match &replacing {
        Some(m) => format!(
            "Project (number, #id, n, or Enter to keep #{} from .amp.json)",
            m.project_id
        ),
        None => "Project (number, #id, n, or Enter to skip)".to_owned(),
    };
    loop {
        let answer = ui.ask(&question, Some(""))?;
        let answer = answer.trim();
        if answer.is_empty() {
            return skip(ui, false);
        }
        if answer.eq_ignore_ascii_case("n") || answer.eq_ignore_ascii_case("new") {
            let p = create_interactive(ui, client, &default_name, taken.clone()).await?;
            return Ok((Some(link(ui, &p, true, dir.clone())?), false));
        }
        let chosen = if let Some(id) = answer.strip_prefix('#') {
            id.trim().parse::<i64>().ok().and_then(|id| {
                projects
                    .iter()
                    .find(|p| p.id == id)
                    .cloned()
                    .or(Some(ProjectInfo {
                        id,
                        name: None,
                        code: None,
                        state: None,
                    }))
            })
        } else {
            answer
                .parse::<usize>()
                .ok()
                .filter(|n| (1..=projects.len().min(PICKER_MAX)).contains(n))
                .map(|n| projects[n - 1].clone())
        };
        let Some(mut p) = chosen else {
            ui.say("  Type a number from the list, #<id>, or n.");
            continue;
        };
        if p.name.is_none() {
            match call_json(client, "amp_get_project", json!({"project_id": p.id})).await {
                Ok(v) => match ProjectInfo::from_value(&v) {
                    Some(found) => p = found,
                    None => {
                        ui.say(&format!("  #{}: not found", p.id));
                        continue;
                    }
                },
                Err(e) => {
                    ui.say(&format!("  #{}: {e}", p.id));
                    continue;
                }
            }
        }
        return Ok((Some(link(ui, &p, false, dir.clone())?), false));
    }
}

async fn create_interactive(
    ui: &mut dyn Ui,
    client: &AmpMcpClient,
    default_name: &str,
    mut taken: BTreeSet<String>,
) -> Result<ProjectInfo, CliError> {
    let name = loop {
        let n = ui.ask("Project name", Some(default_name))?;
        if !n.trim().is_empty() {
            break n.trim().to_owned();
        }
    };
    let code_prompt = "Short code, the ticket prefix (2-6 letters)";
    let mut code = ui.ask(code_prompt, Some(&free_code(&name, &taken)))?;
    let orgs_v = call_json(client, "amp_list_orgs", json!({})).await.ok();
    let orgs: Vec<(i64, String)> = orgs_v
        .as_ref()
        .map(|v| {
            list_of(v, "orgs")
                .into_iter()
                .filter_map(|o| {
                    Some((
                        o["id"].as_i64()?,
                        o["name"].as_str().unwrap_or("").to_owned(),
                    ))
                })
                .collect()
        })
        .unwrap_or_default();
    let org = if orgs.len() > 1 {
        ui.say("Which org should own it?");
        for (i, (id, name)) in orgs.iter().enumerate() {
            ui.say(&format!("  {:>2}) {name} (#{id})", i + 1));
        }
        loop {
            let a = ui.ask("Org", Some("1"))?;
            if let Some(n) = a
                .parse::<usize>()
                .ok()
                .filter(|n| (1..=orgs.len()).contains(n))
            {
                break Some(orgs[n - 1].0);
            }
            ui.say("  Type a number from the list.");
        }
    } else {
        None
    };
    let mut team = None;
    loop {
        match create_project(client, &name, Some(code.clone()), org, team).await {
            Ok(p) => return Ok(p),
            // Codes are unique across AMP, archived projects included: ask for another one
            // instead of ending the first run.
            // #2229: one sentence, not the transport's error, and a free code as the default
            // (amp_list_projects shows only the projects the person can see).
            Err(e) if code_taken(&e) => {
                let upper = code.trim().to_ascii_uppercase();
                ui.say(&format!("  Code {upper} is taken; pick another."));
                taken.insert(upper);
                code = ui.ask(code_prompt, Some(&free_code(&name, &taken)))?;
            }
            Err(e) if team.is_none() && e.message.contains("team") => {
                ui.say(&format!("  {}", amp_message(&e.message)));
                let t = ui.ask("Team id that should own it (see the AMP web UI)", None)?;
                team = Some(
                    t.trim()
                        .trim_start_matches('#')
                        .parse()
                        .map_err(|_| CliError::usage(format!("`{t}` is not a team id")))?,
                );
            }
            Err(e) => return Err(e),
        }
    }
}

async fn create_project(
    client: &AmpMcpClient,
    name: &str,
    code: Option<String>,
    org: Option<i64>,
    team: Option<i64>,
) -> Result<ProjectInfo, CliError> {
    let code = code
        .map(|c| c.trim().to_ascii_uppercase())
        .filter(|c| !c.is_empty())
        .unwrap_or_else(|| derive_code(name));
    let mut args = json!({
        "name": name,
        "code": code,
        "description": "Created by amp-harness setup.",
    });
    if let Some(org) = org {
        args["org_id"] = json!(org);
    }
    if let Some(team) = team {
        args["team_id"] = json!(team);
    }
    let v = call_json(client, "amp_create_project", args).await?;
    ProjectInfo::from_value(&v)
        .ok_or_else(|| CliError::new("amp_create_project: no project id in the answer"))
}

// ---------------------------------------------------------------------------------------------
// Models and provider keys
// ---------------------------------------------------------------------------------------------

pub(crate) fn load_models(
    config: &ConfigFile,
    profile: Option<&str>,
) -> Result<ModelsConfig, CliError> {
    let env = |k: &str| {
        if k == ENV_PROFILE && profile.is_some() {
            return profile.map(str::to_owned);
        }
        std::env::var(k).ok()
    };
    // #2232: `config` over the repo layer of the current folder, as a session would load it.
    let mut paths = match std::env::current_dir() {
        Ok(cwd) => conn::config_paths_in(&cwd),
        Err(_) => conn::config_paths(),
    };
    if paths.user_file.as_deref() != Some(config.path.as_path()) {
        paths = paths.with_user_file(config.path.clone(), amp_mcp::Source::Flag);
    }
    ModelsConfig::load_paths(&env, &paths).map_err(|e| CliError::new(format!("models: {e}")))
}

pub(crate) fn profile_of(cfg: &ModelsConfig) -> (String, String) {
    match &cfg.selection {
        Selection::Profile { name, source } => (name.clone(), source.clone()),
        Selection::Inherit { source, .. } => ("inherit".to_owned(), source.clone()),
    }
}

/// The providers the config's roles use that need a key and have none.
pub(crate) fn missing_keys(cfg: &ModelsConfig, keys: &dyn KeyLookup) -> Vec<String> {
    cfg.providers_in_use()
        .into_iter()
        .filter(|id| {
            cfg.providers
                .get(id)
                .is_some_and(|p| resolve_key(p, keys).1.is_missing())
        })
        .collect()
}

pub(crate) fn provider_name(id: &str) -> String {
    match id {
        "openrouter" => "OpenRouter".to_owned(),
        "anthropic" => "Anthropic".to_owned(),
        "openai" => "OpenAI".to_owned(),
        other => other.to_owned(),
    }
}

/// The provider's display name: the config's `name`, else [`provider_name`].
pub(crate) fn display_name(p: &ProviderConfig) -> String {
    p.name.clone().unwrap_or_else(|| provider_name(&p.id))
}

/// `AMP_CREDENTIAL_STORE=file`: the person turned the OS keychain off.
fn credential_store_is_file() -> bool {
    std::env::var(amp_mcp::ENV_CREDENTIAL_STORE)
        .is_ok_and(|v| v.trim().eq_ignore_ascii_case("file"))
}

/// Why provider keys can't be kept here, for [`first_run_key_message`].
#[derive(Debug, Clone, Copy)]
pub(crate) struct NoKeychain<'a> {
    /// `AMP_CREDENTIAL_STORE=file` (said plainly) rather than a platform failure.
    pub turned_off: bool,
    /// The raw reason ([`keychain_for_keys`]'s error): shown only with `--verbose`.
    pub why: &'a str,
}

/// The one message the chat's first run ends with when no provider key resulted (#2214): what is
/// missing and the next command. With a keychain: `amp-harness connect <id>` (env var for
/// CI/headless). Without one: `export VAR=… && amp-harness`. The platform error is added on a
/// `cause:` line only with `--verbose`, like connection errors (#2178).
pub(crate) fn first_run_key_message(
    missing: &[&ProviderConfig],
    no_keychain: Option<NoKeychain<'_>>,
    verbose: bool,
) -> String {
    let names: Vec<String> = missing.iter().map(|p| display_name(p)).collect();
    let what = if names.is_empty() {
        "a model provider key".to_owned()
    } else {
        format!("a model provider key ({})", names.join(" + "))
    };
    let vars: Vec<&str> = missing
        .iter()
        .filter_map(|p| p.key_env.as_deref())
        .collect();
    let connects: Vec<String> = missing
        .iter()
        .map(|p| format!("`amp-harness connect {}`", p.id))
        .collect();
    match no_keychain {
        None => {
            let mut text = format!(
                "The chat needs {what}. Store it with {}, then run `amp-harness` again",
                connects.join(" and ")
            );
            if !vars.is_empty() {
                text.push_str(&format!(
                    " (CI/headless: set {} instead)",
                    vars.join(" and ")
                ));
            }
            text.push('.');
            text
        }
        Some(nk) => {
            let why = if nk.turned_off {
                format!(
                    "the OS keychain is turned off ({}=file)",
                    amp_mcp::ENV_CREDENTIAL_STORE
                )
            } else {
                "there is no usable OS keychain here".to_owned()
            };
            let mut text = format!("The chat needs {what}, and {why} to keep it in. ");
            if vars.is_empty() {
                text.push_str(&format!(
                    "Give the provider a `key_env` in config.toml and set that variable, then run \
                     `amp-harness` again ({} needs a keychain).",
                    connects.join(" and ")
                ));
            } else {
                let exports: Vec<String> = vars.iter().map(|v| format!("{v}=…")).collect();
                text.push_str(&format!(
                    "Set it in your environment and start again: `export {} && amp-harness`.",
                    exports.join(" ")
                ));
            }
            if verbose && !nk.turned_off {
                text.push_str(&format!("\n  cause: {}", nk.why));
            }
            text
        }
    }
}

/// Whether provider keys can be stored here (the OS keychain only).
pub(crate) fn keychain_for_keys() -> Result<String, String> {
    if credential_store_is_file() {
        return Err(format!(
            "the OS keychain is turned off ({}=file)",
            amp_mcp::ENV_CREDENTIAL_STORE
        ));
    }
    let service = amp_mcp::keychain_service();
    amp_mcp::keychain_available(&service).map_err(|e| format!("no OS keychain here ({e})"))?;
    Ok(service)
}

pub(crate) fn store_key(provider: &str, key: &str) -> Result<(), String> {
    let service = keychain_for_keys()?;
    // Created by this binary (macOS: a fresh item, so this build is its trusted creator), and
    // cached, so it is never read back in this run.
    amp_mcp::keychain_set(&service, &keychain_account(provider), key).map_err(|e| e.to_string())
}

#[derive(Debug, PartialEq, Eq)]
pub(crate) enum KeyCheck {
    Ok,
    Rejected(u16),
    Unknown(String),
}

/// Asks the provider whether the key works, without spending tokens: Anthropic's model list,
/// OpenRouter's key endpoint, an OpenAI-compatible model list. `extra` headers (a custom
/// provider's, secret ones included) go on the request; an empty `key` sends none.
pub(crate) async fn check_key(
    p: &ProviderConfig,
    key: &str,
    extra: &std::collections::BTreeMap<String, String>,
) -> KeyCheck {
    let Ok(http) = amp_mcp::build_http_client(None) else {
        return KeyCheck::Unknown("no HTTP client".to_owned());
    };
    let base = p.base_url.trim_end_matches('/');
    let mut req = match p.api {
        ProviderApi::AnthropicMessages => {
            let r = http
                .get(format!("{base}/v1/models?limit=1"))
                .header("anthropic-version", "2023-06-01");
            if key.is_empty() {
                r
            } else {
                r.header("x-api-key", key)
            }
        }
        ProviderApi::OpenaiCompletions => {
            let r = if p.id == "openrouter" || base.contains("openrouter.ai") {
                http.get(format!("{base}/key"))
            } else {
                http.get(format!("{base}/models"))
            };
            if key.is_empty() {
                r
            } else {
                r.bearer_auth(key)
            }
        }
        _ => return KeyCheck::Ok,
    };
    for (name, value) in p.headers.iter().chain(extra.iter()) {
        req = req.header(name.as_str(), value.as_str());
    }
    match tokio::time::timeout(KEY_CHECK_TIMEOUT, req.timeout(KEY_CHECK_TIMEOUT).send()).await {
        Ok(Ok(resp)) => {
            let status = resp.status().as_u16();
            match status {
                200..=299 => KeyCheck::Ok,
                401 | 403 => KeyCheck::Rejected(status),
                _ => KeyCheck::Unknown(format!("HTTP {status}")),
            }
        }
        Ok(Err(e)) => KeyCheck::Unknown(e.to_string()),
        Err(_) => KeyCheck::Unknown("timed out".to_owned()),
    }
}

/// Reads a key hidden, checks it, stores it in the keychain. `false` if the person skipped it.
async fn prompt_key(ui: &mut dyn Ui, p: &ProviderConfig) -> Result<bool, CliError> {
    let name = provider_name(&p.id);
    for _ in 0..3 {
        let key = ui.secret(&format!(
            "{}, Enter to skip",
            crate::connect::key_prompt(&p.id, &name)
        ))?;
        if key.is_empty() {
            return Ok(false);
        }
        if let Err(why) = crate::connect::refuse_subscription_token(&p.id, &key) {
            ui.say(&format!("  {why}"));
            continue;
        }
        match check_key(p, &key, &Default::default()).await {
            KeyCheck::Ok => ui.say(&format!("  {name} accepted the key.")),
            KeyCheck::Rejected(status) => {
                ui.say(&format!(
                    "  {name} rejected that key (HTTP {status}). Try again."
                ));
                continue;
            }
            KeyCheck::Unknown(why) => ui.say(&format!(
                "  Could not check the key with {name} ({why}); keeping it."
            )),
        }
        store_key(&p.id, &key).map_err(|e| CliError::new(format!("store the {name} key: {e}")))?;
        ui.say(&format!(
            "  Stored in the OS keychain (service {}, account {}).",
            amp_mcp::keychain_service(),
            keychain_account(&p.id)
        ));
        return Ok(true);
    }
    Ok(false)
}

async fn models_step(
    plan: &Plan,
    ui: &mut dyn Ui,
    config: &ConfigFile,
    summary: &mut Summary,
) -> Result<(), CliError> {
    // Keychain reads are cached process-wide by amp-mcp (#2165): each provider's entry is read at
    // most once per run, and a key stored by this run is never read back.
    let keys = SystemKeys::from_env();

    // --profile: check it exists, then save it.
    if let Some(profile) = &plan.profile {
        load_models(config, Some(profile))?;
        config
            .set(&[("models", "profile", profile.as_str())])
            .map_err(CliError::new)?;
    }

    // --key-env VAR: keychain only.
    if let Some(var) = &plan.key_env {
        let cfg = load_models(config, None)?;
        let provider = match &plan.provider {
            Some(id) => cfg
                .providers
                .get(id)
                .ok_or_else(|| CliError::usage(format!("--provider {id}: no such provider")))?,
            None => cfg
                .providers
                .values()
                .find(|p| p.key_env.as_deref() == Some(var.as_str()))
                .ok_or_else(|| {
                    CliError::usage(format!(
                        "--key-env {var}: no provider reads {var}; add --provider <id>"
                    ))
                })?,
        };
        let key = env_var(var)
            .ok_or_else(|| CliError::usage(format!("--key-env {var}: {var} is not set")))?;
        store_key(&provider.id, key.trim()).map_err(|e| {
            CliError::new(format!(
                "store the {} key: {e}. Keys are only kept in the OS keychain; keep {var} set in \
                 the environment instead.",
                provider_name(&provider.id)
            ))
        })?;
        ui.say(&format!(
            "Stored the {} key from {var} in the OS keychain.",
            provider_name(&provider.id)
        ));
    }

    let mut cfg = load_models(config, None)?;
    let (mut profile, source) = profile_of(&cfg);
    let mut missing = missing_keys(&cfg, &keys);
    let explicit = plan.profile.is_some() || source != "default";

    // A profile whose keys exist (only Anthropic's key: the anthropic profile).
    if !missing.is_empty() && !explicit {
        for name in cfg.profiles.keys().cloned().collect::<Vec<_>>() {
            if name == profile {
                continue;
            }
            let Ok(other) = load_models(config, Some(&name)) else {
                continue;
            };
            if missing_keys(&other, &keys).is_empty() {
                config
                    .set(&[("models", "profile", name.as_str())])
                    .map_err(CliError::new)?;
                let have: Vec<String> = other
                    .providers_in_use()
                    .iter()
                    .map(|p| provider_name(p))
                    .collect();
                ui.say(&format!(
                    "Found a {} key, so the roles use the `{name}` model profile (saved in config.toml).",
                    have.join(" + ")
                ));
                cfg = other;
                profile = name;
                missing.clear();
                break;
            }
        }
    }

    // No keychain: nothing to ask. The first run has already linked the project (step 4) and ends
    // below with one message naming the next command (#2214). Interactive init says why it asks
    // nothing (its summary warning has the commands). The platform error only with --verbose.
    let keychain = keychain_for_keys();
    if !missing.is_empty()
        && plan.mode == Mode::Init
        && ui.interactive()
        && let Err(why) = &keychain
    {
        let mut text = format!(
            "Keys are kept only in the OS keychain, and {}: set the key in your environment instead \
             (for example OPENROUTER_API_KEY or ANTHROPIC_API_KEY).",
            if credential_store_is_file() {
                why.as_str()
            } else {
                "there is no usable OS keychain here"
            }
        );
        if crate::conn::verbose() && !credential_store_is_file() {
            text.push_str(&format!("\n  cause: {why}"));
        }
        ui.say(&text);
    }
    if !missing.is_empty() && ui.interactive() && keychain.is_ok() {
        // Which provider does the person have a key for? One option per profile.
        let mut options: Vec<(String, Vec<String>, String)> = Vec::new();
        let mut names: Vec<String> = vec![profile.clone()];
        if !explicit {
            names.extend(
                std::iter::once(amp_core::models::DEFAULT_PROFILE.to_owned())
                    .chain(cfg.profiles.keys().cloned())
                    .filter(|n| *n != profile),
            );
        }
        for name in names {
            let Ok(c) = load_models(config, Some(&name)) else {
                continue;
            };
            let need: Vec<String> = c
                .providers_in_use()
                .into_iter()
                .filter(|id| c.providers.get(id).is_some_and(|p| p.needs_key))
                .collect();
            if need.is_empty() {
                continue;
            }
            let text = if name == amp_core::models::DEFAULT_PROFILE {
                DEFAULT_PROFILE_TEXT.to_owned()
            } else {
                cfg.profiles
                    .get(&name)
                    .cloned()
                    .flatten()
                    .unwrap_or_else(|| format!("profile {name}"))
            };
            options.push((name, need, text));
        }
        ui.say("The agents need a model provider key. It is stored in your OS keychain, never in a file.");
        let chosen = if options.len() > 1 {
            for (i, (_, need, text)) in options.iter().enumerate() {
                // "Anthropic API key (console.anthropic.com)": never a Claude subscription (#2163).
                let who: Vec<String> = need
                    .iter()
                    .map(|p| match p.as_str() {
                        "anthropic" => crate::connect::key_prompt(p, "Anthropic"),
                        other => provider_name(other),
                    })
                    .collect();
                ui.say(&format!("  {}) {} — {text}", i + 1, who.join(" + ")));
            }
            loop {
                let a = ui.ask("Which key do you have", Some("1"))?;
                if let Some(n) = a
                    .parse::<usize>()
                    .ok()
                    .filter(|n| (1..=options.len()).contains(n))
                {
                    break n - 1;
                }
                ui.say("  Type a number from the list.");
            }
        } else {
            0
        };
        if let Some((name, _, _)) = options.get(chosen) {
            let c = load_models(config, Some(name))?;
            for id in missing_keys(&c, &keys) {
                if let Some(p) = c.providers.get(&id)
                    && !prompt_key(ui, p).await?
                {
                    break;
                }
            }
            if *name != profile {
                config
                    .set(&[("models", "profile", name.as_str())])
                    .map_err(CliError::new)?;
                ui.say(&format!(
                    "Using the `{name}` model profile (saved in config.toml)."
                ));
            }
            cfg = load_models(config, None)?;
            profile = profile_of(&cfg).0;
            missing = missing_keys(&cfg, &keys);
        }
    }

    summary.profile = Some(profile);
    for id in cfg.providers_in_use() {
        if let Some(p) = cfg.providers.get(&id) {
            summary
                .providers
                .insert(id.clone(), resolve_key(p, &keys).1.to_string());
        }
    }
    if !missing.is_empty() {
        let how: Vec<String> = missing
            .iter()
            .filter_map(|id| cfg.providers.get(id))
            .map(|p| match &p.key_env {
                Some(var) => format!(
                    "{} (`amp-harness connect {}`, or {var} for CI/headless)",
                    provider_name(&p.id),
                    p.id
                ),
                None => provider_name(&p.id),
            })
            .collect();
        let message = format!("no model provider key: {}", how.join(", "));
        if plan.mode == Mode::FirstRun {
            let providers: Vec<&ProviderConfig> = missing
                .iter()
                .filter_map(|id| cfg.providers.get(id))
                .collect();
            let no_keychain = keychain.as_ref().err().map(|why| NoKeychain {
                turned_off: credential_store_is_file(),
                why: why.as_str(),
            });
            return Err(CliError::new(first_run_key_message(
                &providers,
                no_keychain,
                crate::conn::verbose(),
            )));
        }
        summary.warnings.push(message);
    }
    Ok(())
}

// ---------------------------------------------------------------------------------------------
// PATH
// ---------------------------------------------------------------------------------------------

/// A hint when the running binary's directory is not on PATH.
fn path_hint() -> Option<String> {
    let exe = std::env::current_exe().ok()?;
    let dir = exe.parent()?.to_path_buf();
    let canon = |p: &Path| std::fs::canonicalize(p).unwrap_or_else(|_| p.to_path_buf());
    let dir_c = canon(&dir);
    let on_path = std::env::var_os("PATH")
        .map(|p| std::env::split_paths(&p).any(|e| canon(&e) == dir_c))
        .unwrap_or(false);
    (!on_path).then(|| {
        format!(
            "Tip: {} is not on your PATH, so `amp-harness` only runs by its full path. Add it:\n  \
             echo 'export PATH=\"{}:$PATH\"' >> ~/.zprofile   # zsh (bash: ~/.bash_profile)",
            dir_c.display(),
            dir_c.display()
        )
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// #2214: one message with the next command; the platform error only with --verbose.
    #[test]
    fn the_first_run_key_message_names_the_next_command() {
        let cfg = ModelsConfig::load_with_env(&|_| None, None).unwrap();
        let or = cfg.providers.get("openrouter").unwrap();
        let raw = "keychain read failed: Platform failure: A default keychain could not be found.";

        let with = first_run_key_message(&[or], None, false);
        assert!(with.contains("`amp-harness connect openrouter`"), "{with}");
        assert!(with.contains("OPENROUTER_API_KEY"), "{with}");

        let unusable = NoKeychain {
            turned_off: false,
            why: raw,
        };
        let without = first_run_key_message(&[or], Some(unusable), false);
        assert!(
            without.contains("`export OPENROUTER_API_KEY=… && amp-harness`"),
            "{without}"
        );
        assert!(without.contains("no usable OS keychain"), "{without}");
        assert!(!without.contains("Platform failure"), "{without}");
        assert!(!without.contains('\n'), "{without}");

        let verbose = first_run_key_message(&[or], Some(unusable), true);
        assert!(verbose.ends_with(&format!("\n  cause: {raw}")), "{verbose}");

        let off = NoKeychain {
            turned_off: true,
            why: "the OS keychain is turned off (AMP_CREDENTIAL_STORE=file)",
        };
        let off = first_run_key_message(&[or], Some(off), true);
        assert!(off.contains("AMP_CREDENTIAL_STORE=file"), "{off}");
        assert!(!off.contains("cause:"), "{off}");
    }

    #[test]
    fn server_urls() {
        let n = |s| normalize_server_url(s);
        assert_eq!(n("amp.example.com").unwrap(), "https://amp.example.com/mcp");
        assert_eq!(
            n("https://amp.example.com/").unwrap(),
            "https://amp.example.com/mcp"
        );
        assert_eq!(n("https://amp.local/mcp").unwrap(), "https://amp.local/mcp");
        assert_eq!(n("localhost:18000").unwrap(), "http://localhost:18000/mcp");
        assert_eq!(
            n("127.0.0.1:18000/mcp/").unwrap(),
            "http://127.0.0.1:18000/mcp"
        );
        assert_eq!(n("http://h:1/x/mcp").unwrap(), "http://h:1/x/mcp");
        assert!(n("").is_err());
        assert!(n("ftp://x").is_err());
        assert!(n("a b").is_err());
        assert!(n("https://").is_err());
    }

    #[test]
    fn origins() {
        assert_eq!(
            origin_of("http://localhost:18000/mcp"),
            "http://localhost:18000"
        );
        assert_eq!(
            origin_of("https://amp.simstech.cloud/mcp?x"),
            "https://amp.simstech.cloud"
        );
        assert_eq!(host_of("http://localhost:18000/mcp"), "localhost:18000");
    }

    #[test]
    fn codes() {
        assert_eq!(derive_code("Harness play"), "HP");
        assert_eq!(derive_code("my-cool-app"), "MCA");
        assert_eq!(derive_code("amp"), "AMP");
        assert_eq!(derive_code("northwind"), "NORT");
        assert_eq!(derive_code("x"), "XPR");
        assert_eq!(derive_code("a b c d e f g h"), "ABCDEF");
        assert_eq!(derive_code("Setup test 2113"), "ST");
    }

    #[test]
    fn free_codes() {
        let taken = |c: &[&str]| c.iter().map(|s| s.to_string()).collect::<BTreeSet<_>>();
        assert_eq!(free_code("QA 2216 throwaway", &taken(&[])), "QT");
        assert_eq!(free_code("QA 2216 throwaway", &taken(&["QT"])), "QTH");
        assert_eq!(
            free_code("QA 2216 throwaway", &taken(&["QT", "QTH"])),
            "QTHR"
        );
        assert_eq!(free_code("amp", &taken(&["AMP"])), "AM");
        assert_eq!(free_code("ab", &taken(&["AB"])), "ABA");
        let all: Vec<String> = ('A'..='Z')
            .map(|c| format!("AB{c}"))
            .chain(["AB".into()])
            .collect();
        let all: BTreeSet<String> = all.into_iter().collect();
        assert_eq!(
            free_code("ab", &all),
            "AB",
            "everything taken: the initials"
        );
    }

    #[test]
    fn amp_messages() {
        assert_eq!(
            amp_message(
                "tools/call amp_create_project: Mcp error: -32603: project code \"QT\" already exists"
            ),
            "project code \"QT\" already exists"
        );
        assert_eq!(amp_message("team_id is required"), "team_id is required");
    }

    #[test]
    fn project_info_shapes() {
        let a =
            ProjectInfo::from_value(&json!({"project": {"id": 2, "name": "AMP v2", "code": "AV"}}))
                .unwrap();
        assert_eq!((a.id, a.name.as_deref()), (2, Some("AMP v2")));
        let b =
            ProjectInfo::from_value(&json!({"id": 45, "name": "Harness play", "state": "active"}))
                .unwrap();
        assert_eq!(b.label(), "#45 Harness play");
        assert!(ProjectInfo::from_value(&json!({"id": 0})).is_none());
    }

    #[test]
    fn marker_keeps_other_fields_and_sets_the_origin() {
        let dir =
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../../target/test-scratch/setup-marker");
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(
            dir.join(MARKER_FILE),
            r#"{"project_id": 1, "amp_api": "https://stale", "extra": true}"#,
        )
        .unwrap();
        let p = ProjectInfo {
            id: 45,
            name: Some("Harness play".into()),
            code: Some("HP".into()),
            state: None,
        };
        let path = write_marker(&dir, &p, "http://localhost:18000/mcp").unwrap();
        let v: Value = serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
        assert_eq!(v["project_id"], 45);
        assert_eq!(v["project_code"], "HP");
        assert_eq!(v["amp_api"], "http://localhost:18000");
        assert_eq!(v["extra"], true);
        let m = ProjectMarker::read(&path).unwrap();
        assert_eq!(m.project_name.as_deref(), Some("Harness play"));
        let _ = std::fs::remove_dir_all(&dir);
    }
}
