//! Provider endpoint auth and transport (#2277): every OpenAI-API auth pattern plus Azure and
//! enterprise gateways.
//!
//! - **Transport**: [`http_client`] builds the reqwest 0.12 client rpi's providers take, with the
//!   provider's `ca_file` (PEM bundle, added to the OS trust store, which is on through reqwest's
//!   `rustls-tls-native-roots`), the mTLS identity (`client_cert` + `client_key`, or the key from
//!   the keychain) and the proxy (`proxy`, else HTTPS_PROXY/NO_PROXY as rpi resolves them).
//! - **Tokens**: [`TokenSource`] fetches a short-lived bearer token from `auth_command`, an OAuth
//!   client-credentials token endpoint or Microsoft Entra ID; caches it in memory until shortly
//!   before it expires; and re-fetches once when the endpoint answers 401. Tokens are never logged,
//!   written or shown; a command's stderr is never logged and only its last line, scrubbed of
//!   anything token-shaped, goes into an error message.
//! - **URLs**: rpi builds the request URL from `Model::base_url` and has no query hook. For a
//!   provider with `query` (Azure's `api-version`) or a `{model}` placeholder (Azure deployments),
//!   [`request_base_url`] gives rpi the full URL with the query, and puts the suffix rpi appends
//!   (`/chat/completions`, `/v1/messages`) in the URL's fragment, which is never sent. A mock-server
//!   test asserts the exact request line, so an rpi change that breaks this fails the build.
//! - **Probe**: [`probe`] calls the endpoint's model list (or OpenRouter's `/key`) through the same
//!   client and auth, for setup's key check, `connect`, `config show --check` and doctor, and
//!   classifies a failure (rejected, TLS, token, network) so the fix printed is the one that
//!   applies.

use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
    path::Path,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use rpi_reqwest as reqwest;
use serde_json::Value;

use crate::{
    models::{ProviderApi, ProviderAuth, ProviderConfig, ProxySetting},
    providers::{KeyLookup, KeychainKey, Secret, resolve_key, resolve_secret_headers},
};

/// The keychain item (`provider:<id>:client-key`) holding an mTLS private key (PEM).
pub const CLIENT_KEY_SECRET: &str = "client-key";
/// rpi's pooled-connection idle timeout (`rpi_ai::http::DEFAULT_HTTP_IDLE_TIMEOUT_MS`).
const POOL_IDLE: Duration = Duration::from_millis(300_000);
/// Bound on one token endpoint call or probe.
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
/// Refresh a token this long before it expires (at most a tenth of its lifetime).
const REFRESH_SKEW: Duration = Duration::from_secs(60);

// ------------------------------------------------------------------------------------------------
// Transport
// ------------------------------------------------------------------------------------------------

