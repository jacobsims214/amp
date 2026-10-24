//! Trust for everything a repo's `.amp/harness/` brings (#2310): `amp-harness trust
//! [<kind>:<name>…|--all]`, `amp-harness untrust …`, and the chat's question about repo agents,
//! skills and settings before it loads them.
//!
//! The rules live in [`amp_core::repo_trust`] (agents, skills, `[roles.*]`, `[models] profile`)
//! and [`amp_core::mcp_trust`] (MCP servers, which `amp-harness mcp trust` also handles). Items
//! are named `agent:<id>`, `skill:<name>`, `role:<id>`, `setting:profile` and `mcp:<name>`.

use std::path::{Path, PathBuf};

use amp_core::{
    agent::state_dir,
    mcp_config::{McpConfig, McpServer},
    mcp_trust::{self, TrustState, TrustStore},
    repo_trust::{self, Gate, ItemKind, RepoItem, UntrustedItem},
};
use amp_mcp::ConfigPaths;
use clap::Args;
use serde::Serialize;

use crate::{
    conn::{self, CliError, print_json},
    setup::ui::{self, Ui},
};

#[derive(Args)]
pub struct TrustArgs {
    /// What to trust, as it is now: `agent:<id>`, `skill:<name>`, `role:<id>`, `setting:profile`
    /// or `mcp:<name>`. With none (and no --all): list everything this repo brings and whether
    /// each is trusted.
    pub items: Vec<String>,
    /// Trust everything this repo's .amp/harness/ brings (agents, skills, settings, MCP servers).
    #[arg(long, conflicts_with = "items")]
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
    /// What to forget (`agent:<id>`, `skill:<name>`, `role:<id>`, `setting:profile`,
    /// `mcp:<name>`); each is asked about again (the chat) or left out (headless).
    #[arg(required_unless_present = "all")]
    pub items: Vec<String>,
    /// Forget everything trusted for this repo.
    #[arg(long, conflicts_with = "items")]
    pub all: bool,
    /// The repo (default: the current directory's).
    #[arg(long)]
    pub cwd: Option<PathBuf>,
}

/// One row of `trust`'s list.
#[derive(Serialize)]
struct Row {
    /// `agent:<id>`, `mcp:<name>`, ...
    item: String,
    trust: TrustState,
    what: String,
    file: PathBuf,
}

struct Ctx {
    paths: ConfigPaths,
    repo: PathBuf,
    state: PathBuf,
}

fn context(cwd: Option<&Path>) -> Result<Ctx, CliError> {
    let cwd = match cwd {
        Some(d) => d.to_path_buf(),
        None => {
            std::env::current_dir().map_err(|e| CliError::new(format!("current directory: {e}")))?
        }
    };
    let paths = conn::config_paths_in(&cwd);
    let repo = repo_trust::repo_root(&paths).ok_or_else(|| {
        CliError::usage("no repo here (no .amp.json above this directory); use --cwd <repo>")
    })?;
    let env = |k: &str| std::env::var(k).ok();
    let state = state_dir(&env)
        .ok_or_else(|| CliError::new("no state directory: set HOME or XDG_STATE_HOME"))?;
    Ok(Ctx { paths, repo, state })
}

fn repo_servers(paths: &ConfigPaths) -> Vec<McpServer> {
    McpConfig::load(paths)
        .servers
        .into_values()
        .filter(McpServer::is_repo)
        .collect()
}

/// `agent:x` -> (Some(Agent), "x"); `mcp:x` -> (None, "x").
fn parse_item(word: &str) -> Result<(Option<ItemKind>, String), CliError> {
    let bad = || {
        CliError::usage(format!(
            "`{word}`: name it as agent:<id>, skill:<name>, role:<id>, setting:profile or \
             mcp:<name> (`amp-harness trust` lists them)"
        ))
    };
    let (kind, name) = word.split_once(':').ok_or_else(bad)?;
    let name = name.trim();
    if name.is_empty() {
        return Err(bad());
    }
    if kind.trim().eq_ignore_ascii_case("mcp") {
        return Ok((None, name.to_owned()));
    }
    Ok((
        Some(ItemKind::parse(kind).ok_or_else(bad)?),
        name.to_owned(),
    ))
}

