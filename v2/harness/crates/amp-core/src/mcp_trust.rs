//! Trust for repo-level MCP servers (#2241, H8; D7 of decisions/amp-harness-configurability.md).
//!
//! A `[mcp.<name>]` table in a repo's `.amp/harness/config.toml` came with the code you cloned, not
//! from you, and a server can run any command (stdio) or send what it is given anywhere (http,
//! sse). So a repo server never starts until the person has trusted it:
//!
//! - **What is trusted** is one server of one repo *as configured now*: the canonical repo path,
//!   the server name and a [`fingerprint`] (sha256 of its table as written, plus the contents of a
//!   literal `env_file`). Any change to the table or that file means asking again.
//! - **Where**: `<state>/trust.toml` ([`TrustStore`]; `$XDG_STATE_HOME/amp-harness`, else
//!   `~/.local/state/amp-harness`), never inside the repo, so a repo cannot trust itself.
//! - **Who asks**: the chat asks once per server before its hub starts (`amp-harness` in a
//!   terminal); `amp-harness mcp trust <name>|--all` and `mcp untrust` do it from the CLI.
//!   Headless runs (`run -p`, every worker) never ask: an untrusted server is skipped with a
//!   warning, unless `--trust-repo-mcp` or `AMP_HARNESS_TRUST_REPO_MCP=1` trusts every repo
//!   server for that process (CI). That override is never written to the store.
//! - User-level servers (your own config.toml) are always trusted.
//!
//! [`crate::mcp_hub::McpHub`] applies it: untrusted servers get
//! [`crate::mcp_hub::ServerStatus::Untrusted`] and are never spawned or connected until
//! [`crate::mcp_hub::McpHub::trust`].

use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
    sync::atomic::{AtomicBool, Ordering},
};

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::mcp_config::{McpConfig, McpServer, Transport};

/// `1` (or `true`/`yes`) trusts every repo MCP server for this process, without asking and without
/// recording it. Same as `--trust-repo-mcp`.
pub const ENV_TRUST_REPO_MCP: &str = "AMP_HARNESS_TRUST_REPO_MCP";
/// The trust store's file name in the state directory.
pub const TRUST_FILE: &str = "trust.toml";

static TRUST_ALL: AtomicBool = AtomicBool::new(false);

/// `--trust-repo-mcp`: trust every repo server in this process (main sets it once).
pub fn set_trust_all(on: bool) {
    TRUST_ALL.store(on, Ordering::Release);
}

/// Whether every repo server is trusted for this process: `--trust-repo-mcp` or
/// [`ENV_TRUST_REPO_MCP`].
pub fn trust_all_from(env: &dyn Fn(&str) -> Option<String>) -> bool {
    TRUST_ALL.load(Ordering::Acquire)
        || env(ENV_TRUST_REPO_MCP).is_some_and(|v| {
            matches!(
                v.trim().to_ascii_lowercase().as_str(),
                "1" | "true" | "yes" | "on"
            )
        })
}

/// The repo a repo server belongs to: the directory holding `.amp/harness/config.toml`,
/// canonicalized (so `./x` and a symlink to it are the same repo). `None` for a user server.
pub fn repo_root(server: &McpServer) -> Option<PathBuf> {
    if !server.is_repo() {
        return None;
    }
    let root = server.origin.file.ancestors().nth(3)?.to_path_buf();
    Some(std::fs::canonicalize(&root).unwrap_or(root))
}