/// The text of an error and its causes (reqwest's Display leaves out the TLS reason).
pub fn error_chain(e: &(dyn std::error::Error + 'static)) -> String {
    let mut text = e.to_string();
    let mut source = e.source();
    while let Some(cause) = source {
        let s = cause.to_string();
        if !text.contains(&s) {
            text.push_str(": ");
            text.push_str(&s);
        }
        source = cause.source();
    }
    text
}

/// The HTTP client for a provider's requests: rpi's proxy resolution (or the provider's `proxy`),
/// its connect timeout, CA file and mTLS identity.
pub fn http_client(p: &ProviderConfig, keys: &dyn KeyLookup) -> Result<reqwest::Client, String> {
    let mut builder = match &p.proxy {
        None => rpi_ai::http::client_builder_for(&p.base_url, None, None)
            .map_err(|e| format!("proxy: {e}"))?,
        Some(ProxySetting::Direct) => reqwest::Client::builder()
            .no_proxy()
            .pool_idle_timeout(POOL_IDLE),
        Some(ProxySetting::Url(url)) => {
            let proxy = reqwest::Proxy::all(url.as_str())
                .map_err(|e| format!("proxy `{url}`: {e}"))?
                .no_proxy(reqwest::NoProxy::from_env());
            reqwest::Client::builder()
                .proxy(proxy)
                .pool_idle_timeout(POOL_IDLE)
        }
    };
    builder = builder.redirect(same_origin_redirects());
    if let Some(connect) = p.timeouts.connect {
        builder = builder.connect_timeout(connect);
    }
    if let Some(ca) = &p.tls.ca_file {
        for cert in load_ca(ca)? {
            builder = builder.add_root_certificate(cert);
        }
    }
    if let Some(cert) = &p.tls.client_cert {
        builder = builder.identity(load_identity(p, cert, keys)?);
    }
    builder
        .build()
        .map_err(|e| format!("http client: {}", error_chain(&e)))
}

/// Most redirects a provider request follows (reqwest's default).
const MAX_REDIRECTS: usize = 10;

/// The provider client's redirect policy: follow a redirect only to the origin (scheme, host and
/// port) of the request that was sent, and refuse one to any other origin.
///
/// reqwest's default follows cross-host redirects and drops only `Authorization`, `Cookie` and
/// `Proxy-Authorization`, so `api-key` (Azure, `auth = "header"`), Anthropic's `x-api-key`, every
/// `secret_headers` value and an OAuth token request's form body (the client secret) would be sent
/// to the new host. Refusing is safer than stripping: we cannot know which configured header is a
/// credential, and an https -> http downgrade on the same host is a different origin too. The
/// error names only the target's origin, never its path or query.
pub fn same_origin_redirects() -> reqwest::redirect::Policy {
    reqwest::redirect::Policy::custom(|attempt| {
        let same_origin = attempt
            .previous()
            .first()
            .is_some_and(|first| first.origin() == attempt.url().origin());
        if !same_origin {
            let to = attempt.url().origin().ascii_serialization();
            attempt.error(format!(
                "refused a redirect to another origin ({to}): a provider's credentials are only \
                 sent to its own base_url; set base_url to the address it redirects to"
            ))
        } else if attempt.previous().len() > MAX_REDIRECTS {
            attempt.error(format!("exceeded {MAX_REDIRECTS} redirects"))
        } else {
            attempt.follow()
        }
    })
}

/// The CA bundle, checked the way `[server] ca_file` is (amp-mcp's loader: readable, PEM, at least
/// one certificate), then parsed for rpi's reqwest.
fn load_ca(path: &Path) -> Result<Vec<reqwest::Certificate>, String> {
    amp_mcp::load_ca_file(path).map_err(|e| format!("ca_file: {e}"))?;
    let pem = std::fs::read(path).map_err(|e| format!("ca_file {}: {e}", path.display()))?;
    reqwest::Certificate::from_pem_bundle(&pem)
        .map_err(|e| format!("ca_file {}: {}", path.display(), error_chain(&e)))
}

fn contains(haystack: &[u8], needle: &[u8]) -> bool {
    haystack.windows(needle.len()).any(|w| w == needle)
}

/// The client certificate plus its key: `client_key`, else the key inside the cert file, else the
/// keychain. Error messages name files and keychain items, never their contents.
fn load_identity(
    p: &ProviderConfig,
    cert: &Path,
    keys: &dyn KeyLookup,
) -> Result<reqwest::Identity, String> {
    let mut pem =
        std::fs::read(cert).map_err(|e| format!("client_cert {}: {e}", cert.display()))?;
    if !contains(&pem, b"-----BEGIN CERTIFICATE") {
        return Err(format!(
            "client_cert {}: no PEM certificate in it",
            cert.display()
        ));
    }
    let key_from = if let Some(key) = &p.tls.client_key {
        let bytes = std::fs::read(key).map_err(|e| format!("client_key {}: {e}", key.display()))?;
        pem.push(b'\n');
        pem.extend(bytes);
        format!("client_key {}", key.display())
    } else if contains(&pem, b"PRIVATE KEY-----") {
        format!("client_cert {}", cert.display())
    } else {
        let account = secret_account(&p.id, CLIENT_KEY_SECRET);
        match keys.provider_secret(&p.id, CLIENT_KEY_SECRET) {
            KeychainKey::Found(key) => {
                pem.push(b'\n');
                pem.extend(key.into_bytes());
                format!("the keychain item {account}")
            }
            KeychainKey::Failed(why) => return Err(format!("mTLS key: keychain: {why}")),
            KeychainKey::Off => {
                return Err(format!(
                    "client_cert {} has no private key and the keychain is off here: set \
                     client_key = \"<key file>\"",
                    cert.display()
                ));
            }
            KeychainKey::NotFound => {
                return Err(format!(
                    "no mTLS private key: set client_key = \"<key file>\", or store it in the \
                     keychain with `amp-harness connect {} --client-cert {} --client-key <file> \
                     --store-client-key`",
                    p.id,
                    cert.display()
                ));
            }
        }
    };
    reqwest::Identity::from_pem(&pem).map_err(|e| {
        format!(
            "mTLS identity from client_cert {} and {key_from}: not a PEM certificate and private \
             key ({})",
            cert.display(),
            error_chain(&e)
        )
    })
}

/// `provider:<id>:<name>`: a provider's other keychain items.
pub fn secret_account(provider: &str, name: &str) -> String {
    format!(
        "{}{provider}:{name}",
        crate::providers::KEYCHAIN_ACCOUNT_PREFIX
    )
}

// ------------------------------------------------------------------------------------------------
// URLs
// ------------------------------------------------------------------------------------------------

/// The provider's base URL with `{model}` filled in (Azure classic deployments: the deployment
/// name is the model id).
fn fill_model(base_url: &str, model: &str) -> String {
    base_url.replace("{model}", model)
}

/// rpi's own rule (openai_completions.rs `chat_completions_url`), for a base we hand it complete.
fn chat_completions_url(base: &str) -> String {
    let base = base.trim_end_matches('/');
    let version = |segment: &str| {
        segment.strip_prefix('v').is_some_and(|digits| {
            let n = digits.chars().take_while(char::is_ascii_digit).count();
            n > 0
                && digits
                    .chars()
                    .skip(n)
                    .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
        })
    };
    if base.ends_with("/chat/completions") {
        base.to_owned()
    } else if base.rsplit('/').next().is_some_and(version) {
        format!("{base}/chat/completions")
    } else {
        format!("{base}/v1/chat/completions")
    }
}

fn with_query(url: &str, query: &BTreeMap<String, String>, fragment: Option<&str>) -> String {
    match reqwest::Url::parse(url) {
        Ok(mut parsed) => {
            if !query.is_empty() {
                parsed
                    .query_pairs_mut()
                    .extend_pairs(query.iter().map(|(k, v)| (k.as_str(), v.as_str())));
            }
            parsed.set_fragment(fragment);
            parsed.to_string()
        }
        // Not a URL: leave it to rpi to report.
        Err(_) => url.to_owned(),
    }
}

/// The `Model::base_url` rpi gets for one request, when the provider needs one other than its own:
/// `query` parameters, or a `{model}` placeholder. `None`: send as configured.
///
/// rpi appends `/chat/completions` (OpenAI) or `/v1/messages` (Anthropic) to the base URL, so a
/// query would end up before that suffix. The URL we hand it is complete, query included, with
/// the suffix rpi appends in the fragment, which HTTP never sends: OpenAI's rule keeps a base that
/// ends in `/chat/completions` as is, and Anthropic's suffix lands after `#`.
pub fn request_base_url(
    api: ProviderApi,
    base_url: &str,
    query: &BTreeMap<String, String>,
    model: &str,
) -> Option<String> {
    let templated = base_url.contains("{model}");
    if query.is_empty() && !templated {
        return None;
    }
    let base = fill_model(base_url, model);
    Some(match api {
        ProviderApi::OpenaiCompletions => {
            let full = if templated && !base.ends_with("/chat/completions") {
                format!("{}/chat/completions", base.trim_end_matches('/'))
            } else {
                chat_completions_url(&base)
            };
            if query.is_empty() {
                full
            } else {
                with_query(&full, query, Some("/chat/completions"))
            }
        }
        ProviderApi::AnthropicMessages => {
            if query.is_empty() {
                base
            } else {
                // rpi appends "/v1/messages" after the empty fragment.
                let full = format!("{}/v1/messages", base.trim_end_matches('/'));
                let mut url = with_query(&full, query, None);
                url.push('#');
                url
            }
        }
        ProviderApi::Faux => return None,
    })
}

/// The URL of the endpoint's model list (`/models`, Anthropic `/v1/models`), query included. A
/// `{model}` base (Azure classic) lists at the resource: `…/openai/deployments/{model}` →
/// `…/openai/models`.
pub fn models_url(p: &ProviderConfig, path: Option<&str>) -> String {
    let mut base = p.base_url.trim_end_matches('/').to_owned();
    if let Some(at) = base.find("/deployments/{model}") {
        base.truncate(at);
    } else {
        base = fill_model(&base, "");
    }
    let url = match (p.api, path) {
        (_, Some(path)) => format!("{base}{path}"),
        (ProviderApi::AnthropicMessages, None) => format!("{base}/v1/models"),
        _ => format!("{base}/models"),
    };
    with_query(&url, &p.query, None)
}

// ------------------------------------------------------------------------------------------------
// Tokens
// ------------------------------------------------------------------------------------------------

/// A token and when it stops working.
#[derive(Clone)]
struct Cached {
    value: Secret,
    /// Unix seconds; `None` when the source gave no expiry (re-fetched on 401 only).
    expires_at: Option<u64>,
    /// When to fetch the next one (before `expires_at`).
    refresh_at: Option<SystemTime>,
}

impl Cached {
    fn new(value: Secret, lifetime: Option<Duration>, expires_on: Option<u64>) -> Self {
        let now = SystemTime::now();
        let expires = match (lifetime, expires_on) {
            (Some(l), _) => Some(now + l),
            (None, Some(at)) => Some(UNIX_EPOCH + Duration::from_secs(at)),
            (None, None) => None,
        };
        let refresh_at = expires.map(|at| {
            let lifetime = at.duration_since(now).unwrap_or_default();
            at - REFRESH_SKEW.min(lifetime / 10)
        });
        Self {
            value,
            expires_at: expires.map(unix),
            refresh_at,
        }
    }

    fn fresh(&self) -> bool {
        self.refresh_at.is_none_or(|at| SystemTime::now() < at)
    }
}

fn unix(t: SystemTime) -> u64 {
    t.duration_since(UNIX_EPOCH).map_or(0, |d| d.as_secs())
}

#[derive(Clone)]
enum Fetch {
    Command {
        argv: Vec<String>,
        timeout: Duration,
    },
    ClientCredentials {
        token_url: String,
        client_id: String,
        scopes: Vec<String>,
    },
}

/// Where a provider's short-lived tokens come from, and the one it holds now. Shared by every
/// request to the provider, so concurrent requests wait for one fetch.
pub struct TokenSource {
    provider: String,
    fetch: Fetch,
    http: reqwest::Client,
    client_secret: Option<Secret>,
    cache: tokio::sync::Mutex<Option<Cached>>,
    fetches: AtomicUsize,
}

impl fmt::Debug for TokenSource {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("TokenSource")
            .field("provider", &self.provider)
            .field("fetches", &self.fetches.load(Ordering::Relaxed))
            .finish_non_exhaustive()
    }
}

