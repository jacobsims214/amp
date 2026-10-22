//! Live rpi providers built from [`crate::models::ModelsConfig`]: keys, HTTP clients, stall guard,
//! and the faux provider for tests.
//!
//! Keys (D3): the provider's `key_env` variable first, then the OS keychain (service `amp-harness`,
//! account `provider:<id>`). The keychain is skipped when `AMP_CREDENTIAL_STORE=file`. A key is
//! handed only to its own provider (rpi's `*_without_env_api_key` constructors), never logged, and
//! never printed: [`KeySource`] describes where a key came from, not what it is.
//!
//! Every provider is wrapped in [`ConfiguredProvider`]. It gives the provider its config id (so two
//! providers of the same api, or two faux providers, can be active at once), applies the request
//! deadline and retry count, and runs a stall guard. If no event arrives within `header_timeout`,
//! or the gap between events exceeds `stream_idle_timeout`, the request is cancelled and the stream
//! ends with an `Error` that keeps the partial message (rpi has only a whole-request timeout, and
//! DeepSeek stalls mid-stream).

use std::{
    collections::BTreeMap,
    fmt,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use async_trait::async_trait;
use rpi_ai::{
    AssistantMessage, AssistantMessageEvent, AssistantMessageEventStream, Context, ErrorReason,
    Model, Provider, SimpleStreamOptions, StopReason,
    event_stream::create_assistant_message_event_stream,
    providers::{
        anthropic::AnthropicProvider,
        faux::{FauxProvider, FauxScript},
        openai_completions::OpenAiCompletionsProvider,
    },
};

use crate::models::{
    ModelsConfig, ModelsError, ProviderApi, ProviderConfig, ResolvedRole, Timeouts,
};

/// Keychain account prefix for provider keys (service [`amp_mcp::KEYCHAIN_SERVICE`]).
pub const KEYCHAIN_ACCOUNT_PREFIX: &str = "provider:";

/// The keychain account a provider's key is stored under.
pub fn keychain_account(provider: &str) -> String {
    format!("{KEYCHAIN_ACCOUNT_PREFIX}{provider}")
}

/// The keychain account a provider's secret header value is stored under
/// (`provider:<id>:header:<name, lowercase>`; header names are case-insensitive).
pub fn header_account(provider: &str, header: &str) -> String {
    format!(
        "{KEYCHAIN_ACCOUNT_PREFIX}{provider}:header:{}",
        header.trim().to_ascii_lowercase()
    )
}

/// A provider key. `Debug` and `Display` are redacted.
#[derive(Clone)]
pub struct Secret(String);

impl Secret {
    pub fn new(value: impl Into<String>) -> Option<Self> {
        let value = value.into();
        let trimmed = value.trim();
        (!trimmed.is_empty()).then(|| Self(trimmed.to_owned()))
    }

    pub fn expose(&self) -> &str {
        &self.0
    }
}

impl fmt::Debug for Secret {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("Secret(<redacted>)")
    }
}

/// Where a provider's key came from. Never the key itself.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum KeySource {
    Env(String),
    Keychain,
    /// `auth = "none"` or faux.
    NotNeeded,
    Missing {
        env: Option<String>,
        keychain_checked: bool,
    },
    /// The keychain was asked and failed (denied, needs approval nobody can give, no keychain).
    /// The message says what to do; it never holds a secret. Counts as missing.
    KeychainFailed(String),
}

impl fmt::Display for KeySource {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Env(var) => write!(f, "env {var}"),
            Self::Keychain => f.write_str("keychain"),
            Self::NotNeeded => f.write_str("not needed"),
            Self::Missing {
                env,
                keychain_checked,
            } => {
                f.write_str("missing (")?;
                match env {
                    Some(var) => write!(f, "set {var}")?,
                    None => f.write_str("no key_env")?,
                }
                if *keychain_checked {
                    f.write_str(", or store it with `amp-harness connect`")?;
                } else {
                    f.write_str("; keychain off")?;
                }
                f.write_str(")")
            }
            Self::KeychainFailed(why) => write!(f, "keychain: {why}"),
        }
    }
}

impl KeySource {
    pub fn is_missing(&self) -> bool {
        matches!(self, Self::Missing { .. } | Self::KeychainFailed(_))
    }

    /// Short machine-readable form: `env`, `keychain`, `not-needed`, `missing`.
    pub fn kind(&self) -> &'static str {
        match self {
            Self::Env(_) => "env",
            Self::Keychain => "keychain",
            Self::NotNeeded => "not-needed",
            Self::Missing { .. } => "missing",
            Self::KeychainFailed(_) => "keychain-error",
        }
    }
}

