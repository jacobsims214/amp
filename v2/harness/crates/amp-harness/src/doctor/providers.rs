//! doctor's model-side checks (D9 as changed by #2091; D3 for keys):
//!
//! - `roles`: models.toml + config.toml load, and every role resolves to a provider/model.
//! - `key:<provider>`: each provider some role uses has its key (env, then keychain) or needs none.
//!   A missing key fails for the manager's provider (the TUI cannot start) and warns otherwise
//!   (only the roles that use it cannot run).
//! - `models:<provider>`: every model id the roles pin is in the provider's live catalog (the same
//!   lookup as `config show --check`). An unreadable catalog warns; `--offline` skips it.
//!
//! Keys are described by their source only, never by value.
//!
//! #2277: each provider's auth type is shown (bearer, header, none, command, OAuth client
//! credentials, Entra) with the token's expiry for token auth; the catalog call goes through the
//! provider's own transport (CA file, mTLS, proxy, query) and auth, and a refused key or token, a
//! TLS failure or a token that could not be had gets the fix that applies.

use std::collections::{BTreeMap, BTreeSet};

use amp_core::{
    models::{ModelHints, ModelsConfig, Selection},
    provider_auth::{self, ProbeFailure, ProbeKind},
    providers::{KeyLookup, KeySource, resolve_key},
};
use serde::Serialize;

use super::{CheckStatus, Finding};

/// The role the TUI opens as (assets manifest `default_agent`, `amp-manager`).
const PRIMARY_ROLE: &str = "manager";

/// `doctor --json` `.models`. Add fields, never rename or remove them.
#[derive(Debug, Default, Serialize)]
pub struct ModelsReport {
    /// The config.toml read (it may not exist: then the shipped defaults apply).
    pub config_file: Option<String>,
    pub config_found: bool,
    /// `profile` or `inherit`.
    pub mode: Option<&'static str>,
    pub selection: Option<String>,
    /// Why the config could not be loaded.
    pub error: Option<String>,
    /// Roles that do not resolve, with the reason.
    pub role_errors: BTreeMap<String, String>,
    /// The providers some role uses.
    pub providers: Vec<ProviderReport>,
    /// Each role's provider, model and endpoint (#2275), sorted by role. Since #2246 this also
    /// lists every agent with no `[roles.<id>]` of its own, with how it fell back.
    pub roles: Vec<RoleModel>,
    /// What a role with no model of its own runs on (#2246).
    pub fallback: Option<FallbackReport>,
}

/// `doctor --json` `.models.fallback`: `[models] fallback` and the model it resolves to.
#[derive(Debug, Serialize)]
pub struct FallbackReport {
    /// `[models] fallback` as written (a role or `provider/model`), default `worker-backend`.
    pub setting: String,
    /// Where it was set (`default`, `config <path>`, `inherit ...`).
    pub source: String,
    pub provider: Option<String>,
    pub model: Option<String>,
    /// Why it does not resolve.
    pub error: Option<String>,
    /// The roles running on it (or on another step of the fallback chain), sorted.
    pub roles: Vec<String>,
}

