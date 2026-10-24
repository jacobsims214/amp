//! The first run's "your own OpenAI-compatible endpoint" (and OpenAI) path (#2275): a company
//! gateway such as `api.example.com` serving models called `engineer` and `orchestrator`, set up
//! without editing a file.
//!
//! Asked in order: a name and an id, the endpoint address (`api.example.com` becomes
//! `https://api.example.com/v1` after a confirmation; plain http only for localhost unless the
//! person confirms), the API key (hidden; Enter for an endpoint that takes none; a key already in
//! `<ID>_API_KEY` is used as is), extra header names (values that are secrets are read hidden and
//! kept in the OS keychain), then the models it lists (`GET /models`, or typed when it lists
//! none) and which model each group of roles runs on ([`super::role_map`]), defaulting by name.
//!
//! The result is `[providers.<id>]` plus the role mapping in config.toml, and the key in the OS
//! keychain. OpenRouter is not needed and not contacted on this path.
//!
//! #2277: after the address, the endpoint is tried once without credentials. Only when that fails
//! on TLS is the person asked for a CA bundle (unknown issuer) or a client certificate and key
//! (the endpoint wants mTLS); only when it cannot be reached at all, for a proxy. Then "How does
//! your endpoint authenticate?" (bearer key, a key in its own header such as Azure's `api-key`,
//! none, a token command, OAuth client credentials, Microsoft Entra ID), with only the questions
//! that choice needs. An Azure OpenAI address without a path becomes `…/openai/v1` (which needs no
//! api-version); a classic `…/openai/deployments/…` address is asked for its api-version. OpenAI
//! itself is asked for the optional organization and project ids.

use std::collections::BTreeMap;

use amp_core::{
    models::{ModelsConfig, ProviderAuth, ProviderConfig},
    provider_auth::{CertProblem, ProbeError, ProbeFailure, ProbeKind, probe},
    providers::StaticKeys,
};

use super::{KeyCheck, role_map, ui::Ui};
use crate::{
    conn::CliError,
    connect::{self, CustomSpec},
};

/// What the person set up.
pub struct Endpoint {
    pub spec: CustomSpec,
    /// The key (empty: none). Never printed or written to a file.
    pub key: String,
    /// The key came from `<ID>_API_KEY` (nothing to store).
    pub key_from_env: bool,
    pub assignment: role_map::Assignment,
}

/// Whether `url`'s host is this machine.
pub fn is_local(url: &str) -> bool {
    let rest = url.split_once("://").map_or(url, |(_, r)| r);
    let authority = rest.split(['/', '?', '#']).next().unwrap_or("");
    let host = match authority.strip_prefix('[') {
        Some(v6) => v6.split(']').next().unwrap_or(""),
        None => authority.rsplit_once(':').map_or(authority, |(h, _)| h),
    };
    matches!(
        host.to_ascii_lowercase().as_str(),
        "localhost" | "127.0.0.1" | "::1"
    )
}

/// An OpenAI-compatible base URL from what a person types: `api.example.com` ->
/// `https://api.example.com/v1`, `localhost:11434` -> `http://localhost:11434/v1`. A URL with a
/// path keeps it (`https://gw.example/openai/v1`); a trailing `/` is dropped.
pub fn normalize_base_url(input: &str) -> Result<String, String> {
    let raw = input.trim();
    if raw.is_empty() || raw.contains(char::is_whitespace) {
        return Err(format!("`{raw}` is not an address"));
    }
    let with_scheme = if raw.contains("://") {
        raw.to_owned()
    } else {
        let scheme = if is_local(&format!("x://{raw}")) {
            "http"
        } else {
            "https"
        };
        format!("{scheme}://{raw}")
    };
    let (scheme, rest) = with_scheme.split_once("://").unwrap_or(("", ""));
    let scheme = scheme.to_ascii_lowercase();
    if !matches!(scheme.as_str(), "http" | "https") {
        return Err(format!("`{raw}` is not an http(s) address"));
    }
    let (authority, path) = match rest.find('/') {
        Some(i) => (&rest[..i], rest[i..].trim_end_matches('/')),
        None => (rest, ""),
    };
    if authority.is_empty() || authority.contains(['?', '#', '@']) {
        return Err(format!("`{raw}` has no usable host"));
    }
    let path = if path.is_empty() { "/v1" } else { path };
    Ok(format!("{scheme}://{authority}{path}"))
}