/// What the keychain said about a provider's key.
#[derive(Clone, PartialEq, Eq)]
pub enum KeychainKey {
    /// The keychain is not consulted (`AMP_CREDENTIAL_STORE=file`, tests).
    Off,
    NotFound,
    Found(String),
    /// The lookup failed; the message says what to do.
    Failed(String),
}

impl fmt::Debug for KeychainKey {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Off => f.write_str("Off"),
            Self::NotFound => f.write_str("NotFound"),
            Self::Found(_) => f.write_str("Found(<redacted>)"),
            Self::Failed(why) => f.debug_tuple("Failed").field(why).finish(),
        }
    }
}

/// Where keys are looked up. Tests use a map; the binary uses [`SystemKeys`].
pub trait KeyLookup: Send + Sync {
    fn env(&self, var: &str) -> Option<String>;
    fn keychain(&self, provider: &str) -> KeychainKey;
    /// The value of a provider's secret header (`secret_headers`), from the keychain.
    fn secret_header(&self, provider: &str, header: &str) -> KeychainKey {
        let _ = (provider, header);
        KeychainKey::Off
    }
}

/// Process environment + the OS keychain (unless `AMP_CREDENTIAL_STORE=file`).
#[derive(Debug, Clone)]
pub struct SystemKeys {
    keychain_service: Option<String>,
}

impl SystemKeys {
    pub fn from_env() -> Self {
        let file_only = std::env::var(amp_mcp::ENV_CREDENTIAL_STORE)
            .map(|v| v.trim().eq_ignore_ascii_case("file"))
            .unwrap_or(false);
        Self {
            keychain_service: (!file_only).then(amp_mcp::keychain_service),
        }
    }

    /// Environment only.
    pub fn env_only() -> Self {
        Self {
            keychain_service: None,
        }
    }
}

impl KeyLookup for SystemKeys {
    fn env(&self, var: &str) -> Option<String> {
        std::env::var(var).ok()
    }

    /// Through amp-mcp's process-wide cache: each provider's entry is read at most once per
    /// process, however many times keys are resolved (#2165).
    fn keychain(&self, provider: &str) -> KeychainKey {
        let Some(service) = self.keychain_service.as_deref() else {
            return KeychainKey::Off;
        };
        match amp_mcp::keychain_get(service, &keychain_account(provider)) {
            Ok(Some(key)) => KeychainKey::Found(key),
            Ok(None) => KeychainKey::NotFound,
            // No keychain on this machine (headless Linux): the same as no key there.
            Err(e) if e.kind == amp_mcp::KeychainErrorKind::Unavailable => KeychainKey::NotFound,
            Err(e) => KeychainKey::Failed(e.message),
        }
    }

    fn secret_header(&self, provider: &str, header: &str) -> KeychainKey {
        let Some(service) = self.keychain_service.as_deref() else {
            return KeychainKey::Off;
        };
        match amp_mcp::keychain_get(service, &header_account(provider, header)) {
            Ok(Some(value)) => KeychainKey::Found(value),
            Ok(None) => KeychainKey::NotFound,
            Err(e) if e.kind == amp_mcp::KeychainErrorKind::Unavailable => KeychainKey::NotFound,
            Err(e) => KeychainKey::Failed(e.message),
        }
    }
}

/// A fixed set of env values and keychain entries (tests).
#[derive(Debug, Clone, Default)]
pub struct StaticKeys {
    pub env: BTreeMap<String, String>,
    pub keychain: Option<BTreeMap<String, String>>,
}

impl KeyLookup for StaticKeys {
    fn env(&self, var: &str) -> Option<String> {
        self.env.get(var).cloned()
    }

    fn keychain(&self, provider: &str) -> KeychainKey {
        match &self.keychain {
            None => KeychainKey::Off,
            Some(k) => k
                .get(provider)
                .cloned()
                .map_or(KeychainKey::NotFound, KeychainKey::Found),
        }
    }

    /// Entries keyed `<provider>:header:<name, lowercase>`.
    fn secret_header(&self, provider: &str, header: &str) -> KeychainKey {
        let account = header_account(provider, header);
        let short = account
            .strip_prefix(KEYCHAIN_ACCOUNT_PREFIX)
            .unwrap_or(&account);
        match &self.keychain {
            None => KeychainKey::Off,
            Some(k) => k
                .get(short)
                .cloned()
                .map_or(KeychainKey::NotFound, KeychainKey::Found),
        }
    }
}

