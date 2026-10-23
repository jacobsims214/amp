//! M4-2 (#2114) doctor checks, each with a passing and a failing case, run through the binary:
//! - `ca`: an HTTPS mock AMP signed by a private CA (untrusted, trusted with --ca-file, a bad CA
//!   file, an expired CA, a CA file on a plain-http URL);
//! - `key:<provider>` / `models:<provider>`: a mock OpenAI-compatible provider catalog (listed,
//!   missing, unreachable, no key, `--offline`);
//! - `versions`: harness, pins, asset commit, negotiated protocol (also in `--version`).
//!
//! No secret value may appear in any output.

mod common;

use std::{
    net::SocketAddr,
    path::Path,
    sync::{Arc, Mutex},
};

use axum::{Json, Router, extract::State, http::HeaderMap, routing::get};
use common::{MockAmp, Output, run, spawn, workdir};
use rcgen::{
    BasicConstraints, CertificateParams, CertifiedIssuer, DnType, ExtendedKeyUsagePurpose, IsCa,
    KeyPair, KeyUsagePurpose,
};
use serde_json::{Value, json};
use tokio::net::{TcpListener, TcpStream};
use tokio_rustls::{
    TlsAcceptor,
    rustls::{
        self,
        pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer},
    },
    server::TlsStream,
};

const OR_KEY: (&str, &str) = ("OPENROUTER_API_KEY", "sk-or-FAKE-not-printed");
const MOCK_KEY: &str = "mock-SECRET-key-value";

fn check<'a>(v: &'a Value, name: &str) -> &'a Value {
    v["checks"]
        .as_array()
        .unwrap()
        .iter()
        .find(|c| c["name"] == name)
        .unwrap_or_else(|| panic!("no check {name}: {v}"))
}

fn status(v: &Value, name: &str) -> String {
    check(v, name)["status"].as_str().unwrap().to_owned()
}

fn fix(v: &Value, name: &str) -> String {
    check(v, name)["fix"].as_str().unwrap_or("").to_owned()
}

fn assert_no_secret(out: &Output, secrets: &[&str]) {
    for s in secrets {
        assert!(
            !out.stdout.contains(s) && !out.stderr.contains(s),
            "secret printed:\n{}{}",
            out.stdout,
            out.stderr
        );
    }
}

// --- ca -------------------------------------------------------------------------------------------

struct Pki {
    ca_pem: String,
    leaf_der: CertificateDer<'static>,
    leaf_key: PrivateKeyDer<'static>,
}

