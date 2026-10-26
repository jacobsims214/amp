//! What a running MCP server depends on beyond its `[mcp.<name>]` table (#2311, review #2248
//! finding 14): the keychain secrets it reads and the contents of its `env_file`. Neither changes
//! the table, so without this a live reload kept a server on its old secret until `/mcp restart`.
//!
//! - **Secret versions.** Keychain values are never read to notice a change (that could raise a
//!   keychain dialog, and the value has no business outside the transport). Instead every CLI
//!   write of an MCP secret (`mcp secret`, `mcp add`, `mcp import`, the chat's `/mcp add`,
//!   setup's preset secrets) calls [`bump`], which records a new opaque version per keychain
//!   account in `<state>/mcp-secret-versions.json`. The file holds account names and versions,
//!   never a value.
//! - **env_file.** Its bytes are hashed when the stamp is taken.
//!
//! [`server_stamp`] combines both; [`crate::mcp_hub::McpHub::reload`] restarts a server whose
//! stamp changed even when its table did not, and the chat's config watcher also watches
//! [`versions_path`] and each server's env_file ([`crate::mcp_hub::McpHub::watched_files`]).

use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
};

use sha2::{Digest, Sha256};

use crate::mcp_config::{ExpandContext, McpServer, Transport};

const FILE: &str = "mcp-secret-versions.json";

/// `<state>/mcp-secret-versions.json`.
pub fn versions_path(state_dir: &Path) -> PathBuf {
    state_dir.join(FILE)
}

/// Every account's version (empty when the file is missing or unreadable).
pub fn load(state_dir: &Path) -> BTreeMap<String, String> {
    std::fs::read(versions_path(state_dir))
        .ok()
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default()
}

/// Records that the secrets in `accounts` were just written. Best effort: a version that can't be
/// recorded only means a running chat needs `/mcp restart` to pick the new value up, so the error
/// is returned for the caller to mention, never to fail the write.
pub fn bump<S: AsRef<str>>(state_dir: &Path, accounts: &[S]) -> std::io::Result<()> {
    if accounts.is_empty() {
        return Ok(());
    }
    static SEQ: AtomicU64 = AtomicU64::new(0);
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or_default();
    let version = format!(
        "{nanos}-{}-{}",
        std::process::id(),
        SEQ.fetch_add(1, Ordering::Relaxed)
    );
    let mut versions = load(state_dir);
    for account in accounts {
        versions.insert(account.as_ref().to_owned(), version.clone());
    }
    std::fs::create_dir_all(state_dir)?;
    let path = versions_path(state_dir);
    let tmp = state_dir.join(format!(".{FILE}.{version}.tmp"));
    let bytes = serde_json::to_vec_pretty(&versions).map_err(std::io::Error::other)?;
    std::fs::write(&tmp, bytes)?;
    std::fs::rename(&tmp, &path).inspect_err(|_| {
        let _ = std::fs::remove_file(&tmp);
    })
}

/// The env_file `server` reads, as [`McpServer::resolve`] finds it (`None`: none, or its path
/// can't be expanded here).
pub fn env_file_path(server: &McpServer, ctx: &ExpandContext<'_>) -> Option<PathBuf> {
    let Transport::Stdio(s) = &server.transport else {
        return None;
    };
    let file = PathBuf::from(ctx.expand(s.env_file.as_deref()?).ok()?);
    Some(match server.base_dir(ctx) {
        Some(base) if file.is_relative() => base.join(file),
        _ => file,
    })
}

/// A hash over the versions of `server`'s keychain secrets and its env_file's contents. Equal
/// stamps: nothing outside the table changed since the last one.
pub fn server_stamp(
    server: &McpServer,
    versions: &BTreeMap<String, String>,
    ctx: &ExpandContext<'_>,
) -> String {
    let mut h = Sha256::new();
    h.update(b"amp-harness mcp stamp v1\n");
    for account in server.keychain_accounts() {
        h.update(account.as_bytes());
        h.update(b"=");
        h.update(
            versions
                .get(&account)
                .map_or("-", String::as_str)
                .as_bytes(),
        );
        h.update(b"\n");
    }
    if let Some(path) = env_file_path(server, ctx) {
        h.update(b"env_file:");
        match std::fs::read(&path) {
            Ok(bytes) => h.update(Sha256::digest(&bytes)),
            Err(_) => h.update(b"unreadable"),
        }
    }
    h.finalize().iter().map(|b| format!("{b:02x}")).collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mcp_config::McpConfig;
    use amp_mcp::ConfigPaths;

    fn scratch(name: &str) -> PathBuf {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-scratch/amp-core-mcp-stamp")
            .join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn a_rotated_secret_or_a_changed_env_file_changes_the_stamp() {
        let dir = scratch("stamp");
        let state = dir.join("state");
        let file = dir.join("config.toml");
        let table: toml::Table = toml::from_str(
            "[s]\ncommand = \"srv\"\nenv_file = \"srv.env\"\nsecret_env = [\"API_KEY\"]\n\
             [h]\ntype = \"http\"\nurl = \"https://h.test/mcp\"\n",
        )
        .unwrap();
        let config = McpConfig::from_tables(Some((&table, &file)), None);
        let (s, h) = (&config.servers["s"], &config.servers["h"]);
        let env = |_: &str| None;
        let paths = ConfigPaths::default();
        let ctx = ExpandContext::from_paths(&paths, &env, None);
        assert_eq!(env_file_path(s, &ctx), Some(dir.join("srv.env")));
        std::fs::write(dir.join("srv.env"), "A=1\n").unwrap();

        let stamp = |server| server_stamp(server, &load(&state), &ctx);
        let (s0, h0) = (stamp(s), stamp(h));
        assert_eq!(stamp(s), s0, "stable");

        // `mcp secret s` rotates API_KEY: only s changes.
        bump(&state, &s.keychain_accounts()).unwrap();
        let s1 = stamp(s);
        assert_ne!(s1, s0);
        assert_eq!(stamp(h), h0);
        // Another rotation is another version.
        bump(&state, &s.keychain_accounts()).unwrap();
        assert_ne!(stamp(s), s1);
        let s2 = stamp(s);

        // The env_file's contents count; its mtime alone does not.
        std::fs::write(dir.join("srv.env"), "A=1\n").unwrap();
        assert_eq!(stamp(s), s2);
        std::fs::write(dir.join("srv.env"), "A=2\n").unwrap();
        assert_ne!(stamp(s), s2);

        // The file names accounts and versions only.
        let text = std::fs::read_to_string(versions_path(&state)).unwrap();
        assert!(text.contains("mcp:s:env:API_KEY"), "{text}");
        let _ = std::fs::remove_dir_all(&dir);
    }
}
