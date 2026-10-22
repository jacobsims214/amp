//! Where to connect and how (D4): server URL, extra CA, protocol mode and the static token.
//!
//! Precedence, highest first:
//!
//! | setting  | flag          | env                | `config.toml` `[server]` | default                          |
//! |----------|---------------|--------------------|--------------------------|----------------------------------|
//! | URL      | `--url`       | `AMP_MCP_URL`      | `url`                    | `https://amp.simstech.cloud/mcp` |
//! | CA file  | `--ca-file`   | `AMP_CA_FILE`      | `ca_file`                | none (system roots only)         |
//! | protocol | `--protocol`  | `AMP_MCP_PROTOCOL` | `protocol`               | `auto`                           |
//!
//! The config file is `$XDG_CONFIG_HOME/amp-harness/config.toml`, else
//! `~/.config/amp-harness/config.toml`. A relative `ca_file` in it is relative to the file's directory.
//! `.amp.json` is a project marker only; its `amp_api` field is never read for the URL.
//!
//! The static token comes only from the environment (`AMP_TOKEN`, or the variable named by
//! `--token-env`). It is kept in memory, never written to disk, and its `Debug` is redacted.

use std::{
    fmt,
    path::{Path, PathBuf},
};

use serde::Deserialize;

use crate::{McpError, ProtocolMode};

/// Hosted AMP; used when nothing else names a server.
pub const DEFAULT_MCP_URL: &str = "https://amp.simstech.cloud/mcp";
pub const ENV_MCP_URL: &str = "AMP_MCP_URL";
pub const ENV_CA_FILE: &str = "AMP_CA_FILE";
pub const ENV_PROTOCOL: &str = "AMP_MCP_PROTOCOL";
pub const ENV_TOKEN: &str = "AMP_TOKEN";

/// Values from the command line. `None` = not given.
#[derive(Debug, Clone, Default)]
pub struct Overrides {
    /// `--url`
    pub url: Option<String>,
    /// `--ca-file`
    pub ca_file: Option<PathBuf>,
    /// `--protocol`
    pub protocol: Option<ProtocolMode>,
    /// `--config`: use this file instead of the default path (it must exist).
    pub config_file: Option<PathBuf>,
    /// `--token-env`: read the static token from this variable instead of `AMP_TOKEN`.
    pub token_env: Option<String>,
}

/// Where a resolved setting came from (for `doctor`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Source {
    Flag,
    Env(String),
    ConfigFile(PathBuf),
    Default,
}

impl fmt::Display for Source {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Flag => f.write_str("flag"),
            Self::Env(var) => write!(f, "env {var}"),
            Self::ConfigFile(path) => write!(f, "config {}", path.display()),
            Self::Default => f.write_str("default"),
        }
    }
}

/// A static AMP API token (`amp_…`). Never printed, serialized or written to disk.
#[derive(Clone, PartialEq, Eq)]
pub struct StaticToken(String);

impl StaticToken {
    /// Wraps a token, dropping a `Bearer ` prefix if the user pasted one. `None` if blank.
    pub fn new(raw: impl AsRef<str>) -> Option<Self> {
        let raw = raw.as_ref().trim();
        let raw = raw
            .strip_prefix("Bearer ")
            .or_else(|| raw.strip_prefix("bearer "))
            .unwrap_or(raw)
            .trim();
        (!raw.is_empty()).then(|| Self(raw.to_owned()))
    }

    /// The secret itself, for the `Authorization` header only.
    pub fn expose(&self) -> &str {
        &self.0
    }
}

impl fmt::Debug for StaticToken {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("StaticToken(<redacted>)")
    }
}

/// How the client authenticates. M1-3 adds OAuth with a keychain credential store.
#[derive(Debug, Clone, Default)]
pub enum Auth {
    /// No `Authorization` header (compose: auth off).
    #[default]
    None,
    /// `Authorization: Bearer <token>`; `env` names the variable it came from.
    Static { token: StaticToken, env: String },
}

impl Auth {
    /// A one-word description for `doctor`; never contains the secret.
    pub fn describe(&self) -> String {
        match self {
            Self::None => "none".to_owned(),
            Self::Static { env, .. } => format!("static token (env {env})"),
        }
    }
}

/// `[server]` in `config.toml`. Unknown keys and tables are ignored: the file also holds settings
/// for other parts of the harness.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct FileConfig {
    #[serde(default)]
    pub server: ServerSection,
}

#[derive(Debug, Clone, Default, Deserialize)]
pub struct ServerSection {
    pub url: Option<String>,
    pub ca_file: Option<PathBuf>,
    pub protocol: Option<String>,
}