/// Asks for the endpoint address until it is usable and confirmed.
fn ask_base_url(ui: &mut dyn Ui) -> Result<String, CliError> {
    loop {
        let answer = ui.ask(
            "Endpoint address (e.g. api.example.com or https://api.example.com/v1)",
            None,
        )?;
        let url = match normalize_base_url(&answer) {
            Ok(u) => u,
            Err(e) => {
                ui.say(&format!("  {e}"));
                continue;
            }
        };
        if url != answer.trim().trim_end_matches('/')
            && !ui.confirm(&format!("Use {url}?"), true)?
        {
            continue;
        }
        if url.starts_with("http://")
            && !is_local(&url)
            && !ui.confirm(
                "That is plain http: the key would cross the network unencrypted. Use it anyway?",
                false,
            )?
        {
            continue;
        }
        return Ok(url);
    }
}

/// A name and an id that no shipped or configured provider has.
fn ask_name_and_id(ui: &mut dyn Ui, cfg: &ModelsConfig) -> Result<(String, String), CliError> {
    let name = loop {
        let n = ui.ask("Name for it (shown in the harness)", Some("My models"))?;
        if !n.trim().is_empty() {
            break n.trim().to_owned();
        }
    };
    let shipped = connect::shipped_ids();
    let id = loop {
        let default = Some(connect::slug(&name)).filter(|s| connect::valid_id(s).is_ok());
        let a = ui.ask(
            "Short id, used in model names like <id>/<model> (lowercase, digits, - and _)",
            default.as_deref(),
        )?;
        let a = a.trim().to_owned();
        match connect::valid_id(&a) {
            Ok(()) if shipped.contains(&a) => {
                ui.say(&format!("  {a} is built in; pick another id."))
            }
            Ok(()) if cfg.providers.contains_key(&a) => {
                if ui.confirm(&format!("  {a} is already set up. Replace it?"), false)? {
                    break a;
                }
            }
            Ok(()) => break a,
            Err(e) => ui.say(&format!("  {e}")),
        }
    };
    Ok((name, id))
}

/// Extra headers: names asked one by one; secret values hidden (keychain), others plain.
fn ask_headers(ui: &mut dyn Ui, spec: &mut CustomSpec) -> Result<(), CliError> {
    loop {
        let name = ui.ask(
            "Extra header the endpoint needs (name, Enter for none)",
            Some(""),
        )?;
        let name = name.trim().trim_end_matches(':').to_owned();
        if name.is_empty() {
            return Ok(());
        }
        if connect::secret_name(&name) {
            let v = ui.secret(&format!("{name} value (kept in the OS keychain)"))?;
            if !v.is_empty() {
                spec.secret_headers.insert(name, v);
            }
            continue;
        }
        let v = ui.ask(&format!("{name} value"), None)?;
        if connect::secret_value(&v) {
            ui.say(&format!(
                "  That looks like a secret: {name} goes to the OS keychain, not config.toml."
            ));
            spec.secret_headers.insert(name, v.trim().to_owned());
        } else {
            spec.headers.insert(name, v.trim().to_owned());
        }
    }
}

/// Where the endpoint's key comes from.
#[derive(Debug, Clone, PartialEq, Eq)]
enum KeyAnswer {
    /// Typed (hidden) and checked; empty = the endpoint takes none.
    Typed(String),
    /// From `<ID>_API_KEY`.
    Env(String),
    /// Not asked: no keychain to keep it in, and the variable is not set yet (#2340). The
    /// endpoint still needs one; the person sets the variable.
    Later,
}