/// What trust is recorded against: sha256 over the table's hash
/// ([`crate::mcp_config::table_sha256`]) and, for a stdio server with a literal `env_file`, that
/// file's contents (it sets the server's environment, so a changed file is a changed server).
pub fn fingerprint(server: &McpServer) -> String {
    let mut h = Sha256::new();
    h.update(b"amp-harness repo mcp trust v1\n");
    h.update(server.table_sha256.as_bytes());
    if let Transport::Stdio(s) = &server.transport
        && let Some(file) = &s.env_file
    {
        h.update(b"\nenv_file:");
        if file.contains('$') {
            // A template: the expanded path differs per machine; the table hash covers the text.
            h.update(b"template");
        } else {
            let path = Path::new(file);
            let path = match repo_root(server) {
                Some(root) if path.is_relative() => root.join(path),
                _ => path.to_path_buf(),
            };
            match std::fs::read(&path) {
                Ok(bytes) => h.update(Sha256::digest(&bytes)),
                Err(_) => h.update(b"unreadable"),
            }
        }
    }
    hex(&h.finalize())
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// One line saying what trusting the server lets it do: ``runs `npx -y pkg ${repo}` `` or
/// `connects to https://host`. Templates are shown unexpanded; config files never hold secret
/// values (they are refused), and a URL is shown as its origin only.
pub fn summary(server: &McpServer) -> String {
    match &server.transport {
        Transport::Stdio(s) => {
            let mut line = quote_word(&s.command);
            for a in &s.args {
                line.push(' ');
                line.push_str(&quote_word(a));
            }
            const MAX: usize = 160;
            if line.chars().count() > MAX {
                line = line.chars().take(MAX).collect::<String>() + "…";
            }
            format!("runs `{line}`")
        }
        Transport::Http(r) | Transport::Sse(r) => {
            format!("connects to {}", amp_mcp::url_origin(&r.url))
        }
    }
}

fn quote_word(w: &str) -> String {
    if !w.is_empty()
        && w.chars()
            .all(|c| c.is_ascii_alphanumeric() || "-_./:=@%+,${}~".contains(c))
    {
        w.to_owned()
    } else {
        format!("'{}'", w.replace('\'', "'\\''"))
    }
}

// ---------------------------------------------------------------------------------------------
// The store

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
struct TrustFile {
    /// Canonical repo path -> its trusted servers.
    #[serde(default)]
    repos: BTreeMap<String, RepoEntry>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
struct RepoEntry {
    #[serde(default)]
    mcp: BTreeMap<String, TrustedServer>,
}

/// One trusted server: the fingerprint it was trusted at, when, and what it did then.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct TrustedServer {
    pub sha256: String,
    /// Unix seconds.
    pub trusted_at: i64,
    /// [`summary`] at the time (for people reading the file).
    #[serde(default)]
    pub what: String,
}

const HEADER: &str = "# amp-harness: MCP servers from repo .amp/harness/config.toml files that you \
                      trusted (#2241).\n# Managed by `amp-harness mcp trust` / `mcp untrust` and \
                      the chat. A server whose config changes is asked about again.\n\n";

/// `<state>/trust.toml`: the repo servers the person trusted.
#[derive(Debug, Clone)]
pub struct TrustStore {
    path: PathBuf,
    file: TrustFile,
}

impl TrustStore {
    /// `<state_dir>/trust.toml`.
    pub fn path_in(state_dir: &Path) -> PathBuf {
        state_dir.join(TRUST_FILE)
    }

    /// An empty store that saves to `path`.
    pub fn empty(path: PathBuf) -> Self {
        Self {
            path,
            file: TrustFile::default(),
        }
    }

    /// Reads `path`; a missing file is an empty store. An unreadable or unparsable one is an
    /// error (callers treat every repo server as untrusted then; the message says how to fix it).
    pub fn load(path: &Path) -> Result<Self, String> {
        let text = match std::fs::read_to_string(path) {
            Ok(t) => t,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Ok(Self::empty(path.to_path_buf()));
            }
            Err(e) => return Err(format!("{}: {e}", path.display())),
        };
        let file = toml::from_str::<TrustFile>(&text).map_err(|e| {
            format!(
                "{}: {} (fix or delete the file; every repo MCP server is untrusted meanwhile)",
                path.display(),
                e.message()
            )
        })?;
        Ok(Self {
            path: path.to_path_buf(),
            file,
        })
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    fn key(repo: &Path) -> String {
        repo.to_string_lossy().into_owned()
    }

    /// The record for `name` in `repo`, if any (trusted at some fingerprint).
    pub fn get(&self, repo: &Path, name: &str) -> Option<&TrustedServer> {
        self.file.repos.get(&Self::key(repo))?.mcp.get(name)
    }

    /// Every trusted server of `repo`.
    pub fn servers(&self, repo: &Path) -> Vec<(String, TrustedServer)> {
        self.file
            .repos
            .get(&Self::key(repo))
            .map(|r| r.mcp.iter().map(|(k, v)| (k.clone(), v.clone())).collect())
            .unwrap_or_default()
    }

    /// Records `server` (a repo server) as trusted at its current fingerprint. Returns false for a
    /// user server (always trusted; nothing to record).
    pub fn trust(&mut self, server: &McpServer) -> bool {
        let Some(repo) = repo_root(server) else {
            return false;
        };
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| d.as_secs() as i64);
        self.file
            .repos
            .entry(Self::key(&repo))
            .or_default()
            .mcp
            .insert(
                server.name.clone(),
                TrustedServer {
                    sha256: fingerprint(server),
                    trusted_at: now,
                    what: summary(server),
                },
            );
        true
    }

    /// Forgets `name` in `repo`. Returns whether it was recorded.
    pub fn untrust(&mut self, repo: &Path, name: &str) -> bool {
        let key = Self::key(repo);
        let Some(entry) = self.file.repos.get_mut(&key) else {
            return false;
        };
        let removed = entry.mcp.remove(name).is_some();
        if entry.mcp.is_empty() {
            self.file.repos.remove(&key);
        }
        removed
    }

    /// Forgets every server of `repo`; returns their names.
    pub fn untrust_repo(&mut self, repo: &Path) -> Vec<String> {
        self.file
            .repos
            .remove(&Self::key(repo))
            .map(|r| r.mcp.into_keys().collect())
            .unwrap_or_default()
    }

    /// Writes the store (creating the state directory), replacing the file atomically, readable by
    /// the owner only.
    pub fn save(&self) -> Result<(), String> {
        let fail = |e: std::io::Error| format!("{}: {e}", self.path.display());
        if let Some(dir) = self.path.parent() {
            std::fs::create_dir_all(dir).map_err(fail)?;
        }
        let body = toml::to_string(&self.file).map_err(|e| format!("trust store: {e}"))?;
        let tmp = self.path.with_extension("toml.tmp");
        std::fs::write(&tmp, format!("{HEADER}{body}")).map_err(fail)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(0o600)).map_err(fail)?;
        }
        std::fs::rename(&tmp, &self.path).map_err(fail)
    }
}

