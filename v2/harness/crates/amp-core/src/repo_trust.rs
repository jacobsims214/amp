//! Trust for what a repo's `.amp/harness/` brings besides MCP servers (#2310, review #2248
//! findings 7 and 12): agents (`agents/<id>.md`), skills (`skills/<name>/`), `[roles.<id>]` tables
//! and the `[models] profile` setting of its config.toml.
//!
//! Those came with the code you cloned. A repo agent named like a shipped one replaces it,
//! `permission:` block included, and live reload (#2242) applied it to the running chat within
//! seconds of a `git pull`: `bash: allow` for the manager with nobody asked. A repo `[roles.*]`
//! table can pick another model or add `extra_body` fields to every request of that role. So, as
//! for repo MCP servers ([`crate::mcp_trust`]):
//!
//! - **What is trusted** is one item of one repo *as it is now*: its kind, its name and a
//!   fingerprint ([`agent_fingerprint`], [`skill_fingerprint`], [`role_fingerprint`],
//!   [`setting_fingerprint`]) of exactly the bytes the person was shown. Any change asks again.
//! - **Where**: the same `<state>/trust.toml` ([`TrustStore::item`]), never inside the repo.
//! - **An untrusted item is not loaded**: [`crate::assets::Assets::load_gated`] skips the repo file
//!   (the built-in or your own agent or skill is used), and [`crate::models::ModelsConfig::load_paths`]
//!   leaves the repo's role table or profile out. Live reload goes through the same gate, so a
//!   pulled change waits for trust.
//! - **Who asks**: the chat, once per item, before it starts (`amp-harness` in a terminal);
//!   `amp-harness trust` from the CLI. Headless runs skip untrusted items with a warning unless
//!   `--trust-repo` / `AMP_HARNESS_TRUST_REPO=1` (never recorded).
//! - What a person writes into the repo layer with the CLI (`agents new --repo`, `models set
//!   --repo`, ...) is recorded as trusted when written.

use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
};

use serde::Serialize;
use sha2::{Digest, Sha256};

use crate::{
    assets::{self, AGENTS_DIR, SKILL_FILE, SKILLS_DIR},
    mcp_trust::{self, TrustState, TrustStore},
};

/// What kind of repo item.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum ItemKind {
    /// `.amp/harness/agents/<id>.md`.
    Agent,
    /// `.amp/harness/skills/<name>/`.
    Skill,
    /// `[roles.<id>]` in `.amp/harness/config.toml`.
    Role,
    /// A `[models]` key in `.amp/harness/config.toml` (`profile`).
    Setting,
}

impl ItemKind {
    pub const ALL: [ItemKind; 4] = [Self::Agent, Self::Skill, Self::Role, Self::Setting];

    /// `agent`, `skill`, `role`, `setting`: how the CLI names it (`agent:<id>`).
    pub fn word(self) -> &'static str {
        match self {
            Self::Agent => "agent",
            Self::Skill => "skill",
            Self::Role => "role",
            Self::Setting => "setting",
        }
    }

    pub fn parse(word: &str) -> Option<Self> {
        Self::ALL
            .into_iter()
            .find(|k| k.word() == word.trim().to_ascii_lowercase())
    }
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// sha256 over an agent file's bytes.
pub fn agent_fingerprint(bytes: &[u8]) -> String {
    let mut h = Sha256::new();
    h.update(b"amp-harness repo agent trust v1\n");
    h.update(bytes);
    hex(&h.finalize())
}

/// sha256 over a skill directory's files (`/`-separated relative path and contents, sorted).
pub fn skill_fingerprint<'a>(files: impl IntoIterator<Item = (&'a str, &'a [u8])>) -> String {
    let mut sorted: Vec<(&str, &[u8])> = files.into_iter().collect();
    sorted.sort();
    let mut h = Sha256::new();
    h.update(b"amp-harness repo skill trust v1\n");
    for (rel, bytes) in sorted {
        h.update(rel.as_bytes());
        h.update(b"\0");
        h.update(Sha256::digest(bytes));
        h.update(b"\n");
    }
    hex(&h.finalize())
}

/// sha256 over a `[roles.<id>]` table as written (keys sorted, [`crate::mcp_config::table_sha256`]),
/// so reordering it does not ask again.
pub fn role_fingerprint(table: &toml::Value) -> String {
    let mut h = Sha256::new();
    h.update(b"amp-harness repo role trust v1\n");
    h.update(crate::mcp_config::table_sha256(table).as_bytes());
    hex(&h.finalize())
}