/// The values of a provider's secret headers, or why one is missing (never a value).
pub fn resolve_secret_headers(
    provider: &ProviderConfig,
    keys: &dyn KeyLookup,
) -> Result<BTreeMap<String, String>, String> {
    let mut out = BTreeMap::new();
    for name in &provider.secret_headers {
        match keys.secret_header(&provider.id, name) {
            KeychainKey::Found(value) if !value.trim().is_empty() => {
                out.insert(name.clone(), value.trim().to_owned());
            }
            KeychainKey::Failed(why) => return Err(format!("header {name}: keychain: {why}")),
            KeychainKey::Off => {
                return Err(format!(
                    "header {name} is kept in the OS keychain, which is off here"
                ));
            }
            _ => {
                return Err(format!(
                    "no value for header {name} (run `amp-harness connect {}`)",
                    provider.id
                ));
            }
        }
    }
    Ok(out)
}

/// Find a provider's key: `key_env`, then the keychain.
pub fn resolve_key(provider: &ProviderConfig, keys: &dyn KeyLookup) -> (Option<Secret>, KeySource) {
    if !provider.needs_key {
        return (None, KeySource::NotNeeded);
    }
    if let Some(var) = &provider.key_env
        && let Some(secret) = keys.env(var).and_then(Secret::new)
    {
        return (Some(secret), KeySource::Env(var.clone()));
    }
    let keychain_checked = match keys.keychain(&provider.id) {
        KeychainKey::Found(key) => match Secret::new(key) {
            Some(secret) => return (Some(secret), KeySource::Keychain),
            None => true,
        },
        KeychainKey::Failed(why) => return (None, KeySource::KeychainFailed(why)),
        KeychainKey::NotFound => true,
        KeychainKey::Off => false,
    };
    (
        None,
        KeySource::Missing {
            env: provider.key_env.clone(),
            keychain_checked,
        },
    )
}

/// The providers a session can use, keyed by config id (`Model::provider`).
#[derive(Clone, Default)]
pub struct ProviderSet {
    providers: BTreeMap<String, Arc<ConfiguredProvider>>,
    keys: BTreeMap<String, KeySource>,
    unavailable: BTreeMap<String, String>,
    faux: BTreeMap<String, FauxScript>,
}

impl fmt::Debug for ProviderSet {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("ProviderSet")
            .field("available", &self.providers.keys().collect::<Vec<_>>())
            .field("keys", &self.keys)
            .field("unavailable", &self.unavailable)
            .finish()
    }
}

impl ProviderSet {
    /// Build every configured provider that has what it needs. A provider without a key is
    /// recorded as unavailable, not an error: only the roles that use it fail.
    pub fn build(config: &ModelsConfig, keys: &dyn KeyLookup) -> Self {
        let mut set = Self::default();
        for provider in config.providers.values() {
            let (key, source) = resolve_key(provider, keys);
            set.keys.insert(provider.id.clone(), source.clone());
            if source.is_missing() {
                set.unavailable
                    .insert(provider.id.clone(), format!("no key: {source}"));
                continue;
            }
            let extra = match resolve_secret_headers(provider, keys) {
                Ok(extra) => extra,
                Err(why) => {
                    set.unavailable.insert(provider.id.clone(), why);
                    continue;
                }
            };
            match build_inner(provider, key) {
                Ok((inner, faux)) => {
                    if let Some(script) = faux {
                        set.faux.insert(provider.id.clone(), script);
                    }
                    let mut configured = ConfiguredProvider::new(provider, inner);
                    configured.extra_headers = extra;
                    set.providers
                        .insert(provider.id.clone(), Arc::new(configured));
                }
                Err(e) => {
                    set.unavailable.insert(provider.id.clone(), e);
                }
            }
        }
        set
    }

    /// The same providers rebuilt from `config` with the keys as they are now (after `amp-harness
    /// connect` stored one, or a new provider was added). Faux providers are kept as they are, so
    /// their scripts survive.
    pub fn rebuild(&self, config: &ModelsConfig, keys: &dyn KeyLookup) -> Self {
        let mut next = Self::build(config, keys);
        for (id, script) in &self.faux {
            if let Some(p) = self.providers.get(id) {
                next.providers.insert(id.clone(), Arc::clone(p));
                next.faux.insert(id.clone(), script.clone());
                next.unavailable.remove(id);
            }
        }
        next
    }

    /// The provider registry for `AgentHarnessOptions::models`.
    pub fn all(&self) -> Vec<Arc<dyn Provider>> {
        self.providers
            .values()
            .map(|p| p.clone() as Arc<dyn Provider>)
            .collect()
    }

