//! When remote control may start (protocol.md sections 8.1 and 12.3, R-M2, R-M14).
//!
//! **Off switches** (any one wins; nothing here can turn remote control on):
//! - `AMP_HARNESS_DISABLE_REMOTE=1` (or `true`/`yes`/`on`);
//! - a managed config file with `[remote] enabled = false`: the system file
//!   (`/Library/Application Support/amp-harness/managed.toml` on macOS,
//!   `/etc/amp-harness/managed.toml` elsewhere, `%ProgramData%\amp-harness\managed.toml` on
//!   Windows) and, in addition (never instead), `AMP_HARNESS_MANAGED_CONFIG`;
//! - the person's config.toml `[remote] enabled = false`;
//! - the repo's `.amp/harness/config.toml` `[remote] enabled = false` (a repo can only turn it
//!   off: `true` there is ignored).
//!
//! **The server**: the relay is the AMP server the harness is logged in to (the origin of its MCP
//! URL), never a URL from a repo or a link. It must be `https`, except plain `http` to a loopback
//! host with the dev override `AMP_HARNESS_REMOTE_INSECURE_LOOPBACK=1`, which prints a warning.
//! The login must be a person's OAuth login: an `amp_` API token (`AMP_TOKEN`) is refused, and no
//! login at all means an auth-off AMP or a logged-out harness, refused too.

use std::path::{Path, PathBuf};

pub const ENV_DISABLE: &str = "AMP_HARNESS_DISABLE_REMOTE";
pub const ENV_MANAGED_CONFIG: &str = "AMP_HARNESS_MANAGED_CONFIG";
pub const ENV_INSECURE_LOOPBACK: &str = "AMP_HARNESS_REMOTE_INSECURE_LOOPBACK";

fn truthy(v: &str) -> bool {
    matches!(
        v.trim().to_ascii_lowercase().as_str(),
        "1" | "true" | "yes" | "on"
    )
}

/// The system-wide managed config file of this OS.
pub fn system_managed_path() -> PathBuf {
    if cfg!(target_os = "macos") {
        PathBuf::from("/Library/Application Support/amp-harness/managed.toml")
    } else if cfg!(target_os = "windows") {
        let base = std::env::var_os("ProgramData").unwrap_or_else(|| "C:\\ProgramData".into());
        PathBuf::from(base).join("amp-harness").join("managed.toml")
    } else {
        PathBuf::from("/etc/amp-harness/managed.toml")
    }
}

/// Whether `[remote] enabled = false` is in the TOML file at `path` (missing file: no).
/// A file that exists but doesn't parse counts as off: a broken managed file must not open
/// remote control.
pub fn file_disables(path: &Path) -> Result<bool, String> {
    let text = match std::fs::read_to_string(path) {
        Ok(t) => t,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(e) => return Err(format!("read {}: {e}", path.display())),
    };
    let table: toml::Table =
        toml::from_str(&text).map_err(|e| format!("parse {}: {e}", path.display()))?;
    Ok(table
        .get("remote")
        .and_then(|r| r.get("enabled"))
        .and_then(toml::Value::as_bool)
        == Some(false))
}

/// Where the local switches are read from.
#[derive(Debug, Clone, Default)]
pub struct LocalSwitches {
    /// Managed files (the system one plus `AMP_HARNESS_MANAGED_CONFIG`).
    pub managed: Vec<PathBuf>,
    /// The person's config.toml.
    pub user: Option<PathBuf>,
    /// The repo's `.amp/harness/config.toml`.
    pub repo: Option<PathBuf>,
}

impl LocalSwitches {
    /// The managed files for `env`, plus the given user and repo files.
    pub fn new(
        env: &dyn Fn(&str) -> Option<String>,
        user: Option<PathBuf>,
        repo: Option<PathBuf>,
    ) -> Self {
        let mut managed = vec![system_managed_path()];
        if let Some(p) = env(ENV_MANAGED_CONFIG).filter(|p| !p.trim().is_empty()) {
            managed.push(PathBuf::from(p));
        }
        Self {
            managed,
            user,
            repo,
        }
    }

    /// `Err(why)` when a switch turns remote control off. Re-read on every enable and reconnect.
    pub fn check(&self, env: &dyn Fn(&str) -> Option<String>) -> Result<(), String> {
        if env(ENV_DISABLE).is_some_and(|v| truthy(&v)) {
            return Err(format!(
                "remote control is turned off on this computer ({ENV_DISABLE})"
            ));
        }
        for path in &self.managed {
            match file_disables(path) {
                Ok(false) => {}
                Ok(true) => {
                    return Err(format!(
                        "remote control is turned off by your organization's managed settings ({})",
                        path.display()
                    ));
                }
                Err(e) => {
                    return Err(format!(
                        "remote control is off: the managed settings can't be read ({e})"
                    ));
                }
            }
        }
        for (path, whose) in [
            (&self.user, "your config"),
            (&self.repo, "this repo's config"),
        ] {
            if let Some(path) = path
                && file_disables(path).unwrap_or(false)
            {
                return Err(format!(
                    "remote control is turned off in {whose} ({}: [remote] enabled = false)",
                    path.display()
                ));
            }
        }
        Ok(())
    }
}

/// How the harness is logged in to AMP.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LoginKind {
    /// A person's OAuth login (stored by `amp-harness login`).
    OAuth,
    /// `AMP_TOKEN` (an `amp_` API token).
    StaticToken,
    /// No credentials: an auth-off AMP, or not logged in.
    None,
}