/// sha256 over one `[models] <key> = <value>`.
pub fn setting_fingerprint(key: &str, value: &toml::Value) -> String {
    let mut h = Sha256::new();
    h.update(b"amp-harness repo setting trust v1\n");
    h.update(key.as_bytes());
    h.update(b"=");
    h.update(crate::mcp_config::table_sha256(value).as_bytes());
    hex(&h.finalize())
}

/// The canonical repo root of `paths` (the directory holding `.amp/harness`), as the trust store
/// keys it; `None` outside a repo.
pub fn repo_root(paths: &amp_mcp::ConfigPaths) -> Option<PathBuf> {
    let root = paths.repo_dir.as_deref()?.ancestors().nth(2)?.to_path_buf();
    Some(std::fs::canonicalize(&root).unwrap_or(root))
}

// ---------------------------------------------------------------------------------------------
// The gate

/// The trust decision for one process: the store as read once, and the override. Built by
/// whoever loads agents, skills or role settings for sessions.
#[derive(Debug, Clone)]
pub struct Gate {
    repo: Option<PathBuf>,
    /// `None`: the store could not be read, so nothing is trusted ([`Gate::store_error`]).
    store: Option<TrustStore>,
    store_error: Option<String>,
    trust_all: bool,
    /// Lets everything through (inspection, tests): not a trust decision.
    open: bool,
}

impl Gate {
    /// Reads `<state_dir>/trust.toml` for the repo of `paths`. No state dir: nothing is trusted
    /// unless `trust_all`.
    pub fn load(paths: &amp_mcp::ConfigPaths, state_dir: Option<&Path>, trust_all: bool) -> Self {
        let (store, store_error) = match state_dir {
            Some(dir) => match TrustStore::load(&TrustStore::path_in(dir)) {
                Ok(s) => (Some(s), None),
                Err(e) => (None, Some(e)),
            },
            None => (None, None),
        };
        Self {
            repo: repo_root(paths),
            store,
            store_error,
            trust_all,
            open: false,
        }
    }

    /// [`Gate::load`] with the state dir and override from `env` (the process environment, or a
    /// test's).
    pub fn from_env(paths: &amp_mcp::ConfigPaths, env: &dyn Fn(&str) -> Option<String>) -> Self {
        let state = crate::agent::state_dir(env);
        Self::load(paths, state.as_deref(), mcp_trust::trust_all_from(env))
    }

    /// Lets every repo item through: for code that inspects files rather than runs them.
    pub fn open() -> Self {
        Self {
            repo: None,
            store: None,
            store_error: None,
            trust_all: true,
            open: true,
        }
    }

    /// Why the store could not be read (every repo item is untrusted meanwhile).
    pub fn store_error(&self) -> Option<&str> {
        self.store_error.as_deref()
    }

    /// Where `kind` `name` at fingerprint `sha256` stands.
    pub fn state(&self, kind: ItemKind, name: &str, sha256: &str) -> TrustState {
        if self.open {
            return TrustState::Override;
        }
        let recorded = self
            .repo
            .as_deref()
            .and_then(|r| self.store.as_ref()?.item(r, kind, name));
        match recorded {
            Some(t) if t.sha256 == sha256 => TrustState::Trusted,
            _ if self.trust_all => TrustState::Override,
            Some(_) => TrustState::Changed,
            None => TrustState::Untrusted,
        }
    }

    /// Whether the item may be loaded.
    pub fn allows(&self, kind: ItemKind, name: &str, sha256: &str) -> bool {
        self.state(kind, name, sha256).allows()
    }
}

// ---------------------------------------------------------------------------------------------
// What the repo brings, shown in full

/// One agent, skill or setting the repo brings, read once: what the person is shown
/// ([`RepoItem::details`]) and the fingerprint of exactly that ([`RepoItem::sha256`]).
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct RepoItem {
    pub kind: ItemKind,
    pub name: String,
    /// The file (an agent, the config.toml) or directory (a skill) it comes from.
    pub file: PathBuf,
    pub sha256: String,
    /// One line, for lists, warnings and the store.
    pub summary: String,
    /// Everything it sets, one field per line (the trust question).
    pub details: Vec<String>,
}

impl RepoItem {
    /// `agent:amp-manager`: how the CLI names it.
    pub fn id(&self) -> String {
        format!("{}:{}", self.kind.word(), self.name)
    }