pub fn trust(args: TrustArgs) -> Result<(), CliError> {
    let ctx = context(args.cwd.as_deref())?;
    if args.items.is_empty() && !args.all {
        return list(&ctx, args.json);
    }
    let items = repo_trust::scan(&ctx.paths);
    let servers = repo_servers(&ctx.paths);
    let (chosen_items, chosen_servers): (Vec<&RepoItem>, Vec<&McpServer>) = if args.all {
        if items.is_empty() && servers.is_empty() {
            return Err(CliError::usage(
                "this repo's .amp/harness/ brings no agents, skills, settings or MCP servers",
            ));
        }
        (items.iter().collect(), servers.iter().collect())
    } else {
        let mut its = Vec::new();
        let mut svs = Vec::new();
        for word in &args.items {
            match parse_item(word)? {
                (Some(kind), name) => its.push(
                    items
                        .iter()
                        .find(|i| i.kind == kind && i.name == name)
                        .ok_or_else(|| {
                            CliError::usage(format!(
                                "this repo's .amp/harness/ has no {} `{name}`",
                                kind.word()
                            ))
                        })?,
                ),
                (None, name) => {
                    svs.push(servers.iter().find(|s| s.name == name).ok_or_else(|| {
                        CliError::usage(format!(
                            "no MCP server `{name}` from this repo's .amp/harness/config.toml"
                        ))
                    })?)
                }
            }
        }
        (its, svs)
    };
    let path = TrustStore::path_in(&ctx.state);
    let mut store = TrustStore::load(&path).map_err(CliError::new)?;
    for i in &chosen_items {
        store.trust_item(&ctx.repo, i.kind, &i.name, &i.sha256, &i.summary);
    }
    for s in &chosen_servers {
        store.trust(s);
    }
    store.save().map_err(CliError::new)?;
    if args.json {
        let names: Vec<String> = chosen_items
            .iter()
            .map(|i| i.id())
            .chain(chosen_servers.iter().map(|s| format!("mcp:{}", s.name)))
            .collect();
        return print_json(&serde_json::json!({ "trusted": names, "store": path }));
    }
    for i in &chosen_items {
        println!("trusted {}:", i.id());
        for line in &i.details {
            println!("    {line}");
        }
    }
    for s in &chosen_servers {
        println!("trusted mcp:{}:", s.name);
        for line in mcp_trust::details(s) {
            println!("    {line}");
        }
    }
    eprintln!(
        "Recorded in {}. Any change asks again. A running chat takes it with /reload.",
        path.display()
    );
    Ok(())
}

fn list(ctx: &Ctx, json: bool) -> Result<(), CliError> {
    let env = |k: &str| std::env::var(k).ok();
    let gate = Gate::load(
        &ctx.paths,
        Some(&ctx.state),
        mcp_trust::trust_all_from(&env),
    );
    if let Some(e) = gate.store_error() {
        eprintln!("amp-harness: {e}");
    }
    let store = TrustStore::load(&TrustStore::path_in(&ctx.state)).ok();
    let trust_all = mcp_trust::trust_all_from(&env);
    let mut rows: Vec<Row> = repo_trust::scan(&ctx.paths)
        .into_iter()
        .map(|i| Row {
            item: i.id(),
            trust: gate.state(i.kind, &i.name, &i.sha256),
            what: i.summary,
            file: i.file,
        })
        .collect();
    rows.extend(repo_servers(&ctx.paths).into_iter().map(|s| Row {
        item: format!("mcp:{}", s.name),
        trust: mcp_trust::state_of(&s, store.as_ref(), trust_all),
        what: mcp_trust::summary(&s),
        file: s.origin.file.clone(),
    }));
    if json {
        return print_json(&rows);
    }
    if rows.is_empty() {
        println!(
            "this repo's .amp/harness/ brings no agents, skills, settings or MCP servers ({})",
            ctx.repo.display()
        );
        return Ok(());
    }
    let width = rows.iter().map(|r| r.item.len()).max().unwrap_or(0);
    for r in &rows {
        println!("{:width$}  {:22}  {}", r.item, r.trust.word(), r.what);
    }
    Ok(())
}

pub fn untrust(args: UntrustArgs) -> Result<(), CliError> {
    let ctx = context(args.cwd.as_deref())?;
    let path = TrustStore::path_in(&ctx.state);
    let mut store = TrustStore::load(&path).map_err(CliError::new)?;
    let mut removed = Vec::new();
    if args.all {
        for (kind, name, _) in store.items(&ctx.repo) {
            store.untrust_item(&ctx.repo, kind, &name);
            removed.push(format!("{}:{name}", kind.word()));
        }
        removed.extend(
            store
                .untrust_repo(&ctx.repo)
                .into_iter()
                .map(|n| format!("mcp:{n}")),
        );
    } else {
        for word in &args.items {
            let ok = match parse_item(word)? {
                (Some(kind), name) => store.untrust_item(&ctx.repo, kind, &name),
                (None, name) => store.untrust(&ctx.repo, &name),
            };
            if ok {
                removed.push(word.clone());
            } else {
                eprintln!(
                    "amp-harness: `{word}` was not trusted in {}",
                    ctx.repo.display()
                );
            }
        }
    }
    store.save().map_err(CliError::new)?;
    for r in &removed {
        println!("untrusted {r}");
    }
    Ok(())
}

/// The chat's question (#2310), before the runtime loads agents, skills and role settings: for
/// each repo item not trusted as it is now, everything it sets, then "trust it?". `y` trusts it,
/// `a` trusts it and every one after it, anything else leaves it out for this chat (the built-in
/// or your own one is used). Each answer records the fingerprint of exactly what was shown.
/// Nothing is asked without a person: untrusted items just stay out.
pub fn ask(paths: &ConfigPaths, state: &Path) -> Result<(), CliError> {
    let env = |k: &str| std::env::var(k).ok();
    let gate = Gate::load(paths, Some(state), mcp_trust::trust_all_from(&env));
    let pending = repo_trust::untrusted(&repo_trust::scan(paths), &gate);
    if pending.is_empty() {
        return Ok(());
    }
    let mut ui = ui::detect(true);
    if !ui.interactive() {
        return Ok(());
    }
    ask_with(ui.as_mut(), paths, state, &pending)
}