/// A private CA (valid for `ca_days` from yesterday; negative = already expired) and a
/// `localhost` server certificate it signed. Short validity: macOS rejects long-lived TLS certs.
fn pki(ca_days: i64) -> Pki {
    let now = time::OffsetDateTime::now_utc();
    let mut ca = CertificateParams::new(Vec::<String>::new()).unwrap();
    ca.distinguished_name
        .push(DnType::CommonName, "doctor test CA");
    ca.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    ca.key_usages = vec![
        KeyUsagePurpose::KeyCertSign,
        KeyUsagePurpose::CrlSign,
        KeyUsagePurpose::DigitalSignature,
    ];
    ca.not_before = now - time::Duration::days(40);
    ca.not_after = now + time::Duration::days(ca_days);
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
    leaf.not_after = now + time::Duration::days(20);
    let leaf_key = KeyPair::generate().unwrap();
    let leaf_cert = leaf.signed_by(&leaf_key, &ca).unwrap();
    Pki {
        ca_pem: ca.pem(),
        leaf_der: leaf_cert.der().clone(),
        leaf_key: PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(leaf_key.serialize_der())),
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

struct Server {
    url: String,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Server {
    fn drop(&mut self) {
        self.task.abort();
    }
}

async fn spawn_https(pki: &Pki) -> Server {
    let provider = Arc::new(rustls::crypto::aws_lc_rs::default_provider());
    let config = rustls::ServerConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .unwrap()
        .with_no_client_auth()
        .with_single_cert(vec![pki.leaf_der.clone()], pki.leaf_key.clone_key())
        .unwrap();
    let tcp = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = tcp.local_addr().unwrap().port();
    let listener = TlsListener {
        tcp,
        acceptor: TlsAcceptor::from(Arc::new(config)),
    };
    let app = common::app(&MockAmp::new(), None);
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Server {
        url: format!("https://localhost:{port}/mcp"),
        task,
    }
}

#[tokio::test]
async fn ca_untrusted_then_trusted_with_the_ca_file() {
    let pki = pki(30);
    let server = spawn_https(&pki).await;
    let dir = workdir("doctor-ca-trust", None);
    let ca_path = dir.join("amp-local-ca.crt");
    std::fs::write(&ca_path, &pki.ca_pem).unwrap();

    // Fail: the private CA is not trusted, and the fix says how to trust it.
    let out = run(
        &dir,
        &server.url,
        &["doctor", "--json", "--offline"],
        &[OR_KEY],
    )
    .await;
    assert_eq!(out.code, 1, "{}", out.stdout);
    let v = out.json();
    assert_eq!(status(&v, "ca"), "fail");
    assert!(
        check(&v, "ca")["detail"]
            .as_str()
            .unwrap()
            .contains("not signed by a CA this machine trusts"),
        "{v}"
    );
    assert!(fix(&v, "ca").contains("--ca-file"), "{v}");
    assert!(fix(&v, "ca").contains("AMP_CA_FILE"), "{v}");
    assert_eq!(status(&v, "connect"), "fail");
    assert!(fix(&v, "connect").contains("`ca` check"), "{v}");
    assert!(v["ca"]["tls_error"].is_string());

    // The human output prints the fix under the failing check.
    let out = run(&dir, &server.url, &["doctor", "--offline"], &[OR_KEY]).await;
    assert!(out.stdout.contains("[FAIL] ca "), "{}", out.stdout);
    // #2164: the fix saves the CA with the server instead of a flag on every command.
    assert!(
        out.stdout
            .contains("fix: save the server's CA with it: `amp-harness server set"),
        "{}",
        out.stdout
    );

    // Pass: with the CA file (flag, then env).
    let ca = ca_path.to_str().unwrap();
    for (args, env) in [
        (
            vec!["doctor", "--json", "--offline", "--ca-file", ca],
            vec![OR_KEY],
        ),
        (
            vec!["doctor", "--json", "--offline"],
            vec![OR_KEY, ("AMP_CA_FILE", ca)],
        ),
    ] {
        let out = run(&dir, &server.url, &args, &env).await;
        assert_eq!(out.code, 0, "{}{}", out.stdout, out.stderr);
        let v = out.json();
        assert_eq!(status(&v, "ca"), "ok");
        let detail = check(&v, "ca")["detail"].as_str().unwrap();
        assert!(
            detail.starts_with("CN=doctor test CA from ") && detail.contains("valid until"),
            "{detail}"
        );
        assert_eq!(v["ca"]["trust"], "system + file");
        assert_eq!(v["ca"]["certificates"][0]["expired"], false);
        assert_eq!(status(&v, "connect"), "ok");
    }
}

#[tokio::test]
async fn ca_file_problems_are_reported_with_a_fix() {
    let dir = workdir("doctor-ca-bad", None);
    let pki = pki(30);
    let server = spawn_https(&pki).await;

    // Not PEM.
    let bad = dir.join("bad.crt");
    std::fs::write(&bad, "not a certificate").unwrap();
    let out = run(
        &dir,
        &server.url,
        &["doctor", "--json", "--offline"],
        &[OR_KEY, ("AMP_CA_FILE", bad.to_str().unwrap())],
    )
    .await;
    assert_eq!(out.code, 1);
    let v = out.json();
    assert_eq!(status(&v, "ca"), "fail");
    assert!(
        fix(&v, "ca").starts_with("point AMP_CA_FILE at the server's CA"),
        "{v}"
    );

    // Missing.
    let missing = dir.join("missing.crt");
    let out = run(
        &dir,
        &server.url,
        &[
            "doctor",
            "--json",
            "--offline",
            "--ca-file",
            missing.to_str().unwrap(),
        ],
        &[OR_KEY],
    )
    .await;
    let v = out.json();
    assert_eq!(status(&v, "ca"), "fail");
    assert!(fix(&v, "ca").starts_with("point --ca-file at"), "{v}");

    // Expired.
    let expired = pki_expired_ca(&dir);
    let out = run(
        &dir,
        &server.url,
        &[
            "doctor",
            "--json",
            "--offline",
            "--ca-file",
            expired.to_str().unwrap(),
        ],
        &[OR_KEY],
    )
    .await;
    let v = out.json();
    assert_eq!(status(&v, "ca"), "fail", "{v}");
    assert!(
        check(&v, "ca")["detail"]
            .as_str()
            .unwrap()
            .contains("expired on"),
        "{v}"
    );
    assert!(
        fix(&v, "ca").contains("export the server's current CA"),
        "{v}"
    );
    assert_eq!(v["ca"]["certificates"][0]["expired"], true);

    // A CA file on a plain-http URL is unused: a warning, not a failure.
    let good = dir.join("good.crt");
    std::fs::write(&good, &pki.ca_pem).unwrap();
    let amp = spawn(MockAmp::new(), None).await;
    let out = run(
        &dir,
        &amp.url,
        &[
            "doctor",
            "--json",
            "--offline",
            "--ca-file",
            good.to_str().unwrap(),
        ],
        &[OR_KEY],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stdout);
    let v = out.json();
    assert_eq!(status(&v, "ca"), "warn");
    assert!(fix(&v, "ca").contains("https://"), "{v}");
}

fn pki_expired_ca(dir: &Path) -> std::path::PathBuf {
    let path = dir.join("expired.crt");
    std::fs::write(&path, pki(-2).ca_pem).unwrap();
    path
}

// --- key:* / models:* -----------------------------------------------------------------------------

#[derive(Clone, Default)]
struct Catalog {
    ids: Arc<Mutex<Vec<&'static str>>>,
    auth: Arc<Mutex<Vec<String>>>,
}

async fn models(State(c): State<Catalog>, headers: HeaderMap) -> Json<Value> {
    if let Some(auth) = headers.get("authorization").and_then(|v| v.to_str().ok()) {
        c.auth.lock().unwrap().push(auth.to_owned());
    }
    let data: Vec<Value> = c
        .ids
        .lock()
        .unwrap()
        .iter()
        .map(|id| json!({"id": id, "object": "model"}))
        .collect();
    Json(json!({"object": "list", "data": data}))
}

async fn spawn_catalog(c: Catalog) -> Server {
    let app = Router::new().route("/v1/models", get(models)).with_state(c);
    let tcp = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = tcp.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let _ = axum::serve(tcp, app).await;
    });
    Server {
        url: format!("http://{addr}/v1"),
        task,
    }
}