/// The key: from `<ID>_API_KEY` when set, else asked hidden and checked against the endpoint's
/// model list (3 tries on a 401/403). Without a keychain (`no_keychain` = why) a key typed here
/// could not be kept, so it is not asked for (#2340): the endpoint is tried without one, and the
/// person is told which variable to set.
async fn ask_key(
    ui: &mut dyn Ui,
    provider: &ProviderConfig,
    name: &str,
    keyless_ok: bool,
    extra: &BTreeMap<String, String>,
    no_keychain: Option<&String>,
) -> Result<KeyAnswer, CliError> {
    if let Some(var) = &provider.key_env
        && let Some(key) = std::env::var(var).ok().filter(|v| !v.trim().is_empty())
    {
        ui.say(&format!("Using the {name} key in {var}."));
        return Ok(KeyAnswer::Env(key.trim().to_owned()));
    }
    if let Some(why) = no_keychain {
        if keyless_ok {
            match super::check_key(provider, "", extra).await {
                KeyCheck::Ok => {
                    ui.say(&format!("  {name} answered without a key."));
                    return Ok(KeyAnswer::Typed(String::new()));
                }
                KeyCheck::Untrusted(message) => ui.say(&format!("  {message}")),
                KeyCheck::Rejected(_) | KeyCheck::Unknown(_) => {}
            }
        }
        let var = provider
            .key_env
            .as_deref()
            .unwrap_or("its key_env variable");
        super::say_no_keychain(ui, why, var);
        return Ok(KeyAnswer::Later);
    }
    ask_typed_key(ui, provider, name, keyless_ok, extra)
        .await
        .map(KeyAnswer::Typed)
}

/// [`ask_key`]'s hidden prompt (3 tries). Empty = the endpoint takes none.
async fn ask_typed_key(
    ui: &mut dyn Ui,
    provider: &ProviderConfig,
    name: &str,
    keyless_ok: bool,
    extra: &BTreeMap<String, String>,
) -> Result<String, CliError> {
    for _ in 0..3 {
        let suffix = if keyless_ok {
            ", Enter if it needs none"
        } else {
            ""
        };
        let key = ui.secret(&format!(
            "{}{suffix}",
            connect::key_prompt_for(provider, name)
        ))?;
        if key.is_empty() && !keyless_ok {
            return Err(CliError::usage(format!(
                "no {name} key given: nothing stored"
            )));
        }
        if let Err(why) = connect::refuse_subscription_token(&provider.id, &key) {
            ui.say(&format!("  {why}"));
            continue;
        }
        let check = super::check_key(provider, &key, extra).await;
        if key.is_empty() {
            match &check {
                KeyCheck::Ok => ui.say(&format!("  {name} answered without a key.")),
                KeyCheck::Rejected(status) => {
                    ui.say(&format!(
                        "  {name} wants a key (HTTP {status}). Enter it, or Enter again to keep none."
                    ));
                    let again = ui.secret(&connect::key_prompt(&provider.id, name))?;
                    if again.is_empty() {
                        return Ok(String::new());
                    }
                    let check = super::check_key(provider, &again, extra).await;
                    ui.say(&format!("  {}", connect::check_note(name, &check)));
                    if let KeyCheck::Rejected(_) = check {
                        continue;
                    }
                    return Ok(again);
                }
                KeyCheck::Unknown(why) => ui.say(&format!(
                    "  Could not reach {name} ({why}); going on without a key."
                )),
                KeyCheck::Untrusted(message) => ui.say(&format!("  {message}")),
            }
            return Ok(String::new());
        }
        ui.say(&format!("  {}", connect::check_note(name, &check)));
        if let KeyCheck::Rejected(_) = check {
            continue;
        }
        return Ok(key);
    }
    Err(CliError::new(format!(
        "no {name} key was accepted; nothing was stored"
    )))
}