impl TokenSource {
    /// `None` for auth that sends no token (bearer and header keys, none).
    pub fn new(
        p: &ProviderConfig,
        http: reqwest::Client,
        client_secret: Option<Secret>,
    ) -> Option<Arc<Self>> {
        let auth = match &p.auth {
            ProviderAuth::AzureEntra(inner) => inner.as_ref(),
            other => other,
        };
        let fetch = match auth {
            ProviderAuth::Command { argv, timeout } => Fetch::Command {
                argv: argv.clone(),
                timeout: *timeout,
            },
            ProviderAuth::ClientCredentials {
                token_url,
                client_id,
                scopes,
            } => Fetch::ClientCredentials {
                token_url: token_url.clone(),
                client_id: client_id.clone(),
                scopes: scopes.clone(),
            },
            _ => return None,
        };
        Some(Arc::new(Self {
            provider: p.id.clone(),
            fetch,
            http,
            client_secret,
            cache: tokio::sync::Mutex::new(None),
            fetches: AtomicUsize::new(0),
        }))
    }

    /// The current token, fetched when there is none or it is about to expire.
    pub async fn token(&self) -> Result<Secret, String> {
        let mut cache = self.cache.lock().await;
        if let Some(c) = cache.as_ref()
            && c.fresh()
        {
            return Ok(c.value.clone());
        }
        let next = self.fetch().await?;
        let value = next.value.clone();
        *cache = Some(next);
        Ok(value)
    }