/// Every role on `mock/m-1` (inherit), except the reviewer on `mock/m-2`.
fn write_config(dir: &Path, base_url: &str) {
    let cfg = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&cfg).unwrap();
    std::fs::write(
        cfg.join("config.toml"),
        format!(
            "[models]\nprofile = \"inherit\"\ninherit = \"mock/m-1\"\n\
             [providers.mock]\napi = \"openai-completions\"\nbase_url = \"{base_url}\"\n\
             key_env = \"MOCK_KEY\"\n\
             [providers.mock.models.\"m-1\"]\n[providers.mock.models.\"m-2\"]\n\
             [roles.reviewer]\nmodel = \"mock/m-2\"\n"
        ),
    )
    .unwrap();
}

#[tokio::test]
async fn provider_keys_and_pinned_models() {
    let amp = spawn(MockAmp::new(), None).await;
    let catalog = Catalog::default();
    *catalog.ids.lock().unwrap() = vec!["m-1", "m-2", "other"];
    let provider = spawn_catalog(catalog.clone()).await;
    let dir = workdir("doctor-models", None);
    write_config(&dir, &provider.url);
    let key = [("MOCK_KEY", MOCK_KEY)];

    // Pass: key from env, both pinned models listed; the key went to the provider only.
    let out = run(&dir, &amp.url, &["doctor", "--json"], &key).await;
    assert_eq!(out.code, 0, "{}{}", out.stdout, out.stderr);
    assert_no_secret(&out, &[MOCK_KEY]);
    let v = out.json();
    assert_eq!(status(&v, "roles"), "ok");
    assert_eq!(check(&v, "key:mock")["detail"], "key from env MOCK_KEY");
    assert_eq!(
        check(&v, "models:mock")["detail"],
        "2 models listed: m-1, m-2"
    );
    assert_eq!(v["models"]["mode"], "inherit");
    let p = &v["models"]["providers"][0];
    assert_eq!(
        (&p["id"], &p["key"], &p["catalog_checked"]),
        (&json!("mock"), &json!("env"), &json!(true))
    );
    assert_eq!(
        p["models"][1],
        json!({"id": "m-2", "roles": ["reviewer"], "live": true})
    );
    assert_eq!(
        catalog.auth.lock().unwrap().as_slice(),
        [format!("Bearer {MOCK_KEY}")]
    );

    // Fail: the reviewer's model is gone from the catalog.
    *catalog.ids.lock().unwrap() = vec!["m-1"];
    let out = run(&dir, &amp.url, &["doctor", "--json"], &key).await;
    assert_eq!(out.code, 1);
    let v = out.json();
    assert_eq!(status(&v, "models:mock"), "fail");
    assert_eq!(
        check(&v, "models:mock")["detail"],
        "mock does not list m-2 (roles: reviewer)"
    );
    assert!(
        fix(&v, "models:mock").contains("[roles.reviewer] model = \"mock/<model id>\""),
        "{v}"
    );
    assert_eq!(v["models"]["providers"][0]["models"][1]["live"], false);
    let human = run(&dir, &amp.url, &["doctor"], &key).await;
    assert!(
        human.stdout.contains("[FAIL] models:mock"),
        "{}",
        human.stdout
    );
    assert!(
        human.stdout.contains("Failed: models:mock"),
        "{}",
        human.stdout
    );

    // Skip: --offline does not call the provider.
    let calls = catalog.auth.lock().unwrap().len();
    let out = run(&dir, &amp.url, &["doctor", "--json", "--offline"], &key).await;
    assert_eq!(out.code, 0);
    assert_eq!(status(&out.json(), "models:mock"), "skip");
    assert_eq!(catalog.auth.lock().unwrap().len(), calls);

    // Fail: no key for the manager's provider; the model check is skipped.
    let out = run(&dir, &amp.url, &["doctor", "--json"], &[]).await;
    assert_eq!(out.code, 1);
    let v = out.json();
    assert_eq!(status(&v, "key:mock"), "fail");
    assert!(
        fix(&v, "key:mock").starts_with("set MOCK_KEY in the environment"),
        "{v}"
    );
    assert_eq!(status(&v, "models:mock"), "skip");
    assert_eq!(v["models"]["providers"][0]["key"], "missing");
}

