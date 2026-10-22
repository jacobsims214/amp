//! Where OAuth logins are kept (D3): the OS keychain, or a 0600 file when there is no keychain.
//!
//! One entry per MCP server URL ([`credential_key`]), so local, cluster and hosted logins coexist.
//! An entry is rmcp's [`StoredCredentials`] as JSON: the client id from dynamic registration, the
//! token response (access + refresh token), the granted scopes and the issuer.
//!
//! | backend  | where                                                                          |
//! |----------|--------------------------------------------------------------------------------|
//! | keychain | service `amp-harness`, account = server URL (macOS Keychain, Windows Credential Manager, Secret Service) |
//! | file     | `$XDG_CONFIG_HOME/amp-harness/credentials.json`, else `~/.config/amp-harness/credentials.json`, mode 0600 |
//!
//! `AMP_CREDENTIAL_STORE=keychain|file|auto` picks one; `auto` (the default) uses the keychain when
//! it answers and the file otherwise. `AMP_TOKEN` never comes through here: it stays in memory.
//!
//! Nothing in this module logs or formats a token: [`StoredCredentials`]'s `Debug` is redacted, and
//! errors carry only backend messages.

use std::{
    collections::{BTreeMap, HashMap},
    path::{Path, PathBuf},
    sync::{Arc, LazyLock, Mutex as StdMutex},
};

use rmcp::transport::auth::{
    AuthError, CredentialRefreshGuard, CredentialStore, StoredCredentials,
};
use serde::{Deserialize, Serialize};

use crate::McpError;

/// The keychain service every real login is stored under.
pub const KEYCHAIN_SERVICE: &str = "amp-harness";
/// Test hook: store everything under this keychain service instead of [`KEYCHAIN_SERVICE`], so a
/// test can use the real keychain without touching the user's entries.
pub const ENV_KEYCHAIN_SERVICE: &str = "AMP_HARNESS_KEYCHAIN_SERVICE";

/// The keychain service in use: `AMP_HARNESS_KEYCHAIN_SERVICE` (tests), else [`KEYCHAIN_SERVICE`].
pub fn keychain_service() -> String {
    std::env::var(ENV_KEYCHAIN_SERVICE)
        .ok()
        .map(|s| s.trim().to_owned())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| KEYCHAIN_SERVICE.to_owned())
}

/// Picks the backend: `keychain`, `file` or `auto` (default).
pub const ENV_CREDENTIAL_STORE: &str = "AMP_CREDENTIAL_STORE";
/// The file fallback's name inside the `amp-harness` config dir.
pub const CREDENTIALS_FILE_NAME: &str = "credentials.json";

/// The key a server's login is stored under: the URL, normalised (lowercase scheme and host, no
/// default port, no query or fragment, no trailing slash). Falls back to the trimmed input.
pub fn credential_key(url: &str) -> String {
    match reqwest::Url::parse(url.trim()) {
        Ok(mut parsed) => {
            parsed.set_query(None);
            parsed.set_fragment(None);
            parsed.as_str().trim_end_matches('/').to_owned()
        }
        Err(_) => url.trim().trim_end_matches('/').to_owned(),
    }
}

/// Where logins are kept.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum CredentialBackend {
    /// The OS keychain under `service` (accounts are server URLs).
    Keychain { service: String },
    /// A JSON file holding every server's entry, mode 0600.
    File { path: PathBuf },
}

impl CredentialBackend {
    pub fn keychain(service: impl Into<String>) -> Self {
        Self::Keychain {
            service: service.into(),
        }
    }

    pub fn file(path: impl Into<PathBuf>) -> Self {
        Self::File { path: path.into() }
    }

    /// `$XDG_CONFIG_HOME/amp-harness/credentials.json`, else `~/.config/amp-harness/credentials.json`.
    pub fn default_file_path(env: &dyn Fn(&str) -> Option<String>) -> Option<PathBuf> {
        crate::settings::default_config_path(env).map(|p| p.with_file_name(CREDENTIALS_FILE_NAME))
    }

    /// Resolves against the real environment and keychain.
    pub fn resolve() -> Result<Self, McpError> {
        let service = keychain_service();
        Self::resolve_with_service(&|key| std::env::var(key).ok(), &service, &|| {
            keychain_available(&service)
        })
    }

    /// Resolves from `AMP_CREDENTIAL_STORE` (default `auto`). `keychain_check` says whether the
    /// keychain works; `auto` falls back to the file when it doesn't.
    pub fn resolve_with(
        env: &dyn Fn(&str) -> Option<String>,
        keychain_check: &dyn Fn() -> Result<(), String>,
    ) -> Result<Self, McpError> {
        Self::resolve_with_service(env, KEYCHAIN_SERVICE, keychain_check)
    }