    /// A new token after the endpoint refused `rejected` (401). If another request already
    /// replaced it, that one is used.
    pub async fn refresh(&self, rejected: &Secret) -> Result<Secret, String> {
        let mut cache = self.cache.lock().await;
        if let Some(c) = cache.as_ref()
            && c.value.expose() != rejected.expose()
            && c.fresh()
        {
            return Ok(c.value.clone());
        }
        *cache = None;
        let next = self.fetch().await?;
        let value = next.value.clone();
        *cache = Some(next);
        Ok(value)
    }

    /// When the held token expires (Unix seconds), if one is held and its expiry is known.
    pub fn expires_at(&self) -> Option<u64> {
        self.cache
            .try_lock()
            .ok()
            .and_then(|c| c.as_ref().and_then(|c| c.expires_at))
    }

    /// How many tokens were fetched (tests, doctor).
    pub fn fetch_count(&self) -> usize {
        self.fetches.load(Ordering::Relaxed)
    }

    async fn fetch(&self) -> Result<Cached, String> {
        self.fetches.fetch_add(1, Ordering::Relaxed);
        match &self.fetch {
            Fetch::Command { argv, timeout } => run_command(argv, *timeout).await,
            Fetch::ClientCredentials {
                token_url,
                client_id,
                scopes,
            } => {
                let Some(secret) = &self.client_secret else {
                    return Err(format!(
                        "no client secret for {} (set its key_env, or store it with \
                         `amp-harness connect {}`)",
                        self.provider, self.provider
                    ));
                };
                client_credentials(&self.http, token_url, client_id, secret, scopes).await
            }
        }
    }
}

/// The last non-empty line of a command's stderr, cut short, with anything token-shaped removed.
pub fn scrub_stderr(stderr: &str) -> String {
    let line = stderr
        .lines()
        .map(str::trim)
        .rfind(|l| !l.is_empty())
        .unwrap_or("");
    let words: Vec<String> = line
        .split_whitespace()
        .map(|w| {
            let tokenish = w.len() >= 20
                && w.chars()
                    .all(|c| c.is_ascii_alphanumeric() || "._-~+/=:".contains(c));
            if tokenish {
                "<redacted>".to_owned()
            } else {
                w.to_owned()
            }
        })
        .collect();
    let mut out = words.join(" ");
    if out.chars().count() > 200 {
        out = out.chars().take(200).collect::<String>() + "...";
    }
    out
}

fn as_secs(v: &Value) -> Option<u64> {
    match v {
        Value::Number(n) => n.as_u64().or_else(|| n.as_f64().map(|f| f as u64)),
        Value::String(s) => s.trim().parse().ok(),
        _ => None,
    }
}