#[tokio::test]
async fn unreachable_catalog_warns_and_other_providers_key_warns() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("doctor-models-down", None);
    // Nothing listens there.
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let base = format!("http://{}/v1", listener.local_addr().unwrap());
    drop(listener);
    write_config(&dir, &base);
    // A second provider only the reviewer uses, without its key.
    let cfg = dir.join("home/.config/amp-harness/config.toml");
    let text = std::fs::read_to_string(&cfg).unwrap().replace(
        "model = \"mock/m-2\"",
        "model = \"anthropic/claude-opus-5-5\"",
    );
    std::fs::write(&cfg, text).unwrap();

    let out = run(
        &dir,
        &amp.url,
        &["doctor", "--json"],
        &[("MOCK_KEY", MOCK_KEY)],
    )
    .await;
    assert_no_secret(&out, &[MOCK_KEY]);
    // Warnings only: the manager can still run.
    assert_eq!(out.code, 0, "{}", out.stdout);
    let v = out.json();
    assert_eq!(status(&v, "models:mock"), "warn");
    assert!(fix(&v, "models:mock").contains("--offline"), "{v}");
    assert_eq!(status(&v, "key:anthropic"), "warn");
    assert!(
        check(&v, "key:anthropic")["detail"]
            .as_str()
            .unwrap()
            .ends_with("cannot run: reviewer"),
        "{v}"
    );
    assert!(
        fix(&v, "key:anthropic").starts_with("set ANTHROPIC_API_KEY in the environment"),
        "{v}"
    );

    let human = run(&dir, &amp.url, &["doctor"], &[("MOCK_KEY", MOCK_KEY)]).await;
    assert!(
        human.stdout.contains("No failures (3 warnings)."),
        "{}",
        human.stdout
    );
}