// ---------------------------------------------------------------------------------------------
// Decisions

/// Where one server stands.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum TrustState {
    /// A user-level server: the person wrote it.
    User,
    /// A repo server trusted at its current fingerprint.
    Trusted,
    /// A repo server trusted only because of `--trust-repo-mcp` / [`ENV_TRUST_REPO_MCP`].
    Override,
    /// A repo server never trusted.
    Untrusted,
    /// A repo server trusted before, but its config changed since.
    Changed,
}

impl TrustState {
    /// Whether the server may start.
    pub fn allows(self) -> bool {
        matches!(self, Self::User | Self::Trusted | Self::Override)
    }

    pub fn word(self) -> &'static str {
        match self {
            Self::User => "user",
            Self::Trusted => "trusted",
            Self::Override => "trusted (--trust-repo-mcp)",
            Self::Untrusted => "not trusted",
            Self::Changed => "changed since trusted",
        }
    }
}

/// `server`'s trust given the store (`None`: unreadable, so nothing is trusted) and the
/// process-wide override.
pub fn state_of(server: &McpServer, store: Option<&TrustStore>, trust_all: bool) -> TrustState {
    let Some(repo) = repo_root(server) else {
        return TrustState::User;
    };
    let recorded = store.and_then(|s| s.get(&repo, &server.name));
    match recorded {
        Some(t) if t.sha256 == fingerprint(server) => TrustState::Trusted,
        _ if trust_all => TrustState::Override,
        Some(_) => TrustState::Changed,
        None => TrustState::Untrusted,
    }
}

/// A repo server that may not start yet.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Untrusted {
    pub name: String,
    /// [`summary`].
    pub what: String,
    /// It was trusted before, at another fingerprint.
    pub changed: bool,
    /// The repo config file it comes from.
    pub file: PathBuf,
}

impl Untrusted {
    /// Why it is not running and how to change that (status text, warnings).
    pub fn reason(&self) -> String {
        let why = if self.changed {
            "its config in this repo changed since you trusted it"
        } else {
            "it comes from this repo's .amp/harness/config.toml"
        };
        format!(
            "{why} ({}); trust it with `amp-harness mcp trust {}` (the chat asks at start), or \
             pass --trust-repo-mcp",
            self.what, self.name
        )
    }
}

