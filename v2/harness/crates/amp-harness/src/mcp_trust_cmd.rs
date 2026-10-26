//! Trust for repo-level MCP servers (#2241, H8): `amp-harness mcp trust [<name>…|--all]`,
//! `mcp untrust <name>…|--all`, and the chat's question before its MCP hub starts.
//!
//! The rules live in [`amp_core::mcp_trust`]: a server from `<repo>/.amp/harness/config.toml` runs
//! only once trusted at its current config (`<state>/trust.toml`, outside the repo); headless runs
//! never ask and skip untrusted servers unless `--trust-repo` / `AMP_HARNESS_TRUST_REPO=1`.

use std::path::{Path, PathBuf};

use amp_core::{
    agent::state_dir,
    mcp_config::{McpConfig, McpServer},
    mcp_hub::McpHub,
    mcp_trust::{self, TrustState, TrustStore},
};
use clap::Args;
use serde::Serialize;

use crate::{
    conn::{self, CliError, print_json},
    setup::ui::{self, Ui},
};

#[derive(Args)]
pub struct TrustArgs {
    /// Repo MCP servers to trust, as configured now. With no names (and no --all): list this
    /// repo's servers and whether each is trusted.
    pub names: Vec<String>,
    /// Trust every MCP server in this repo's .amp/harness/config.toml.
    #[arg(long, conflicts_with = "names")]
    pub all: bool,
    /// The repo (default: the current directory's).
    #[arg(long)]
    pub cwd: Option<PathBuf>,
    /// Print JSON (the list, or what was trusted).
    #[arg(long)]
    pub json: bool,
}

#[derive(Args)]
pub struct UntrustArgs {
    /// Repo MCP servers to forget; each is asked about again (the chat) or skipped (headless).
    #[arg(required_unless_present = "all")]
    pub names: Vec<String>,
    /// Forget every trusted server of this repo.
    #[arg(long, conflicts_with = "names")]
    pub all: bool,
    /// The repo (default: the current directory's).
    #[arg(long)]
    pub cwd: Option<PathBuf>,
}

/// One server in `mcp trust`'s list.
#[derive(Serialize)]
struct Row {
    name: String,
    /// user or repo.
    layer: &'static str,
    enabled: bool,
    trust: TrustState,
    what: String,
    file: PathBuf,
}

struct Ctx {
    config: McpConfig,
    store_path: PathBuf,
    repo: Option<PathBuf>,
}

fn context(cwd: Option<&Path>) -> Result<Ctx, CliError> {
    let cwd = match cwd {
        Some(d) => d.to_path_buf(),
        None => {
            std::env::current_dir().map_err(|e| CliError::new(format!("current directory: {e}")))?
        }
    };
    let paths = conn::config_paths_in(&cwd);
    let repo = paths
        .repo_dir
        .as_deref()
        .and_then(|d| d.ancestors().nth(2))
        .map(|r| std::fs::canonicalize(r).unwrap_or_else(|_| r.to_path_buf()));
    let env = |k: &str| std::env::var(k).ok();
    let state = state_dir(&env)
        .ok_or_else(|| CliError::new("no state directory: set HOME or XDG_STATE_HOME"))?;
    Ok(Ctx {
        config: McpConfig::load(&paths),
        store_path: TrustStore::path_in(&state),
        repo,
    })
}

fn load_store(path: &Path) -> Result<TrustStore, CliError> {
    TrustStore::load(path).map_err(CliError::new)
}

/// The repo server `name`, or an error saying why it can't be trusted.
fn repo_server<'a>(ctx: &'a Ctx, name: &str) -> Result<&'a McpServer, CliError> {
    match ctx.config.servers.get(name) {
        Some(s) if s.is_repo() => Ok(s),
        Some(s) => Err(CliError::usage(format!(
            "`{name}` is a user-level MCP server ({}); those are always trusted",
            s.origin.file.display()
        ))),
        None if ctx.config.broken.contains(name) => Err(CliError::usage(format!(
            "MCP server `{name}` has configuration errors; fix them first (amp-harness config \
             check)"
        ))),
        None => Err(CliError::usage(format!(
            "no MCP server `{name}` in this repo's .amp/harness/config.toml{}",
            if ctx.repo.is_none() {
                " (no repo here: no .amp.json above this directory)"
            } else {
                ""
            }
        ))),
    }
}

pub fn trust(args: TrustArgs) -> Result<(), CliError> {
    let ctx = context(args.cwd.as_deref())?;
    if args.names.is_empty() && !args.all {
        return list(&ctx, args.json);
    }
    // #2331: `mcp trust <name>` for a repo server ignored in favour of the person's own says why.
    crate::mcp_crud::print_shadowed(&ctx.config, false);
    let servers: Vec<&McpServer> = if args.all {
        let all: Vec<&McpServer> = ctx
            .config
            .servers
            .values()
            .filter(|s| s.is_repo())
            .collect();
        if all.is_empty() {
            return Err(CliError::usage(
                "this repo's .amp/harness/config.toml defines no MCP servers",
            ));
        }
        all
    } else {
        args.names
            .iter()
            .map(|n| repo_server(&ctx, n))
            .collect::<Result<_, _>>()?
    };
    let mut store = load_store(&ctx.store_path)?;
    for s in &servers {
        store.trust(s);
    }
    store.save().map_err(CliError::new)?;
    if args.json {
        let names: Vec<&str> = servers.iter().map(|s| s.name.as_str()).collect();
        return print_json(&serde_json::json!({
            "trusted": names,
            "store": ctx.store_path,
        }));
    }
    for s in &servers {
        println!("trusted {}: {}", s.name, mcp_trust::summary(s));
        // #2310: everything it runs, sends and reads, as trusted.
        for line in mcp_trust::details(s) {
            println!("    {line}");
        }
    }
    eprintln!(
        "Recorded in {}. A change to a server's config asks again.",
        ctx.store_path.display()
    );
    Ok(())
}

