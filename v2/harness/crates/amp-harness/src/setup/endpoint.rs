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

use std::collections::BTreeMap;

use amp_core::models::{ModelsConfig, ProviderConfig};

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

/// The key: from `<ID>_API_KEY` when set, else asked hidden and checked against the endpoint's
/// model list (3 tries on a 401/403). Empty = the endpoint takes none.
async fn ask_key(
    ui: &mut dyn Ui,
    provider: &ProviderConfig,
    name: &str,
    keyless_ok: bool,
    extra: &BTreeMap<String, String>,
) -> Result<(String, bool), CliError> {
    if let Some(var) = &provider.key_env
        && let Some(key) = std::env::var(var).ok().filter(|v| !v.trim().is_empty())
    {
        ui.say(&format!("Using the {name} key in {var}."));
        return Ok((key.trim().to_owned(), true));
    }
    for _ in 0..3 {
        let suffix = if keyless_ok {
            ", Enter if it needs none"
        } else {
            ""
        };
        let key = ui.secret(&format!(
            "{}{suffix}",
            connect::key_prompt(&provider.id, name)
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
                        return Ok((String::new(), false));
                    }
                    let check = super::check_key(provider, &again, extra).await;
                    ui.say(&format!("  {}", connect::check_note(name, &check)));
                    if let KeyCheck::Rejected(_) = check {
                        continue;
                    }
                    return Ok((again, false));
                }
                KeyCheck::Unknown(why) => ui.say(&format!(
                    "  Could not reach {name} ({why}); going on without a key."
                )),
            }
            return Ok((String::new(), false));
        }
        ui.say(&format!("  {}", connect::check_note(name, &check)));
        if let KeyCheck::Rejected(_) = check {
            continue;
        }
        return Ok((key, false));
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
    let listed = connect::list_models(provider, key, extra).await;
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

/// The whole "own endpoint" conversation (or OpenAI's, with `openai = true`). Nothing is stored
/// yet: the caller writes the provider, the roles and the key ([`store`]).
pub async fn ask(ui: &mut dyn Ui, cfg: &ModelsConfig, openai: bool) -> Result<Endpoint, CliError> {
    let mut spec = if openai {
        connect::openai_preset()
    } else {
        ui.say(
            "Your own OpenAI-compatible endpoint: a company gateway, vLLM, LiteLLM, Ollama, LM \
             Studio… Its models get mapped to the agent roles.",
        );
        let (name, id) = ask_name_and_id(ui, cfg)?;
        let base_url = ask_base_url(ui)?;
        CustomSpec {
            key_env: Some(CustomSpec::default_key_env(&id)),
            id,
            name: Some(name),
            base_url,
            ..CustomSpec::default()
        }
    };
    if !openai {
        ask_headers(ui, &mut spec)?;
    }
    let provider = spec.config().map_err(CliError::new)?;
    let name = super::display_name(&provider);
    let extra = spec.secret_headers.clone();
    let (key, key_from_env) = ask_key(ui, &provider, &name, !openai, &extra).await?;
    if key.is_empty() {
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