/// The enabled repo servers of `config` that may not start, by name.
pub fn untrusted(
    config: &McpConfig,
    store: Option<&TrustStore>,
    trust_all: bool,
) -> BTreeMap<String, Untrusted> {
    config
        .enabled()
        .filter_map(|s| {
            let state = state_of(s, store, trust_all);
            (!state.allows()).then(|| {
                (
                    s.name.clone(),
                    Untrusted {
                        name: s.name.clone(),
                        what: summary(s),
                        changed: state == TrustState::Changed,
                        file: s.origin.file.clone(),
                    },
                )
            })
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mcp_config::McpConfig;

    fn scratch(name: &str) -> PathBuf {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-scratch/amp-core-mcp-trust")
            .join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    /// A repo at `<dir>/repo` whose `.amp/harness/config.toml` is `repo_toml`, and a user file.
    fn config(dir: &Path, user_toml: &str, repo_toml: &str) -> McpConfig {
        let repo_file = dir.join("repo/.amp/harness/config.toml");
        std::fs::create_dir_all(repo_file.parent().unwrap()).unwrap();
        std::fs::write(&repo_file, repo_toml).unwrap();
        let user_file = dir.join("user/config.toml");
        std::fs::create_dir_all(user_file.parent().unwrap()).unwrap();
        std::fs::write(&user_file, user_toml).unwrap();
        let paths = amp_mcp::ConfigPaths {
            user_file: Some(user_file),
            user_dir: Some(dir.join("user")),
            repo_dir: Some(dir.join("repo/.amp/harness")),
            ..amp_mcp::ConfigPaths::default()
        };
        let c = McpConfig::load(&paths);
        assert!(c.errors().next().is_none(), "{:?}", c.problems);
        c
    }

    const REPO: &str = "[mcp.fs]\ncommand = \"npx\"\nargs = [\"-y\", \"pkg\", \"${repo}\"]\nenv_file = \".env.mcp\"\n\n[mcp.web]\ntype = \"http\"\nurl = \"https://mcp.example.com/v1/mcp?x=1\"\n";

    #[test]
    fn repo_servers_need_trust_and_user_servers_do_not() {
        let dir = scratch("basic");
        let c = config(&dir, "[mcp.mine]\ncommand = \"mine\"\n", REPO);
        let store = TrustStore::empty(dir.join("state/trust.toml"));
        assert_eq!(
            state_of(&c.servers["mine"], Some(&store), false),
            TrustState::User
        );
        let u = untrusted(&c, Some(&store), false);
        assert_eq!(u.keys().collect::<Vec<_>>(), ["fs", "web"]);
        assert_eq!(u["fs"].what, "runs `npx -y pkg ${repo}`");
        assert_eq!(u["web"].what, "connects to https://mcp.example.com");
        assert!(!u["fs"].changed);
        assert!(
            u["fs"].reason().contains("`amp-harness mcp trust fs`"),
            "{}",
            u["fs"].reason()
        );
        // The override trusts everything, records nothing.
        assert!(untrusted(&c, Some(&store), true).is_empty());
        assert_eq!(
            state_of(&c.servers["fs"], Some(&store), true),
            TrustState::Override
        );
        // An unreadable store trusts nothing.
        assert_eq!(untrusted(&c, None, false).len(), 2);
    }

    #[test]
    fn trust_is_saved_outside_the_repo_and_survives_a_reload() {
        let dir = scratch("roundtrip");
        let c = config(&dir, "", REPO);
        let path = dir.join("state/trust.toml");
        let mut store = TrustStore::load(&path).unwrap();
        assert!(store.trust(&c.servers["fs"]));
        store.save().unwrap();
        let text = std::fs::read_to_string(&path).unwrap();
        assert!(text.starts_with("# amp-harness"), "{text}");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = std::fs::metadata(&path).unwrap().permissions().mode();
            assert_eq!(mode & 0o777, 0o600);
        }
        let store = TrustStore::load(&path).unwrap();
        assert_eq!(
            state_of(&c.servers["fs"], Some(&store), false),
            TrustState::Trusted
        );
        assert_eq!(
            state_of(&c.servers["web"], Some(&store), false),
            TrustState::Untrusted
        );
        let repo = repo_root(&c.servers["fs"]).unwrap();
        assert_eq!(repo, std::fs::canonicalize(dir.join("repo")).unwrap());
        assert_eq!(store.servers(&repo).len(), 1);
        // Nothing was written into the repo.
        let in_repo: Vec<_> = std::fs::read_dir(dir.join("repo/.amp/harness"))
            .unwrap()
            .map(|e| e.unwrap().file_name())
            .collect();
        assert_eq!(in_repo, ["config.toml"]);
    }

    #[test]
    fn any_change_to_the_table_or_its_env_file_asks_again() {
        let dir = scratch("changes");
        std::fs::create_dir_all(dir.join("repo")).unwrap();
        std::fs::write(dir.join("repo/.env.mcp"), "A=1\n").unwrap();
        let c = config(&dir, "", REPO);
        let mut store = TrustStore::empty(dir.join("state/trust.toml"));
        store.trust(&c.servers["fs"]);
        assert_eq!(
            state_of(&c.servers["fs"], Some(&store), false),
            TrustState::Trusted
        );

        // Same table written differently: still trusted.
        let reordered = "[mcp.web]\nurl = \"https://mcp.example.com/v1/mcp?x=1\"\ntype = \"http\"\n\n[mcp.fs]\nenv_file = '.env.mcp'\nargs = [ \"-y\", \"pkg\", \"${repo}\" ]\ncommand = \"npx\"\n";
        let c2 = config(&dir, "", reordered);
        assert_eq!(
            state_of(&c2.servers["fs"], Some(&store), false),
            TrustState::Trusted
        );

        // A changed argument: changed.
        let c3 = config(&dir, "", &REPO.replace("\"pkg\"", "\"evil\""));
        assert_eq!(
            state_of(&c3.servers["fs"], Some(&store), false),
            TrustState::Changed
        );
        assert!(untrusted(&c3, Some(&store), false)["fs"].changed);
        assert!(
            untrusted(&c3, Some(&store), false)["fs"]
                .reason()
                .contains("changed since you trusted it")
        );

        // A new key (startup) changes it too.
        let c4 = config(
            &dir,
            "",
            &REPO.replace("env_file", "startup = \"eager\"\nenv_file"),
        );
        assert_eq!(
            state_of(&c4.servers["fs"], Some(&store), false),
            TrustState::Changed
        );

        // The env file's contents.
        std::fs::write(dir.join("repo/.env.mcp"), "NODE_OPTIONS=--require ./x.js\n").unwrap();
        let c5 = config(&dir, "", REPO);
        assert_eq!(
            state_of(&c5.servers["fs"], Some(&store), false),
            TrustState::Changed
        );
    }

    #[test]
    fn a_user_enabled_switch_does_not_change_the_fingerprint_and_disabled_servers_are_not_listed() {
        let dir = scratch("switch");
        let c = config(&dir, "", REPO);
        let mut store = TrustStore::empty(dir.join("state/trust.toml"));
        store.trust(&c.servers["web"]);
        let c2 = config(
            &dir,
            "[mcp.web]\nenabled = true\n[mcp.fs]\nenabled = false\n",
            REPO,
        );
        assert_eq!(
            state_of(&c2.servers["web"], Some(&store), false),
            TrustState::Trusted
        );
        assert!(untrusted(&c2, Some(&store), false).is_empty());
    }

    #[test]
    fn a_repo_table_replacing_a_user_one_needs_trust() {
        let dir = scratch("shadow");
        let c = config(&dir, "[mcp.fs]\ncommand = \"safe\"\n", REPO);
        assert!(c.servers["fs"].is_repo());
        assert_eq!(
            state_of(&c.servers["fs"], None, false),
            TrustState::Untrusted
        );
    }

    #[test]
    fn untrust_forgets_one_or_all() {
        let dir = scratch("untrust");
        let c = config(&dir, "", REPO);
        let mut store = TrustStore::empty(dir.join("state/trust.toml"));
        store.trust(&c.servers["fs"]);
        store.trust(&c.servers["web"]);
        let repo = repo_root(&c.servers["fs"]).unwrap();
        assert!(store.untrust(&repo, "fs"));
        assert!(!store.untrust(&repo, "fs"));
        assert_eq!(
            state_of(&c.servers["fs"], Some(&store), false),
            TrustState::Untrusted
        );
        assert_eq!(store.untrust_repo(&repo), ["web"]);
        assert!(store.servers(&repo).is_empty());
    }

    #[test]
    fn a_broken_store_is_an_error_that_names_the_file() {
        let dir = scratch("broken");
        let path = dir.join("trust.toml");
        std::fs::write(&path, "repos = 3\n").unwrap();
        let e = TrustStore::load(&path).unwrap_err();
        assert!(e.contains("trust.toml"), "{e}");
        assert!(e.contains("untrusted meanwhile"), "{e}");
        assert!(TrustStore::load(&dir.join("missing.toml")).is_ok());
    }

    #[test]
    fn the_override_reads_the_environment() {
        assert!(trust_all_from(
            &|k| (k == ENV_TRUST_REPO_MCP).then(|| "1".into())
        ));
        assert!(trust_all_from(
            &|k| (k == ENV_TRUST_REPO_MCP).then(|| "true".into())
        ));
        assert!(!trust_all_from(
            &|k| (k == ENV_TRUST_REPO_MCP).then(|| "0".into())
        ));
        assert!(!trust_all_from(&|_| None));
    }

    #[test]
    fn summaries_quote_odd_words() {
        assert_eq!(quote_word("a b"), "'a b'");
        assert_eq!(quote_word("it's"), "'it'\\''s'");
        assert_eq!(quote_word("${env:X}"), "${env:X}");
    }
}