    /// `repo agent `amp-manager``.
    pub fn label(&self) -> String {
        format!("repo {} `{}`", self.kind.word(), self.name)
    }
}

/// Every agent, skill, `[roles.*]` table and `[models]` setting in the repo layer of `paths`, read
/// now. A file that can't be read is listed with what went wrong (it would not load anyway).
pub fn scan(paths: &amp_mcp::ConfigPaths) -> Vec<RepoItem> {
    let Some(dir) = paths.repo_dir.as_deref() else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for path in assets::list_dir(&dir.join(AGENTS_DIR)).unwrap_or_default() {
        let Some(id) = assets::md_stem(&path) else {
            continue;
        };
        if !path.is_file() {
            continue;
        }
        let Ok(bytes) = std::fs::read(&path) else {
            continue;
        };
        out.push(agent_item(paths, &id, &path, &bytes));
    }
    for skill_dir in assets::list_dir(&dir.join(SKILLS_DIR)).unwrap_or_default() {
        if !skill_dir.is_dir() {
            continue;
        }
        let name = assets::file_name(&skill_dir);
        let files = assets::read_skill_dir(&skill_dir).unwrap_or_default();
        out.push(skill_item(paths, &name, &skill_dir, &files));
    }
    if let Some(file) = paths.repo_file()
        && let Ok(text) = std::fs::read_to_string(&file)
        && let Ok(table) = toml::from_str::<toml::Table>(&text)
    {
        if let Some(toml::Value::Table(roles)) = table.get("roles") {
            for (role, value) in roles {
                out.push(role_item(&file, role, value));
            }
        }
        if let Some(toml::Value::Table(models)) = table.get("models") {
            for (key, value) in models {
                out.push(setting_item(&file, key, value));
            }
        }
    }
    out
}

/// A repo agent file as shown and fingerprinted.
pub fn agent_item(paths: &amp_mcp::ConfigPaths, id: &str, path: &Path, bytes: &[u8]) -> RepoItem {
    let mut details = Vec::new();
    mcp_trust::detail_field(&mut details, "file", &path.display().to_string());
    let user = paths
        .user_dir
        .as_ref()
        .map(|d| d.join(AGENTS_DIR).join(format!("{id}.md")))
        .filter(|p| p.is_file());
    let replaces = match (&user, assets::Assets::embedded_agent_text(id).is_some()) {
        (Some(p), _) => format!("your agent {}", p.display()),
        (None, true) => format!("the built-in agent {id}"),
        (None, false) => "nothing (a new agent)".to_owned(),
    };
    mcp_trust::detail_field(&mut details, "replaces", &replaces);
    let text = String::from_utf8_lossy(bytes);
    let mut keys = Vec::new();
    match assets::parse_frontmatter(&text) {
        Ok((fm, prompt)) => {
            if let Some(map) = fm.as_object() {
                for (k, v) in map {
                    keys.push(k.clone());
                    match (k.as_str(), v.as_object()) {
                        ("permission", Some(rules)) => {
                            let mut first = true;
                            for (tool, rule) in rules {
                                mcp_trust::detail_field(
                                    &mut details,
                                    if first { "permission" } else { "" },
                                    &format!("{tool}: {}", compact(rule)),
                                );
                                first = false;
                            }
                        }
                        _ => mcp_trust::detail_field(&mut details, k, &compact(v)),
                    }
                }
            }
            mcp_trust::detail_field(&mut details, "prompt", &{
                let n = prompt.lines().count();
                format!(
                    "{n} line{} (read it in the file above)",
                    if n == 1 { "" } else { "s" }
                )
            });
        }
        Err(e) => mcp_trust::detail_field(&mut details, "problem", &e),
    }
    let summary = format!(
        "agent {id} (replaces {replaces}; sets {})",
        if keys.is_empty() {
            "no frontmatter".to_owned()
        } else {
            keys.join(", ")
        }
    );
    RepoItem {
        kind: ItemKind::Agent,
        name: id.to_owned(),
        file: path.to_path_buf(),
        sha256: agent_fingerprint(bytes),
        summary,
        details,
    }
}

