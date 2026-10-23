//! Org presets: `amp-harness setup --preset <file|https-url>` (#2275).
//!
//! An admin ships one small TOML file with **no secrets**: the AMP server, the company's
//! OpenAI-compatible endpoint(s) with their models, which model each role runs on, and optionally
//! MCP servers. Setup applies it to the person's config.toml, then asks each person only for
//! their own key(s) and their AMP login. See `v2/harness/examples/enterprise-preset.toml`.
//!
//! ```toml
//! [preset]
//! name = "Example Corp"                    # optional, shown when applied
//!
//! [server]                                 # optional
//! url = "https://amp.example.com"          # like the first run's answer (/mcp is added)
//!
//! [providers.example]
//! name = "Example AI"
//! base_url = "https://api.example.com/v1"  # api.example.com works too
//! models = ["engineer", "orchestrator"]    # or [providers.example.models.engineer] tables
//! # api = "openai-completions"   key_env = "EXAMPLE_API_KEY"
//! # auth = "bearer" (or "key") | "header" | "none" | "command" | "oauth-client-credentials"
//! #        | "azure-entra"   (#2277; with auth_header, auth_command, token_url, client_id,
//! #        scopes, tenant_id, authority_host as config.toml takes them; secrets are asked)
//! # organization = "org-..."  project = "proj_..."  query = { "api-version" = "2024-10-21" }
//! # ca_file = "/etc/ssl/corp-ca.pem"  client_cert = "..."  client_key = "..."  proxy = "..."
//! # headers = { "X-Team" = "platform" }    secret_headers = ["X-Api-Key"]  (values asked)
//!
//! [roles]                                  # a group, a role or `default` -> model
//! default = "example/engineer"
//! manager = "example/orchestrator"
//! reviewers = "example/orchestrator"
//!
//! [mcp.docs]                               # optional: the config.toml [mcp.<name>] schema
//! type = "http"
//! url = "https://mcp.example.com/mcp"
//! ```
//!
//! Validation: unknown keys are refused with the key's name; anything that looks like a secret
//! (a key-like setting, a header value, a token-shaped string) is refused with where it is; URLs
//! must be https unless they point at this machine. A preset URL is fetched only over https (or
//! http to localhost), with a size cap.

use std::{collections::BTreeMap, path::PathBuf, time::Duration};

use amp_core::{
    mcp_config::{self, Layer, Origin, Severity},
    models::ModelsConfig,
};
use serde::Deserialize;

use super::{ConfigFile, endpoint, role_map};
use crate::{
    conn::CliError,
    connect::{self, CustomSpec},
};