impl FileConfig {
    /// Reads `path`. `Ok(None)` if it does not exist; a file that exists but does not parse is an
    /// error rather than silently ignored.
    pub fn load(path: &Path) -> Result<Option<Self>, McpError> {
        let text = match std::fs::read_to_string(path) {
            Ok(text) => text,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(e) => {
                return Err(McpError::Config(format!("read {}: {e}", path.display())));
            }
        };
        toml::from_str(&text)
            .map(Some)
            .map_err(|e| McpError::Config(format!("parse {}: {e}", path.display())))
    }
}

/// The default config file path, from `XDG_CONFIG_HOME` or `HOME`.
pub fn default_config_path(env: &dyn Fn(&str) -> Option<String>) -> Option<PathBuf> {
    let base = non_empty(env("XDG_CONFIG_HOME"))
        .map(PathBuf::from)
        .or_else(|| non_empty(env("HOME")).map(|home| Path::new(&home).join(".config")))?;
    Some(base.join("amp-harness").join("config.toml"))
}

/// The settings a connection needs, resolved from flags, env, config file and defaults.
#[derive(Debug, Clone)]
pub struct Settings {
    pub url: String,
    pub url_source: Source,
    pub ca_file: Option<PathBuf>,
    pub ca_file_source: Option<Source>,
    pub protocol: ProtocolMode,
    pub protocol_source: Source,
    pub auth: Auth,
    /// The config file that was read, if one existed.
    pub config_file: Option<PathBuf>,
}

impl Settings {
    /// Resolves against the real process environment.
    pub fn resolve(overrides: &Overrides) -> Result<Self, McpError> {
        Self::resolve_with_env(overrides, &|key| std::env::var(key).ok())
    }

    /// Resolves with an explicit environment lookup (tests, embedding).
    pub fn resolve_with_env(
        overrides: &Overrides,
        env: &dyn Fn(&str) -> Option<String>,
    ) -> Result<Self, McpError> {
        let (file, config_file) = match &overrides.config_file {
            Some(path) => match FileConfig::load(path)? {
                Some(file) => (file, Some(path.clone())),
                None => {
                    return Err(McpError::Config(format!(
                        "config file {} does not exist",
                        path.display()
                    )));
                }
            },
            None => match default_config_path(env) {
                Some(path) => match FileConfig::load(&path)? {
                    Some(file) => (file, Some(path)),
                    None => (FileConfig::default(), None),
                },
                None => (FileConfig::default(), None),
            },
        };
        let file_source = || Source::ConfigFile(config_file.clone().unwrap_or_default());

        // URL: --url > AMP_MCP_URL > config > default.
        let (url, url_source) = if let Some(url) = non_empty(overrides.url.clone()) {
            (url, Source::Flag)
        } else if let Some(url) = non_empty(env(ENV_MCP_URL)) {
            (url, Source::Env(ENV_MCP_URL.to_owned()))
        } else if let Some(url) = non_empty(file.server.url.clone()) {
            (url, file_source())
        } else {
            (DEFAULT_MCP_URL.to_owned(), Source::Default)
        };
        let url = validate_url(&url, &url_source)?;

        // CA file: --ca-file > AMP_CA_FILE > config (relative to the config dir) > none.
        let (ca_file, ca_file_source) = if let Some(path) = overrides.ca_file.clone() {
            (Some(path), Some(Source::Flag))
        } else if let Some(path) = non_empty(env(ENV_CA_FILE)) {
            (
                Some(PathBuf::from(path)),
                Some(Source::Env(ENV_CA_FILE.to_owned())),
            )
        } else if let Some(path) = file.server.ca_file.clone() {
            let path = match (&config_file, path.is_relative()) {
                (Some(cfg), true) => cfg.parent().unwrap_or(Path::new(".")).join(path),
                _ => path,
            };
            (Some(path), Some(file_source()))
        } else {
            (None, None)
        };

        // Protocol: --protocol > AMP_MCP_PROTOCOL > config > auto.
        let (protocol, protocol_source) = if let Some(mode) = overrides.protocol {
            (mode, Source::Flag)
        } else if let Some(raw) = non_empty(env(ENV_PROTOCOL)) {
            let mode = raw
                .parse()
                .map_err(|e| McpError::Config(format!("{ENV_PROTOCOL}: {e}")))?;
            (mode, Source::Env(ENV_PROTOCOL.to_owned()))
        } else if let Some(raw) = non_empty(file.server.protocol.clone()) {
            let mode = raw
                .parse()
                .map_err(|e| McpError::Config(format!("config protocol: {e}")))?;
            (mode, file_source())
        } else {
            (ProtocolMode::Auto, Source::Default)
        };

        // Static token: only ever from the environment (D3).
        let token_env = non_empty(overrides.token_env.clone()).unwrap_or(ENV_TOKEN.to_owned());
        let auth = match env(&token_env).and_then(StaticToken::new) {
            Some(token) => Auth::Static {
                token,
                env: token_env,
            },
            None => Auth::None,
        };

        Ok(Self {
            url,
            url_source,
            ca_file,
            ca_file_source,
            protocol,
            protocol_source,
            auth,
            config_file,
        })
    }
}