/// The models to add: all it lists (up to [`connect::MAX_AUTO_MODELS`]), else typed.
async fn ask_models(
    ui: &mut dyn Ui,
    provider: &ProviderConfig,
    key: &str,
    extra: &BTreeMap<String, String>,
) -> Result<Vec<String>, CliError> {
    let listed = connect::list_models(provider, key, extra, None).await;
    let ids = match listed {
        Ok(ids) if !ids.is_empty() && ids.len() <= connect::MAX_AUTO_MODELS => {
            ui.say(&format!(
                "It serves {} model{}: {}.",
                ids.len(),
                if ids.len() == 1 { "" } else { "s" },
                ids.join(", ")
            ));
            return Ok(ids);
        }
        Ok(ids) if ids.is_empty() => {
            ui.say("It lists no models.");
            ids
        }
        Ok(ids) => {
            ui.say(&format!("It serves {} models, for example:", ids.len()));
            for id in ids.iter().take(connect::PICKER_MODELS) {
                ui.say(&format!("  {id}"));
            }
            ids
        }
        Err(connect::ListError::Untrusted(m)) => {
            ui.say(&m);
            Vec::new()
        }
        Err(e) => {
            ui.say(&format!("Could not list its models ({e})."));
            Vec::new()
        }
    };
    loop {
        let a = ui.ask("Model ids to use (comma-separated)", None)?;
        let mut out: Vec<String> = a
            .split(',')
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .map(str::to_owned)
            .collect();
        out.dedup();
        let unknown: Vec<&String> = out
            .iter()
            .filter(|m| !ids.is_empty() && !ids.contains(m))
            .collect();
        if !unknown.is_empty() {
            ui.say(&format!(
                "  It does not list {}.",
                unknown
                    .iter()
                    .map(|s| s.as_str())
                    .collect::<Vec<_>>()
                    .join(", ")
            ));
            continue;
        }
        if !out.is_empty() {
            return Ok(out);
        }
        ui.say("  Name at least one model.");
    }
}

/// Which model each group runs on: the name-matched defaults first, changed one by one on "n".
pub fn ask_roles(
    ui: &mut dyn Ui,
    provider: &str,
    models: &[String],
    roles: &[String],
) -> Result<role_map::Assignment, CliError> {
    let guessed = role_map::guess(models);
    let mut per_group: BTreeMap<&str, String> = guessed
        .iter()
        .map(|(g, m)| (*g, format!("{provider}/{m}")))
        .collect();
    let show = |ui: &mut dyn Ui, per_group: &BTreeMap<&str, String>| {
        for g in role_map::GROUPS {
            ui.say(&format!(
                "  {:<10} {:<28} {} ({})",
                g.name,
                per_group[g.name],
                g.about,
                g.roles.join(", ")
            ));
        }
    };
    if models.len() == 1 {
        ui.say(&format!("Every role runs on {provider}/{}.", models[0]));
        return Ok(role_map::from_groups(&per_group, roles));
    }
    ui.say("Which model each role runs on (picked by name; you can change them):");
    show(ui, &per_group);
    if ui.confirm("Use these?", true)? {
        return Ok(role_map::from_groups(&per_group, roles));
    }
    for (i, m) in models.iter().enumerate() {
        ui.say(&format!("  {:>2}) {m}", i + 1));
    }
    for g in role_map::GROUPS {
        let current = guessed[g.name].clone();
        loop {
            let a = ui.ask(
                &format!("Model for {} ({})", g.name, g.about),
                Some(&current),
            )?;
            let a = a.trim();
            let chosen = a
                .parse::<usize>()
                .ok()
                .filter(|n| (1..=models.len()).contains(n))
                .map(|n| models[n - 1].clone())
                .or_else(|| {
                    let id = a.strip_prefix(&format!("{provider}/")).unwrap_or(a);
                    models.iter().find(|m| *m == id).cloned()
                });
            match chosen {
                Some(m) => {
                    per_group.insert(g.name, format!("{provider}/{m}"));
                    break;
                }
                None => ui.say("  Type a number or a model id from the list."),
            }
        }
    }
    ui.say("The roles will run on:");
    show(ui, &per_group);
    Ok(role_map::from_groups(&per_group, roles))
}

