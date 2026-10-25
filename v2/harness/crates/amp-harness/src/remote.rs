//! The chat's remote link (#2373, protocol.md): built from the session the chat runs, so the
//! relay is always the AMP server the harness is logged in to (the MCP URL's origin), never a URL
//! from a repo or a link. `/remote` (#2376) turns it on with [`amp_core::remote::RemoteLink::enable`].
//!
//! - Login: the person's OAuth login from the credential store (keychain or file). `AMP_TOKEN`
//!   (an `amp_` API token) and no login at all (an auth-off AMP) are refused.
//! - Transport: `https`; plain `http` only to loopback with `AMP_HARNESS_REMOTE_INSECURE_LOOPBACK=1`
//!   (warned).
//! - Off switches (re-read on every enable and reconnect): `AMP_HARNESS_DISABLE_REMOTE=1`, the
//!   managed file's, the user config's and the repo config's `[remote] enabled = false`.
//! - State: the harness key in the keychain (or `remote-harness-key.json` 0600 when logins are in
//!   a file), pins in `<state>/remote-devices.json`, the local action log in
//!   `<state>/remote-actions.log` (metadata only).
//! - Redaction: provider keys and secret headers, secret-looking environment values, the login's
//!   tokens.

use std::{path::PathBuf, sync::Arc};

use amp_core::remote::{
    LinkTuning, RemoteDeps, RemoteHost, RemoteLink, RemoteSetup, TokenSource,
    crypto::KeyStore,
    policy::{self, LocalSwitches, LoginKind},
    redact::Redactor,
};
use amp_mcp::{AmpMcpClient, Auth, CredentialBackend, Settings};
use futures::future::BoxFuture;

use amp_core::{agent::Runtime, manager_feed::ManagerFeed};

/// The harness's AMP login, for the relay.
struct HarnessLogin {
    settings: Settings,
    mcp: Arc<AmpMcpClient>,
}

impl TokenSource for HarnessLogin {
    fn oauth_token(&self) -> BoxFuture<'static, Result<String, String>> {
        let settings = self.settings.clone();
        Box::pin(async move {
            if matches!(settings.auth, Auth::Static { .. }) {
                return Err(
                    policy::check_server(&settings.url, LoginKind::StaticToken, &|_| None)
                        .err()
                        .unwrap_or_default(),
                );
            }
            let backend = CredentialBackend::resolve().map_err(|e| e.to_string())?;
            amp_mcp::oauth::access_token(&settings.url, settings.ca_file.as_deref(), &backend)
                .await
                .map_err(|e| match e {
                    amp_mcp::McpError::AuthRequired { .. } => {
                        "remote control needs your AMP login; run `amp-harness login`".to_owned()
                    }
                    other => format!("your AMP login could not be used: {other}"),
                })
        })
    }

    fn user_id(&self) -> BoxFuture<'static, Result<i64, String>> {
        let mcp = Arc::clone(&self.mcp);
        Box::pin(async move {
            let me = crate::whoami::fetch(&mcp)
                .await
                .map_err(|e| format!("who you are in AMP (amp_whoami): {e}"))?;
            if me.account.as_deref() == Some("agent") {
                return Err("remote control is for people: this login is an agent account".into());
            }
            if me.via_api_token {
                return Err(
                    "remote control can't use an amp_ API token; run `amp-harness login`".into(),
                );
            }
            me.user_id
                .ok_or_else(|| "AMP did not say who you are (amp_whoami)".to_owned())
        })
    }
}

fn host_name() -> String {
    std::env::var("AMP_HARNESS_HOST_NAME")
        .ok()
        .or_else(|| {
            std::process::Command::new("hostname")
                .arg("-s")
                .output()
                .ok()
                .filter(|o| o.status.success())
                .and_then(|o| String::from_utf8(o.stdout).ok())
        })
        .map(|s| s.trim().chars().take(64).collect::<String>())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| "laptop".to_owned())
}

fn os_name() -> &'static str {
    if cfg!(target_os = "macos") {
        "macos"
    } else if cfg!(target_os = "windows") {
        "windows"
    } else {
        "linux"
    }
}

/// The login kind `settings` give, without asking AMP.
async fn login_kind(settings: &Settings) -> LoginKind {
    if matches!(settings.auth, Auth::Static { .. }) {
        return LoginKind::StaticToken;
    }
    match CredentialBackend::resolve() {
        Ok(backend) => match amp_mcp::oauth::has_login(&settings.url, &backend).await {
            Ok(true) => LoginKind::OAuth,
            _ => LoginKind::None,
        },
        Err(_) => LoginKind::None,
    }
}

/// The chat's remote link, or why remote control can't be used here (shown by `/remote`).
/// `Ok` carries the dev-override warning to show, if any. Nothing is sent until `enable`.
pub async fn build(
    rt: &Arc<Runtime>,
    feed: &Arc<ManagerFeed>,
    project_id: Option<i64>,
    settings: &Settings,
    host: Arc<dyn RemoteHost>,
) -> Result<(RemoteLink, Option<String>), String> {
    let env = |k: &str| std::env::var(k).ok();
    let paths = rt.paths.clone();
    let switches = LocalSwitches::new(
        &env,
        paths.user_file.clone(),
        paths.repo_dir.as_ref().map(|d| d.join("config.toml")),
    );
    switches.check(&env)?;
    let server = policy::check_server(&settings.url, login_kind(settings).await, &env)?;
    let redactor = Redactor::new();
    redactor.add_env(std::env::vars());
    {
        let rt = Arc::clone(rt);
        redactor.add_source(Arc::new(move || {
            amp_core::providers::secret_values(
                &rt.current_models(),
                &amp_core::providers::SystemKeys::from_env(),
            )
        }));
    }
    let keys = KeyStore::resolve()?;
    let state: PathBuf = rt.state_dir.clone();
    let link = RemoteLink::new(RemoteDeps {
        setup: RemoteSetup {
            api_base: server.api_base,
            web_origin: amp_mcp::url_origin(settings.web_url.as_deref().unwrap_or(&settings.url)),
            ca_file: settings.ca_file.clone(),
            project_id,
            host: host_name(),
            os: os_name().to_owned(),
            version: env!("CARGO_PKG_VERSION").to_owned(),
        },
        tokens: Arc::new(HarnessLogin {
            settings: settings.clone(),
            mcp: Arc::clone(&rt.mcp),
        }),
        host,
        feed: Arc::clone(feed),
        keys,
        pins: Some(state.join(amp_core::remote::pairing::PIN_FILE)),
        action_log: Some(state.join("remote-actions.log")),
        redactor,
        switches: Arc::new(move || switches.check(&|k: &str| std::env::var(k).ok())),
        tuning: LinkTuning::default(),
    });
    Ok((link, server.warning))
}