#[tokio::test]
async fn a_broken_models_config_fails_roles() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("doctor-models-broken", None);
    let cfg = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&cfg).unwrap();
    std::fs::write(
        cfg.join("config.toml"),
        "[roles.manager]\nmodle = \"x/y\"\n",
    )
    .unwrap();
    let out = run(
        &dir,
        &amp.url,
        &["doctor", "--json", "--offline"],
        &[OR_KEY],
    )
    .await;
    assert_eq!(out.code, 1);
    let v = out.json();
    assert_eq!(status(&v, "roles"), "fail");
    assert!(fix(&v, "roles").contains("config.toml"), "{v}");
    // AMP's checks still ran.
    assert_eq!(status(&v, "whoami"), "ok");
}

// --- versions -------------------------------------------------------------------------------------

#[tokio::test]
async fn versions_report_pins_assets_and_protocol() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("doctor-versions", None);
    let out = run(
        &dir,
        &amp.url,
        &["doctor", "--json", "--offline"],
        &[OR_KEY],
    )
    .await;
    let v = out.json();
    assert_eq!(status(&v, "versions"), "ok");
    let detail = check(&v, "versions")["detail"].as_str().unwrap();
    let commit = v["versions"]["assets_commit"].as_str().unwrap();
    assert!(
        detail.contains(&format!("assets {commit}, MCP 2026-07-28")),
        "{detail}"
    );
    assert_eq!(v["versions"]["mcp_protocol"], "2026-07-28");

    // Not connected: the versions are still reported, without a protocol.
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let down = format!("http://{}/mcp", listener.local_addr().unwrap());
    drop(listener);
    let v = run(&dir, &down, &["doctor", "--json", "--offline"], &[OR_KEY])
        .await
        .json();
    assert_eq!(status(&v, "versions"), "ok");
    assert_eq!(v["versions"]["mcp_protocol"], Value::Null);
    assert!(
        check(&v, "versions")["detail"]
            .as_str()
            .unwrap()
            .ends_with("MCP not connected")
    );

    // `--version` carries the asset commit too (D11).
    let out = std::process::Command::new(env!("CARGO_BIN_EXE_amp-harness"))
        .arg("--version")
        .output()
        .unwrap();
    let text = String::from_utf8(out.stdout).unwrap();
    assert!(text.contains(&format!("\nassets {commit}")), "{text}");
}

/// #2206: a `.amp.json` made for another server (its `amp_api`), or naming another project than
/// this server's, is a [warn] with the way out; a marker that agrees is ok.
#[tokio::test]
async fn a_marker_made_for_another_server_is_a_warning() {
    let amp = spawn(MockAmp::new(), None).await;
    // workdir's marker: project #2 "AMP v2", amp_api https://stale.invalid.
    let dir = workdir("doctor-foreign-marker", Some(2));
    let marker = dir.join("project/.amp.json");
    let out = run(
        &dir,
        &amp.url,
        &["doctor", "--json", "--offline"],
        &[OR_KEY],
    )
    .await;
    assert_eq!(out.code, 0, "{}", out.stdout);
    let v = out.json();
    let want = format!(
        ".amp.json at {} was made for https://stale.invalid; this server's #2 is \"AMP v2\"",
        marker.display()
    );
    assert_eq!(status(&v, "project"), "warn");
    assert_eq!(check(&v, "project")["detail"], want.as_str());
    assert!(
        fix(&v, "project").contains("run `amp-harness init` here"),
        "{v}"
    );
    assert_eq!(v["project"]["warning"], want.as_str());
    // The text report shows it as [warn] with the fix.
    let out = run(&dir, &amp.url, &["doctor", "--offline"], &[OR_KEY]).await;
    assert!(out.stdout.contains("[warn] project"), "{}", out.stdout);
    assert!(out.stdout.contains(&want), "{}", out.stdout);

    // Same origin, another name: still a warning.
    let origin = amp.url.trim_end_matches("/mcp");
    std::fs::write(
        &marker,
        json!({"project_id": 2, "project_name": "Somebody else's", "amp_api": origin}).to_string(),
    )
    .unwrap();
    let v = run(
        &dir,
        &amp.url,
        &["doctor", "--json", "--offline"],
        &[OR_KEY],
    )
    .await
    .json();
    assert_eq!(status(&v, "project"), "warn");
    assert!(
        check(&v, "project")["detail"]
            .as_str()
            .unwrap()
            .ends_with("names project #2 \"Somebody else's\", but this server's #2 is \"AMP v2\""),
        "{v}"
    );

    // Made for this server (127.0.0.1 and localhost are one host) with its name: ok.
    std::fs::write(
        &marker,
        json!({"project_id": 2, "project_name": "AMP v2",
               "amp_api": origin.replace("127.0.0.1", "localhost")})
        .to_string(),
    )
    .unwrap();
    let v = run(
        &dir,
        &amp.url,
        &["doctor", "--json", "--offline"],
        &[OR_KEY],
    )
    .await
    .json();
    assert_eq!(status(&v, "project"), "ok", "{v}");
    assert_eq!(v["project"]["warning"], Value::Null);
}