/// A repo skill directory as shown and fingerprinted (`files`: relative path -> contents).
pub fn skill_item(
    paths: &amp_mcp::ConfigPaths,
    name: &str,
    dir: &Path,
    files: &BTreeMap<String, Vec<u8>>,
) -> RepoItem {
    let mut details = Vec::new();
    mcp_trust::detail_field(&mut details, "dir", &dir.display().to_string());
    let user = paths
        .user_dir
        .as_ref()
        .map(|d| d.join(SKILLS_DIR).join(name))
        .filter(|p| p.is_dir());
    let replaces = match (&user, assets::Assets::is_embedded_skill(name)) {
        (Some(p), _) => format!("files of your skill {}", p.display()),
        (None, true) => format!("files of the built-in skill {name}"),
        (None, false) => "nothing (a new skill)".to_owned(),
    };
    mcp_trust::detail_field(&mut details, "replaces", &replaces);
    if let Some(entry) = files.get(SKILL_FILE)
        && let Ok((fm, _)) = assets::parse_frontmatter(&String::from_utf8_lossy(entry))
        && let Some(d) = fm.get("description").and_then(serde_json::Value::as_str)
    {
        mcp_trust::detail_field(&mut details, "about", d);
    }
    let list: Vec<String> = files
        .iter()
        .map(|(rel, bytes)| {
            let exec = is_executable(&dir.join(rel));
            format!(
                "{rel} ({}{})",
                size(bytes.len()),
                if exec { ", executable" } else { "" }
            )
        })
        .collect();
    mcp_trust::detail_field(&mut details, "files", &list.join(", "));
    RepoItem {
        kind: ItemKind::Skill,
        name: name.to_owned(),
        file: dir.to_path_buf(),
        sha256: skill_fingerprint(files.iter().map(|(k, v)| (k.as_str(), v.as_slice()))),
        summary: format!(
            "skill {name} ({} file{}; replaces {replaces})",
            files.len(),
            if files.len() == 1 { "" } else { "s" }
        ),
        details,
    }
}

/// A repo `[roles.<id>]` table as shown and fingerprinted.
pub fn role_item(file: &Path, role: &str, value: &toml::Value) -> RepoItem {
    let mut details = Vec::new();
    mcp_trust::detail_field(&mut details, "file", &file.display().to_string());
    let mut parts = Vec::new();
    match value.as_table() {
        Some(t) => {
            for (k, v) in t {
                let shown = v.to_string();
                mcp_trust::detail_field(&mut details, k, &shown);
                parts.push(format!("{k} = {shown}"));
            }
        }
        None => mcp_trust::detail_field(&mut details, "value", &value.to_string()),
    }
    RepoItem {
        kind: ItemKind::Role,
        name: role.to_owned(),
        file: file.to_path_buf(),
        sha256: role_fingerprint(value),
        summary: format!("[roles.{role}] {}", parts.join(", ")),
        details,
    }
}

/// A repo `[models] <key>` setting as shown and fingerprinted.
pub fn setting_item(file: &Path, key: &str, value: &toml::Value) -> RepoItem {
    let mut details = Vec::new();
    mcp_trust::detail_field(&mut details, "file", &file.display().to_string());
    mcp_trust::detail_field(&mut details, key, &value.to_string());
    RepoItem {
        kind: ItemKind::Setting,
        name: key.to_owned(),
        file: file.to_path_buf(),
        sha256: setting_fingerprint(key, value),
        summary: format!("[models] {key} = {value}"),
        details,
    }
}

fn compact(v: &serde_json::Value) -> String {
    match v {
        serde_json::Value::String(s) => s.clone(),
        other => other.to_string(),
    }
}

fn size(n: usize) -> String {
    if n < 1024 {
        format!("{n} B")
    } else {
        format!("{:.1} KB", n as f64 / 1024.0)
    }
}

fn is_executable(path: &Path) -> bool {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::metadata(path).is_ok_and(|m| m.permissions().mode() & 0o111 != 0)
    }
    #[cfg(not(unix))]
    {
        let _ = path;
        false
    }
}

// ---------------------------------------------------------------------------------------------
// What is left out

/// A repo item that is not loaded because it is not trusted (as it is now).
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct UntrustedItem {
    #[serde(flatten)]
    pub item: RepoItem,
    /// It was trusted before, at another fingerprint.
    pub changed: bool,
}

impl UntrustedItem {
    /// Why it is not used and how to change that (warnings, the reload report).
    pub fn reason(&self) -> String {
        let why = if self.changed {
            "it changed since you trusted it"
        } else {
            "it came with this repo and you have not trusted it"
        };
        format!(
            "{} not used: {why} ({}); review it, then `amp-harness trust {}` (the chat asks at \
             start), or pass --trust-repo",
            self.item.label(),
            self.item.summary,
            self.item.id()
        )
    }
}

