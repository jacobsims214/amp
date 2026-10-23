//! #2277: provider endpoint auth and transport against in-crate mock endpoints.
//!
//! - bearer, a key in its own header (Azure's `api-key`), none;
//! - OpenAI-Organization / OpenAI-Project headers, `query` (api-version) and an Azure
//!   `…/deployments/{model}` URL, asserted on the exact request line;
//! - a token command: cached, re-run at expiry and once on a 401, a timeout, a failing command
//!   whose stderr holds a secret;
//! - OAuth client credentials and Microsoft Entra ID against a mock token endpoint, with refresh;
//! - a custom CA (private, self-signed), mTLS (the server requires a client certificate; the key
//!   from a file or the keychain), and a per-provider proxy.
//!
//! Every secret used here is planted and asserted absent from Debug output and error messages.
//! Files live under cargo's target dir, never /tmp.

use std::{
    collections::BTreeMap,
    net::SocketAddr,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use amp_core::{
    models::{ModelsConfig, ModelsFile, ProviderAuth},
    provider_auth::{ProbeFailure, ProbeKind, probe},
    providers::{ProviderSet, StaticKeys, live_model_ids},
};
use axum::{
    Json, Router,
    extract::{Form, OriginalUri, State},
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::{get, post},
};
use rcgen::{
    BasicConstraints, CertificateParams, CertifiedIssuer, DnType, ExtendedKeyUsagePurpose, IsCa,
    KeyPair, KeyUsagePurpose,
};
use rpi_ai::{AssistantMessage, Context, Message, SimpleStreamOptions, StopReason, UserMessage};
use serde_json::{Value, json};
use tokio::net::{TcpListener, TcpStream};
use tokio_rustls::{
    TlsAcceptor,
    rustls::{
        self, RootCertStore,
        pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer},
        server::WebPkiClientVerifier,
    },
    server::TlsStream,
};

const KEY: &str = "sk-gw-2277-planted-never-print";
const CLIENT_SECRET: &str = "cs-2277-planted-client-secret";

// ------------------------------------------------------------------------------------------------
// The mock endpoint
// ------------------------------------------------------------------------------------------------

/// One request the mock saw.
#[derive(Debug, Clone)]
struct Seen {
    /// The request target as sent (path and query; absolute-form through a proxy).
    uri: String,
    headers: BTreeMap<String, String>,
}

#[derive(Clone, Default)]
struct Mock {
    /// Accepted `authorization` values, or `(header, value)` for a key in its own header. Empty:
    /// anything goes.
    accept: Arc<Mutex<Vec<(String, String)>>>,
    seen: Arc<Mutex<Vec<Seen>>>,
    /// Token endpoint: tokens handed out so far, their lifetime, and the forms it got (no secret).
    issued: Arc<Mutex<u32>>,
    token_lifetime: Arc<Mutex<u64>>,
    token_forms: Arc<Mutex<Vec<BTreeMap<String, String>>>>,
    /// #2309: answer every request with a 307 to this base URL plus the request's path and query.
    redirect: Arc<Mutex<Option<String>>>,
}

impl Mock {
    fn accept(&self, header: &str, value: &str) {
        self.accept
            .lock()
            .unwrap()
            .push((header.to_owned(), value.to_owned()));
    }

    fn seen(&self) -> Vec<Seen> {
        self.seen.lock().unwrap().clone()
    }

    fn chat_auth(&self) -> Vec<String> {
        self.seen()
            .iter()
            .filter(|s| s.uri.contains("/chat/completions"))
            .map(|s| s.headers.get("authorization").cloned().unwrap_or_default())
            .collect()
    }

    fn record(&self, uri: &OriginalUri, headers: &HeaderMap) -> bool {
        let headers: BTreeMap<String, String> = headers
            .iter()
            .map(|(k, v)| {
                (
                    k.as_str().to_ascii_lowercase(),
                    v.to_str().unwrap_or("").to_owned(),
                )
            })
            .collect();
        let ok = {
            let accept = self.accept.lock().unwrap();
            accept.is_empty()
                || accept
                    .iter()
                    .any(|(h, v)| headers.get(&h.to_ascii_lowercase()) == Some(v))
        };
        self.seen.lock().unwrap().push(Seen {
            uri: uri.0.to_string(),
            headers,
        });
        ok
    }

    /// The 307 to answer with, when this mock redirects everything (#2309).
    fn redirected(&self, uri: &OriginalUri) -> Option<Response> {
        let to = self.redirect.lock().unwrap().clone()?;
        let target = format!("{to}{}", uri.0);
        Some((StatusCode::TEMPORARY_REDIRECT, [("location", target)]).into_response())
    }
}

async fn models(State(m): State<Mock>, uri: OriginalUri, headers: HeaderMap) -> Response {
    let accepted = m.record(&uri, &headers);
    if let Some(r) = m.redirected(&uri) {
        return r;
    }
    if !accepted {
        return (StatusCode::UNAUTHORIZED, Json(json!({"error": "no"}))).into_response();
    }
    Json(json!({"object": "list", "data": [{"id": "gpt-4o"}, {"id": "m-1"}]})).into_response()
}