/// A command's stdout: a bare token (an optional `Bearer ` prefix is dropped), or JSON with
/// `token` / `access_token` / `accessToken` and `expires_in` (seconds) or `expires_on` (Unix
/// seconds; `az account get-access-token` prints both `expires_on` and `expiresOn`).
pub fn parse_command_output(
    stdout: &str,
) -> Result<(Secret, Option<Duration>, Option<u64>), String> {
    let text = stdout.trim();
    if text.is_empty() {
        return Err("printed nothing (it must print a token, or JSON with a token)".to_owned());
    }
    if text.starts_with('{') {
        let json: Value = serde_json::from_str(text)
            .map_err(|_| "printed JSON that does not parse".to_owned())?;
        let token = ["token", "access_token", "accessToken"]
            .iter()
            .find_map(|k| json.get(*k).and_then(Value::as_str))
            .and_then(Secret::new)
            .ok_or("printed JSON with no token, access_token or accessToken field")?;
        let lifetime = json
            .get("expires_in")
            .and_then(as_secs)
            .map(Duration::from_secs);
        let expires_on = ["expires_on", "expiresOn"]
            .iter()
            .find_map(|k| json.get(*k).and_then(as_secs));
        return Ok((token, lifetime, expires_on));
    }
    let line = text.lines().next().unwrap_or("").trim();
    let line = line
        .strip_prefix("Bearer ")
        .or_else(|| line.strip_prefix("bearer "))
        .unwrap_or(line)
        .trim();
    if line.split_whitespace().count() != 1 {
        return Err("printed more than one word on its first line (expected a token)".to_owned());
    }
    Ok((Secret::new(line).expect("non-empty"), None, None))
}

async fn run_command(argv: &[String], timeout: Duration) -> Result<Cached, String> {
    let program = argv.first().map(String::as_str).unwrap_or("");
    let mut command = tokio::process::Command::new(program);
    command
        .args(&argv[1..])
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .kill_on_drop(true);
    let child = command.spawn().map_err(|e| {
        if e.kind() == std::io::ErrorKind::NotFound {
            format!("auth_command `{program}` was not found (is it installed and on PATH?)")
        } else {
            format!("auth_command `{program}`: {e}")
        }
    })?;
    let output = match tokio::time::timeout(timeout, child.wait_with_output()).await {
        Ok(Ok(output)) => output,
        Ok(Err(e)) => return Err(format!("auth_command `{program}`: {e}")),
        // Dropping the future kills the child (kill_on_drop).
        Err(_) => {
            return Err(format!(
                "auth_command `{program}` did not finish within {}s (auth_command_timeout_secs)",
                timeout.as_secs()
            ));
        }
    };
    if !output.status.success() {
        let tail = scrub_stderr(&String::from_utf8_lossy(&output.stderr));
        return Err(format!(
            "auth_command `{program}` failed ({}){}",
            output.status,
            if tail.is_empty() {
                String::new()
            } else {
                format!(": {tail}")
            }
        ));
    }
    let stdout = String::from_utf8_lossy(&output.stdout);
    let (token, lifetime, expires_on) =
        parse_command_output(&stdout).map_err(|why| format!("auth_command `{program}` {why}"))?;
    Ok(Cached::new(token, lifetime, expires_on))
}

async fn client_credentials(
    http: &reqwest::Client,
    token_url: &str,
    client_id: &str,
    secret: &Secret,
    scopes: &[String],
) -> Result<Cached, String> {
    let scope = scopes.join(" ");
    let mut form = vec![
        ("grant_type", "client_credentials"),
        ("client_id", client_id),
        ("client_secret", secret.expose()),
    ];
    if !scope.is_empty() {
        form.push(("scope", scope.as_str()));
    }
    let response = http
        .post(token_url)
        .header("accept", "application/json")
        .form(&form)
        .timeout(REQUEST_TIMEOUT)
        .send()
        .await
        .map_err(|e| format!("token endpoint {token_url}: {}", error_chain(&e)))?;
    let status = response.status();
    let body = response.text().await.unwrap_or_default();
    let json: Value = serde_json::from_str(&body).unwrap_or(Value::Null);
    if !status.is_success() {
        let code = json["error"].as_str().unwrap_or("");
        let description: String = json["error_description"]
            .as_str()
            .unwrap_or("")
            .chars()
            .take(200)
            .collect();
        return Err(format!(
            "token endpoint {token_url} refused the client credentials (HTTP {}){}",
            status.as_u16(),
            match (code.is_empty(), description.is_empty()) {
                (true, true) => String::new(),
                (false, true) => format!(": {code}"),
                (true, false) => format!(": {description}"),
                (false, false) => format!(": {code}: {description}"),
            }
        ));
    }
    let token = json["access_token"]
        .as_str()
        .and_then(Secret::new)
        .ok_or_else(|| format!("token endpoint {token_url} answered without an access_token"))?;
    let lifetime = json
        .get("expires_in")
        .and_then(as_secs)
        .map(Duration::from_secs);
    Ok(Cached::new(token, lifetime, None))
}