/// The items of `items` that `gate` does not let through.
pub fn untrusted(items: &[RepoItem], gate: &Gate) -> Vec<UntrustedItem> {
    items
        .iter()
        .filter_map(|i| {
            let state = gate.state(i.kind, &i.name, &i.sha256);
            (!state.allows()).then(|| UntrustedItem {
                item: i.clone(),
                changed: state == TrustState::Changed,
            })
        })
        .collect()
}

/// Records every item of `items` as trusted at its fingerprint in `<state_dir>/trust.toml`
/// (what the person wrote or was shown). Does nothing outside a repo.
pub fn record(
    paths: &amp_mcp::ConfigPaths,
    state_dir: &Path,
    items: &[&RepoItem],
) -> Result<(), String> {
    let Some(repo) = repo_root(paths) else {
        return Ok(());
    };
    if items.is_empty() {
        return Ok(());
    }
    let mut store = TrustStore::load(&TrustStore::path_in(state_dir))?;
    for i in items {
        store.trust_item(&repo, i.kind, &i.name, &i.sha256, &i.summary);
    }
    store.save()
}

/// Records the repo items of `kind` named `name` as they are now (the CLI wrote them for the
/// person). Missing ones are skipped.
pub fn record_current(
    paths: &amp_mcp::ConfigPaths,
    state_dir: &Path,
    kind: ItemKind,
    name: &str,
) -> Result<(), String> {
    let items = scan(paths);
    let mine: Vec<&RepoItem> = items
        .iter()
        .filter(|i| i.kind == kind && i.name == name)
        .collect();
    record(paths, state_dir, &mine)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(name: &str) -> PathBuf {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-scratch/amp-core-repo-trust")
            .join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn paths(dir: &Path) -> amp_mcp::ConfigPaths {
        amp_mcp::ConfigPaths {
            user_file: Some(dir.join("user/config.toml")),
            user_dir: Some(dir.join("user")),
            repo_dir: Some(dir.join("repo/.amp/harness")),
            ..amp_mcp::ConfigPaths::default()
        }
    }

    const MANAGER: &str = "---\ndescription: pulled\nmode: primary\npermission:\n  bash:\n    \"*\": allow\n  edit: allow\n---\nDo things.\n";

    fn plant(dir: &Path) {
        let h = dir.join("repo/.amp/harness");
        std::fs::create_dir_all(h.join("agents")).unwrap();
        std::fs::create_dir_all(h.join("skills/pdf-tools")).unwrap();
        std::fs::write(h.join("agents/amp-manager.md"), MANAGER).unwrap();
        std::fs::write(
            h.join("skills/pdf-tools/SKILL.md"),
            "---\ndescription: pdf things\n---\nUse run.sh\n",
        )
        .unwrap();
        std::fs::write(h.join("skills/pdf-tools/run.sh"), "#!/bin/sh\necho hi\n").unwrap();
        std::fs::write(
            h.join("config.toml"),
            "[models]\nprofile = \"anthropic\"\n\n[roles.manager]\nmodel = \"a/b\"\nextra_body = { store = true }\n",
        )
        .unwrap();
    }

    #[test]
    fn scan_shows_everything_and_fingerprints_it() {
        let dir = scratch("scan");
        plant(&dir);
        let items = scan(&paths(&dir));
        let ids: Vec<String> = items.iter().map(RepoItem::id).collect();
        assert_eq!(
            ids,
            [
                "agent:amp-manager",
                "skill:pdf-tools",
                "role:manager",
                "setting:profile"
            ]
        );
        let agent = items[0].details.join("\n");
        assert!(agent.contains("the built-in agent amp-manager"), "{agent}");
        assert!(
            agent.contains("permission bash: {\"*\":\"allow\"}"),
            "{agent}"
        );
        assert!(agent.contains("edit: allow"), "{agent}");
        assert!(agent.contains("mode       primary"), "{agent}");
        let skill = items[1].details.join("\n");
        assert!(skill.contains("run.sh (18 B)"), "{skill}");
        assert!(skill.contains("nothing (a new skill)"), "{skill}");
        let role = items[2].details.join("\n");
        assert!(role.contains("extra_body { store = true }"), "{role}");
        assert!(role.contains("model      \"a/b\""), "{role}");
        assert_eq!(items[3].summary, "[models] profile = \"anthropic\"");
        // Stable, and any change changes it.
        assert_eq!(scan(&paths(&dir)), items);
        std::fs::write(
            dir.join("repo/.amp/harness/agents/amp-manager.md"),
            MANAGER.replace("edit: allow", "edit: deny"),
        )
        .unwrap();
        assert_ne!(scan(&paths(&dir))[0].sha256, items[0].sha256);
        std::fs::write(
            dir.join("repo/.amp/harness/skills/pdf-tools/run.sh"),
            "#!/bin/sh\ncurl x\n",
        )
        .unwrap();
        assert_ne!(scan(&paths(&dir))[1].sha256, items[1].sha256);
    }

    #[test]
    fn the_gate_trusts_what_was_recorded_and_asks_again_on_change() {
        let dir = scratch("gate");
        plant(&dir);
        let p = paths(&dir);
        let state = dir.join("state");
        let items = scan(&p);
        let gate = Gate::load(&p, Some(&state), false);
        assert_eq!(untrusted(&items, &gate).len(), 4);
        let r = untrusted(&items, &gate)[0].reason();
        assert!(r.contains("`amp-harness trust agent:amp-manager`"), "{r}");
        assert!(r.contains("--trust-repo"), "{r}");
        // The override lets everything through without recording.
        assert!(untrusted(&items, &Gate::load(&p, Some(&state), true)).is_empty());
        assert!(!state.join("trust.toml").exists());

        record(&p, &state, &[&items[0], &items[2]]).unwrap();
        let gate = Gate::load(&p, Some(&state), false);
        let left: Vec<String> = untrusted(&items, &gate)
            .iter()
            .map(|u| u.item.id())
            .collect();
        assert_eq!(left, ["skill:pdf-tools", "setting:profile"]);
        // Saved outside the repo, next to the MCP records.
        let text = std::fs::read_to_string(state.join("trust.toml")).unwrap();
        assert!(text.contains(".agents.amp-manager]"), "{text}");
        assert!(text.contains(".roles.manager]"), "{text}");

        // A pulled change: asked again, as changed.
        std::fs::write(
            dir.join("repo/.amp/harness/agents/amp-manager.md"),
            MANAGER.replace("Do things.", "Do other things."),
        )
        .unwrap();
        let now = scan(&p);
        let u = untrusted(&now, &gate);
        assert!(u[0].changed && u[0].item.name == "amp-manager");
        assert!(u[0].reason().contains("changed since you trusted it"));
        // Reordering the role table does not.
        std::fs::write(
            dir.join("repo/.amp/harness/config.toml"),
            "[roles.manager]\nextra_body = { store = true }\nmodel = \"a/b\"\n",
        )
        .unwrap();
        let now = scan(&p);
        assert!(
            untrusted(&now, &gate)
                .iter()
                .all(|u| u.item.kind != ItemKind::Role)
        );
    }

    #[test]
    fn a_broken_or_missing_store_trusts_nothing() {
        let dir = scratch("broken");
        plant(&dir);
        let p = paths(&dir);
        std::fs::create_dir_all(dir.join("state")).unwrap();
        std::fs::write(dir.join("state/trust.toml"), "repos = 1\n").unwrap();
        let gate = Gate::load(&p, Some(&dir.join("state")), false);
        assert!(gate.store_error().is_some());
        assert_eq!(untrusted(&scan(&p), &gate).len(), 4);
        assert_eq!(untrusted(&scan(&p), &Gate::load(&p, None, false)).len(), 4);
        assert!(untrusted(&scan(&p), &Gate::open()).is_empty());
    }

    #[test]
    fn mcp_records_and_item_records_live_side_by_side() {
        let dir = scratch("side-by-side");
        plant(&dir);
        let p = paths(&dir);
        let state = dir.join("state");
        let items = scan(&p);
        record(&p, &state, &[&items[1]]).unwrap();
        let path = TrustStore::path_in(&state);
        let mut store = TrustStore::load(&path).unwrap();
        let repo = repo_root(&p).unwrap();
        // `mcp untrust --all` forgets servers only.
        assert!(store.untrust_repo(&repo).is_empty());
        assert!(store.item(&repo, ItemKind::Skill, "pdf-tools").is_some());
        assert!(store.untrust_item(&repo, ItemKind::Skill, "pdf-tools"));
        assert!(!store.knows_repo(&repo));
    }

    #[test]
    fn kinds_parse() {
        for k in ItemKind::ALL {
            assert_eq!(ItemKind::parse(k.word()), Some(k));
        }
        assert_eq!(ItemKind::parse("mcp"), None);
    }
}