/// One role's model in `doctor --json` `.models.roles`.
#[derive(Debug, Serialize)]
pub struct RoleModel {
    pub role: String,
    pub provider: String,
    pub model: String,
    /// The provider's base URL (the endpoint the role's requests go to).
    pub endpoint: String,
    /// Which layer set it (`default`, `inherit (config ...)`, `config ...`).
    pub source: String,
    /// How a role with no `[roles.<id>]` got its model (#2246): `fallback worker-backend`,
    /// `[models] fallback`, `based_on <role>` or `agent file model`. `null` for its own setting.
    pub fallback: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct ProviderReport {
    pub id: String,
    /// Display name (`[providers.<id>] name`, else the id).
    pub name: String,
    pub api: &'static str,
    pub base_url: String,
    /// `env`, `keychain`, `not-needed` or `missing`.
    pub key: &'static str,
    /// How requests authenticate (#2277): bearer, header, none, command,
    /// oauth-client-credentials, azure-entra.
    pub auth: &'static str,
    /// The auth in words (the header name, the command, the client id; never a secret).
    pub auth_detail: String,
    /// When the token doctor fetched expires (`YYYY-MM-DD HH:MM:SS UTC`), for token auth.
    pub token_expires: Option<String>,
    /// Extra CA file / mTLS certificate / proxy, when configured.
    pub ca_file: Option<String>,
    pub client_cert: Option<String>,
    pub proxy: Option<String>,
    /// Where the key came from (never the key).
    pub key_source: String,
    pub roles: Vec<String>,
    pub models: Vec<ModelReport>,
    /// `true` when the live catalog was read.
    pub catalog_checked: bool,
    /// Why the live catalog could not be read.
    pub catalog_error: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct ModelReport {
    pub id: String,
    pub roles: Vec<String>,
    /// `true` listed, `false` missing, `null` not checked.
    pub live: Option<bool>,
}

/// One named finding (`roles`, `key:<id>`, `models:<id>`).
pub type Named = (String, Finding);

/// `agents`: every agent's model role and fallback hints, so roles with no model of their own are
/// checked with the model they really run on (#2246).
pub async fn check(
    config: Result<ModelsConfig, String>,
    config_path: Option<String>,
    agents: &[(String, ModelHints)],
    keys: &dyn KeyLookup,
    offline: bool,
) -> (ModelsReport, Vec<Named>) {
    let mut report = ModelsReport {
        config_file: config_path.clone(),
        ..ModelsReport::default()
    };
    let mut out = Vec::new();
    let where_ = config_path
        .clone()
        .unwrap_or_else(|| "~/.config/amp-harness/config.toml".to_owned());

    let config = match config {
        Ok(c) => c,
        Err(e) => {
            report.error = Some(e.clone());
            out.push((
                "roles".to_owned(),
                Finding::new(CheckStatus::Fail, e).fix(format!(
                    "fix {where_} (`amp-harness config show` prints the same error)"
                )),
            ));
            return (report, out);
        }
    };
    report.config_file = config
        .config_file
        .as_ref()
        .map(|p| p.display().to_string())
        .or(config_path);
    report.config_found = config.config_found;
    let (mode, selection) = match &config.selection {
        Selection::Profile { name, .. } => ("profile", name.clone()),
        Selection::Inherit { model, .. } => ("inherit", model.clone()),
    };
    report.mode = Some(mode);
    report.selection = Some(selection.clone());

    // provider -> model -> roles
    let mut used: BTreeMap<String, BTreeMap<String, Vec<String>>> = BTreeMap::new();
    let mut primary_provider = None;
    let mut resolved = 0usize;
    let mut fell_back: Vec<String> = Vec::new();
    for (role, r) in config.resolve_agents(agents) {
        match r {
            Ok(r) => {
                resolved += 1;
                let fallback = r.fell_back();
                if let Some(how) = &fallback {
                    fell_back.push(format!("{role} -> {} ({how})", r.model_ref()));
                }
                if role == PRIMARY_ROLE {
                    primary_provider = Some(r.provider.clone());
                }
                report.roles.push(RoleModel {
                    role: role.clone(),
                    provider: r.provider.clone(),
                    model: r.model_id.clone(),
                    endpoint: config
                        .providers
                        .get(&r.provider)
                        .map(|p| p.base_url.clone())
                        .unwrap_or_default(),
                    source: r.source.clone(),
                    fallback,
                });
                used.entry(r.provider)
                    .or_default()
                    .entry(r.model_id)
                    .or_default()
                    .push(role);
            }
            Err(e) => {
                report.role_errors.insert(role, e.to_string());
            }
        }
    }
    let roles_finding = if report.role_errors.is_empty() {
        let mut detail = format!("{resolved} roles resolve ({mode} {selection})");
        if !fell_back.is_empty() {
            detail.push_str(&format!(
                "; {} without a model of their own: {}",
                fell_back.len(),
                fell_back.join(", ")
            ));
        }
        Finding::new(CheckStatus::Ok, detail)
    } else {
        let list: Vec<String> = report
            .role_errors
            .iter()
            .map(|(role, e)| format!("{role}: {e}"))
            .collect();
        Finding::new(CheckStatus::Fail, list.join("; ")).fix(format!(
            "give each role a `provider/model` that exists under [providers] in {where_}, e.g. \
             [roles.{}] model = \"openrouter/<model id>\"",
            report
                .role_errors
                .keys()
                .next()
                .map_or("manager", String::as_str)
        ))
    };
    out.push(("roles".to_owned(), roles_finding));
    let (fallback, fallback_finding) = fallback_check(&config, &report.roles, &where_);
    report.fallback = Some(fallback);
    out.push(("fallback".to_owned(), fallback_finding));

    for (id, models) in used {
        let Some(provider) = config.providers.get(&id) else {
            continue;
        };
        let mut roles: Vec<String> = models.values().flatten().cloned().collect();
        roles.sort();
        let (_, source) = resolve_key(provider, keys);
        let has_key = !source.is_missing();

        let key_finding = if has_key {
            Finding::new(
                CheckStatus::Ok,
                match (&source, &provider.auth) {
                    (KeySource::NotNeeded, auth) if auth.is_token() => {
                        format!("no key needed: {}", auth.describe())
                    }
                    (KeySource::NotNeeded, _) => "no key needed".to_owned(),
                    (other, amp_core::models::ProviderAuth::Bearer) => format!("key from {other}"),
                    (other, auth) => {
                        format!("{} from {other} ({})", auth.key_word(), auth.describe())
                    }
                },
            )
        } else {
            let primary = primary_provider.as_deref() == Some(id.as_str());
            let status = if primary {
                CheckStatus::Fail
            } else {
                CheckStatus::Warn
            };
            Finding::new(
                status,
                match &source {
                    KeySource::KeychainFailed(_) => format!(
                        "the keychain did not give the API key, so these roles cannot run: {}",
                        roles.join(", ")
                    ),
                    _ => format!(
                        "no API key, so these roles cannot run: {}",
                        roles.join(", ")
                    ),
                },
            )
            .fix(key_fix(&id, &source, &where_))
        };
        out.push((format!("key:{id}"), key_finding));

        let mut catalog_checked = false;
        let mut catalog_error = None;
        let mut token_expires = None;
        let primary = primary_provider.as_deref() == Some(id.as_str());
        let mut live: BTreeMap<String, Option<bool>> =
            models.keys().map(|m| (m.clone(), None)).collect();
        let models_finding = if offline {
            Finding::new(CheckStatus::Skip, "--offline: model catalog not checked")
        } else if !has_key {
            Finding::new(CheckStatus::Skip, "no key: model catalog not checked")
        } else {
            match provider_auth::probe(provider, keys, ProbeKind::Catalog).await {
                Ok(found) => {
                    catalog_checked = true;
                    token_expires = found.token_expires_at.map(provider_auth::format_unix);
                    let ids = found.ids;
                    for (m, v) in live.iter_mut() {
                        *v = Some(ids.contains(m));
                    }
                    let mut f = models_finding(&id, &models, &ids, &where_);
                    if let Some(at) = &token_expires {
                        f.detail.push_str(&format!(" (token valid until {at})"));
                    }
                    f
                }
                Err(e) => {
                    catalog_error = Some(e.message.clone());
                    probe_finding(provider, &e, primary, &where_)
                }
            }
        };
        out.push((format!("models:{id}"), models_finding));

        report.providers.push(ProviderReport {
            name: crate::setup::display_name(provider),
            api: provider.api.as_str(),
            base_url: provider.base_url.clone(),
            key: source.kind(),
            key_source: source.to_string(),
            auth: provider.auth.name(),
            auth_detail: provider.auth.describe(),
            token_expires,
            ca_file: provider
                .tls
                .ca_file
                .as_ref()
                .map(|p| p.display().to_string()),
            client_cert: provider
                .tls
                .client_cert
                .as_ref()
                .map(|p| p.display().to_string()),
            proxy: provider.proxy.as_ref().map(|p| match p {
                amp_core::models::ProxySetting::Url(u) => u.clone(),
                amp_core::models::ProxySetting::Direct => "direct".to_owned(),
            }),
            models: models
                .iter()
                .map(|(m, r)| ModelReport {
                    id: m.clone(),
                    roles: r.clone(),
                    live: live.get(m).copied().flatten(),
                })
                .collect(),
            roles,
            catalog_checked,
            catalog_error,
            id,
        });
    }
    (report, out)
}

/// `fallback` (#2246): what a role with no model of its own runs on. It fails only when a role
/// needs it and it does not resolve (that role is already a `roles` failure); otherwise a broken
/// fallback is a warning, since the next agent created would have no model.
fn fallback_check(
    config: &ModelsConfig,
    roles: &[RoleModel],
    where_: &str,
) -> (FallbackReport, Finding) {
    let f = config.fallback_model();
    let users: Vec<String> = roles
        .iter()
        .filter(|r| r.fallback.is_some())
        .map(|r| r.role.clone())
        .collect();
    let what = if f.setting.contains('/') {
        format!("[models] fallback {}", f.setting)
    } else if f.source.starts_with("inherit") {
        format!("inherit {}", f.setting)
    } else {
        format!("{}'s model", f.setting)
    };
    let finding = match &f.resolved {
        Ok(r) => Finding::new(
            CheckStatus::Ok,
            format!(
                "roles without a model of their own run on {} ({what}, {}){}",
                r.model_ref(),
                f.source,
                if users.is_empty() {
                    String::new()
                } else {
                    format!("; used by {}", users.join(", "))
                }
            ),
        ),
        Err(e) => {
            Finding::new(CheckStatus::Warn, format!("{what} ({}): {}", f.source, e.0)).fix(format!(
                "set [models] fallback in {where_} to a role that has a model or to a \
                 provider/model, e.g. fallback = \"worker-backend\""
            ))
        }
    };
    let report = FallbackReport {
        setting: f.setting.clone(),
        source: f.source.clone(),
        provider: f.resolved.as_ref().ok().map(|r| r.provider.clone()),
        model: f.resolved.as_ref().ok().map(|r| r.model_id.clone()),
        error: f.resolved.as_ref().err().map(|e| e.0.clone()),
        roles: users,
    };
    (report, finding)
}

fn models_finding(
    provider: &str,
    models: &BTreeMap<String, Vec<String>>,
    ids: &BTreeSet<String>,
    where_: &str,
) -> Finding {
    let missing: Vec<(&String, &Vec<String>)> =
        models.iter().filter(|(m, _)| !ids.contains(*m)).collect();
    if missing.is_empty() {
        let names: Vec<&str> = models.keys().map(String::as_str).collect();
        return Finding::new(
            CheckStatus::Ok,
            format!(
                "{} listed: {}",
                plural(names.len(), "model"),
                names.join(", ")
            ),
        );
    }
    let detail: Vec<String> = missing
        .iter()
        .map(|(m, roles)| format!("{m} (roles: {})", roles.join(", ")))
        .collect();
    let example_role = missing[0].1[0].as_str();
    Finding::new(
        CheckStatus::Fail,
        format!("{provider} does not list {}", detail.join("; ")),
    )
    .fix(format!(
        "pick a model the provider lists and set it in {where_}, e.g. [roles.{example_role}] \
         model = \"{provider}/<model id>\" (`amp-harness config show --check` shows every role)"
    ))
}

/// A failed catalog call, with the fix for what failed (#2277). Network trouble stays a warning
/// (the old wording); a refused key or token, a TLS failure or no token fails for the manager's
/// provider and warns for others, like a missing key.
fn probe_finding(
    p: &amp_core::models::ProviderConfig,
    e: &provider_auth::ProbeError,
    primary: bool,
    where_: &str,
) -> Finding {
    let id = &p.id;
    let serious = if primary {
        CheckStatus::Fail
    } else {
        CheckStatus::Warn
    };
    let lower = e.message.to_ascii_lowercase();
    match e.failure {
        ProbeFailure::Rejected => {
            let status = e.status.unwrap_or(401);
            let (what, fix) = match &p.auth {
                auth if auth.is_token() => (
                    format!("the token ({})", auth.describe()),
                    match auth {
                        amp_core::models::ProviderAuth::Command { argv, .. } => format!(
                            "run `{}` yourself and check it prints a current token for this \
                             endpoint (log in to the tool again if it needs that)",
                            argv.join(" ")
                        ),
                        amp_core::models::ProviderAuth::AzureEntra(_) => format!(
                            "check that the identity has a role on the resource (Cognitive \
                             Services OpenAI User) and that [providers.{id}] tenant_id / client_id \
                             or `az login` are right"
                        ),
                        _ => format!(
                            "check [providers.{id}] client_id and scopes; store a new client \
                             secret with `amp-harness connect {id}`"
                        ),
                    },
                ),
                amp_core::models::ProviderAuth::Header { name } => (
                    format!("the key in the {name} header"),
                    format!(
                        "store the right key with `amp-harness connect {id}` (or check that \
                         {name} is the header the endpoint reads)"
                    ),
                ),
                amp_core::models::ProviderAuth::None => (
                    "a request without credentials".to_owned(),
                    format!(
                        "the endpoint wants credentials: set auth in [providers.{id}] ({where_}) \
                         and run `amp-harness connect {id}`"
                    ),
                ),
                _ => (
                    "the key".to_owned(),
                    format!(
                        "store a working key with `amp-harness connect {id}` (it is checked \
                         before it is kept)"
                    ),
                ),
            };
            let why = if status == 403 {
                "refused (HTTP 403: authenticated, but not allowed; check the key's permissions \
                 or the organization/project)"
                    .to_owned()
            } else {
                format!("refused (HTTP {status})")
            };
            Finding::new(serious, format!("{} {why} {what}", amp_mcp::redact_url(&p.base_url))).fix(fix)
        }
        ProbeFailure::Tls => {
            let mtls = ["certificaterequired", "certificate required", "badcertificate", "bad certificate", "handshakefailure", "unknownca"]
                .iter()
                .any(|n| lower.contains(n));
            let fix = if mtls && p.tls.client_cert.is_none() {
                format!(
                    "the endpoint asks for a client certificate (mTLS): `amp-harness connect {id} \
                     --client-cert <PEM> --client-key <PEM>` (or client_cert / client_key in \
                     [providers.{id}])"
                )
            } else if mtls {
                format!(
                    "the endpoint refused the client certificate {}: use one its CA signed",
                    p.tls
                        .client_cert
                        .as_ref()
                        .map(|c| c.display().to_string())
                        .unwrap_or_default()
                )
            } else if lower.contains("expired") {
                "the endpoint's certificate (or its CA) has expired: ask its owner to renew it"
                    .to_owned()
            } else if lower.contains("notvalidforname") || lower.contains("not valid for name") {
                format!("its certificate does not name this host: use the host name it was issued for in [providers.{id}] base_url")
            } else if let Some(ca) = &p.tls.ca_file {
                format!(
                    "its certificate is not signed by the CA in {}: export the CA that signed it",
                    ca.display()
                )
            } else {
                format!(
                    "its certificate is not signed by a CA this machine trusts: save the \
                     company CA (PEM) and set ca_file = \"<PEM>\" in [providers.{id}] (or \
                     `amp-harness connect {id} --ca-file <PEM>`), or add it to the OS trust store"
                )
            };
            Finding::new(serious, format!("TLS to {} failed: {}", amp_mcp::redact_url(&p.base_url), amp_mcp::redact_urls(&e.message))).fix(fix)
        }
        ProbeFailure::Token => Finding::new(serious, format!("no token: {}", e.message)).fix(
            match &p.auth {
                amp_core::models::ProviderAuth::Command { .. }
                | amp_core::models::ProviderAuth::AzureEntra(_)
                    if lower.contains("not found") =>
                {
                    let program = match &p.auth {
                        amp_core::models::ProviderAuth::Command { argv, .. } => argv[0].clone(),
                        _ => "az".to_owned(),
                    };
                    format!("install `{program}` (or put it on PATH), or change auth_command in [providers.{id}]")
                }
                amp_core::models::ProviderAuth::AzureEntra(_) => format!(
                    "run `az login` (or set tenant_id and client_id in [providers.{id}] and store the \
                     client secret with `amp-harness connect {id}`)"
                ),
                amp_core::models::ProviderAuth::Command { .. } => {
                    "run the auth_command yourself to see why it fails; raise \
                     auth_command_timeout_secs if it needs longer"
                        .to_owned()
                }
                _ => format!(
                    "check [providers.{id}] token_url, client_id and scopes, and the client secret \
                     (`amp-harness connect {id}` stores a new one)"
                ),
            },
        ),
        ProbeFailure::Config => Finding::new(serious, e.message.clone()).fix(format!(
            "fix [providers.{id}] in {where_} (`amp-harness config show` checks it)"
        )),
        ProbeFailure::Http | ProbeFailure::Network => Finding::new(
            CheckStatus::Warn,
            format!(
                "could not read the model catalog at {}: {}",
                p.base_url, e.message
            ),
        )
        .fix(format!(
            "check the network and [providers.{id}] base_url{}, then run `amp-harness doctor` \
             again (or `--offline` to skip this check)",
            if p.proxy.is_none() {
                " (behind a proxy: set HTTPS_PROXY, or proxy in the table)"
            } else {
                " and proxy"
            }
        )),
    }
}

fn key_fix(provider: &str, source: &KeySource, where_: &str) -> String {
    match source {
        // The message already says what to do (#2165).
        KeySource::KeychainFailed(why) => why.clone(),
        // #2163: connect asks for the key hidden, checks it and keeps it in the keychain. The
        // environment variable stays the CI/headless path.
        KeySource::Missing {
            env: Some(var),
            keychain_checked: true,
        } => format!(
            "run `amp-harness connect {provider}` (asks for the key hidden, checks it and keeps it \
             in the OS keychain); for CI/headless set {var} instead"
        ),
        KeySource::Missing {
            env: Some(var),
            keychain_checked: false,
        } => format!(
            "set {var} in the environment (the keychain is not read while \
             AMP_CREDENTIAL_STORE=file, so `amp-harness connect` can't store it)"
        ),
        _ => format!(
            "run `amp-harness connect {provider}`, or add key_env = \"<ENV VAR>\" to \
             [providers.{provider}] in {where_} and set it"
        ),
    }
}

fn plural(n: usize, word: &str) -> String {
    if n == 1 {
        format!("1 {word}")
    } else {
        format!("{n} {word}s")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use amp_core::{models::ModelsFile, providers::StaticKeys};

    fn config(user: &str) -> ModelsConfig {
        let user = ModelsFile::parse(user, "test").unwrap();
        ModelsConfig::from_files(ModelsFile::defaults(), Some(user), "config test", &|_| None)
            .unwrap()
    }

    fn find<'a>(out: &'a [Named], name: &str) -> &'a Finding {
        &out.iter().find(|(n, _)| n == name).unwrap().1
    }

    #[tokio::test]
    async fn missing_key_fails_for_the_manager_and_warns_otherwise() {
        // Everything on faux except the reviewers, which use anthropic.
        let c = config(
            "[models]\nprofile = \"inherit\"\ninherit = \"f/m\"\n[providers.f]\napi = \"faux\"\n\
             [providers.f.models.\"m\"]\n[roles.reviewer]\nmodel = \"anthropic/claude-opus-5-5\"\n",
        );
        let keys = StaticKeys::default();
        let (report, out) = check(Ok(c), None, &[], &keys, true).await;
        assert_eq!(find(&out, "roles").status, CheckStatus::Ok);
        assert_eq!(find(&out, "key:f").status, CheckStatus::Ok);
        let anthropic = find(&out, "key:anthropic");
        assert_eq!(anthropic.status, CheckStatus::Warn);
        assert!(anthropic.detail.contains("reviewer"), "{anthropic:?}");
        assert!(
            anthropic
                .fix
                .as_deref()
                .unwrap()
                .starts_with("set ANTHROPIC_API_KEY in the environment"),
            "the keychain is off in this test: {anthropic:?}"
        );
        assert_eq!(find(&out, "models:anthropic").status, CheckStatus::Skip);
        assert_eq!(report.providers.len(), 2);

        // The manager on anthropic too: now it fails.
        let c = config("[roles.manager]\nmodel = \"anthropic/claude-opus-5-5\"\n");
        let (_, out) = check(Ok(c), None, &[], &keys, true).await;
        assert_eq!(find(&out, "key:anthropic").status, CheckStatus::Fail);
    }

    #[tokio::test]
    async fn roles_without_a_model_show_the_fallback() {
        let c = config(
            "[providers.f]\napi = \"faux\"\n[providers.f.models.\"m\"]\n\
             [providers.g]\napi = \"faux\"\n[providers.g.models.\"n\"]\n\
             [roles.manager]\nmodel = \"f/m\"\n[roles.worker-backend]\nmodel = \"g/n\"\n",
        );
        let agents = vec![
            ("smoke-helper".to_owned(), ModelHints::default()),
            (
                "copy".to_owned(),
                ModelHints {
                    based_on: Some("manager".into()),
                    ..ModelHints::default()
                },
            ),
            // Configured: not a fallback.
            ("worker-backend".to_owned(), ModelHints::default()),
        ];
        let keys = StaticKeys::default();
        let (report, out) = check(Ok(c), None, &agents, &keys, true).await;
        let role = |id: &str| report.roles.iter().find(|r| r.role == id).unwrap();
        assert_eq!(role("smoke-helper").model, "n");
        assert_eq!(
            role("smoke-helper").fallback.as_deref(),
            Some("fallback worker-backend")
        );
        assert_eq!(role("copy").model, "m");
        assert_eq!(role("copy").fallback.as_deref(), Some("based_on manager"));
        assert_eq!(role("worker-backend").fallback, None);
        let roles = find(&out, "roles");
        assert!(
            roles
                .detail
                .contains("smoke-helper -> g/n (fallback worker-backend"),
            "{roles:?}"
        );
        let fb = find(&out, "fallback");
        assert_eq!(fb.status, CheckStatus::Ok);
        assert!(
            fb.detail.starts_with(
                "roles without a model of their own run on g/n (worker-backend's model, default)"
            ),
            "{fb:?}"
        );
        assert!(fb.detail.contains("used by copy, smoke-helper"), "{fb:?}");
        let f = report.fallback.unwrap();
        assert_eq!(
            (f.provider.as_deref(), f.model.as_deref()),
            (Some("g"), Some("n"))
        );

        // A fallback that names a role nobody configured: a warning with the fix.
        let c = config(
            "[providers.f]\napi = \"faux\"\n[providers.f.models.\"m\"]\n\
             [roles.manager]\nmodel = \"f/m\"\n[models]\nfallback = \"nobody\"\n",
        );
        let (report, out) = check(Ok(c), None, &[], &keys, true).await;
        let fb = find(&out, "fallback");
        assert_eq!(fb.status, CheckStatus::Warn, "{fb:?}");
        assert!(fb.fix.as_deref().unwrap().contains("[models] fallback"));
        assert!(report.fallback.unwrap().error.is_some());
    }

    #[test]
    fn a_missing_key_fix_names_connect() {
        let source = KeySource::Missing {
            env: Some("OPENROUTER_API_KEY".into()),
            keychain_checked: true,
        };
        let fix = key_fix("openrouter", &source, "config.toml");
        assert!(
            fix.starts_with("run `amp-harness connect openrouter`"),
            "{fix}"
        );
        assert!(
            fix.contains("OPENROUTER_API_KEY"),
            "the headless path stays: {fix}"
        );
        assert!(
            !fix.contains("export ") && !fix.contains("security "),
            "{fix}"
        );
    }

    #[tokio::test]
    async fn keys_are_never_printed() {
        let c = config("[roles.manager]\nmodel = \"anthropic/claude-opus-5-5\"\n");
        let keys = StaticKeys {
            env: BTreeMap::from([
                ("ANTHROPIC_API_KEY".into(), "sk-ant-TOPSECRET".into()),
                ("OPENROUTER_API_KEY".into(), "sk-or-TOPSECRET".into()),
            ]),
            keychain: None,
        };
        let (report, out) = check(Ok(c), None, &[], &keys, true).await;
        let text = format!("{}{out:?}", serde_json::to_string(&report).unwrap());
        assert!(!text.contains("TOPSECRET"), "{text}");
        assert_eq!(
            find(&out, "key:anthropic").detail,
            "key from env ANTHROPIC_API_KEY"
        );
    }

    #[tokio::test]
    async fn config_errors_fail_roles() {
        let (report, out) = check(
            Err("bad toml".into()),
            Some("/c.toml".into()),
            &[],
            &StaticKeys::default(),
            true,
        )
        .await;
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].1.status, CheckStatus::Fail);
        assert!(out[0].1.fix.as_deref().unwrap().contains("/c.toml"));
        assert_eq!(report.error.as_deref(), Some("bad toml"));
    }

    #[test]
    fn missing_models_name_their_roles() {
        let models = BTreeMap::from([
            ("a".to_owned(), vec!["manager".to_owned()]),
            (
                "b".to_owned(),
                vec!["reviewer".to_owned(), "github-reviewer".to_owned()],
            ),
        ]);
        let ids = BTreeSet::from(["a".to_owned()]);
        let f = models_finding("openrouter", &models, &ids, "/c.toml");
        assert_eq!(f.status, CheckStatus::Fail);
        assert_eq!(
            f.detail,
            "openrouter does not list b (roles: reviewer, github-reviewer)"
        );
        assert!(
            f.fix
                .unwrap()
                .contains("[roles.reviewer] model = \"openrouter/<model id>\"")
        );
        let ids = BTreeSet::from(["a".to_owned(), "b".to_owned()]);
        assert_eq!(
            models_finding("p", &models, &ids, "").status,
            CheckStatus::Ok
        );
    }

    #[test]
    fn failed_probes_get_the_fix_that_applies() {
        let c = config(
            "[providers.gw]\napi = \"openai-completions\"\nbase_url = \"https://gw.example/v1\"\n\
             auth = \"header\"\n[providers.gw.models.\"m\"]\n\
             [providers.sso]\napi = \"openai-completions\"\nbase_url = \"https://gw.example/v1\"\n\
             auth = \"command\"\nauth_command = [\"corp-sso\"]\n[providers.sso.models.\"m\"]\n",
        );
        let e = |failure, status: Option<u16>, message: &str| provider_auth::ProbeError {
            failure,
            status,
            message: message.to_owned(),
        };
        let gw = &c.providers["gw"];
        let f = probe_finding(
            gw,
            &e(ProbeFailure::Rejected, Some(401), "HTTP 401"),
            true,
            "c.toml",
        );
        assert_eq!(f.status, CheckStatus::Fail);
        assert!(
            f.detail
                .contains("refused (HTTP 401) the key in the api-key header"),
            "{f:?}"
        );
        assert!(f.fix.unwrap().contains("amp-harness connect gw"));
        let f = probe_finding(
            gw,
            &e(ProbeFailure::Rejected, Some(403), "HTTP 403"),
            false,
            "",
        );
        assert_eq!(f.status, CheckStatus::Warn);
        assert!(f.detail.contains("not allowed"), "{f:?}");

        let f = probe_finding(
            gw,
            &e(
                ProbeFailure::Tls,
                None,
                "request: invalid peer certificate: UnknownIssuer",
            ),
            true,
            "",
        );
        assert!(f.fix.as_deref().unwrap().contains("ca_file"), "{f:?}");
        let f = probe_finding(
            gw,
            &e(
                ProbeFailure::Tls,
                None,
                "request: received fatal alert: CertificateRequired",
            ),
            true,
            "",
        );
        assert!(f.fix.as_deref().unwrap().contains("--client-cert"), "{f:?}");

        let sso = &c.providers["sso"];
        let f = probe_finding(
            sso,
            &e(ProbeFailure::Rejected, Some(401), "HTTP 401"),
            true,
            "",
        );
        assert!(
            f.fix
                .as_deref()
                .unwrap()
                .contains("run `corp-sso` yourself"),
            "{f:?}"
        );
        let f = probe_finding(
            sso,
            &e(
                ProbeFailure::Token,
                None,
                "auth_command `corp-sso` was not found (is it installed and on PATH?)",
            ),
            true,
            "",
        );
        assert!(
            f.fix.as_deref().unwrap().contains("install `corp-sso`"),
            "{f:?}"
        );
        // Network trouble keeps the old wording and stays a warning.
        let f = probe_finding(
            gw,
            &e(ProbeFailure::Network, None, "request: refused"),
            true,
            "",
        );
        assert_eq!(f.status, CheckStatus::Warn);
        assert!(
            f.detail.starts_with("could not read the model catalog at"),
            "{f:?}"
        );
    }
}