fn is_loopback(url: &reqwest::Url) -> bool {
    let Some(host) = url.host_str() else {
        return false;
    };
    host == "localhost"
        || host
            .trim_start_matches('[')
            .trim_end_matches(']')
            .parse::<std::net::IpAddr>()
            .is_ok_and(|ip| ip.is_loopback())
}

/// What [`check_server`] allows: the relay's base (the MCP URL's origin) and a warning to show
/// when the dev override is in use.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ServerOk {
    pub api_base: String,
    pub warning: Option<String>,
}

/// Checks the AMP server and the login (R-M14, T-H-1, T-H-8).
pub fn check_server(
    mcp_url: &str,
    login: LoginKind,
    env: &dyn Fn(&str) -> Option<String>,
) -> Result<ServerOk, String> {
    match login {
        LoginKind::OAuth => {}
        LoginKind::StaticToken => {
            return Err(
                "remote control needs your own AMP login: an API token (AMP_TOKEN, amp_…) can't \
                 enable it. Unset AMP_TOKEN and run `amp-harness login`."
                    .to_owned(),
            );
        }
        LoginKind::None => {
            return Err(
                "remote control needs your AMP login, and isn't available on an AMP server with \
                 sign-in turned off. Run `amp-harness login`."
                    .to_owned(),
            );
        }
    }
    let url = reqwest::Url::parse(mcp_url.trim())
        .map_err(|_| "the AMP server address is not a URL".to_owned())?;
    let origin = url.origin().ascii_serialization();
    match url.scheme() {
        "https" => Ok(ServerOk {
            api_base: origin,
            warning: None,
        }),
        "http" if is_loopback(&url) => {
            if env(ENV_INSECURE_LOOPBACK).is_some_and(|v| truthy(&v)) {
                Ok(ServerOk {
                    api_base: origin.clone(),
                    warning: Some(format!(
                        "WARNING: remote control over plain http to {origin} \
                         ({ENV_INSECURE_LOOPBACK}=1). For local testing only: never use this \
                         with a real AMP."
                    )),
                })
            } else {
                Err(format!(
                    "remote control needs https; {origin} is plain http (for local testing on \
                     this computer only, set {ENV_INSECURE_LOOPBACK}=1)"
                ))
            }
        }
        "http" => Err(format!(
            "remote control needs https; refusing plain http to {origin}"
        )),
        other => Err(format!("remote control can't use a {other}: AMP address")),
    }
}

/// An OAuth access token must not be an `amp_` API token (the relay refuses it too).
pub fn check_token(token: &str) -> Result<(), String> {
    if token.trim().starts_with("amp_") {
        return Err("remote control can't use an amp_ API token; run `amp-harness login`".into());
    }
    if token.trim().is_empty() {
        return Err("no AMP login; run `amp-harness login`".into());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn env_of(pairs: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
        let map: std::collections::HashMap<String, String> = pairs
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect();
        move |k| map.get(k).cloned()
    }

    #[test]
    fn server_and_login_checks() {
        let none = env_of(&[]);
        assert!(check_server("https://amp.example/mcp", LoginKind::OAuth, &none).is_ok());
        assert!(check_server("https://amp.example/mcp", LoginKind::StaticToken, &none).is_err());
        assert!(check_server("https://amp.example/mcp", LoginKind::None, &none).is_err());
        assert!(check_server("http://amp.example/mcp", LoginKind::OAuth, &none).is_err());
        assert!(check_server("http://127.0.0.1:9/mcp", LoginKind::OAuth, &none).is_err());
        let dev = env_of(&[(ENV_INSECURE_LOOPBACK, "1")]);
        let ok = check_server("http://127.0.0.1:9/mcp", LoginKind::OAuth, &dev).unwrap();
        assert_eq!(ok.api_base, "http://127.0.0.1:9");
        assert!(ok.warning.unwrap().contains("WARNING"));
        // The override never allows a non-loopback host.
        assert!(check_server("http://10.0.0.5/mcp", LoginKind::OAuth, &dev).is_err());
        assert!(check_token("amp_abc").is_err());
        assert!(check_token("eyJhbGciOi").is_ok());
    }

    #[test]
    fn switches_only_turn_it_off() {
        let dir = std::env::temp_dir().join(format!("amp-rc-policy-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let on = dir.join("on.toml");
        let off = dir.join("off.toml");
        std::fs::write(&on, "[remote]\nenabled = true\n").unwrap();
        std::fs::write(&off, "[remote]\nenabled = false\n").unwrap();
        let none = env_of(&[]);
        let s = |managed: Vec<PathBuf>, user: Option<&Path>, repo: Option<&Path>| LocalSwitches {
            managed,
            user: user.map(Path::to_path_buf),
            repo: repo.map(Path::to_path_buf),
        };
        assert!(s(vec![], Some(&on), Some(&on)).check(&none).is_ok());
        assert!(s(vec![], None, Some(&off)).check(&none).is_err());
        assert!(s(vec![], Some(&off), Some(&on)).check(&none).is_err());
        assert!(s(vec![off.clone()], Some(&on), None).check(&none).is_err());
        let disabled = env_of(&[(ENV_DISABLE, "1")]);
        assert!(s(vec![], None, None).check(&disabled).is_err());
        let _ = std::fs::remove_dir_all(dir);
    }
}