/// Whether `url` is an Azure OpenAI / AI Foundry endpoint.
pub fn is_azure(url: &str) -> bool {
    let rest = url.split_once("://").map_or(url, |(_, r)| r);
    let host = rest
        .split(['/', ':', '?', '#'])
        .next()
        .unwrap_or("")
        .to_ascii_lowercase();
    [
        ".openai.azure.com",
        ".cognitiveservices.azure.com",
        ".services.ai.azure.com",
    ]
    .iter()
    .any(|s| host.ends_with(s))
}

/// The newest GA Azure OpenAI api-version for classic deployment URLs.
pub const AZURE_API_VERSION: &str = "2024-10-21";

/// Azure: a bare resource address means the v1 API (`…/openai/v1`, no api-version), and a
/// deployment address keeps the deployment as `{model}` (the deployment name is the model id).
fn azure_url(url: &str) -> String {
    let base = url.trim_end_matches('/');
    if let Some(root) = base.strip_suffix("/v1")
        && !root.contains("/openai")
    {
        return format!("{root}/openai/v1");
    }
    if let Some(at) = base.find("/openai/deployments/") {
        return format!("{}/openai/deployments/{{model}}", &base[..at]);
    }
    base.to_owned()
}

/// Tries the endpoint once without credentials; on a TLS failure asks for a CA bundle or a client
/// certificate (mTLS), on no connection for a proxy. Asks nothing when it answers (any status).
async fn ask_transport(ui: &mut dyn Ui, spec: &mut CustomSpec) -> Result<(), CliError> {
    let mut asked_proxy = false;
    for _ in 0..4 {
        let mut p = spec.config().map_err(CliError::new)?;
        p.auth = ProviderAuth::None;
        p.needs_key = false;
        p.secret_headers.clear();
        let keys = StaticKeys {
            keychain: Some(BTreeMap::new()),
            ..StaticKeys::default()
        };
        let failure = match probe(&p, &keys, ProbeKind::Auth).await {
            Ok(_) => return Ok(()),
            Err(e) => e,
        };
        let lower = failure.message.to_ascii_lowercase();
        match failure.failure {
            ProbeFailure::Tls
                if [
                    "certificaterequired",
                    "certificate required",
                    "badcertificate",
                    "bad certificate",
                    "handshakefailure",
                ]
                .iter()
                .any(|n| lower.contains(n))
                    && spec.auth.client_cert.is_none() =>
            {
                ui.say("  The endpoint asks for a client certificate (mutual TLS).");
                let cert = ui.ask("Client certificate file (PEM, Enter to skip)", Some(""))?;
                if cert.trim().is_empty() {
                    return Ok(());
                }
                let key = ui.ask(
                    "Client key file (PEM, Enter if the certificate file holds it)",
                    Some(""),
                )?;
                spec.auth.client_cert = Some(absolute_path(cert.trim()));
                spec.auth.client_key = Some(key.trim().to_owned())
                    .filter(|k| !k.is_empty())
                    .map(|k| absolute_path(&k));
            }
            // #2333: an expired certificate or another host's name: a CA would not help.
            _ if matches!(
                failure.cert_problem(),
                Some(CertProblem::Expired | CertProblem::NameMismatch)
            ) =>
            {
                ui.say(&format!("  {}", cert_text(&p, &failure)));
                return Ok(());
            }
            ProbeFailure::Tls | ProbeFailure::Config if spec.auth.ca_file.is_none() => {
                if failure.cert_problem().is_some() {
                    ui.say(&format!("  {}", cert_sentence(&p, &failure)));
                } else {
                    ui.say(&format!(
                        "  Its TLS certificate is not trusted here ({}).",
                        failure.message
                    ));
                }
                let ca = ui.ask(
                    "CA bundle that signed it (PEM file, Enter to skip)",
                    Some(""),
                )?;
                if ca.trim().is_empty() {
                    return Ok(());
                }
                spec.auth.ca_file = Some(absolute_path(ca.trim()));
            }
            ProbeFailure::Network if !asked_proxy => {
                asked_proxy = true;
                ui.say(&format!("  Could not reach it ({}).", failure.message));
                let proxy = ui.ask("Proxy for it (http://host:port, Enter for none)", Some(""))?;
                if proxy.trim().is_empty() {
                    return Ok(());
                }
                spec.auth.proxy = Some(proxy.trim().to_owned());
            }
            _ => {
                if failure.cert_problem().is_some() {
                    ui.say(&format!("  {}", cert_text(&p, &failure)));
                } else if matches!(failure.failure, ProbeFailure::Tls | ProbeFailure::Config) {
                    ui.say(&format!("  Still failing: {}", failure.message));
                }
                return Ok(());
            }
        }
        // The new settings must load (a bad path is said, and asked again next round).
        if let Err(e) = spec.config() {
            ui.say(&format!("  {e}"));
            spec.auth.ca_file = None;
            spec.auth.client_cert = None;
            spec.auth.client_key = None;
            spec.auth.proxy = None;
        }
    }
    Ok(())
}