fn ask_with(
    ui: &mut dyn Ui,
    paths: &ConfigPaths,
    state: &Path,
    pending: &[UntrustedItem],
) -> Result<(), CliError> {
    let store_path = TrustStore::path_in(state);
    let repo = repo_trust::repo_root(paths).unwrap_or_default();
    let first_time = TrustStore::load(&store_path)
        .map(|s| s.items(&repo).is_empty())
        .unwrap_or(true);
    ui.say(&format!(
        "{} brings agents, skills or model settings you have not trusted.",
        repo.join(".amp/harness").display()
    ));
    if first_time {
        ui.say(
            "amp-harness now asks before it uses what a repo's .amp/harness/ brings, as it does for \
             its MCP servers: a repo agent can change what the manager may run, a skill adds \
             instructions and scripts, and a [roles.*] table picks models and request fields. You \
             are asked once for each; a change asks again. Until you trust one, the built-in or \
             your own is used.",
        );
    }
    let mut all = false;
    let mut trusted: Vec<&RepoItem> = Vec::new();
    for u in pending {
        let i = &u.item;
        let changed = if u.changed {
            " (changed since you trusted it)"
        } else {
            ""
        };
        ui.say(&format!("\n  {}{changed}:", i.label()));
        for line in &i.details {
            ui.say(&format!("    {line}"));
        }
        let yes = all || {
            let answer = ui.ask(
                &format!(
                    "  Trust {} `{}`? [y: yes, a: all, N: not now]",
                    i.kind.word(),
                    i.name
                ),
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
            trusted.push(i);
        } else {
            ui.say(&format!(
                "  Left out {} for this chat (`amp-harness trust {}` trusts it).",
                i.label(),
                i.id()
            ));
        }
    }
    if trusted.is_empty() {
        return Ok(());
    }
    let names = trusted
        .iter()
        .map(|i| i.id())
        .collect::<Vec<_>>()
        .join(", ");
    match repo_trust::record(paths, state, &trusted) {
        Ok(()) => ui.say(&format!(
            "  Trusted {names} (recorded in {}).",
            store_path.display()
        )),
        // Not recorded means not trusted: the runtime reads the store.
        Err(e) => ui.say(&format!(
            "  Could not record {names}, so they stay out: {e}"
        )),
    }
    Ok(())
}

/// What the CLI writes into this repo's `.amp/harness/` for the person (`agents new --repo`,
/// `skills edit --repo`, `models set <role> --repo`, ...) is theirs: [`RepoWrites::finish`] records
/// every repo item the command added or changed as trusted, so their own change is not asked
/// about. Items that were already there unchanged (say, from a `git pull`) are left alone.
pub struct RepoWrites {
    paths: ConfigPaths,
    state: Option<PathBuf>,
    before: Vec<RepoItem>,
}

impl RepoWrites {
    /// Notes the repo items of the current directory's repo before a CLI command runs.
    pub fn start() -> Self {
        let paths = std::env::current_dir()
            .map(|d| conn::config_paths_in(&d))
            .unwrap_or_else(|_| conn::config_paths());
        let env = |k: &str| std::env::var(k).ok();
        Self {
            before: repo_trust::scan(&paths),
            state: state_dir(&env),
            paths,
        }
    }

    /// After the command: when it succeeded, records what it added or changed.
    pub fn finish(self, ok: bool) {
        let Some(state) = self.state.filter(|_| ok) else {
            return;
        };
        let after = repo_trust::scan(&self.paths);
        let mine: Vec<&RepoItem> = after
            .iter()
            .filter(|i| {
                !self
                    .before
                    .iter()
                    .any(|b| b.id() == i.id() && b.sha256 == i.sha256)
            })
            .collect();
        if let Err(e) = repo_trust::record(&self.paths, &state, &mine) {
            eprintln!(
                "amp-harness: could not record your change as trusted ({e}); `amp-harness trust` \
                 does it"
            );
        }
    }
}

/// One line per untrusted repo item, for stderr (`run -p`) or the transcript.
pub fn warnings(items: &[UntrustedItem]) -> Vec<String> {
    items.iter().map(UntrustedItem::reason).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn items_parse() {
        assert_eq!(
            parse_item("agent:amp-manager").unwrap(),
            (Some(ItemKind::Agent), "amp-manager".to_owned())
        );
        assert_eq!(parse_item("mcp:fs").unwrap(), (None, "fs".to_owned()));
        assert!(parse_item("amp-manager").is_err());
        assert!(parse_item("thing:x").is_err());
        assert!(parse_item("agent:").is_err());
    }
}