// --- connect (#2208) ------------------------------------------------------------------------------

/// A refused connection reads as the one sentence the other commands print, with no rmcp types and
/// no AMP-repo hint (`make up`, :8000); the cause chain appears only with --verbose. The URL flag
/// works before the subcommand too (`amp-harness --url X doctor`).
#[tokio::test]
async fn doctor_connect_failure_is_one_sentence() {
    let dir = workdir("doctor-connect-sentence", None);
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    drop(listener);
    let url = format!("http://{addr}/mcp");
    let sentence = format!("Can't reach AMP at {addr} (connection refused).");

    for args in [
        &["doctor", "--offline", "--url", &url][..],
        &["--url", &url, "doctor", "--offline"][..],
    ] {
        // AMP_MCP_URL points elsewhere: the flag must win in both orders.
        let out = run(&dir, "http://127.0.0.1:9/mcp", args, &[OR_KEY]).await;
        assert_eq!(out.code, 1, "{args:?}: {}{}", out.stdout, out.stderr);
        assert!(out.stderr.is_empty(), "{args:?}: {}", out.stderr);
        let line = out
            .stdout
            .lines()
            .find(|l| l.contains("[FAIL] connect"))
            .unwrap_or_else(|| panic!("{args:?}: {}", out.stdout));
        assert!(line.trim_end().ends_with(&sentence), "{line}");
        for leaked in [
            "rmcp::",
            "Transport",
            "os error",
            "make up",
            ":8000",
            "cause:",
        ] {
            assert!(!out.stdout.contains(leaked), "{leaked}: {}", out.stdout);
        }
        assert!(
            out.stdout.contains(&format!(
                "fix: is AMP running at {url} (from flag)? Start it, or"
            )),
            "{}",
            out.stdout
        );
    }

    // --verbose (either side of the subcommand) adds the chain on a cause: line under connect.
    for args in [
        &["doctor", "--offline", "--verbose"][..],
        &["--verbose", "doctor", "--offline"][..],
    ] {
        let out = run(&dir, &url, args, &[OR_KEY]).await;
        let lines: Vec<&str> = out.stdout.lines().collect();
        let at = lines
            .iter()
            .position(|l| l.contains("[FAIL] connect"))
            .unwrap_or_else(|| panic!("{args:?}: {}", out.stdout));
        let cause = lines[at + 1..]
            .iter()
            .take(2)
            .find(|l| l.trim_start().starts_with("cause: "))
            .unwrap_or_else(|| panic!("{args:?}: {}", out.stdout));
        assert!(cause.contains("onnection refused"), "{cause}");
    }

    // JSON: the check's detail is the sentence; connection.error keeps the full chain for scripts.
    let out = run(&dir, &url, &["doctor", "--json", "--offline"], &[OR_KEY]).await;
    let v = out.json();
    assert_eq!(check(&v, "connect")["detail"], sentence.as_str());
    assert!(v["connection"]["error"].as_str().unwrap().len() > sentence.len());
}