fn non_empty(value: Option<String>) -> Option<String> {
    value.map(|v| v.trim().to_owned()).filter(|v| !v.is_empty())
}

fn validate_url(raw: &str, source: &Source) -> Result<String, McpError> {
    let parsed = reqwest::Url::parse(raw)
        .map_err(|e| McpError::Config(format!("MCP URL {raw:?} from {source}: {e}")))?;
    if !matches!(parsed.scheme(), "http" | "https") || parsed.host_str().is_none() {
        return Err(McpError::Config(format!(
            "MCP URL {raw:?} from {source}: must be an http(s) URL with a host"
        )));
    }
    Ok(raw.to_owned())
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;

    use super::*;

    fn env_of(pairs: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> + use<> {
        let map: HashMap<String, String> = pairs
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect();
        move |key| map.get(key).cloned()
    }

    /// A per-test directory under cargo's target dir (never /tmp: D13).
    fn scratch(name: &str) -> PathBuf {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-scratch/settings")
            .join(name);
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn write_config(dir: &Path, body: &str) -> PathBuf {
        let path = dir.join("amp-harness").join("config.toml");
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(&path, body).unwrap();
        path
    }

    #[test]
    fn default_when_nothing_is_set() {
        let s = Settings::resolve_with_env(&Overrides::default(), &env_of(&[])).unwrap();
        assert_eq!(s.url, DEFAULT_MCP_URL);
        assert_eq!(s.url_source, Source::Default);
        assert_eq!(s.ca_file, None);
        assert_eq!(s.protocol, ProtocolMode::Auto);
        assert!(matches!(s.auth, Auth::None));
        assert_eq!(s.config_file, None);
    }

    #[test]
    fn url_precedence_flag_env_config_default() {
        let dir = scratch("url-precedence");
        write_config(&dir, "[server]\nurl = \"http://from-config:1/mcp\"\n");
        let xdg = dir.to_str().unwrap();

        // config beats default
        let s =
            Settings::resolve_with_env(&Overrides::default(), &env_of(&[("XDG_CONFIG_HOME", xdg)]))
                .unwrap();
        assert_eq!(s.url, "http://from-config:1/mcp");
        assert!(matches!(s.url_source, Source::ConfigFile(_)));

        // env beats config
        let s = Settings::resolve_with_env(
            &Overrides::default(),
            &env_of(&[
                ("XDG_CONFIG_HOME", xdg),
                (ENV_MCP_URL, "http://from-env:2/mcp"),
            ]),
        )
        .unwrap();
        assert_eq!(s.url, "http://from-env:2/mcp");
        assert_eq!(s.url_source, Source::Env(ENV_MCP_URL.into()));

        // flag beats env
        let s = Settings::resolve_with_env(
            &Overrides {
                url: Some("http://from-flag:3/mcp".into()),
                ..Default::default()
            },
            &env_of(&[
                ("XDG_CONFIG_HOME", xdg),
                (ENV_MCP_URL, "http://from-env:2/mcp"),
            ]),
        )
        .unwrap();
        assert_eq!(s.url, "http://from-flag:3/mcp");
        assert_eq!(s.url_source, Source::Flag);

        // blank values count as unset
        let s = Settings::resolve_with_env(
            &Overrides {
                url: Some("  ".into()),
                ..Default::default()
            },
            &env_of(&[("XDG_CONFIG_HOME", xdg), (ENV_MCP_URL, "")]),
        )
        .unwrap();
        assert_eq!(s.url, "http://from-config:1/mcp");
    }

    #[test]
    fn config_under_home_dot_config() {
        let dir = scratch("home");
        write_config(
            &dir.join(".config"),
            "[server]\nurl = \"https://amp.local:8443/mcp\"\n",
        );
        let s = Settings::resolve_with_env(
            &Overrides::default(),
            &env_of(&[("HOME", dir.to_str().unwrap())]),
        )
        .unwrap();
        assert_eq!(s.url, "https://amp.local:8443/mcp");
    }

    #[test]
    fn ca_file_precedence_and_relative_paths() {
        let dir = scratch("ca");
        let cfg = write_config(&dir, "[server]\nca_file = \"amp-local-ca.crt\"\n");
        let xdg = dir.to_str().unwrap();

        let s =
            Settings::resolve_with_env(&Overrides::default(), &env_of(&[("XDG_CONFIG_HOME", xdg)]))
                .unwrap();
        assert_eq!(
            s.ca_file,
            Some(cfg.parent().unwrap().join("amp-local-ca.crt"))
        );

        let s = Settings::resolve_with_env(
            &Overrides::default(),
            &env_of(&[("XDG_CONFIG_HOME", xdg), (ENV_CA_FILE, "/env/ca.pem")]),
        )
        .unwrap();
        assert_eq!(s.ca_file, Some(PathBuf::from("/env/ca.pem")));
        assert_eq!(s.ca_file_source, Some(Source::Env(ENV_CA_FILE.into())));

        let s = Settings::resolve_with_env(
            &Overrides {
                ca_file: Some("/flag/ca.pem".into()),
                ..Default::default()
            },
            &env_of(&[("XDG_CONFIG_HOME", xdg), (ENV_CA_FILE, "/env/ca.pem")]),
        )
        .unwrap();
        assert_eq!(s.ca_file, Some(PathBuf::from("/flag/ca.pem")));
        assert_eq!(s.ca_file_source, Some(Source::Flag));
    }

    #[test]
    fn protocol_precedence() {
        let dir = scratch("protocol");
        write_config(&dir, "[server]\nprotocol = \"2025-11-25\"\n");
        let xdg = dir.to_str().unwrap();
        let s =
            Settings::resolve_with_env(&Overrides::default(), &env_of(&[("XDG_CONFIG_HOME", xdg)]))
                .unwrap();
        assert_eq!(s.protocol, ProtocolMode::Legacy);
        let s = Settings::resolve_with_env(
            &Overrides::default(),
            &env_of(&[("XDG_CONFIG_HOME", xdg), (ENV_PROTOCOL, "2026-07-28")]),
        )
        .unwrap();
        assert_eq!(s.protocol, ProtocolMode::Modern);
        let s = Settings::resolve_with_env(
            &Overrides {
                protocol: Some(ProtocolMode::Auto),
                ..Default::default()
            },
            &env_of(&[("XDG_CONFIG_HOME", xdg), (ENV_PROTOCOL, "2026-07-28")]),
        )
        .unwrap();
        assert_eq!(s.protocol, ProtocolMode::Auto);
        assert!(
            Settings::resolve_with_env(
                &Overrides::default(),
                &env_of(&[(ENV_PROTOCOL, "1999-01-01")])
            )
            .is_err()
        );
    }

    #[test]
    fn bad_inputs_are_errors() {
        let bad_url = Overrides {
            url: Some("amp.simstech.cloud/mcp".into()),
            ..Default::default()
        };
        assert!(Settings::resolve_with_env(&bad_url, &env_of(&[])).is_err());
        let ftp = Overrides {
            url: Some("ftp://x/mcp".into()),
            ..Default::default()
        };
        assert!(Settings::resolve_with_env(&ftp, &env_of(&[])).is_err());

        let dir = scratch("bad-config");
        let cfg = write_config(&dir, "[server\nurl = ");
        let explicit = Overrides {
            config_file: Some(cfg),
            ..Default::default()
        };
        assert!(Settings::resolve_with_env(&explicit, &env_of(&[])).is_err());
        let missing = Overrides {
            config_file: Some(dir.join("nope.toml")),
            ..Default::default()
        };
        assert!(Settings::resolve_with_env(&missing, &env_of(&[])).is_err());
    }

    #[test]
    fn token_from_env_only_and_redacted() {
        let s = Settings::resolve_with_env(
            &Overrides::default(),
            &env_of(&[(ENV_TOKEN, "Bearer amp_secret_value")]),
        )
        .unwrap();
        let Auth::Static { token, env } = &s.auth else {
            panic!("expected a static token");
        };
        assert_eq!(token.expose(), "amp_secret_value");
        assert_eq!(env, ENV_TOKEN);
        let debug = format!("{s:?}");
        assert!(!debug.contains("amp_secret_value"), "{debug}");
        assert!(!s.auth.describe().contains("amp_secret_value"));

        let s = Settings::resolve_with_env(
            &Overrides {
                token_env: Some("CI_AMP_TOKEN".into()),
                ..Default::default()
            },
            &env_of(&[(ENV_TOKEN, "amp_a"), ("CI_AMP_TOKEN", "amp_b")]),
        )
        .unwrap();
        assert!(matches!(&s.auth, Auth::Static { token, .. } if token.expose() == "amp_b"));
        assert!(StaticToken::new("   ").is_none());
    }
}