    /// [`Self::resolve_with`] with the keychain service to use.
    pub fn resolve_with_service(
        env: &dyn Fn(&str) -> Option<String>,
        service: &str,
        keychain_check: &dyn Fn() -> Result<(), String>,
    ) -> Result<Self, McpError> {
        let file = || {
            Self::default_file_path(env).map(Self::file).ok_or_else(|| {
                McpError::Config(
                    "no HOME or XDG_CONFIG_HOME: nowhere to keep the credentials file".to_owned(),
                )
            })
        };
        let choice = env(ENV_CREDENTIAL_STORE)
            .map(|v| v.trim().to_ascii_lowercase())
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| "auto".to_owned());
        match choice.as_str() {
            "keychain" => Ok(Self::keychain(service)),
            "file" => file(),
            "auto" => match keychain_check() {
                Ok(()) => Ok(Self::keychain(service)),
                Err(reason) => {
                    tracing::debug!(%reason, "no usable keychain; using the credentials file");
                    file()
                }
            },
            other => Err(McpError::Config(format!(
                "{ENV_CREDENTIAL_STORE}={other:?}: expected keychain, file or auto"
            ))),
        }
    }

    /// For `login`/`doctor` output. Never contains a secret.
    pub fn describe(&self) -> String {
        match self {
            Self::Keychain { service } => format!("{} (service {service})", keychain_name()),
            Self::File { path } => format!("file {}", path.display()),
        }
    }

    pub fn is_keychain(&self) -> bool {
        matches!(self, Self::Keychain { .. })
    }

    /// The store for one server's login.
    pub fn store_for(&self, url: &str) -> AmpCredentialStore {
        AmpCredentialStore {
            backend: self.clone(),
            key: credential_key(url),
        }
    }
}

fn keychain_name() -> &'static str {
    if cfg!(target_os = "macos") {
        "macOS Keychain"
    } else if cfg!(target_os = "windows") {
        "Windows Credential Manager"
    } else {
        "Secret Service keyring"
    }
}

/// Whether the OS keychain can be used: the platform store initialises and a lookup of a probe
/// entry answers (found or not found). Headless Linux without a Secret Service fails here.
pub fn keychain_available(service: &str) -> Result<(), String> {
    if let Err(e) = keyring::Entry::store_status() {
        return Err(e.to_string());
    }
    let entry = keyring::Entry::new(service, "amp-harness-probe").map_err(|e| e.to_string())?;
    match entry.get_password() {
        Ok(_) | Err(keyring::Error::NoEntry) => Ok(()),
        Err(e) => Err(e.to_string()),
    }
}

/// One server's login in a [`CredentialBackend`]. Implements rmcp's [`CredentialStore`], so the
/// OAuth state machine loads, saves (after login and every refresh) and clears through it.
#[derive(Debug, Clone)]
pub struct AmpCredentialStore {
    backend: CredentialBackend,
    key: String,
}

impl AmpCredentialStore {
    pub fn backend(&self) -> &CredentialBackend {
        &self.backend
    }

    /// The normalised server URL this entry is stored under.
    pub fn key(&self) -> &str {
        &self.key
    }

    pub async fn load_credentials(&self) -> Result<Option<StoredCredentials>, McpError> {
        let this = self.clone();
        blocking(move || this.load_sync()).await
    }

    pub async fn save_credentials(&self, credentials: StoredCredentials) -> Result<(), McpError> {
        let this = self.clone();
        blocking(move || this.save_sync(&credentials)).await
    }

    /// Removes this server's entry. `Ok(true)` if there was one.
    pub async fn clear_credentials(&self) -> Result<bool, McpError> {
        let this = self.clone();
        blocking(move || this.clear_sync()).await
    }

    fn load_sync(&self) -> Result<Option<StoredCredentials>, McpError> {
        match &self.backend {
            CredentialBackend::Keychain { service } => {
                match keychain_entry(service, &self.key)?.get_password() {
                    Ok(json) => parse_entry(&json).map(Some),
                    Err(keyring::Error::NoEntry) => Ok(None),
                    Err(e) => Err(store_error("keychain read", e)),
                }
            }
            CredentialBackend::File { path } => {
                let _lock = file_lock();
                Ok(read_file(path)?.servers.remove(&self.key))
            }
        }
    }

    fn save_sync(&self, credentials: &StoredCredentials) -> Result<(), McpError> {
        match &self.backend {
            CredentialBackend::Keychain { service } => {
                let json = serde_json::to_string(credentials)
                    .map_err(|e| McpError::CredentialStore(format!("encode credentials: {e}")))?;
                keychain_entry(service, &self.key)?
                    .set_password(&json)
                    .map_err(|e| store_error("keychain write", e))
            }
            CredentialBackend::File { path } => {
                let _lock = file_lock();
                let mut file = read_file(path)?;
                file.servers.insert(self.key.clone(), credentials.clone());
                write_file(path, &file)
            }
        }
    }