    pub fn get(&self, id: &str) -> Result<Arc<dyn Provider>, ModelsError> {
        if let Some(p) = self.providers.get(id) {
            return Ok(p.clone());
        }
        Err(ModelsError(match self.unavailable.get(id) {
            Some(why) => format!("provider `{id}` is unavailable: {why}"),
            None => format!("unknown provider `{id}`"),
        }))
    }

    /// The provider serving a resolved role.
    pub fn for_role(&self, role: &ResolvedRole) -> Result<Arc<dyn Provider>, ModelsError> {
        self.get(&role.provider)
            .map_err(|e| ModelsError(format!("role `{}`: {e}", role.role)))
    }

    pub fn key_source(&self, id: &str) -> Option<&KeySource> {
        self.keys.get(id)
    }

    pub fn unavailable(&self) -> &BTreeMap<String, String> {
        &self.unavailable
    }

    /// The scripted response queue of a faux provider (`api = "faux"`).
    pub fn faux_script(&self, id: &str) -> Option<&FauxScript> {
        self.faux.get(id)
    }

    /// The ids of the available faux providers.
    pub fn faux_ids(&self) -> Vec<String> {
        self.faux.keys().cloned().collect()
    }
}

type Inner = (Arc<dyn Provider>, Option<FauxScript>);

fn build_inner(provider: &ProviderConfig, key: Option<Secret>) -> Result<Inner, String> {
    let models: Vec<Model> = provider.models.values().cloned().collect();
    if provider.api == ProviderApi::Faux {
        let script = FauxScript::new();
        let faux: Arc<dyn Provider> = FauxProvider::with_models(script.clone(), models);
        return Ok((faux, Some(script)));
    }
    let mut builder = rpi_ai::http::client_builder_for(&provider.base_url, None, None)?;
    if let Some(connect) = provider.timeouts.connect {
        builder = builder.connect_timeout(connect);
    }
    let http = builder.build().map_err(|e| format!("http client: {e}"))?;
    let key = key.map(|k| k.expose().to_owned());
    let inner: Arc<dyn Provider> = match provider.api {
        ProviderApi::AnthropicMessages => Arc::new(
            AnthropicProvider::with_models_without_env_api_key(key, http, models),
        ),
        ProviderApi::OpenaiCompletions if provider.needs_key => {
            Arc::new(OpenAiCompletionsProvider::with_models_without_env_api_key(
                provider.id.clone(),
                key,
                http,
                models,
            ))
        }
        ProviderApi::OpenaiCompletions => Arc::new(
            OpenAiCompletionsProvider::with_models_without_auth(provider.id.clone(), http, models),
        ),
        ProviderApi::Faux => unreachable!("handled above"),
    };
    Ok((inner, None))
}

/// A provider under its config id, with the configured deadline, retries and stall guard.
pub struct ConfiguredProvider {
    id: String,
    inner: Arc<dyn Provider>,
    /// Secret header values (`secret_headers`, from the keychain), added to every request.
    /// Never logged or shown.
    extra_headers: BTreeMap<String, String>,
    models: Vec<Model>,
    timeouts: Timeouts,
    max_retries: Option<u32>,
}

impl ConfiguredProvider {
    pub fn new(config: &ProviderConfig, inner: Arc<dyn Provider>) -> Self {
        Self {
            id: config.id.clone(),
            inner,
            extra_headers: BTreeMap::new(),
            models: config.models.values().cloned().collect(),
            timeouts: config.timeouts,
            max_retries: config.max_retries,
        }
    }

    pub fn timeouts(&self) -> Timeouts {
        self.timeouts
    }
}

#[async_trait]
impl Provider for ConfiguredProvider {
    fn id(&self) -> &str {
        &self.id
    }

    fn models(&self) -> &[Model] {
        &self.models
    }

    async fn stream_simple(
        &self,
        model: &Model,
        ctx: &Context,
        opts: &SimpleStreamOptions,
    ) -> AssistantMessageEventStream {
        let mut opts = opts.clone();
        if let Some(request) = self.timeouts.request {
            opts.timeout = Some(request);
        }
        if opts.max_retries.is_none() {
            opts.max_retries = self.max_retries;
        }
        // Our own child token, so a stall cancels this request without cancelling the caller.
        if !self.extra_headers.is_empty() {
            let headers = opts.headers.get_or_insert_with(Default::default);
            for (name, value) in &self.extra_headers {
                headers.insert(name.clone(), value.clone());
            }
        }
        let cancel = opts.signal.child_token();
        opts.signal = cancel.clone();
        let inner = self.inner.stream_simple(model, ctx, &opts).await;
        if self.timeouts.header.is_none() && self.timeouts.stream_idle.is_none() {
            return inner;
        }
        guard_stream(inner, cancel, self.timeouts, &self.id, model)
    }
}

fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or_default()
}

fn partial_of(event: &AssistantMessageEvent) -> Option<&Arc<AssistantMessage>> {
    use AssistantMessageEvent as E;
    match event {
        E::Start { partial }
        | E::TextStart { partial, .. }
        | E::TextDelta { partial, .. }
        | E::TextEnd { partial, .. }
        | E::ThinkingStart { partial, .. }
        | E::ThinkingDelta { partial, .. }
        | E::ThinkingEnd { partial, .. }
        | E::ToolCallStart { partial, .. }
        | E::ToolCallDelta { partial, .. }
        | E::ToolCallEnd { partial, .. } => Some(partial),
        E::Done { .. } | E::Error { .. } => None,
    }
}

/// Forward `inner` to a new stream, ending it with an error when it stalls.
fn guard_stream(
    mut inner: AssistantMessageEventStream,
    cancel: tokio_util::sync::CancellationToken,
    timeouts: Timeouts,
    provider: &str,
    model: &Model,
) -> AssistantMessageEventStream {
    let (mut producer, consumer) = create_assistant_message_event_stream();
    let provider = provider.to_owned();
    let model = model.clone();
    tokio::spawn(async move {
        let mut started = false;
        let mut last: Option<Arc<AssistantMessage>> = None;
        loop {
            let (limit, what) = if started {
                (timeouts.stream_idle, "stream_idle_timeout_secs")
            } else {
                (timeouts.header, "header_timeout_secs")
            };
            let next = match limit {
                Some(limit) => match tokio::time::timeout(limit, inner.next()).await {
                    Ok(next) => next,
                    Err(_) => {
                        cancel.cancel();
                        producer.push(stall_error(&provider, &model, last.as_deref(), limit, what));
                        return;
                    }
                },
                None => inner.next().await,
            };
            let Some(event) = next else { return };
            started = true;
            if let Some(partial) = partial_of(&event) {
                last = Some(partial.clone());
            }
            let terminal = event.is_terminal();
            if !producer.push(event) {
                // The consumer went away.
                cancel.cancel();
                return;
            }
            if terminal {
                return;
            }
        }
    });
    consumer
}

fn stall_error(
    provider: &str,
    model: &Model,
    partial: Option<&AssistantMessage>,
    limit: Duration,
    what: &str,
) -> AssistantMessageEvent {
    let message = if what == "header_timeout_secs" {
        format!(
            "{provider}/{}: no response within {limit:?} ({what}); request cancelled",
            model.id
        )
    } else {
        format!(
            "{provider}/{}: stream stalled, no data for {limit:?} ({what}); request cancelled",
            model.id
        )
    };
    let mut error = match partial {
        Some(p) => p.clone(),
        None => AssistantMessage::empty(model.api.clone(), provider, model.id.clone(), now_ms()),
    };
    error.stop_reason = StopReason::Error;
    error.error_message = Some(message);
    AssistantMessageEvent::Error {
        reason: ErrorReason::Error,
        error,
    }
}

/// Send a request built on rpi's reqwest client and decode the JSON body. A macro because rpi-ai
/// does not re-export its reqwest (0.12) and we don't depend on that major ourselves.
macro_rules! get_json {
    ($request:expr) => {
        async {
            let response = $request.send().await.map_err(|e| format!("request: {e}"))?;
            let status = response.status();
            let body = response.text().await.map_err(|e| format!("read: {e}"))?;
            if !status.is_success() {
                let snippet: String = body.chars().take(200).collect();
                return Err::<serde_json::Value, String>(format!("HTTP {status}: {snippet}"));
            }
            serde_json::from_str::<serde_json::Value>(&body).map_err(|e| format!("decode: {e}"))
        }
    };
}
/// The model ids a provider serves right now, from its own catalog endpoint: `GET /v1/models`
/// (Anthropic; needs the key) or `GET /models` (OpenAI-compatible; OpenRouter's is public). For
/// `amp-harness config show --check` and doctor. Faux returns its configured models.
pub async fn live_model_ids(
    provider: &ProviderConfig,
    keys: &dyn KeyLookup,
) -> Result<std::collections::BTreeSet<String>, String> {
    if provider.api == ProviderApi::Faux {
        return Ok(provider.models.keys().cloned().collect());
    }
    let (key, source) = resolve_key(provider, keys);
    let secret_headers = resolve_secret_headers(provider, keys)?;
    let http = rpi_ai::http::client_builder_for(&provider.base_url, None, None)?
        .timeout(Duration::from_secs(30))
        .build()
        .map_err(|e| format!("http client: {e}"))?;
    let mut ids = std::collections::BTreeSet::new();
    match provider.api {
        ProviderApi::AnthropicMessages => {
            let Some(key) = key else {
                return Err(format!("skipped: key {source}"));
            };
            let mut after: Option<String> = None;
            loop {
                let mut url = format!("{}/v1/models?limit=1000", provider.base_url);
                if let Some(after) = &after {
                    url.push_str("&after_id=");
                    url.push_str(after);
                }
                let mut request = http
                    .get(&url)
                    .header("x-api-key", key.expose())
                    .header("anthropic-version", "2023-06-01");
                for (name, value) in provider.headers.iter().chain(secret_headers.iter()) {
                    request = request.header(name.as_str(), value.as_str());
                }
                let page = get_json!(request).await?;
                collect_ids(&page, &mut ids);
                let more = page["has_more"].as_bool().unwrap_or(false);
                match page["last_id"].as_str() {
                    Some(last) if more => after = Some(last.to_owned()),
                    _ => break,
                }
            }
        }
        ProviderApi::OpenaiCompletions => {
            let mut request = http.get(format!("{}/models", provider.base_url));
            for (name, value) in provider.headers.iter().chain(secret_headers.iter()) {
                request = request.header(name.as_str(), value.as_str());
            }
            if let Some(key) = &key {
                request = request.bearer_auth(key.expose());
            }
            collect_ids(&get_json!(request).await?, &mut ids);
        }
        ProviderApi::Faux => unreachable!("handled above"),
    }
    Ok(ids)
}