fn list(ctx: &Ctx, json: bool) -> Result<(), CliError> {
    let store = match TrustStore::load(&ctx.store_path) {
        Ok(s) => Some(s),
        Err(e) => {
            eprintln!("amp-harness: {e}");
            None
        }
    };
    crate::mcp_crud::print_shadowed(&ctx.config, !json);
    let trust_all = mcp_trust::trust_all_from(&|k| std::env::var(k).ok());
    let rows: Vec<Row> = ctx
        .config
        .servers
        .values()
        .map(|s| Row {
            name: s.name.clone(),
            layer: if s.is_repo() { "repo" } else { "user" },
            enabled: s.enabled,
            trust: mcp_trust::state_of(s, store.as_ref(), trust_all),
            what: mcp_trust::summary(s),
            file: s.origin.file.clone(),
        })
        .collect();
    if json {
        return print_json(&rows);
    }
    if rows.is_empty() {
        println!("no MCP servers configured");
        return Ok(());
    }
    let width = rows.iter().map(|r| r.name.len()).max().unwrap_or(0);
    for r in &rows {
        let off = if r.enabled { "" } else { " (disabled)" };
        println!(
            "{:width$}  {:4}  {:26}  {}{off}",
            r.name,
            r.layer,
            r.trust.word(),
            r.what
        );
    }
    Ok(())
}

pub fn untrust(args: UntrustArgs) -> Result<(), CliError> {
    let ctx = context(args.cwd.as_deref())?;
    let repo = ctx.repo.clone().ok_or_else(|| {
        CliError::usage("no repo here (no .amp.json above this directory); use --cwd <repo>")
    })?;
    let mut store = load_store(&ctx.store_path)?;
    let removed = if args.all {
        store.untrust_repo(&repo)
    } else {
        let mut removed = Vec::new();
        for name in &args.names {
            if store.untrust(&repo, name) {
                removed.push(name.clone());
            } else {
                eprintln!(
                    "amp-harness: `{name}` was not trusted in {}",
                    repo.display()
                );
            }
        }
        removed
    };
    store.save().map_err(CliError::new)?;
    for name in &removed {
        println!("untrusted {name}");
    }
    Ok(())
}

/// The chat's question (D7), before the hub starts: for each untrusted repo server, "trust it?"
/// `y` trusts it, `a` trusts it and every one after it, anything else skips it this time. Trusted
/// ones are recorded in the store and allowed to start. Nothing is asked without a person
/// (no terminal and no `AMP_HARNESS_PROMPTS=stdin`): untrusted servers just stay off.
pub fn ask(hub: &McpHub, state: &Path) -> Result<(), CliError> {
    let pending = hub.untrusted();
    if pending.is_empty() {
        return Ok(());
    }
    let mut ui = ui::detect(true);
    if !ui.interactive() {
        return Ok(());
    }
    ask_with(ui.as_mut(), hub, &pending, &TrustStore::path_in(state))
}

fn ask_with(
    ui: &mut dyn Ui,
    hub: &McpHub,
    pending: &[mcp_trust::Untrusted],
    store_path: &Path,
) -> Result<(), CliError> {
    let file = pending[0].file.display().to_string();
    ui.say(&format!(
        "{file} defines MCP servers you have not trusted. A trusted server can run commands on \
         this machine (stdio) or receive what the agents send it (http). Here is everything each \
         one runs, sends and reads:"
    ));
    let mut all = false;
    // #2310 (review #2248 finding 15): each question shows the server as it was when `pending`
    // was worked out, and exactly that server is recorded and let start. A reload that lands in
    // between can't get a config the person never saw trusted.
    let mut trusted: Vec<&McpServer> = Vec::new();
    for u in pending {
        let server = &u.server;
        let changed = if u.changed {
            " (its config changed since you trusted it)"
        } else {
            ""
        };
        ui.say(&format!("\n  `{}`{changed}:", u.name));
        for line in mcp_trust::details(server) {
            ui.say(&format!("    {line}"));
        }
        let yes = all || {
            let answer = ui.ask(
                &format!("  Trust `{}`? [y: yes, a: all, N: not now]", u.name),
                Some(""),
            )?;
            match answer.trim().to_ascii_lowercase().as_str() {
                "y" | "yes" => true,
                "a" | "all" => {
                    all = true;
                    true
                }
                _ => false,
            }
        };
        if yes {
            trusted.push(server);
        } else {
            ui.say(&format!(
                "  Skipped `{}` for this chat (`amp-harness mcp trust {}` trusts it).",
                u.name, u.name
            ));
        }
    }
    if trusted.is_empty() {
        return Ok(());
    }
    // Recorded first: a server starts only if its trust could be kept, or the person is told.
    match TrustStore::load(store_path).and_then(|mut store| {
        for s in &trusted {
            store.trust(s);
        }
        store.save()
    }) {
        Ok(()) => ui.say(&format!(
            "  Trusted {} (recorded in {}).",
            names(&trusted),
            store_path.display()
        )),
        Err(e) => ui.say(&format!(
            "  Trusted {} for this chat only: could not record it: {e}",
            names(&trusted)
        )),
    }
    for s in trusted {
        if !hub.trust_as_shown(s).map_err(CliError::new)? {
            ui.say(&format!(
                "  `{}` changed while you were asked (a reload); it stays off until you are asked \
                 about the new config.",
                s.name
            ));
        }
    }
    Ok(())
}

fn names(servers: &[&McpServer]) -> String {
    servers
        .iter()
        .map(|s| format!("`{}`", s.name))
        .collect::<Vec<_>>()
        .join(", ")
}