/// The certificate sentence alone (the question that follows is the fix), with the cause under
/// it only with --verbose.
fn cert_sentence(p: &ProviderConfig, failure: &ProbeError) -> String {
    let Some(problem) = failure.cert_problem() else {
        return failure.message.clone();
    };
    let mut text = connect::cert_sentence(p, problem);
    if crate::conn::verbose() {
        text.push_str(&format!(
            "\n    cause: {}",
            amp_mcp::redact_urls(&failure.message)
        ));
    }
    text
}

/// The certificate sentence and its fix ([`connect::cert_message`]).
fn cert_text(p: &ProviderConfig, failure: &ProbeError) -> String {
    connect::cert_message(p, failure).unwrap_or_else(|| failure.message.clone())
}

fn absolute_path(p: &str) -> String {
    let path = match p.strip_prefix("~/") {
        Some(rest) => std::env::var_os("HOME")
            .map(|h| std::path::PathBuf::from(h).join(rest))
            .unwrap_or_else(|| p.into()),
        None => std::path::PathBuf::from(p),
    };
    std::path::absolute(&path)
        .unwrap_or(path)
        .display()
        .to_string()
}

/// "How does your endpoint authenticate?" and only the questions that choice needs.
fn ask_auth(ui: &mut dyn Ui, spec: &mut CustomSpec) -> Result<(), CliError> {
    let azure = is_azure(&spec.base_url);
    ui.say("How does your endpoint authenticate?");
    ui.say("  1) An API key sent as a bearer token (Authorization: Bearer …) — most gateways");
    ui.say("  2) An API key in a header of its own (Azure OpenAI's api-key, X-Api-Key, …)");
    ui.say("  3) No authentication (a local or internal server)");
    ui.say("  4) A command that prints a token (an SSO helper, Vault, az …)");
    ui.say("  5) OAuth 2.0 client credentials (a client id and secret, a token endpoint)");
    ui.say("  6) Microsoft Entra ID (Azure OpenAI / AI Foundry)");
    let default = if azure { "2" } else { "1" };
    let choice = loop {
        let a = ui.ask("Authentication", Some(default))?;
        match a.trim() {
            n @ ("1" | "2" | "3" | "4" | "5" | "6") => break n.to_owned(),
            _ => ui.say("  Type a number from 1 to 6."),
        }
    };
    match choice.as_str() {
        "1" => {}
        "2" => {
            spec.auth.kind = "header".to_owned();
            let h = ui.ask("Header name", Some("api-key"))?;
            let h = h.trim().trim_end_matches(':');
            spec.auth.auth_header = Some(if h.is_empty() { "api-key" } else { h }.to_owned());
        }
        "3" => {
            spec.auth.kind = "none".to_owned();
            spec.needs_key = false;
        }
        "4" => {
            spec.auth.kind = "command".to_owned();
            loop {
                let c = ui.ask(
                    "Command that prints a token (program and arguments, e.g. vault read -field=token secret/llm)",
                    None,
                )?;
                let argv: Vec<String> = c.split_whitespace().map(str::to_owned).collect();
                if !argv.is_empty() {
                    spec.auth.auth_command = argv;
                    break;
                }
            }
        }
        "5" => {
            spec.auth.kind = "oauth-client-credentials".to_owned();
            loop {
                let u = ui.ask("Token endpoint URL", None)?;
                if u.trim().starts_with("https://") || is_local(u.trim()) {
                    spec.auth.token_url = Some(u.trim().to_owned());
                    break;
                }
                ui.say("  It must be an https:// URL (http only for localhost).");
            }
            spec.auth.client_id = Some(ask_nonempty(ui, "Client id")?);
            let scopes = ui.ask("Scopes (space-separated, Enter for none)", Some(""))?;
            spec.auth.scopes = scopes.split_whitespace().map(str::to_owned).collect();
        }
        _ => {
            spec.auth.kind = "azure-entra".to_owned();
            let tenant = ui.ask(
                "Tenant id for a service principal (Enter to use your Azure CLI login, `az login`)",
                Some(""),
            )?;
            if !tenant.trim().is_empty() {
                spec.auth.tenant_id = Some(tenant.trim().to_owned());
                spec.auth.client_id = Some(ask_nonempty(ui, "Client (application) id")?);
            }
        }
    }
    Ok(())
}