    fn clear_sync(&self) -> Result<bool, McpError> {
        match &self.backend {
            CredentialBackend::Keychain { service } => {
                match keychain_entry(service, &self.key)?.delete_credential() {
                    Ok(()) => Ok(true),
                    Err(keyring::Error::NoEntry) => Ok(false),
                    Err(e) => Err(store_error("keychain delete", e)),
                }
            }
            CredentialBackend::File { path } => {
                let _lock = file_lock();
                let mut file = read_file(path)?;
                if file.servers.remove(&self.key).is_none() {
                    return Ok(false);
                }
                write_file(path, &file)?;
                Ok(true)
            }
        }
    }
}

#[async_trait::async_trait]
impl CredentialStore for AmpCredentialStore {
    async fn load(&self) -> Result<Option<StoredCredentials>, AuthError> {
        self.load_credentials().await.map_err(auth_store_error)
    }

    async fn save(&self, credentials: StoredCredentials) -> Result<(), AuthError> {
        self.save_credentials(credentials)
            .await
            .map_err(auth_store_error)
    }

    async fn clear(&self) -> Result<(), AuthError> {
        self.clear_credentials()
            .await
            .map(|_| ())
            .map_err(auth_store_error)
    }

    /// Serialises refreshes of one login within this process (the TUI's poller and the main client
    /// share it), so two refreshes never race on a rotating refresh token.
    async fn acquire_refresh_guard(&self) -> Result<Option<CredentialRefreshGuard>, AuthError> {
        let lock = {
            let mut locks = REFRESH_LOCKS.lock().unwrap_or_else(|e| e.into_inner());
            locks
                .entry((self.backend.clone(), self.key.clone()))
                .or_default()
                .clone()
        };
        Ok(Some(CredentialRefreshGuard::new(lock.lock_owned().await)))
    }
}

type RefreshLocks = HashMap<(CredentialBackend, String), Arc<tokio::sync::Mutex<()>>>;
static REFRESH_LOCKS: LazyLock<StdMutex<RefreshLocks>> = LazyLock::new(Default::default);

fn auth_store_error(e: McpError) -> AuthError {
    AuthError::CredentialStoreError(e.to_string())
}

fn store_error(operation: &str, e: keyring::Error) -> McpError {
    McpError::CredentialStore(format!("{operation}: {e}"))
}

fn keychain_entry(service: &str, key: &str) -> Result<keyring::Entry, McpError> {
    keyring::Entry::new(service, key).map_err(|e| store_error("keychain", e))
}

fn parse_entry(json: &str) -> Result<StoredCredentials, McpError> {
    // serde_json errors name a position, never the input, so no token reaches the message.
    serde_json::from_str(json)
        .map_err(|e| McpError::CredentialStore(format!("stored credentials do not parse: {e}")))
}

async fn blocking<T: Send + 'static>(
    f: impl FnOnce() -> Result<T, McpError> + Send + 'static,
) -> Result<T, McpError> {
    tokio::task::spawn_blocking(f)
        .await
        .map_err(|e| McpError::CredentialStore(format!("credential store task: {e}")))?
}

/// The fallback file: every server's entry, keyed by [`credential_key`].
#[derive(Debug, Default, Serialize, Deserialize)]
struct CredentialsFile {
    #[serde(default = "file_version")]
    version: u32,
    #[serde(default)]
    servers: BTreeMap<String, StoredCredentials>,
}

fn file_version() -> u32 {
    1
}

/// Serialises read-modify-write of the credentials file within this process.
fn file_lock() -> std::sync::MutexGuard<'static, ()> {
    static LOCK: StdMutex<()> = StdMutex::new(());
    LOCK.lock().unwrap_or_else(|e| e.into_inner())
}

fn read_file(path: &Path) -> Result<CredentialsFile, McpError> {
    let bytes = match std::fs::read(path) {
        Ok(bytes) => bytes,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Ok(CredentialsFile {
                version: file_version(),
                ..Default::default()
            });
        }
        Err(e) => {
            return Err(McpError::CredentialStore(format!(
                "read {}: {e}",
                path.display()
            )));
        }
    };
    tighten_permissions(path)?;
    serde_json::from_slice(&bytes)
        .map_err(|e| McpError::CredentialStore(format!("{} does not parse: {e}", path.display())))
}