/// The largest preset accepted (it is a few dozen lines).
const MAX_PRESET_BYTES: usize = 256 * 1024;
const FETCH_TIMEOUT: Duration = Duration::from_secs(15);

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Preset {
    #[serde(default)]
    pub preset: PresetInfo,
    pub server: Option<PresetServer>,
    #[serde(default)]
    pub providers: BTreeMap<String, PresetProvider>,
    /// Role, group or `default` -> `provider/model` (or a bare model id with one provider).
    #[serde(default)]
    pub roles: BTreeMap<String, String>,
    /// `[mcp.<name>]` tables, checked by [`mcp_config::parse_server`].
    #[serde(default)]
    pub mcp: toml::Table,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PresetInfo {
    pub name: Option<String>,
    pub description: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PresetServer {
    pub url: Option<String>,
    pub web_url: Option<String>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PresetProvider {
    pub name: Option<String>,
    pub base_url: String,
    pub api: Option<String>,
    /// `bearer`/`key` (default), `header`, `none`, `command`, `oauth-client-credentials`,
    /// `azure-entra` (#2277).
    pub auth: Option<String>,
    pub auth_header: Option<String>,
    pub auth_command: Option<Vec<String>>,
    pub auth_command_timeout_secs: Option<u64>,
    pub token_url: Option<String>,
    pub client_id: Option<String>,
    #[serde(default)]
    pub scopes: Vec<String>,
    pub tenant_id: Option<String>,
    pub authority_host: Option<String>,
    pub organization: Option<String>,
    pub project: Option<String>,
    #[serde(default)]
    pub query: BTreeMap<String, String>,
    /// Paths on each person's machine (no key material: that is refused).
    pub ca_file: Option<String>,
    pub client_cert: Option<String>,
    pub client_key: Option<String>,
    pub proxy: Option<String>,
    pub key_env: Option<String>,
    #[serde(default)]
    pub headers: BTreeMap<String, String>,
    #[serde(default)]
    pub secret_headers: Vec<String>,
    pub models: PresetModels,
}

#[derive(Debug, Deserialize)]
#[serde(untagged)]
pub enum PresetModels {
    List(Vec<String>),
    /// id -> the config.toml model settings (`context_window`, `max_tokens`, ...).
    Table(BTreeMap<String, toml::Table>),
}

/// A checked preset, ready to apply.
#[derive(Debug)]
pub struct Checked {
    /// Where it came from (a path or URL), for messages.
    pub source: String,
    pub name: Option<String>,
    pub description: Option<String>,
    /// The AMP server's MCP URL.
    pub server_url: Option<String>,
    pub web_url: Option<String>,
    pub providers: Vec<CustomSpec>,
    /// Role words -> models, resolved against the config once the providers are in it.
    pub roles: Vec<(String, String)>,
    /// MCP server name -> its table.
    pub mcp: BTreeMap<String, toml::Value>,
}

/// What applying did, for the summary.
#[derive(Debug, Default, serde::Serialize)]
pub struct Applied {
    pub source: String,
    pub name: Option<String>,
    pub description: Option<String>,
    pub providers: Vec<String>,
    pub roles: BTreeMap<String, String>,
    pub mcp: Vec<String>,
    pub server: Option<String>,
    /// MCP secrets still to store (no person or no keychain to ask, #2243), each with the
    /// command that stores it. Never a value.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub mcp_secrets_needed: Vec<String>,
}

fn bad(source: &str, message: impl std::fmt::Display) -> CliError {
    CliError::usage(format!("preset {source}: {message}"))
}

/// Reads the preset from a file or an https URL (http only for localhost).
pub async fn read(source: &str) -> Result<String, CliError> {
    let s = source.trim();
    if s.contains("://") {
        let lower = s.to_ascii_lowercase();
        if !(lower.starts_with("https://") || lower.starts_with("http://") && endpoint::is_local(s))
        {
            return Err(bad(
                s,
                "only https:// URLs (or http:// to localhost) are fetched",
            ));
        }
        let http = amp_mcp::build_http_client(None).map_err(|e| bad(s, e))?;
        let resp = tokio::time::timeout(FETCH_TIMEOUT, http.get(s).timeout(FETCH_TIMEOUT).send())
            .await
            .map_err(|_| bad(s, "timed out"))?
            .map_err(|e| bad(s, format!("could not fetch it ({e})")))?;
        if !resp.status().is_success() {
            return Err(bad(
                s,
                format!("the server answered HTTP {}", resp.status()),
            ));
        }
        let bytes = resp
            .bytes()
            .await
            .map_err(|e| bad(s, format!("could not read it ({e})")))?;
        if bytes.len() > MAX_PRESET_BYTES {
            return Err(bad(s, "it is larger than 256 KiB; not a preset"));
        }
        return String::from_utf8(bytes.to_vec()).map_err(|_| bad(s, "it is not UTF-8 text"));
    }
    let path = PathBuf::from(s);
    let meta = std::fs::metadata(&path).map_err(|e| bad(s, format!("can't read it ({e})")))?;
    if meta.len() as usize > MAX_PRESET_BYTES {
        return Err(bad(s, "it is larger than 256 KiB; not a preset"));
    }
    std::fs::read_to_string(&path).map_err(|e| bad(s, format!("can't read it ({e})")))
}

/// Setting names that would hold a secret (`api_key`, `token`, `password`, ...).
fn secret_setting(name: &str) -> bool {
    let n = name.to_ascii_lowercase().replace('-', "_");
    if matches!(
        n.as_str(),
        "key_env" | "secret_headers" | "secret_env" | "bearer_env" | "client_secret"
            // #2277: a path to a key file, not a key (PEM content is refused below).
            | "client_key" | "auth_header"
    ) {
        return false;
    }
    n == "key"
        || n == "token"
        || n == "secret"
        || n.contains("password")
        || n.contains("api_key")
        || n.contains("apikey")
        || n.ends_with("_key")
        || n.ends_with("_token")
        || n.ends_with("_secret")
}

/// Refuses anything secret-shaped outside `[mcp]` (which checks its own): a key-like setting
/// name, or a token-shaped string value. Names the dotted key, never the value.
fn refuse_secrets(source: &str, path: &str, value: &toml::Value) -> Result<(), CliError> {
    let advice = "presets never carry secrets: each person enters their own key at setup, and it \
                  is kept in their OS keychain";
    match value {
        toml::Value::Table(t) => {
            for (k, v) in t {
                let here = if path.is_empty() {
                    k.clone()
                } else {
                    format!("{path}.{k}")
                };
                if here == "mcp" {
                    continue;
                }
                // Header names are names; their values are checked below.
                let in_headers = path.ends_with(".headers");
                if !in_headers && secret_setting(k) {
                    return Err(bad(source, format!("`{here}`: {advice}")));
                }
                if in_headers
                    && let toml::Value::String(s) = v
                    && (connect::secret_name(k) || connect::secret_value(s))
                {
                    return Err(bad(
                        source,
                        format!(
                            "`{here}` looks like a secret: list the header in secret_headers \
                             instead (each person is asked for its value, kept in their OS \
                             keychain)"
                        ),
                    ));
                }
                refuse_secrets(source, &here, v)?;
            }
        }
        toml::Value::Array(items) => {
            for (i, v) in items.iter().enumerate() {
                refuse_secrets(source, &format!("{path}[{i}]"), v)?;
            }
        }
        toml::Value::String(s)
            if mcp_config::looks_secret_value(s) || s.contains("PRIVATE KEY-----") =>
        {
            return Err(bad(
                source,
                format!("`{path}` looks like a secret: {advice}"),
            ));
        }
        _ => {}
    }
    Ok(())
}

/// https, or http to this machine.
fn secure_url(source: &str, key: &str, url: &str) -> Result<(), CliError> {
    if url.starts_with("https://") || endpoint::is_local(url) {
        return Ok(());
    }
    Err(bad(
        source,
        format!("`{key}`: only https:// addresses are allowed (http only for localhost)"),
    ))
}

/// Parses and checks a preset's text. `source` names it in messages.
pub fn check(text: &str, source: &str) -> Result<Checked, CliError> {
    let raw: toml::Table = toml::from_str(text)
        .map_err(|e| bad(source, format!("not valid TOML: {}", e.message().trim())))?;
    refuse_secrets(source, "", &toml::Value::Table(raw.clone()))?;
    let preset: Preset = toml::from_str(text).map_err(|e| {
        let m = e.message().trim().to_owned();
        let m = mcp_config_redact(&m);
        bad(source, m)
    })?;

    let mut out = Checked {
        source: source.to_owned(),
        name: preset.preset.name.clone(),
        description: preset.preset.description.clone(),
        server_url: None,
        web_url: None,
        providers: Vec::new(),
        roles: Vec::new(),
        mcp: BTreeMap::new(),
    };
    if let Some(server) = &preset.server {
        if let Some(url) = &server.url {
            let url = super::normalize_server_url(url)
                .map_err(|e| bad(source, format!("`server.url`: {e}")))?;
            secure_url(source, "server.url", &url)?;
            out.server_url = Some(url);
        }
        if let Some(web) = &server.web_url {
            let web = super::origin_of(web.trim())
                .trim_end_matches('/')
                .to_owned();
            if !web.starts_with("http://") && !web.starts_with("https://") {
                return Err(bad(source, "`server.web_url`: not an http(s) address"));
            }
            secure_url(source, "server.web_url", &web)?;
            out.web_url = Some(web);
        }
    }

    let shipped = connect::shipped_ids();
    for (id, p) in &preset.providers {
        let key = |k: &str| format!("providers.{id}.{k}");
        connect::valid_id(id).map_err(|e| bad(source, format!("`providers.{id}`: {e}")))?;
        if shipped.contains(id) {
            return Err(bad(
                source,
                format!("`providers.{id}`: {id} is built in; give the endpoint its own id"),
            ));
        }
        let base_url = endpoint::normalize_base_url(&p.base_url)
            .map_err(|e| bad(source, format!("`{}`: {e}", key("base_url"))))?;
        secure_url(source, &key("base_url"), &base_url)?;
        let api = match &p.api {
            Some(a) => {
                connect::parse_api(a).map_err(|e| bad(source, format!("`{}`: {e}", key("api"))))?
            }
            None => amp_core::models::ProviderApi::OpenaiCompletions,
        };
        let kind = p
            .auth
            .as_deref()
            .map(|a| a.trim().to_ascii_lowercase())
            .unwrap_or_default();
        let needs_key = kind != "none";
        let kind = if kind == "none" || kind == "key" {
            String::new()
        } else {
            kind
        };
        for (k, url) in [
            ("token_url", &p.token_url),
            ("authority_host", &p.authority_host),
        ] {
            if let Some(url) = url {
                secure_url(source, &key(k), url)?;
            }
        }
        if let Some(cmd) = &p.auth_command
            && cmd.iter().any(|a| mcp_config::looks_secret_value(a))
        {
            return Err(bad(
                source,
                format!("`{}` looks like it holds a secret", key("auth_command")),
            ));
        }
        let auth = connect::AuthSpec {
            kind,
            auth_header: p.auth_header.clone(),
            auth_command: p.auth_command.clone().unwrap_or_default(),
            token_url: p.token_url.clone(),
            client_id: p.client_id.clone(),
            scopes: p.scopes.clone(),
            tenant_id: p.tenant_id.clone(),
            organization: p.organization.clone(),
            project: p.project.clone(),
            query: p.query.clone(),
            ca_file: p.ca_file.clone(),
            client_cert: p.client_cert.clone(),
            client_key: p.client_key.clone(),
            proxy: p.proxy.clone(),
            authority_host: p.authority_host.clone(),
            auth_command_timeout_secs: p.auth_command_timeout_secs,
            client_key_pem: None,
        };
        if let Some(var) = &p.key_env
            && (var.is_empty()
                || !var
                    .chars()
                    .all(|c| c.is_ascii_uppercase() || c.is_ascii_digit() || c == '_'))
        {
            return Err(bad(
                source,
                format!(
                    "`{}`: an environment variable name like EXAMPLE_API_KEY",
                    key("key_env")
                ),
            ));
        }
        let (models, model_specs) = match &p.models {
            PresetModels::List(ids) => (ids.clone(), BTreeMap::new()),
            PresetModels::Table(t) => (t.keys().cloned().collect(), t.clone()),
        };
        if models.is_empty() || models.iter().any(|m| m.trim().is_empty()) {
            return Err(bad(
                source,
                format!("`{}`: name at least one model", key("models")),
            ));
        }
        let mut secret_headers = BTreeMap::new();
        for h in &p.secret_headers {
            if h.trim().is_empty() {
                return Err(bad(
                    source,
                    format!("`{}`: an empty header name", key("secret_headers")),
                ));
            }
            // The value is asked from each person at setup.
            secret_headers.insert(h.trim().to_owned(), String::new());
        }
        let spec = CustomSpec {
            id: id.clone(),
            name: p.name.clone(),
            base_url,
            api,
            key_env: Some(
                p.key_env
                    .clone()
                    .unwrap_or_else(|| CustomSpec::default_key_env(id)),
            ),
            needs_key,
            headers: p.headers.clone(),
            secret_headers,
            models,
            model_specs,
            note: Some(format!(
                "From the org preset{}.",
                preset
                    .preset
                    .name
                    .as_deref()
                    .map(|n| format!(" \"{n}\""))
                    .unwrap_or_default()
            )),
            auth,
            merge: false,
        };
        // It must load as config.toml would (model settings included).
        spec.config().map_err(|e| {
            bad(
                source,
                format!("`providers.{id}`: {}", mcp_config_redact(&e)),
            )
        })?;
        out.providers.push(spec);
    }

    for (word, model) in &preset.roles {
        if model.trim().is_empty() {
            return Err(bad(source, format!("`roles.{word}`: name a model")));
        }
        out.roles.push((word.clone(), model.clone()));
    }
    // The role words and models, against the defaults plus the preset's providers.
    let blocks: Vec<String> = out.providers.iter().map(CustomSpec::block).collect();
    let file = amp_core::models::ModelsFile::parse(&blocks.join("\n"), "the preset")
        .map_err(|e| bad(source, mcp_config_redact(&e.0)))?;
    let cfg = ModelsConfig::from_files(
        amp_core::models::ModelsFile::defaults(),
        Some(file),
        "preset",
        &|_| None,
    )
    .map_err(|e| bad(source, mcp_config_redact(&e.0)))?;
    assignment(&out, &cfg)?;

    for (name, value) in &preset.mcp {
        let origin = Origin {
            layer: Layer::User,
            file: PathBuf::from(source),
        };
        match mcp_config::parse_server(name, value, origin) {
            Ok((_, problems)) => {
                if let Some(p) = problems.iter().find(|p| p.severity == Severity::Error) {
                    return Err(bad(source, format!("`{}`: {}", p.key, p.message)));
                }
            }
            Err(problems) => {
                let p = &problems[0];
                let fix = p
                    .fix
                    .as_deref()
                    .map(|f| format!(" (fix: {f})"))
                    .unwrap_or_default();
                return Err(bad(source, format!("`{}`: {}{fix}", p.key, p.message)));
            }
        }
        out.mcp.insert(name.clone(), value.clone());
    }
    Ok(out)
}

/// serde's quoted values (`string "..."`) out of a message (a preset may hold a typo'd secret).
fn mcp_config_redact(message: &str) -> String {
    let mut out = String::new();
    let mut rest = message;
    while let Some(i) = rest.find("string \"") {
        out.push_str(&rest[..i]);
        out.push_str("a string");
        let after = &rest[i + 8..];
        rest = after.find('"').map_or("", |j| &after[j + 1..]);
    }
    out.push_str(rest);
    out
}

/// Resolves the preset's `[roles]` against the config with its providers in it.
fn assignment(
    checked: &Checked,
    cfg: &ModelsConfig,
) -> Result<Option<role_map::Assignment>, CliError> {
    if checked.roles.is_empty() {
        return Ok(None);
    }
    let only = (checked.providers.len() == 1).then(|| checked.providers[0].id.clone());
    let ids: Vec<String> = cfg.providers.keys().cloned().collect();
    let knows = |p: &str, m: &str| cfg.knows_model(&format!("{p}/{m}"));
    let resolve = |m: &str| role_map::model_ref(m, only.as_deref(), &knows, &ids);
    // `default` first, so the explicit words win over it in order.
    let mut pairs = checked.roles.clone();
    pairs.sort_by_key(|(w, _)| {
        !matches!(
            w.trim().to_ascii_lowercase().as_str(),
            "default" | "all" | "*"
        )
    });
    let roles = role_map::all_roles(cfg);
    // Roles the preset doesn't name go to its default (or stay on its provider).
    let provider = only.clone().unwrap_or_default();
    let current = if provider.is_empty() {
        BTreeMap::new()
    } else {
        role_map::current(cfg)
    };
    let models: Vec<String> = cfg
        .providers
        .get(&provider)
        .map(|p| p.models.keys().map(|m| format!("{provider}/{m}")).collect())
        .unwrap_or_default();
    role_map::from_pairs(&pairs, &roles, &resolve, &provider, &models, &current)
        .map(Some)
        .map_err(|e| bad(&checked.source, format!("[roles]: {e}")))
}

/// The `[mcp.<name>]` block for config.toml.
fn mcp_block(name: &str, value: &toml::Value, source: &str) -> Result<String, CliError> {
    let mut inner = toml::Table::new();
    inner.insert(name.to_owned(), value.clone());
    let mut outer = toml::Table::new();
    outer.insert("mcp".to_owned(), toml::Value::Table(inner));
    let text = toml::to_string(&outer).map_err(|e| bad(source, e))?;
    // The comment goes right under the server's own header.
    let header = format!("[mcp.{}]", super::config_file::toml_key(name));
    let note = format!("# From the org preset ({source}).");
    Ok(match text.find(&header) {
        Some(i) => {
            let end = i + header.len();
            format!("{}\n{note}{}", &text[..end], &text[end..])
        }
        None => text,
    })
}

/// Writes the preset into `config`: the providers, the role mapping and the MCP servers. The
/// server is left to setup's server step (it is tried before it is saved). Every write is checked
/// to load first; nothing secret is written.
pub fn apply(checked: &Checked, config: &ConfigFile) -> Result<Applied, CliError> {
    let mut applied = Applied {
        source: checked.source.clone(),
        name: checked.name.clone(),
        description: checked.description.clone(),
        server: checked.server_url.clone(),
        ..Applied::default()
    };
    for spec in &checked.providers {
        spec.write(config).map_err(CliError::new)?;
        applied.providers.push(spec.id.clone());
    }
    let cfg = super::load_models(config, None)?;
    if let Some(a) = assignment(checked, &cfg)? {
        role_map::write(config, &a).map_err(CliError::new)?;
        applied.roles = a.roles.clone();
    }
    for (name, value) in &checked.mcp {
        let block = mcp_block(name, value, &checked.source)?;
        let wanted = name.clone();
        let check = move |text: &str| -> Result<(), String> {
            let doc: toml::Table = toml::from_str(text).map_err(|e| e.message().to_owned())?;
            let Some(toml::Value::Table(mcp)) = doc.get("mcp") else {
                return Err("[mcp] is missing".to_owned());
            };
            let file = std::path::Path::new("config.toml");
            let parsed = mcp_config::McpConfig::from_tables(Some((mcp, file)), None);
            if parsed.servers.contains_key(&wanted) {
                Ok(())
            } else {
                Err(parsed
                    .problems
                    .first()
                    .map(|p| p.to_string())
                    .unwrap_or_else(|| format!("[mcp.{wanted}] did not load")))
            }
        };
        config
            .replace_tables(&format!("mcp.{name}"), Some(&block), &check)
            .map_err(CliError::new)?;
        applied.mcp.push(name.clone());
    }
    Ok(applied)
}

#[cfg(test)]
mod tests {
    use super::*;

    const EXAMPLE: &str = include_str!("../../../../examples/enterprise-preset.toml");

    #[test]
    fn the_shipped_example_checks() {
        let c = check(EXAMPLE, "enterprise-preset.toml").unwrap();
        assert_eq!(c.providers.len(), 1);
        let p = &c.providers[0];
        assert_eq!(p.id, "example");
        assert_eq!(p.base_url, "https://api.example.com/v1");
        assert_eq!(
            p.models,
            vec!["engineer".to_owned(), "orchestrator".to_owned()]
        );
        assert_eq!(c.server_url.as_deref(), Some("https://amp.example.com/mcp"));
        assert!(
            c.roles
                .iter()
                .any(|(w, m)| w == "manager" && m.ends_with("orchestrator"))
        );
    }

    fn err(text: &str) -> String {
        check(text, "p.toml").unwrap_err().message
    }

    const OK_PROVIDER: &str =
        "[providers.ex]\nbase_url = \"api.example.com\"\nmodels = [\"engineer\"]\n";

    #[test]
    fn bad_presets_are_refused_clearly() {
        let planted = "sk-live-0123456789abcdef";
        for (text, want) in [
            ("[presets]\nname = \"x\"\n", "unknown field `presets`"),
            (
                &format!("{OK_PROVIDER}flavour = \"x\"\n") as &str,
                "unknown field `flavour`",
            ),
            (
                &format!("{OK_PROVIDER}api_key = \"{planted}\"\n"),
                "`providers.ex.api_key`: presets never carry secrets",
            ),
            (
                &format!("{OK_PROVIDER}headers = {{ \"Authorization\" = \"Bearer {planted}\" }}\n"),
                "`providers.ex.headers.Authorization` looks like a secret",
            ),
            (
                &format!("[preset]\nname = \"{planted}\"\n"),
                "`preset.name` looks like a secret",
            ),
            (
                "[providers.ex]\nbase_url = \"http://api.example.com\"\nmodels = [\"m\"]\n",
                "only https:// addresses",
            ),
            (
                "[server]\nurl = \"http://amp.example.com\"\n",
                "`server.url`: only https://",
            ),
            (
                "[providers.openrouter]\nbase_url = \"https://x\"\nmodels = [\"m\"]\n",
                "openrouter is built in",
            ),
            (
                "[providers.ex]\nbase_url = \"https://x\"\nmodels = []\n",
                "name at least one model",
            ),
            (
                &format!("{OK_PROVIDER}[roles]\nboss = \"ex/engineer\"\n"),
                "unknown role `boss`",
            ),
            (
                "[mcp.Bad]\ntype = \"http\"\nurl = \"https://x/mcp\"\n",
                "`mcp.Bad`",
            ),
            (
                &format!(
                    "[mcp.gh]\ntype = \"http\"\nurl = \"https://x/mcp\"\nheaders = {{ \"Authorization\" = \"Bearer {planted}\" }}\n"
                ),
                "`mcp.gh.headers",
            ),
            ("not toml", "not valid TOML"),
        ] {
            let e = err(text);
            assert!(e.contains(want), "{text}\n=> {e}");
            assert!(!e.contains(planted), "a secret in the message: {e}");
            assert!(e.starts_with("preset p.toml: "), "{e}");
        }
    }

    #[test]
    fn localhost_may_use_http_and_model_tables_load() {
        let c = check(
            "[providers.local]\nbase_url = \"localhost:11434\"\nauth = \"none\"\n\
             [providers.local.models.big]\ncontext_window = 200000\nmax_tokens = 8192\n",
            "p.toml",
        )
        .unwrap();
        let p = c.providers[0].config().unwrap();
        assert_eq!(p.base_url, "http://localhost:11434/v1");
        assert!(!p.needs_key);
        assert_eq!(p.models["big"].context_window, 200000);
        let bad = err(
            "[providers.local]\nbase_url = \"localhost:1\"\n[providers.local.models.big]\nwindow = 1\n",
        );
        assert!(bad.contains("window"), "{bad}");
    }

    #[test]
    fn mcp_blocks_round_trip() {
        let v: toml::Value = toml::from_str::<toml::Table>(
            "type = \"stdio\"\ncommand = \"npx\"\nargs = [\"-y\", \"x\"]\nenv = { A = \"b\" }\n",
        )
        .unwrap()
        .into();
        let block = mcp_block("fs", &v, "p.toml").unwrap();
        assert!(
            block.starts_with("[mcp.fs]\n# From the org preset (p.toml)."),
            "{block}"
        );
        let doc: toml::Table = toml::from_str(&block).unwrap();
        assert_eq!(doc["mcp"]["fs"]["command"].as_str(), Some("npx"));
    }

    #[test]
    fn auth_and_transport_settings_round_trip_and_key_material_is_refused() {
        let text = "[providers.az]\nbase_url = \"https://r.openai.azure.com/openai/deployments/{model}\"\n\
                    auth = \"header\"\nquery = { \"api-version\" = \"2024-10-21\" }\n\
                    models = [\"gpt-4o\"]\n\
                    [providers.sso]\nbase_url = \"https://gw.example/v1\"\nauth = \"command\"\n\
                    auth_command = [\"corp-sso\", \"token\", \"--json\"]\n\
                    ca_file = \"/etc/ssl/corp-ca.pem\"\nclient_cert = \"/etc/ssl/me.pem\"\n\
                    client_key = \"/etc/ssl/me.key\"\nproxy = \"http://proxy.example:3128\"\n\
                    organization = \"org-1\"\nmodels = [\"m\"]\n\
                    [providers.ent]\nbase_url = \"https://e.openai.azure.com/openai/v1\"\n\
                    auth = \"azure-entra\"\ntenant_id = \"t-1\"\nclient_id = \"app-1\"\n\
                    models = [\"gpt-4o\"]\n\
                    [providers.cc]\nbase_url = \"https://cc.example/v1\"\n\
                    auth = \"oauth-client-credentials\"\ntoken_url = \"https://idp.example/token\"\n\
                    client_id = \"amp\"\nscopes = [\"api://cc/.default\"]\nmodels = [\"m\"]\n";
        let checked = check(text, "p.toml").unwrap();
        let by_id = |id: &str| {
            checked
                .providers
                .iter()
                .find(|p| p.id == id)
                .unwrap()
                .config()
                .unwrap()
        };
        let az = by_id("az");
        assert_eq!(az.auth.name(), "header");
        assert_eq!(az.query["api-version"], "2024-10-21");
        let sso = by_id("sso");
        assert_eq!(sso.auth.name(), "command");
        assert!(!sso.needs_key);
        assert_eq!(
            sso.tls.client_key.as_deref(),
            Some(std::path::Path::new("/etc/ssl/me.key"))
        );
        assert_eq!(sso.headers["OpenAI-Organization"], "org-1");
        let ent = by_id("ent");
        assert!(ent.needs_key, "the client secret is asked at setup");
        assert_eq!(ent.auth.key_word(), "client secret");
        assert!(by_id("cc").needs_key);

        let pem = "-----BEGIN PRIVATE KEY-----\\nMIIEvQ\\n-----END PRIVATE KEY-----";
        for (bad, want) in [
            (
                format!("{OK_PROVIDER}client_cert = \"/c.pem\"\nclient_key = \"{pem}\"\n"),
                "`providers.ex.client_key` looks like a secret",
            ),
            (
                format!(
                    "{OK_PROVIDER}auth = \"command\"\nauth_command = [\"t\", \"sk-abcdefghijklmnopqrstuvwx1234\"]\n"
                ),
                "looks like a secret",
            ),
            (
                format!(
                    "{OK_PROVIDER}auth = \"oauth-client-credentials\"\ntoken_url = \"http://idp.example/t\"\nclient_id = \"a\"\n"
                ),
                "only https://",
            ),
            (
                format!("{OK_PROVIDER}auth = \"magic\"\n"),
                "unknown auth `magic`",
            ),
        ] {
            let e = err(&bad);
            assert!(e.contains(want), "{bad}: {e}");
            assert!(!e.contains("MIIEvQ") && !e.contains("sk-abcdef"), "{e}");
        }
    }
}