fn ask_nonempty(ui: &mut dyn Ui, prompt: &str) -> Result<String, CliError> {
    loop {
        let a = ui.ask(prompt, None)?;
        if !a.trim().is_empty() {
            return Ok(a.trim().to_owned());
        }
    }
}

/// The whole "own endpoint" conversation (or OpenAI's, with `openai = true`). Nothing is stored
/// yet: the caller writes the provider, the roles and the key ([`store`]).
pub async fn ask(
    ui: &mut dyn Ui,
    cfg: &ModelsConfig,
    openai: bool,
    no_keychain: Option<&String>,
) -> Result<Endpoint, CliError> {
    let mut spec = if openai {
        connect::openai_preset()
    } else {
        ui.say(
            "Your own OpenAI-compatible endpoint: a company gateway, vLLM, LiteLLM, Ollama, LM \
             Studio… Its models get mapped to the agent roles.",
        );
        let (name, id) = ask_name_and_id(ui, cfg)?;
        let mut base_url = ask_base_url(ui)?;
        if is_azure(&base_url) {
            let azure = azure_url(&base_url);
            if azure != base_url {
                ui.say(&format!(
                    "  Azure OpenAI: using {azure}{}",
                    if azure.contains("{model}") {
                        " (the deployment name is the model id)"
                    } else {
                        " (the v1 API: deployment names are the model ids)"
                    }
                ));
                base_url = azure;
            }
        }
        CustomSpec {
            key_env: Some(CustomSpec::default_key_env(&id)),
            id,
            name: Some(name),
            base_url,
            ..CustomSpec::default()
        }
    };
    ask_transport(ui, &mut spec).await?;
    if openai {
        // OpenAI's optional organization and project headers.
        let org = ui.ask("OpenAI organization id (Enter for your default)", Some(""))?;
        spec.auth.organization = Some(org.trim().to_owned()).filter(|o| !o.is_empty());
        let project = ui.ask("OpenAI project id (Enter for your default)", Some(""))?;
        spec.auth.project = Some(project.trim().to_owned()).filter(|p| !p.is_empty());
    } else {
        ask_auth(ui, &mut spec)?;
        if spec.base_url.contains("/deployments/") && is_azure(&spec.base_url) {
            let v = ui.ask("Azure api-version", Some(AZURE_API_VERSION))?;
            let v = if v.trim().is_empty() {
                AZURE_API_VERSION
            } else {
                v.trim()
            };
            spec.auth
                .query
                .insert("api-version".to_owned(), v.to_owned());
        }
        ask_headers(ui, &mut spec)?;
    }
    let provider = spec.config().map_err(CliError::new)?;
    let name = super::display_name(&provider);
    let extra = spec.secret_headers.clone();
    let answer = if provider.needs_key || provider.auth == ProviderAuth::Bearer {
        let keyless = !openai && provider.auth == ProviderAuth::Bearer;
        ask_key(ui, &provider, &name, keyless, &extra, no_keychain).await?
    } else {
        // none / a token command / Entra through az: nothing to ask; say whether it answers.
        let check = super::check_key(&provider, "", &extra).await;
        ui.say(&format!("  {}", connect::check_note(&name, &check)));
        KeyAnswer::Typed(String::new())
    };
    // Later: the endpoint needs a key the person has yet to set; it stays `needs_key`.
    let later = answer == KeyAnswer::Later;
    let (key, key_from_env) = match answer {
        KeyAnswer::Typed(k) => (k, false),
        KeyAnswer::Env(k) => (k, true),
        KeyAnswer::Later => (String::new(), false),
    };
    if key.is_empty() && provider.auth == ProviderAuth::Bearer && !later {
        spec.needs_key = false;
    }
    let provider = spec.config().map_err(CliError::new)?;
    spec.models = ask_models(ui, &provider, &key, &extra).await?;
    let roles = role_map::all_roles(cfg);
    let assignment = ask_roles(ui, &spec.id, &spec.models, &roles)?;
    Ok(Endpoint {
        spec,
        key,
        key_from_env,
        assignment,
    })
}