async fn chat(State(m): State<Mock>, uri: OriginalUri, headers: HeaderMap) -> Response {
    let accepted = m.record(&uri, &headers);
    if let Some(r) = m.redirected(&uri) {
        return r;
    }
    if !accepted {
        return (
            StatusCode::UNAUTHORIZED,
            Json(json!({"error": {"message": "invalid token"}})),
        )
            .into_response();
    }
    let chunk = |delta: Value, finish: Value| {
        json!({"id": "c", "object": "chat.completion.chunk", "created": 0, "model": "m",
               "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]})
    };
    let mut body = String::new();
    for event in [
        chunk(json!({"role": "assistant", "content": "pong"}), Value::Null),
        chunk(json!({}), json!("stop")),
    ] {
        body.push_str(&format!("data: {event}\n\n"));
    }
    body.push_str("data: [DONE]\n\n");
    ([("content-type", "text/event-stream")], body).into_response()
}

/// OAuth 2.0 client credentials (also Entra's `/{tenant}/oauth2/v2.0/token`).
async fn token(
    State(m): State<Mock>,
    uri: OriginalUri,
    headers: HeaderMap,
    Form(form): Form<BTreeMap<String, String>>,
) -> Response {
    m.record(&uri, &headers);
    if let Some(r) = m.redirected(&uri) {
        return r;
    }
    let mut shown = form.clone();
    let secret_ok = shown.remove("client_secret").as_deref() == Some(CLIENT_SECRET);
    m.token_forms.lock().unwrap().push(shown);
    if form.get("grant_type").map(String::as_str) != Some("client_credentials") || !secret_ok {
        return (
            StatusCode::UNAUTHORIZED,
            Json(json!({"error": "invalid_client", "error_description": "bad client secret"})),
        )
            .into_response();
    }
    let n = {
        let mut issued = m.issued.lock().unwrap();
        *issued += 1;
        *issued
    };
    let lifetime = *m.token_lifetime.lock().unwrap();
    Json(
        json!({"access_token": format!("cc-token-{n}"), "token_type": "Bearer",
                "expires_in": lifetime}),
    )
    .into_response()
}

fn router(mock: Mock) -> Router {
    Router::new()
        .route("/v1/models", get(models))
        .route("/v1/chat/completions", post(chat))
        .route("/openai/models", get(models))
        .route("/openai/deployments/{dep}/chat/completions", post(chat))
        .route("/token", post(token))
        .route("/{tenant}/oauth2/v2.0/token", post(token))
        .route("/moved/{*rest}", get(moved).post(moved))
        .with_state(mock)
}

/// `/moved/<rest>` -> 307 to `/<rest>` on the same origin (#2309).
async fn moved(State(m): State<Mock>, uri: OriginalUri, headers: HeaderMap) -> Response {
    m.record(&uri, &headers);
    let target = uri.0.to_string().replacen("/moved", "", 1);
    (StatusCode::TEMPORARY_REDIRECT, [("location", target)]).into_response()
}

struct Server {
    url: String,
    mock: Mock,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Server {
    fn drop(&mut self) {
        self.task.abort();
    }
}

async fn spawn_http() -> Server {
    let mock = Mock::default();
    *mock.token_lifetime.lock().unwrap() = 3600;
    let tcp = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = tcp.local_addr().unwrap();
    let app = router(mock.clone());
    let task = tokio::spawn(async move {
        let _ = axum::serve(tcp, app).await;
    });
    Server {
        url: format!("http://{addr}"),
        mock,
        task,
    }
}

// ------------------------------------------------------------------------------------------------
// TLS fixtures
// ------------------------------------------------------------------------------------------------

struct Pki {
    ca_pem: String,
    leaf_der: CertificateDer<'static>,
    leaf_key: PrivateKeyDer<'static>,
    ca_der: CertificateDer<'static>,
    client_cert_pem: String,
    client_key_pem: String,
}

fn pki() -> Pki {
    let now = time::OffsetDateTime::now_utc();
    let mut ca = CertificateParams::new(Vec::<String>::new()).unwrap();
    ca.distinguished_name
        .push(DnType::CommonName, "amp-core 2277 test CA");
    ca.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    ca.key_usages = vec![
        KeyUsagePurpose::KeyCertSign,
        KeyUsagePurpose::CrlSign,
        KeyUsagePurpose::DigitalSignature,
    ];
    ca.not_before = now - time::Duration::days(1);
    ca.not_after = now + time::Duration::days(30);
    let ca = CertifiedIssuer::self_signed(ca, KeyPair::generate().unwrap()).unwrap();

    let mut leaf =
        CertificateParams::new(vec!["localhost".to_owned(), "127.0.0.1".to_owned()]).unwrap();
    leaf.distinguished_name
        .push(DnType::CommonName, "localhost");
    leaf.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
    leaf.key_usages = vec![
        KeyUsagePurpose::DigitalSignature,
        KeyUsagePurpose::KeyEncipherment,
    ];
    leaf.not_before = now - time::Duration::days(1);
    leaf.not_after = now + time::Duration::days(30);
    let leaf_key = KeyPair::generate().unwrap();
    let leaf_cert = leaf.signed_by(&leaf_key, &ca).unwrap();

    let mut client = CertificateParams::new(Vec::<String>::new()).unwrap();
    client
        .distinguished_name
        .push(DnType::CommonName, "amp-harness test client");
    client.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
    client.key_usages = vec![KeyUsagePurpose::DigitalSignature];
    client.not_before = now - time::Duration::days(1);
    client.not_after = now + time::Duration::days(30);
    let client_key = KeyPair::generate().unwrap();
    let client_cert = client.signed_by(&client_key, &ca).unwrap();

    Pki {
        ca_pem: ca.pem(),
        ca_der: ca.der().clone(),
        leaf_der: leaf_cert.der().clone(),
        leaf_key: PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(leaf_key.serialize_der())),
        client_cert_pem: client_cert.pem(),
        client_key_pem: client_key.serialize_pem(),
    }
}

struct TlsListener {
    tcp: TcpListener,
    acceptor: TlsAcceptor,
}

impl axum::serve::Listener for TlsListener {
    type Io = TlsStream<TcpStream>;
    type Addr = SocketAddr;

    async fn accept(&mut self) -> (Self::Io, Self::Addr) {
        loop {
            let Ok((stream, addr)) = self.tcp.accept().await else {
                continue;
            };
            if let Ok(tls) = self.acceptor.accept(stream).await {
                return (tls, addr);
            }
        }
    }

    fn local_addr(&self) -> std::io::Result<Self::Addr> {
        self.tcp.local_addr()
    }
}

/// HTTPS signed by the private CA; with `mtls`, client certificates from that CA are required.
async fn spawn_https(pki: &Pki, mtls: bool) -> Server {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let builder = rustls::ServerConfig::builder_with_provider(provider.clone())
        .with_safe_default_protocol_versions()
        .unwrap();
    let builder = if mtls {
        let mut roots = RootCertStore::empty();
        roots.add(pki.ca_der.clone()).unwrap();
        let verifier = WebPkiClientVerifier::builder_with_provider(Arc::new(roots), provider)
            .build()
            .unwrap();
        builder.with_client_cert_verifier(verifier)
    } else {
        builder.with_no_client_auth()
    };
    let config = builder
        .with_single_cert(vec![pki.leaf_der.clone()], pki.leaf_key.clone_key())
        .unwrap();
    let tcp = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = tcp.local_addr().unwrap().port();
    let listener = TlsListener {
        tcp,
        acceptor: TlsAcceptor::from(Arc::new(config)),
    };
    let mock = Mock::default();
    let app = router(mock.clone());
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Server {
        url: format!("https://localhost:{port}"),
        mock,
        task,
    }
}

// ------------------------------------------------------------------------------------------------
// Helpers
// ------------------------------------------------------------------------------------------------

fn scratch(name: &str) -> PathBuf {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("provider-auth-2277")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

fn config(text: &str) -> ModelsConfig {
    let user = ModelsFile::parse(text, "test").unwrap();
    ModelsConfig::from_files(ModelsFile::defaults(), Some(user), "test", &|_| None).unwrap()
}

fn config_err(text: &str) -> String {
    let user = ModelsFile::parse(text, "test").unwrap();
    ModelsConfig::from_files(ModelsFile::defaults(), Some(user), "test", &|_| None)
        .expect_err("refused")
        .0
}

fn keys(env: &[(&str, &str)], keychain: &[(&str, &str)]) -> StaticKeys {
    StaticKeys {
        env: env
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect(),
        keychain: Some(
            keychain
                .iter()
                .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
                .collect(),
        ),
    }
}

async fn ask(set: &ProviderSet, id: &str, model: &str) -> AssistantMessage {
    let p = set.get(id).unwrap_or_else(|e| panic!("{e}: {set:?}"));
    let model = p.models().iter().find(|m| m.id == model).unwrap().clone();
    let ctx = Context {
        system_prompt: None,
        messages: vec![Message::User(UserMessage::new("ping", 0))],
        tools: Vec::new(),
    };
    tokio::time::timeout(
        Duration::from_secs(30),
        p.stream_simple(&model, &ctx, &SimpleStreamOptions::default())
            .await
            .result(),
    )
    .await
    .expect("answered within 30 s")
    .unwrap()
}

fn text(m: &AssistantMessage) -> String {
    m.content
        .iter()
        .filter_map(|c| match c {
            rpi_ai::Content::Text(t) => Some(t.text.clone()),
            _ => None,
        })
        .collect()
}

fn ok(m: &AssistantMessage) {
    assert_ne!(m.stop_reason, StopReason::Error, "{:?}", m.error_message);
    assert_eq!(text(m), "pong");
}

/// A token command: prints `{"token": "<prefix>-N", "expires_in": <life>}`, N counting its runs
/// (in `dir/count`).
fn token_script(dir: &Path, prefix: &str, life: u64) -> Vec<String> {
    let script = dir.join("token.sh");
    std::fs::write(
        &script,
        format!(
            "n=$(cat \"$1/count\" 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > \"$1/count\"\n\
             echo '{{\"token\":\"{prefix}-'$n'\",\"expires_in\":{life}}}'\n"
        ),
    )
    .unwrap();
    vec![
        "/bin/sh".to_owned(),
        script.display().to_string(),
        dir.display().to_string(),
    ]
}

fn runs(dir: &Path) -> u32 {
    std::fs::read_to_string(dir.join("count"))
        .map(|s| s.trim().parse().unwrap_or(0))
        .unwrap_or(0)
}

fn toml_list(argv: &[String]) -> String {
    format!(
        "[{}]",
        argv.iter()
            .map(|a| format!("{a:?}"))
            .collect::<Vec<_>>()
            .join(", ")
    )
}

// ------------------------------------------------------------------------------------------------
// Tests
// ------------------------------------------------------------------------------------------------

#[tokio::test]
async fn bearer_org_project_query_and_the_azure_deployment_url() {
    let s = spawn_http().await;
    s.mock.accept("authorization", &format!("Bearer {KEY}"));
    let c = config(&format!(
        "[providers.az]\napi = \"openai-completions\"\n\
         base_url = \"{}/openai/deployments/{{model}}\"\nkey_env = \"AZ_KEY\"\n\
         organization = \"org-2277\"\nproject = \"proj_2277\"\n\
         query = {{ \"api-version\" = \"2024-10-21\" }}\n[providers.az.models.\"gpt-4o\"]\n",
        s.url
    ));
    let keys = keys(&[("AZ_KEY", KEY)], &[]);
    let set = ProviderSet::build(&c, &keys);
    ok(&ask(&set, "az", "gpt-4o").await);
    let seen = s.mock.seen();
    let last = seen.last().unwrap();
    // The deployment is the model id; the query is on the wire; the fragment never is.
    assert_eq!(
        last.uri,
        "/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21"
    );
    assert_eq!(last.headers["openai-organization"], "org-2277");
    assert_eq!(last.headers["openai-project"], "proj_2277");
    assert_eq!(last.headers["authorization"], format!("Bearer {KEY}"));

    // The catalog lists at the resource, with the query.
    let ids = live_model_ids(&c.providers["az"], &keys).await.unwrap();
    assert!(ids.contains("gpt-4o"));
    assert_eq!(
        s.mock.seen().last().unwrap().uri,
        "/openai/models?api-version=2024-10-21"
    );
    assert!(!format!("{set:?}").contains(KEY));
}

#[tokio::test]
async fn header_auth_sends_the_key_in_its_own_header_and_none_sends_nothing() {
    let s = spawn_http().await;
    s.mock.accept("api-key", KEY);
    let c = config(&format!(
        "[providers.gw]\napi = \"openai-completions\"\nbase_url = \"{0}/v1\"\nkey_env = \"GW\"\n\
         auth = \"header\"\n[providers.gw.models.\"m-1\"]\n\
         [providers.open]\napi = \"openai-completions\"\nbase_url = \"{0}/v1\"\nauth = \"none\"\n\
         [providers.open.models.\"m-1\"]\n",
        s.url
    ));
    assert_eq!(
        c.providers["gw"].auth,
        ProviderAuth::Header {
            name: "api-key".into()
        }
    );
    let set = ProviderSet::build(&c, &keys(&[("GW", KEY)], &[]));
    ok(&ask(&set, "gw", "m-1").await);
    let last = s.mock.seen().last().unwrap().clone();
    assert_eq!(last.headers["api-key"], KEY);
    assert!(!last.headers.contains_key("authorization"), "{last:?}");

    // none: no credentials at all (the mock refuses it).
    let m = ask(&set, "open", "m-1").await;
    assert_eq!(m.stop_reason, StopReason::Error);
    let last = s.mock.seen().last().unwrap().clone();
    assert!(
        !last.headers.contains_key("authorization") && !last.headers.contains_key("api-key"),
        "{last:?}"
    );
    // The probe says it was refused, and how.
    let e = probe(
        &c.providers["open"],
        &StaticKeys::default(),
        ProbeKind::Auth,
    )
    .await
    .unwrap_err();
    assert_eq!((e.failure, e.status), (ProbeFailure::Rejected, Some(401)));
}

#[tokio::test]
async fn command_tokens_are_cached_refreshed_at_expiry_and_once_on_401() {
    let s = spawn_http().await;
    let dir = scratch("command");
    let argv = token_script(&dir, "cmd-tok", 3600);
    let c = config(&format!(
        "[providers.sso]\napi = \"openai-completions\"\nbase_url = \"{}/v1\"\n\
         auth = \"command\"\nauth_command = {}\n[providers.sso.models.\"m-1\"]\n",
        s.url,
        toml_list(&argv)
    ));
    assert!(!c.providers["sso"].needs_key);
    // Only the second token works: the first request gets a 401, the command runs again, and the
    // request is retried with the new token.
    s.mock.accept("authorization", "Bearer cmd-tok-2");
    let set = ProviderSet::build(&c, &StaticKeys::default());
    ok(&ask(&set, "sso", "m-1").await);
    assert_eq!(runs(&dir), 2);
    assert_eq!(s.mock.chat_auth(), ["Bearer cmd-tok-1", "Bearer cmd-tok-2"]);
    // Cached: no new run for the next requests.
    ok(&ask(&set, "sso", "m-1").await);
    ok(&ask(&set, "sso", "m-1").await);
    assert_eq!(runs(&dir), 2);

    // A token that keeps failing: one refresh, one retry, then the error (no loop).
    s.mock.accept.lock().unwrap().clear();
    s.mock.accept("authorization", "Bearer nobody-has-this");
    let before = s.mock.chat_auth().len();
    let m = ask(&set, "sso", "m-1").await;
    assert_eq!(m.stop_reason, StopReason::Error);
    assert!(m.error_message.as_deref().unwrap().contains("401"), "{m:?}");
    assert_eq!(s.mock.chat_auth().len() - before, 2);
    assert_eq!(runs(&dir), 3);
}

#[tokio::test]
async fn command_tokens_are_refreshed_before_they_expire() {
    let s = spawn_http().await;
    let dir = scratch("command-expiry");
    let argv = token_script(&dir, "short", 1);
    let c = config(&format!(
        "[providers.sso]\napi = \"openai-completions\"\nbase_url = \"{}/v1\"\n\
         auth = \"command\"\nauth_command = {}\n[providers.sso.models.\"m-1\"]\n",
        s.url,
        toml_list(&argv)
    ));
    let set = ProviderSet::build(&c, &StaticKeys::default());
    ok(&ask(&set, "sso", "m-1").await);
    ok(&ask(&set, "sso", "m-1").await);
    assert_eq!(runs(&dir), 1, "cached while fresh");
    tokio::time::sleep(Duration::from_millis(1_200)).await;
    ok(&ask(&set, "sso", "m-1").await);
    assert_eq!(runs(&dir), 2, "re-run once it expired");
    assert_eq!(
        s.mock.chat_auth(),
        ["Bearer short-1", "Bearer short-1", "Bearer short-2"]
    );
}

#[tokio::test]
async fn a_slow_or_failing_command_is_an_error_that_leaks_nothing() {
    let s = spawn_http().await;
    let c = config(&format!(
        "[providers.slow]\napi = \"openai-completions\"\nbase_url = \"{0}/v1\"\n\
         auth = \"command\"\nauth_command = [\"/bin/sh\", \"-c\", \"sleep 10\"]\n\
         auth_command_timeout_secs = 1\n[providers.slow.models.\"m-1\"]\n\
         [providers.bad]\napi = \"openai-completions\"\nbase_url = \"{0}/v1\"\n\
         auth = \"command\"\nauth_command = [\"/bin/sh\", \"-c\", \
         \"echo debug >&2; echo denied for sk-abcdefghijklmnopqrstuvwxyz0123 >&2; exit 3\"]\n\
         [providers.bad.models.\"m-1\"]\n\
         [providers.gone]\napi = \"openai-completions\"\nbase_url = \"{0}/v1\"\n\
         auth = \"command\"\nauth_command = [\"/nonexistent/amp-2277-helper\"]\n\
         [providers.gone.models.\"m-1\"]\n",
        s.url
    ));
    let set = ProviderSet::build(&c, &StaticKeys::default());
    let started = Instant::now();
    let m = ask(&set, "slow", "m-1").await;
    assert!(
        started.elapsed() < Duration::from_secs(5),
        "{:?}",
        started.elapsed()
    );
    let e = m.error_message.unwrap();
    assert!(e.contains("did not finish within 1s"), "{e}");

    let e = ask(&set, "bad", "m-1").await.error_message.unwrap();
    assert!(e.contains("failed (exit status: 3)"), "{e}");
    assert!(e.contains("denied for <redacted>"), "{e}");
    assert!(!e.contains("sk-abcdefghijklmnopqrstuvwxyz0123"), "{e}");

    let e = probe(
        &c.providers["gone"],
        &StaticKeys::default(),
        ProbeKind::Auth,
    )
    .await
    .unwrap_err();
    assert_eq!(e.failure, ProbeFailure::Token);
    assert!(e.message.contains("was not found"), "{e}");
    // Nothing reached the endpoint.
    assert!(s.mock.seen().is_empty(), "{:?}", s.mock.seen());
}

#[tokio::test]
async fn oauth_client_credentials_fetch_refresh_and_never_show_the_secret() {
    let s = spawn_http().await;
    *s.mock.token_lifetime.lock().unwrap() = 1;
    let c = config(&format!(
        "[providers.gw]\napi = \"openai-completions\"\nbase_url = \"{0}/v1\"\n\
         auth = \"oauth-client-credentials\"\ntoken_url = \"{0}/token\"\nclient_id = \"amp\"\n\
         scopes = [\"api://gw/.default\"]\nkey_env = \"GW_SECRET\"\n\
         [providers.gw.models.\"m-1\"]\n",
        s.url
    ));
    let p = &c.providers["gw"];
    assert!(p.needs_key);
    assert_eq!(p.auth.key_word(), "client secret");
    let k = keys(&[("GW_SECRET", CLIENT_SECRET)], &[]);
    let set = ProviderSet::build(&c, &k);
    ok(&ask(&set, "gw", "m-1").await);
    let forms = s.mock.token_forms.lock().unwrap().clone();
    assert_eq!(forms.len(), 1);
    assert_eq!(forms[0]["client_id"], "amp");
    assert_eq!(forms[0]["scope"], "api://gw/.default");
    assert_eq!(s.mock.chat_auth(), ["Bearer cc-token-1"]);
    // Expired after a second: a new token before the next request.
    tokio::time::sleep(Duration::from_millis(1_200)).await;
    ok(&ask(&set, "gw", "m-1").await);
    assert_eq!(*s.mock.issued.lock().unwrap(), 2);
    assert_eq!(s.mock.chat_auth()[1], "Bearer cc-token-2");

    // On a 401 the token is replaced once: token 2 has expired, so 3 is fetched, refused, and 4
    // replaces it.
    *s.mock.token_lifetime.lock().unwrap() = 3600;
    tokio::time::sleep(Duration::from_millis(1_200)).await;
    s.mock.accept("authorization", "Bearer cc-token-4");
    ok(&ask(&set, "gw", "m-1").await);
    assert_eq!(*s.mock.issued.lock().unwrap(), 4);

    // The probe (its own token source) reports the token's expiry.
    s.mock.accept.lock().unwrap().clear();
    let found = probe(p, &k, ProbeKind::Catalog).await.unwrap();
    assert!(found.token_expires_at.is_some());

    // A wrong secret: the endpoint's error, never the secret.
    let wrong = keys(&[("GW_SECRET", "cs-wrong-2277-planted")], &[]);
    let e = probe(p, &wrong, ProbeKind::Auth).await.unwrap_err();
    assert_eq!(e.failure, ProbeFailure::Token);
    assert!(
        e.message
            .contains("refused the client credentials (HTTP 401): invalid_client"),
        "{e}"
    );
    assert!(!e.message.contains("cs-wrong") && !e.message.contains(CLIENT_SECRET));
    let set = ProviderSet::build(&c, &wrong);
    let m = ask(&set, "gw", "m-1").await;
    assert!(!format!("{m:?}{set:?}").contains("cs-wrong"));
}

/// #2309 (H18 finding 6): a provider endpoint that redirects to another origin never gets the
/// key in its own header, a secret header, or an OAuth client secret forwarded; a redirect on the
/// same origin is still followed, credentials and all.
#[tokio::test]
async fn a_redirect_to_another_origin_carries_no_credentials() {
    let other = spawn_http().await;
    let gw = spawn_http().await;
    *gw.mock.redirect.lock().unwrap() = Some(other.url.clone());
    let c = config(&format!(
        "[providers.hdr]\napi = \"openai-completions\"\nbase_url = \"{0}/v1\"\nkey_env = \"GW\"\n\
         auth = \"header\"\n[providers.hdr.models.\"m-1\"]\n\
         [providers.sec]\napi = \"openai-completions\"\nbase_url = \"{0}/v1\"\nkey_env = \"GW\"\n\
         secret_headers = [\"X-Api-Key\"]\n[providers.sec.models.\"m-1\"]\n\
         [providers.cc]\napi = \"openai-completions\"\nbase_url = \"{0}/v1\"\n\
         auth = \"oauth-client-credentials\"\ntoken_url = \"{0}/token\"\nclient_id = \"amp\"\n\
         key_env = \"GW_SECRET\"\n[providers.cc.models.\"m-1\"]\n\
         [providers.same]\napi = \"openai-completions\"\nbase_url = \"{1}/moved/v1\"\n\
         key_env = \"GW\"\nauth = \"header\"\n[providers.same.models.\"m-1\"]\n",
        gw.url, other.url
    ));
    let k = keys(
        &[("GW", KEY), ("GW_SECRET", CLIENT_SECRET)],
        &[("sec:header:x-api-key", "xk-2309-planted-secret-header")],
    );
    let set = ProviderSet::build(&c, &k);
    for id in ["hdr", "sec", "cc"] {
        let m = ask(&set, id, "m-1").await;
        assert_eq!(m.stop_reason, StopReason::Error, "{id}: {m:?}");
        let shown = format!("{m:?}");
        assert!(
            !shown.contains(KEY) && !shown.contains(CLIENT_SECRET),
            "{id}"
        );
    }
    // The gateway saw each request (with its credentials); the other origin saw nothing at all.
    assert!(gw.mock.seen().len() >= 3, "{:?}", gw.mock.seen());
    assert!(other.mock.seen().is_empty(), "{:?}", other.mock.seen());
    // The probe says why, naming only the target's origin.
    let e = probe(&c.providers["hdr"], &k, ProbeKind::Auth)
        .await
        .unwrap_err();
    assert!(e.message.contains("another origin"), "{e}");
    assert!(other.mock.seen().is_empty());

    // Same origin: `/moved/v1/chat/completions` -> `/v1/chat/completions`, key still sent.
    ok(&ask(&set, "same", "m-1").await);
    let seen = other.mock.seen();
    let last = seen.last().unwrap();
    assert_eq!(last.uri, "/v1/chat/completions");
    assert_eq!(last.headers["api-key"], KEY);
}

#[tokio::test]
async fn azure_entra_client_credentials_use_the_tenant_endpoint_and_cognitive_scope() {
    let s = spawn_http().await;
    let c = config(&format!(
        "[providers.aoai]\napi = \"openai-completions\"\nbase_url = \"{0}/v1\"\n\
         auth = \"azure-entra\"\ntenant_id = \"tenant-2277\"\nclient_id = \"app-1\"\n\
         authority_host = \"{0}\"\nkey_env = \"AOAI_SECRET\"\n[providers.aoai.models.\"gpt-4o\"]\n",
        s.url
    ));
    let k = keys(&[("AOAI_SECRET", CLIENT_SECRET)], &[]);
    probe(&c.providers["aoai"], &k, ProbeKind::Auth)
        .await
        .unwrap();
    let set = ProviderSet::build(&c, &k);
    ok(&ask(&set, "aoai", "gpt-4o").await);
    let seen = s.mock.seen();
    let token_call = seen
        .iter()
        .find(|r| r.uri.contains("/oauth2/v2.0/token"))
        .unwrap();
    assert_eq!(token_call.uri, "/tenant-2277/oauth2/v2.0/token");
    let forms = s.mock.token_forms.lock().unwrap().clone();
    assert_eq!(
        forms[0]["scope"],
        "https://cognitiveservices.azure.com/.default"
    );
    assert_eq!(forms[0]["client_id"], "app-1");
    // The catalog call (the probe's own token) and the chat request carried Entra tokens.
    assert_eq!(s.mock.chat_auth(), ["Bearer cc-token-2"]);
    let models_call = seen.iter().find(|r| r.uri.contains("/models")).unwrap();
    assert_eq!(models_call.headers["authorization"], "Bearer cc-token-1");

    // Without a tenant and client id it is `az account get-access-token`.
    let c = config(
        "[providers.az]\napi = \"openai-completions\"\nbase_url = \"https://r.openai.azure.com/openai/v1\"\n\
         auth = \"azure-entra\"\n[providers.az.models.\"gpt-4o\"]\n",
    );
    let p = &c.providers["az"];
    assert!(!p.needs_key);
    match &p.auth {
        ProviderAuth::AzureEntra(inner) => match inner.as_ref() {
            ProviderAuth::Command { argv, .. } => assert_eq!(
                argv.join(" "),
                "az account get-access-token --scope https://cognitiveservices.azure.com/.default --output json"
            ),
            other => panic!("{other:?}"),
        },
        other => panic!("{other:?}"),
    }
}

#[tokio::test]
async fn a_private_ca_is_trusted_only_with_ca_file() {
    let pki = pki();
    let s = spawn_https(&pki, false).await;
    let dir = scratch("ca");
    let ca = dir.join("ca.pem");
    std::fs::write(&ca, &pki.ca_pem).unwrap();
    let table = |extra: &str| {
        format!(
            "[providers.tls]\napi = \"openai-completions\"\nbase_url = \"{}/v1\"\nauth = \"none\"\n\
             {extra}[providers.tls.models.\"m-1\"]\n",
            s.url
        )
    };
    let without = config(&table(""));
    let e = probe(
        &without.providers["tls"],
        &StaticKeys::default(),
        ProbeKind::Auth,
    )
    .await
    .unwrap_err();
    assert_eq!(e.failure, ProbeFailure::Tls, "{e}");
    assert!(
        e.message.to_ascii_lowercase().contains("certificate"),
        "{e}"
    );

    let with = config(&table(&format!(
        "ca_file = {:?}\n",
        ca.display().to_string()
    )));
    probe(
        &with.providers["tls"],
        &StaticKeys::default(),
        ProbeKind::Auth,
    )
    .await
    .unwrap();
    let set = ProviderSet::build(&with, &StaticKeys::default());
    ok(&ask(&set, "tls", "m-1").await);

    // A CA file that is not PEM: the provider is unavailable, with the path in the reason.
    let junk = dir.join("junk.pem");
    std::fs::write(&junk, "not a certificate").unwrap();
    let bad = config(&table(&format!(
        "ca_file = {:?}\n",
        junk.display().to_string()
    )));
    let set = ProviderSet::build(&bad, &StaticKeys::default());
    let why = set.unavailable()["tls"].clone();
    assert!(why.contains("junk.pem"), "{why}");
}

#[tokio::test]
async fn mtls_with_the_key_in_a_file_or_in_the_keychain() {
    let pki = pki();
    let s = spawn_https(&pki, true).await;
    let dir = scratch("mtls");
    let ca = dir.join("ca.pem");
    let cert = dir.join("client.pem");
    let key = dir.join("client.key");
    std::fs::write(&ca, &pki.ca_pem).unwrap();
    std::fs::write(&cert, &pki.client_cert_pem).unwrap();
    std::fs::write(&key, &pki.client_key_pem).unwrap();
    let table = |extra: &str| {
        format!(
            "[providers.m]\napi = \"openai-completions\"\nbase_url = \"{}/v1\"\nauth = \"none\"\n\
             ca_file = {:?}\n{extra}[providers.m.models.\"m-1\"]\n",
            s.url,
            ca.display().to_string()
        )
    };
    // No client certificate: the server ends the handshake.
    let c = config(&table(""));
    let e = probe(&c.providers["m"], &StaticKeys::default(), ProbeKind::Auth)
        .await
        .unwrap_err();
    assert!(
        matches!(e.failure, ProbeFailure::Tls | ProbeFailure::Network),
        "{e:?}"
    );

    // The key in a file.
    let c = config(&table(&format!(
        "client_cert = {:?}\nclient_key = {:?}\n",
        cert.display().to_string(),
        key.display().to_string()
    )));
    let set = ProviderSet::build(&c, &StaticKeys::default());
    ok(&ask(&set, "m", "m-1").await);

    // The key in the keychain (provider:m:client-key).
    let c = config(&table(&format!(
        "client_cert = {:?}\n",
        cert.display().to_string()
    )));
    let k = keys(&[], &[("m:client-key", &pki.client_key_pem)]);
    let set = ProviderSet::build(&c, &k);
    ok(&ask(&set, "m", "m-1").await);
    assert!(!format!("{set:?}").contains("PRIVATE KEY"));

    // Not in the keychain: unavailable, saying how to fix it, with no key material.
    let set = ProviderSet::build(&c, &keys(&[], &[]));
    let why = set.unavailable()["m"].clone();
    assert!(why.contains("no mTLS private key"), "{why}");
    assert!(why.contains("--store-client-key"), "{why}");
}

#[tokio::test]
async fn a_provider_proxy_carries_its_requests() {
    // The mock is the proxy: requests arrive in absolute form for the upstream host.
    let proxy = spawn_http().await;
    let c = config(&format!(
        "[providers.far]\napi = \"openai-completions\"\nbase_url = \"http://upstream.invalid:9/v1\"\n\
         auth = \"none\"\nproxy = \"{}\"\n[providers.far.models.\"m-1\"]\n",
        proxy.url
    ));
    let set = ProviderSet::build(&c, &StaticKeys::default());
    ok(&ask(&set, "far", "m-1").await);
    let seen = proxy.mock.seen();
    assert_eq!(
        seen.last().unwrap().uri,
        "http://upstream.invalid:9/v1/chat/completions"
    );
    live_model_ids(&c.providers["far"], &StaticKeys::default())
        .await
        .unwrap();
    assert_eq!(
        proxy.mock.seen().last().unwrap().uri,
        "http://upstream.invalid:9/v1/models"
    );
}

#[test]
fn bad_auth_settings_are_refused_with_the_field_named() {
    let base = "[providers.x]\napi = \"openai-completions\"\nbase_url = \"https://x.example/v1\"\n";
    for (extra, want) in [
        ("auth = \"magic\"\n", "unknown auth `magic`"),
        ("auth = \"command\"\n", "needs `auth_command"),
        (
            "auth_header = \"api-key\"\n",
            "`auth_header` applies to auth = \"header\"",
        ),
        (
            "auth = \"oauth-client-credentials\"\nclient_id = \"a\"\n",
            "needs `token_url`",
        ),
        (
            "auth = \"oauth-client-credentials\"\ntoken_url = \"http://idp.example/token\"\nclient_id = \"a\"\n",
            "must be https://",
        ),
        (
            "auth = \"azure-entra\"\ntenant_id = \"t\"\n",
            "need both `tenant_id` and `client_id`",
        ),
        (
            "tenant_id = \"t\"\n",
            "`tenant_id` applies to auth = \"azure-entra\"",
        ),
        (
            "client_key = \"/k.pem\"\n",
            "`client_key` needs `client_cert`",
        ),
        ("proxy = \"socks5://p:1080\"\n", "SOCKS"),
        ("proxy = \"http://u:hunter2@p:3128\"\n", "holds a password"),
        (
            "organization = \"o\"\nheaders = { \"OpenAI-Organization\" = \"o\" }\n",
            "keep one",
        ),
    ] {
        let e = config_err(&format!("{base}{extra}"));
        assert!(e.contains(want), "{extra}: {e}");
        assert!(!e.contains("hunter2"), "{e}");
    }
    // Old configs keep working: `auth = "key"` is bearer.
    let c = config(&format!("{base}auth = \"key\"\n"));
    assert_eq!(c.providers["x"].auth, ProviderAuth::Bearer);
}