/// Writes atomically: a 0600 temp file in the same directory (0700), then rename.
fn write_file(path: &Path, file: &CredentialsFile) -> Result<(), McpError> {
    let io_error =
        |what: &str, e: std::io::Error| McpError::CredentialStore(format!("{what}: {e}"));
    let dir = path.parent().unwrap_or(Path::new("."));
    create_private_dir(dir).map_err(|e| io_error(&format!("create {}", dir.display()), e))?;
    let json = serde_json::to_vec_pretty(file)
        .map_err(|e| McpError::CredentialStore(format!("encode credentials: {e}")))?;
    let tmp = path.with_extension(format!("json.tmp-{}", std::process::id()));
    let result = (|| {
        use std::io::Write;
        let mut out = open_private(&tmp)?;
        out.write_all(&json)?;
        out.sync_all()?;
        std::fs::rename(&tmp, path)
    })();
    if let Err(e) = result {
        let _ = std::fs::remove_file(&tmp);
        return Err(io_error(&format!("write {}", path.display()), e));
    }
    Ok(())
}

#[cfg(unix)]
fn open_private(path: &Path) -> std::io::Result<std::fs::File> {
    use std::os::unix::fs::OpenOptionsExt;
    std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(path)
}

#[cfg(not(unix))]
fn open_private(path: &Path) -> std::io::Result<std::fs::File> {
    std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .open(path)
}

#[cfg(unix)]
fn create_private_dir(dir: &Path) -> std::io::Result<()> {
    use std::os::unix::fs::DirBuilderExt;
    if dir.is_dir() {
        return Ok(());
    }
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)
}

#[cfg(not(unix))]
fn create_private_dir(dir: &Path) -> std::io::Result<()> {
    std::fs::create_dir_all(dir)
}

/// A credentials file someone loosened (or an old umask) is put back to 0600 before it is used.
#[cfg(unix)]
fn tighten_permissions(path: &Path) -> Result<(), McpError> {
    use std::os::unix::fs::PermissionsExt;
    let meta = std::fs::metadata(path)
        .map_err(|e| McpError::CredentialStore(format!("stat {}: {e}", path.display())))?;
    if meta.permissions().mode() & 0o077 != 0 {
        tracing::warn!(path = %path.display(), "credentials file was readable by others; setting 0600");
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))
            .map_err(|e| McpError::CredentialStore(format!("chmod {}: {e}", path.display())))?;
    }
    Ok(())
}

#[cfg(not(unix))]
fn tighten_permissions(_path: &Path) -> Result<(), McpError> {
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keys_are_normalised_urls() {
        assert_eq!(
            credential_key("HTTPS://AMP.local:443/mcp/"),
            "https://amp.local/mcp"
        );
        assert_eq!(
            credential_key(" https://amp.local:8443/mcp?x=1#f "),
            "https://amp.local:8443/mcp"
        );
        assert_eq!(
            credential_key("http://localhost:8000/mcp"),
            "http://localhost:8000/mcp"
        );
        assert_ne!(
            credential_key("https://amp.local:8443/mcp"),
            credential_key("https://amp.simstech.cloud/mcp")
        );
    }

    #[test]
    fn backend_resolution() {
        let env = |pairs: &'static [(&'static str, &'static str)]| {
            move |key: &str| {
                pairs
                    .iter()
                    .find(|(k, _)| *k == key)
                    .map(|(_, v)| (*v).to_owned())
            }
        };
        let ok = || Ok(());
        let none = || Err("no secret service".to_owned());

        let home = env(&[("HOME", "/home/u")]);
        assert_eq!(
            CredentialBackend::resolve_with(&home, &ok).unwrap(),
            CredentialBackend::keychain(KEYCHAIN_SERVICE)
        );
        assert_eq!(
            CredentialBackend::resolve_with(&home, &none).unwrap(),
            CredentialBackend::file("/home/u/.config/amp-harness/credentials.json")
        );
        let xdg_file = env(&[("XDG_CONFIG_HOME", "/x"), (ENV_CREDENTIAL_STORE, "file")]);
        assert_eq!(
            CredentialBackend::resolve_with(&xdg_file, &ok).unwrap(),
            CredentialBackend::file("/x/amp-harness/credentials.json")
        );
        let forced = env(&[("HOME", "/home/u"), (ENV_CREDENTIAL_STORE, "KEYCHAIN")]);
        assert!(
            CredentialBackend::resolve_with(&forced, &none)
                .unwrap()
                .is_keychain()
        );
        let bad = env(&[(ENV_CREDENTIAL_STORE, "vault")]);
        assert!(CredentialBackend::resolve_with(&bad, &ok).is_err());
        let nowhere = env(&[(ENV_CREDENTIAL_STORE, "file")]);
        assert!(CredentialBackend::resolve_with(&nowhere, &ok).is_err());
    }
}