/// Writes the provider table and the roles, and keeps the key (and secret header values) in the
/// OS keychain. Returns a note when the key could not be kept (no keychain): the person must set
/// the provider's variable instead.
pub fn store(
    ui: &mut dyn Ui,
    config: &super::ConfigFile,
    e: &Endpoint,
) -> Result<Option<String>, CliError> {
    let name = e
        .spec
        .name
        .clone()
        .unwrap_or_else(|| super::provider_name(&e.spec.id));
    let key = if e.key_from_env { "" } else { e.key.as_str() };
    let mut unstored = None;
    match connect::store_secrets(
        &e.spec.id,
        key,
        &e.spec.secret_headers,
        e.spec.key_env.as_deref(),
    ) {
        Ok(()) => {
            if !key.is_empty() {
                ui.say(&format!(
                    "Stored the {name} key in the OS keychain (service {}, account {}).",
                    amp_mcp::keychain_service(),
                    amp_core::providers::keychain_account(&e.spec.id)
                ));
            }
        }
        Err(why) => unstored = Some(why),
    }
    e.spec.write(config).map_err(CliError::new)?;
    role_map::write(config, &e.assignment).map_err(CliError::new)?;
    ui.say(&format!(
        "Saved {name} ([providers.{}], {} model{}) and the roles in {}.",
        e.spec.id,
        e.spec.models.len(),
        if e.spec.models.len() == 1 { "" } else { "s" },
        config.path.display()
    ));
    Ok(unstored)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base_urls_are_normalised() {
        let n = |s| normalize_base_url(s).unwrap();
        assert_eq!(n("api.example.com"), "https://api.example.com/v1");
        assert_eq!(n("api.example.com/"), "https://api.example.com/v1");
        assert_eq!(
            n("https://api.example.com/v1/"),
            "https://api.example.com/v1"
        );
        assert_eq!(
            n("https://gw.example/openai/v1"),
            "https://gw.example/openai/v1"
        );
        assert_eq!(n("localhost:11434"), "http://localhost:11434/v1");
        assert_eq!(n("127.0.0.1:8000/v1"), "http://127.0.0.1:8000/v1");
        assert_eq!(n("HTTPS://api.example.com"), "https://api.example.com/v1");
        assert_eq!(n("http://10.0.0.5:8000"), "http://10.0.0.5:8000/v1");
        for bad in ["", "a b", "ftp://x", "https://", "https://u@h"] {
            assert!(normalize_base_url(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn local_hosts() {
        assert!(is_local("http://localhost:1/v1"));
        assert!(is_local("http://127.0.0.1/v1"));
        assert!(is_local("http://[::1]:8/v1"));
        assert!(!is_local("http://api.example.com/v1"));
        assert!(!is_local("http://localhost.example.com/v1"));
    }
}