// ------------------------------------------------------------------------------------------------
// Headers for our own requests (probe); streamed requests get theirs in ConfiguredProvider.
// ------------------------------------------------------------------------------------------------

/// The auth header for a request with `key` (API key) or `token` (short-lived). Anthropic's API
/// key goes in `x-api-key`, every token as `Authorization: Bearer`.
pub fn auth_header(
    p: &ProviderConfig,
    key: Option<&Secret>,
    token: Option<&Secret>,
) -> Option<(String, String)> {
    if let Some(token) = token {
        return Some((
            "authorization".to_owned(),
            format!("Bearer {}", token.expose()),
        ));
    }
    let key = key?;
    match &p.auth {
        ProviderAuth::Header { name } => Some((name.clone(), key.expose().to_owned())),
        ProviderAuth::Bearer if p.api == ProviderApi::AnthropicMessages => {
            Some(("x-api-key".to_owned(), key.expose().to_owned()))
        }
        ProviderAuth::Bearer => Some((
            "authorization".to_owned(),
            format!("Bearer {}", key.expose()),
        )),
        _ => None,
    }
}

// ------------------------------------------------------------------------------------------------
// Probe
// ------------------------------------------------------------------------------------------------

/// Why a probe failed.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProbeFailure {
    /// The endpoint answered 401 or 403.
    Rejected,
    /// The TLS handshake failed (unknown CA, client certificate required or refused, ...).
    Tls,
    /// No token: the command or token endpoint failed.
    Token,
    /// The configuration cannot be used (CA file, client cert, missing key).
    Config,
    /// Any other HTTP status.
    Http,
    /// Connecting failed or timed out (not TLS).
    Network,
}

#[derive(Debug, Clone)]
pub struct ProbeError {
    pub failure: ProbeFailure,
    pub status: Option<u16>,
    /// One line; never a secret.
    pub message: String,
}

impl fmt::Display for ProbeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}

impl ProbeError {
    /// What is wrong with the endpoint's certificate, when the TLS failure was the certificate
    /// check (#2333); `None` for any other failure (mTLS refusals, https to an http port, ...).
    pub fn cert_problem(&self) -> Option<CertProblem> {
        if self.failure != ProbeFailure::Tls {
            return None;
        }
        CertProblem::classify(&self.message)
    }
}

/// Why the endpoint's certificate was not accepted (#2333).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CertProblem {
    /// Signed by a CA this machine does not trust (a private or company CA, or self-signed).
    UnknownIssuer,
    /// Expired, or not valid yet (also a wrong clock here).
    Expired,
    /// Issued for another host name.
    NameMismatch,
}

impl CertProblem {
    /// Reads rustls' (and the OS verifier's) wording: `invalid peer certificate: UnknownIssuer`,
    /// `certificate expired: …`, `certificate not valid for name "…"`, `NotValidForName`, ...
    pub fn classify(text: &str) -> Option<Self> {
        let lower = text.to_ascii_lowercase();
        let has = |needles: &[&str]| needles.iter().any(|n| lower.contains(n));
        // The server refusing *our* client certificate is not a problem with its certificate.
        if has(&[
            "received fatal alert",
            "certificaterequired",
            "certificate required",
        ]) {
            return None;
        }
        if has(&["notvalidforname", "not valid for name", "name mismatch"]) {
            Some(Self::NameMismatch)
        } else if has(&["expired", "notvalidyet", "not valid yet"]) {
            Some(Self::Expired)
        } else if has(&[
            "unknownissuer",
            "unknown issuer",
            "self signed",
            "self-signed",
            "unable to get local issuer",
            "not trusted",
            "untrusted",
        ]) {
            Some(Self::UnknownIssuer)
        } else {
            None
        }
    }

    /// One sentence naming the problem for `host` ("api.example.com's certificate isn't trusted:
    /// it's signed by a CA this machine doesn't know."). `ca_file` is the CA bundle the provider
    /// already has, if any.
    pub fn sentence(self, host: &str, ca_file: Option<&std::path::Path>) -> String {
        match (self, ca_file) {
            (Self::UnknownIssuer, None) => format!(
                "{host}'s certificate isn't trusted: it's signed by a CA this machine doesn't know."
            ),
            (Self::UnknownIssuer, Some(ca)) => format!(
                "{host}'s certificate isn't trusted: it isn't signed by the CA in {}.",
                ca.display()
            ),
            (Self::Expired, _) => format!(
                "{host}'s certificate isn't valid now: it has expired (or this machine's clock is \
                 wrong)."
            ),
            (Self::NameMismatch, _) => {
                format!("{host}'s certificate isn't trusted: it was issued for another host name.")
            }
        }
    }
}

/// What a probe found.
#[derive(Debug, Clone, Default)]
pub struct Probe {
    /// The model ids listed (empty for OpenRouter's `/key`).
    pub ids: BTreeSet<String>,
    /// When the token used expires (Unix seconds), for token auth.
    pub token_expires_at: Option<u64>,
}