fn collect_ids(page: &serde_json::Value, ids: &mut std::collections::BTreeSet<String>) {
    if let Some(data) = page["data"].as_array() {
        ids.extend(
            data.iter()
                .filter_map(|m| m["id"].as_str().map(str::to_owned)),
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::models::ModelsFile;
    use rpi_ai::{Message, UserMessage, providers::faux::FauxStep};

    fn config(user: &str) -> ModelsConfig {
        let user = ModelsFile::parse(user, "test").unwrap();
        ModelsConfig::from_files(ModelsFile::defaults(), Some(user), "config test", &|_| None)
            .unwrap()
    }

    #[test]
    fn secret_headers_come_from_the_keychain_or_the_provider_is_unavailable() {
        let c = config(
            "[providers.gw]\nname = \"Gateway\"\napi = \"openai-completions\"\n\
             base_url = \"http://gw.invalid/v1\"\nauth = \"none\"\n\
             headers = { \"X-Title\" = \"AMP\" }\nsecret_headers = [\"X-Api-Key\"]\n\
             [providers.gw.models.\"m\"]\n",
        );
        let gw = &c.providers["gw"];
        assert_eq!(gw.name.as_deref(), Some("Gateway"));
        assert_eq!(gw.headers["X-Title"], "AMP");
        assert_eq!(gw.secret_headers, ["X-Api-Key"]);
        assert_eq!(
            header_account("gw", "X-Api-Key"),
            "provider:gw:header:x-api-key"
        );

        // No value stored: unavailable, with the way to fix it (and never a value).
        let none = StaticKeys {
            keychain: Some(BTreeMap::new()),
            ..StaticKeys::default()
        };
        let set = ProviderSet::build(&c, &none);
        let why = set.unavailable()["gw"].clone();
        assert!(why.contains("amp-harness connect gw"), "{why}");
        // The keychain off: says so.
        let set = ProviderSet::build(&c, &StaticKeys::default());
        assert!(set.unavailable()["gw"].contains("keychain"), "{set:?}");

        // Stored: available; the value is never in Debug.
        let keys = StaticKeys {
            keychain: Some(BTreeMap::from([(
                "gw:header:x-api-key".to_owned(),
                "HDR-TOPSECRET".to_owned(),
            )])),
            ..StaticKeys::default()
        };
        assert_eq!(
            resolve_secret_headers(gw, &keys).unwrap()["X-Api-Key"],
            "HDR-TOPSECRET"
        );
        let set = ProviderSet::build(&c, &keys);
        assert!(set.get("gw").is_ok(), "{set:?}");
        assert!(!format!("{set:?}").contains("TOPSECRET"));
    }

    #[test]
    fn rebuild_keeps_faux_scripts_and_picks_up_new_keys() {
        let c = config(TWO_FAUX);
        let set = ProviderSet::build(&c, &StaticKeys::default());
        assert!(set.get("openrouter").is_err());
        let script = set.faux_script("alpha").unwrap().clone();
        let keys = StaticKeys {
            env: BTreeMap::from([("OPENROUTER_API_KEY".to_owned(), "sk-or-x".to_owned())]),
            keychain: None,
        };
        let next = set.rebuild(&c, &keys);
        assert!(next.get("openrouter").is_ok());
        // The same faux provider (and script) as before.
        assert!(Arc::ptr_eq(
            &(set.get("alpha").unwrap()),
            &(next.get("alpha").unwrap())
        ));
        script.set_responses(vec![]);
        assert_eq!(next.faux_script("alpha").unwrap().pending_count(), 0);
    }

    /// Two faux providers (offline) for two roles.
    const TWO_FAUX: &str = r#"
[providers.alpha]
api = "faux"
header_timeout_secs = 5
[providers.alpha.models."alpha-1"]
[providers.beta]
api = "faux"
[providers.beta.models."beta-1"]
reasoning = true

[roles.manager]
model = "alpha/alpha-1"
[roles.worker-backend]
model = "beta/beta-1"
thinking = "high"
"#;

    fn context() -> Context {
        Context {
            system_prompt: Some("test".into()),
            messages: vec![Message::User(UserMessage::new("hi", 0))],
            tools: Vec::new(),
        }
    }

    async fn reply(provider: &Arc<dyn Provider>, model: &Model) -> AssistantMessage {
        provider
            .stream_simple(model, &context(), &SimpleStreamOptions::default())
            .await
            .result()
            .await
            .unwrap()
    }

    fn text_of(message: &AssistantMessage) -> String {
        message
            .content
            .iter()
            .filter_map(|c| match c {
                rpi_ai::Content::Text(t) => Some(t.text.clone()),
                _ => None,
            })
            .collect()
    }

    #[tokio::test]
    async fn two_roles_on_two_providers() {
        let c = config(TWO_FAUX);
        let set = ProviderSet::build(&c, &StaticKeys::default());
        let manager = c.resolve_role("manager").unwrap();
        let worker = c.resolve_role("amp-worker-backend").unwrap();
        assert_eq!(manager.provider, "alpha");
        assert_eq!(worker.provider, "beta");
        assert_eq!(worker.thinking, rpi_ai::ThinkingLevel::High);

        set.faux_script("alpha")
            .unwrap()
            .set_responses(vec![FauxStep::text("from alpha")]);
        set.faux_script("beta")
            .unwrap()
            .set_responses(vec![FauxStep::text("from beta")]);

        let p_manager = set.for_role(&manager).unwrap();
        let p_worker = set.for_role(&worker).unwrap();
        assert_eq!(p_manager.id(), "alpha");
        assert_eq!(p_worker.id(), "beta");
        assert_eq!(
            text_of(&reply(&p_manager, &manager.model).await),
            "from alpha"
        );
        assert_eq!(text_of(&reply(&p_worker, &worker.model).await), "from beta");

        // The registry for AgentHarnessOptions::models holds both, keyed by Model::provider.
        let ids: Vec<String> = set.all().iter().map(|p| p.id().to_owned()).collect();
        assert!(ids.contains(&"alpha".to_owned()) && ids.contains(&"beta".to_owned()));
    }

    #[tokio::test]
    async fn providers_without_a_key_are_unavailable_not_fatal() {
        let c = config(TWO_FAUX);
        let set = ProviderSet::build(&c, &StaticKeys::default());
        assert!(set.get("alpha").is_ok());
        let e = set.get("openrouter").err().unwrap().0;
        assert!(e.contains("OPENROUTER_API_KEY"), "{e}");
        assert!(!e.contains("sk-"), "{e}");
        let reviewer = c.resolve_role("reviewer").unwrap();
        assert!(set.for_role(&reviewer).is_err());
    }

    #[test]
    fn keys_come_from_env_then_keychain_and_are_redacted() {
        let c = config("");
        let anthropic = &c.providers["anthropic"];
        let openrouter = &c.providers["openrouter"];
        let keys = StaticKeys {
            env: BTreeMap::from([("ANTHROPIC_API_KEY".into(), "sk-ant-secret".into())]),
            keychain: Some(BTreeMap::from([(
                "openrouter".into(),
                "sk-or-secret".into(),
            )])),
        };
        let (key, source) = resolve_key(anthropic, &keys);
        assert_eq!(source, KeySource::Env("ANTHROPIC_API_KEY".into()));
        assert_eq!(key.as_ref().unwrap().expose(), "sk-ant-secret");
        assert_eq!(format!("{key:?}"), "Some(Secret(<redacted>))");
        let (_, source) = resolve_key(openrouter, &keys);
        assert_eq!(source, KeySource::Keychain);

        let none = StaticKeys::default();
        let (key, source) = resolve_key(openrouter, &none);
        assert!(key.is_none());
        assert_eq!(
            source.to_string(),
            "missing (set OPENROUTER_API_KEY; keychain off)"
        );

        let set = ProviderSet::build(&c, &keys);
        assert!(set.get("anthropic").is_ok() && set.get("openrouter").is_ok());
        assert!(!format!("{set:?}").contains("secret"));
    }

    #[tokio::test]
    async fn stall_guard_ends_a_silent_stream_with_an_error() {
        let c = config(TWO_FAUX);
        let alpha = &c.providers["alpha"];
        let model = alpha.models["alpha-1"].clone();
        // A provider that never answers.
        let slow: Arc<dyn Provider> = Arc::new(Silent);
        let mut cfg = alpha.clone();
        cfg.timeouts.header = Some(Duration::from_millis(100));
        let guarded: Arc<dyn Provider> = Arc::new(ConfiguredProvider::new(&cfg, slow));
        let started = std::time::Instant::now();
        let message = reply(&guarded, &model).await;
        assert!(started.elapsed() < Duration::from_secs(5));
        assert_eq!(message.stop_reason, StopReason::Error);
        let error = message.error_message.unwrap();
        assert!(
            error.contains("no response within 100ms (header_timeout_secs)"),
            "{error}"
        );
    }

    #[tokio::test]
    async fn stall_guard_passes_a_healthy_stream_through() {
        let c = config(TWO_FAUX);
        let set = ProviderSet::build(&c, &StaticKeys::default());
        set.faux_script("alpha")
            .unwrap()
            .set_responses(vec![FauxStep::text("fine")]);
        let manager = c.resolve_role("manager").unwrap();
        let message = reply(&set.for_role(&manager).unwrap(), &manager.model).await;
        assert_eq!(text_of(&message), "fine");
        assert_ne!(message.stop_reason, StopReason::Error);
    }

    #[tokio::test]
    async fn stall_guard_catches_a_stream_that_stops_mid_way() {
        let c = config(TWO_FAUX);
        let mut cfg = c.providers["alpha"].clone();
        let model = cfg.models["alpha-1"].clone();
        cfg.timeouts.header = Some(Duration::from_secs(5));
        cfg.timeouts.stream_idle = Some(Duration::from_millis(100));
        let guarded: Arc<dyn Provider> =
            Arc::new(ConfiguredProvider::new(&cfg, Arc::new(StartThenSilent)));
        let message = reply(&guarded, &model).await;
        assert_eq!(message.stop_reason, StopReason::Error);
        let error = message.error_message.unwrap();
        assert!(
            error.contains("stream stalled, no data for 100ms"),
            "{error}"
        );
        // The partial message survives the stall.
        assert_eq!(message.response_id.as_deref(), Some("partial-1"));
    }

    /// Sends Start, then nothing until cancelled.
    struct StartThenSilent;

    #[async_trait]
    impl Provider for StartThenSilent {
        fn id(&self) -> &str {
            "start-then-silent"
        }
        fn models(&self) -> &[Model] {
            &[]
        }
        async fn stream_simple(
            &self,
            model: &Model,
            ctx: &Context,
            opts: &SimpleStreamOptions,
        ) -> AssistantMessageEventStream {
            let mut partial = AssistantMessage::empty(model.api.clone(), "x", model.id.clone(), 0);
            partial.response_id = Some("partial-1".into());
            let mut inner = Silent.stream_simple(model, ctx, opts).await;
            let (mut producer, consumer) = create_assistant_message_event_stream();
            producer.push(AssistantMessageEvent::Start {
                partial: Arc::new(partial),
            });
            tokio::spawn(async move {
                while let Some(event) = inner.next().await {
                    producer.push(event);
                }
            });
            consumer
        }
    }

    /// Emits nothing until cancelled, then reports the abort (like a real provider).
    struct Silent;

    #[async_trait]
    impl Provider for Silent {
        fn id(&self) -> &str {
            "silent"
        }
        fn models(&self) -> &[Model] {
            &[]
        }
        async fn stream_simple(
            &self,
            model: &Model,
            _ctx: &Context,
            opts: &SimpleStreamOptions,
        ) -> AssistantMessageEventStream {
            let (mut producer, consumer) = create_assistant_message_event_stream();
            let signal = opts.signal.clone();
            let model = model.clone();
            tokio::spawn(async move {
                signal.cancelled().await;
                producer.push(AssistantMessageEvent::Error {
                    reason: ErrorReason::Aborted,
                    error: AssistantMessage::terminal(
                        model.api.clone(),
                        "silent",
                        model.id.clone(),
                        StopReason::Aborted,
                        "aborted",
                        0,
                    ),
                });
            });
            consumer
        }
    }
}