fn is_tls(text: &str) -> bool {
    let lower = text.to_ascii_lowercase();
    [
        "certificate",
        "unknownissuer",
        "unknown issuer",
        "handshake",
        "tls",
        "received fatal alert",
        "self signed",
        "self-signed",
    ]
    .iter()
    .any(|n| lower.contains(n))
}

/// Which request to make.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProbeKind {
    /// The whole model list (pages followed for Anthropic).
    Catalog,
    /// Only whether the credentials work: OpenRouter `/key` (its `/models` is public), else one
    /// model-list page.
    Auth,
}

/// Calls the endpoint's model list with the provider's transport and auth.
pub async fn probe(
    p: &ProviderConfig,
    keys: &dyn KeyLookup,
    kind: ProbeKind,
) -> Result<Probe, ProbeError> {
    let fail = |failure, status, message: String| ProbeError {
        failure,
        status,
        message,
    };
    if p.api == ProviderApi::Faux {
        return Ok(Probe {
            ids: p.models.keys().cloned().collect(),
            token_expires_at: None,
        });
    }
    let (key, source) = resolve_key(p, keys);
    if source.is_missing() {
        return Err(fail(
            ProbeFailure::Config,
            None,
            format!("skipped: {} {source}", p.auth.key_word()),
        ));
    }
    let secret_headers =
        resolve_secret_headers(p, keys).map_err(|e| fail(ProbeFailure::Config, None, e))?;
    let http = http_client(p, keys).map_err(|e| fail(ProbeFailure::Config, None, e))?;
    let tokens = TokenSource::new(p, http.clone(), key.clone());
    let token = match &tokens {
        Some(t) => Some(
            t.token()
                .await
                .map_err(|e| fail(ProbeFailure::Token, None, e))?,
        ),
        None => None,
    };
    let openrouter = p.id == "openrouter" || p.base_url.contains("openrouter.ai");
    let mut ids = BTreeSet::new();
    let mut after: Option<String> = None;
    loop {
        let url = match (p.api, kind) {
            (ProviderApi::OpenaiCompletions, ProbeKind::Auth) if openrouter => {
                models_url(p, Some("/key"))
            }
            (ProviderApi::AnthropicMessages, ProbeKind::Auth) => with_query(
                &models_url(p, None),
                &BTreeMap::from([("limit".to_owned(), "1".to_owned())]),
                None,
            ),
            (ProviderApi::AnthropicMessages, ProbeKind::Catalog) => {
                let mut q = BTreeMap::from([("limit".to_owned(), "1000".to_owned())]);
                if let Some(a) = &after {
                    q.insert("after_id".to_owned(), a.clone());
                }
                with_query(&models_url(p, None), &q, None)
            }
            _ => models_url(p, None),
        };
        let mut request = http.get(&url).timeout(REQUEST_TIMEOUT);
        if p.api == ProviderApi::AnthropicMessages {
            request = request.header("anthropic-version", "2023-06-01");
        }
        for (name, value) in p.headers.iter().chain(secret_headers.iter()) {
            request = request.header(name.as_str(), value.as_str());
        }
        if let Some((name, value)) = auth_header(p, key.as_ref(), token.as_ref()) {
            request = request.header(name, value);
        }
        let response = request.send().await.map_err(|e| {
            let text = error_chain(&e);
            let failure = if is_tls(&text) {
                ProbeFailure::Tls
            } else {
                ProbeFailure::Network
            };
            fail(failure, None, format!("request: {text}"))
        })?;
        let status = response.status();
        let body = response.text().await.unwrap_or_default();
        if !status.is_success() {
            let snippet: String = body.chars().take(200).collect();
            let failure = if matches!(status.as_u16(), 401 | 403) {
                ProbeFailure::Rejected
            } else {
                ProbeFailure::Http
            };
            return Err(fail(
                failure,
                Some(status.as_u16()),
                format!("HTTP {status}: {snippet}"),
            ));
        }
        if kind == ProbeKind::Auth && openrouter {
            break;
        }
        let page: Value = serde_json::from_str(&body)
            .map_err(|e| fail(ProbeFailure::Http, None, format!("decode: {e}")))?;
        if let Some(data) = page["data"].as_array() {
            ids.extend(
                data.iter()
                    .filter_map(|m| m["id"].as_str().map(str::to_owned)),
            );
        }
        if p.api != ProviderApi::AnthropicMessages || kind == ProbeKind::Auth {
            break;
        }
        match page["last_id"].as_str() {
            Some(last) if page["has_more"].as_bool().unwrap_or(false) => {
                after = Some(last.to_owned());
            }
            _ => break,
        }
    }
    Ok(Probe {
        ids,
        token_expires_at: tokens.and_then(|t| t.expires_at()),
    })
}

/// Unix seconds as `YYYY-MM-DD HH:MM:SS UTC` (doctor's token expiry).
pub fn format_unix(secs: u64) -> String {
    chrono::DateTime::<chrono::Utc>::from_timestamp(secs as i64, 0)
        .map(|t| t.format("%Y-%m-%d %H:%M:%S UTC").to_string())
        .unwrap_or_else(|| secs.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// #2333: rustls' wording for each certificate problem; mTLS refusals are not one.
    #[test]
    fn certificate_problems_are_recognised() {
        let tls = |message: &str| ProbeError {
            failure: ProbeFailure::Tls,
            status: None,
            message: message.to_owned(),
        };
        for (text, want) in [
            (
                "request: error sending request for url (https://h/v1/models): client error \
                 (Connect): invalid peer certificate: UnknownIssuer",
                Some(CertProblem::UnknownIssuer),
            ),
            (
                "invalid peer certificate: certificate expired: verification time 2 (UNIX), but \
                 certificate is not valid after 1 (1 seconds ago)",
                Some(CertProblem::Expired),
            ),
            (
                "invalid peer certificate: certificate not valid yet: verification time 1",
                Some(CertProblem::Expired),
            ),
            (
                "invalid peer certificate: certificate not valid for name \"localhost\"; \
                 certificate is only valid for other.example.com",
                Some(CertProblem::NameMismatch),
            ),
            (
                "invalid peer certificate: NotValidForName",
                Some(CertProblem::NameMismatch),
            ),
            ("received fatal alert: CertificateRequired", None),
            ("received fatal alert: UnknownCA", None),
            ("received corrupt message of type InvalidContentType", None),
        ] {
            assert_eq!(tls(text).cert_problem(), want, "{text}");
        }
        // Only a TLS failure is a certificate problem.
        let mut network = tls("invalid peer certificate: UnknownIssuer");
        network.failure = ProbeFailure::Network;
        assert_eq!(network.cert_problem(), None);
        assert_eq!(
            CertProblem::UnknownIssuer.sentence("api.example.com", None),
            "api.example.com's certificate isn't trusted: it's signed by a CA this machine \
             doesn't know."
        );
    }

    #[test]
    fn query_and_model_placeholder_urls() {
        let q = BTreeMap::from([("api-version".to_owned(), "2024-10-21".to_owned())]);
        let none = BTreeMap::new();
        assert_eq!(
            request_base_url(ProviderApi::OpenaiCompletions, "https://x/v1", &none, "m"),
            None
        );
        assert_eq!(
            request_base_url(
                ProviderApi::OpenaiCompletions,
                "https://r.openai.azure.com/openai/deployments/{model}",
                &q,
                "gpt-4o"
            )
            .unwrap(),
            "https://r.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21#/chat/completions"
        );
        assert_eq!(
            request_base_url(ProviderApi::OpenaiCompletions, "https://gw/api/v1", &q, "m").unwrap(),
            "https://gw/api/v1/chat/completions?api-version=2024-10-21#/chat/completions"
        );
        assert_eq!(
            request_base_url(ProviderApi::AnthropicMessages, "https://gw", &q, "m").unwrap(),
            "https://gw/v1/messages?api-version=2024-10-21#"
        );
    }

    #[test]
    fn command_output_forms() {
        let (t, l, e) = parse_command_output("tok-123\n").unwrap();
        assert_eq!((t.expose(), l, e), ("tok-123", None, None));
        let (t, _, _) = parse_command_output("Bearer abc").unwrap();
        assert_eq!(t.expose(), "abc");
        let (t, l, _) = parse_command_output(r#"{"token":"j","expires_in":"90"}"#).unwrap();
        assert_eq!((t.expose(), l), ("j", Some(Duration::from_secs(90))));
        let (t, l, e) = parse_command_output(
            r#"{"accessToken":"az","expiresOn":"2026-10-23 10:00:00.000000","expires_on":1792750000,"tokenType":"Bearer"}"#,
        )
        .unwrap();
        assert_eq!((t.expose(), l, e), ("az", None, Some(1_792_750_000)));
        assert!(parse_command_output("").is_err());
        assert!(parse_command_output("two words").is_err());
        let e = parse_command_output(r#"{"nope":"SECRET-VALUE"}"#).unwrap_err();
        assert!(!e.contains("SECRET-VALUE"), "{e}");
    }

    #[test]
    fn stderr_is_scrubbed() {
        let s = scrub_stderr("debug\nerror: token eyJhbGciOiJIUzI1NiJ9.payload.sig rejected\n");
        assert_eq!(s, "error: token <redacted> rejected");
    }

    #[test]
    fn refresh_before_expiry() {
        let c = Cached::new(
            Secret::new("t").unwrap(),
            Some(Duration::from_secs(3600)),
            None,
        );
        assert!(c.fresh());
        let c = Cached::new(Secret::new("t").unwrap(), Some(Duration::ZERO), None);
        assert!(!c.fresh());
        let c = Cached::new(Secret::new("t").unwrap(), None, None);
        assert!(c.fresh() && c.expires_at.is_none());
    }
}
