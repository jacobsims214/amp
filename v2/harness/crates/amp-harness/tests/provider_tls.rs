//! #2333: a model endpoint whose certificate this machine does not trust. A self-signed CA made
//! here (never added to any trust store) signs a `localhost` certificate for the mock
//! OpenAI-compatible provider, served over TLS.
//!
//! - `connect` without `--ca-file`: one sentence naming the problem and the `--ca-file` fix, no
//!   `--model ID` hint, no raw rustls text unless `--verbose`; nothing stored. With `--ca-file` it
//!   connects.
//! - an expired certificate and one issued for another name: their own sentence, no `--model`.
//! - `doctor`'s `models:<id>`: the same sentence and fix; the cause only with `--verbose`.
//! - the first run's own-endpoint path asks for the CA when the endpoint fails without one.
//!
//! HOME and config live under cargo's target dir; no keychain (`AMP_CREDENTIAL_STORE=file`),
//! the key comes from the environment and must never be printed.

mod common;

use std::{net::SocketAddr, path::Path, process::Stdio, sync::Arc, time::Duration};

use common::{
    MockAmp, Output,
    provider::{ProviderState, provider_app},
    spawn, workdir,
};
use rcgen::{
    BasicConstraints, CertificateParams, CertifiedIssuer, DnType, ExtendedKeyUsagePurpose, IsCa,
    KeyPair, KeyUsagePurpose,
};
use tokio::{
    io::AsyncWriteExt,
    net::{TcpListener, TcpStream},
};
use tokio_rustls::{
    TlsAcceptor,
    rustls::{
        self,
        pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer},
    },
    server::TlsStream,
};

const KEY: &str = "sk-example-2333-never-print-me";

// --- a private CA and an https provider -----------------------------------------------------------

struct Pki {
    ca_pem: String,
    leaf_der: CertificateDer<'static>,
    leaf_key: PrivateKeyDer<'static>,
}

/// A private CA and a server certificate it signed for `names`, valid from `from_days` to
/// `to_days` relative to now (negative `to_days`: already expired).
fn pki(names: &[&str], from_days: i64, to_days: i64) -> Pki {
    let now = time::OffsetDateTime::now_utc();
    let mut ca = CertificateParams::new(Vec::<String>::new()).unwrap();
    ca.distinguished_name
        .push(DnType::CommonName, "provider tls test CA");
    ca.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    ca.key_usages = vec![
        KeyUsagePurpose::KeyCertSign,
        KeyUsagePurpose::CrlSign,
        KeyUsagePurpose::DigitalSignature,
    ];
    ca.not_before = now - time::Duration::days(40);
    ca.not_after = now + time::Duration::days(40);
    let ca = CertifiedIssuer::self_signed(ca, KeyPair::generate().unwrap()).unwrap();

    let mut leaf =
        CertificateParams::new(names.iter().map(|n| (*n).to_owned()).collect::<Vec<_>>()).unwrap();
    leaf.distinguished_name.push(DnType::CommonName, names[0]);
    leaf.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
    leaf.key_usages = vec![
        KeyUsagePurpose::DigitalSignature,
        KeyUsagePurpose::KeyEncipherment,
    ];
    leaf.not_before = now + time::Duration::days(from_days);
    leaf.not_after = now + time::Duration::days(to_days);
    let leaf_key = KeyPair::generate().unwrap();
    let leaf_cert = leaf.signed_by(&leaf_key, &ca).unwrap();
    Pki {
        ca_pem: ca.pem(),
        leaf_der: leaf_cert.der().clone(),
        leaf_key: PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(leaf_key.serialize_der())),
    }
}

/// A valid `localhost` certificate from the private CA.
fn good_pki() -> Pki {
    pki(&["localhost", "127.0.0.1"], -1, 20)
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

struct Endpoint {
    /// `https://localhost:PORT/v1`
    url: String,
    /// `localhost:PORT`, as the sentences name it.
    host: String,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Endpoint {
    fn drop(&mut self) {
        self.task.abort();
    }
}

/// The mock provider (models `engineer` and `orchestrator`; only [`KEY`] when `key`) over TLS.
async fn spawn_https_provider(pki: &Pki, key: bool) -> Endpoint {
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
    let app = provider_app(ProviderState {
        key: key.then(|| KEY.to_owned()),
        models: vec!["engineer".into(), "orchestrator".into()],
        reply: "on it".into(),
        ..ProviderState::default()
    });
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Endpoint {
        url: format!("https://localhost:{port}/v1"),
        host: format!("localhost:{port}"),
        task,
    }
}

// --- running the binary -----------------------------------------------------------------------------

/// Runs the binary in `dir/project` with HOME/XDG under `dir/home`, no keychain, no proxy, and
/// no provider keys or AMP settings from this machine. `stdin` answers the prompts
/// (`AMP_HARNESS_PROMPTS=stdin` is set by the caller when a run should read them).
async fn run(dir: &Path, args: &[&str], env: &[(&str, &str)], stdin: Option<&str>) -> Output {
    let mut cmd = tokio::process::Command::new(env!("CARGO_BIN_EXE_amp-harness"));
    cmd.args(args)
        .current_dir(dir.join("project"))
        .env("HOME", dir.join("home"))
        .env("XDG_CONFIG_HOME", dir.join("home/.config"))
        .env("XDG_STATE_HOME", dir.join("home/.local/state"))
        .env("AMP_CREDENTIAL_STORE", "file")
        .env("AMP_HARNESS_CEILING_DIR", dir)
        .env("AMP_WEB_URL", "http://ui.invalid")
        .env("NO_PROXY", "*")
        .env("no_proxy", "*");
    for var in [
        "AMP_MCP_URL",
        "AMP_TOKEN",
        "AMP_CA_FILE",
        "AMP_MCP_PROTOCOL",
        "AMP_HARNESS_PROFILE",
        "AMP_HARNESS_PROMPTS",
        "AMP_HARNESS_FAUX_SCRIPT",
        "AMP_HARNESS_CONFIG",
        "AMP_HARNESS_CONFIG_DIR",
        "ANTHROPIC_API_KEY",
        "OPENROUTER_API_KEY",
        "OPENAI_API_KEY",
        "EXAMPLE_API_KEY",
        "RUST_LOG",
        "HTTPS_PROXY",
        "https_proxy",
        "HTTP_PROXY",
        "http_proxy",
        "ALL_PROXY",
        "all_proxy",
        "SSL_CERT_FILE",
        "SSL_CERT_DIR",
    ] {
        cmd.env_remove(var);
    }
    for (k, v) in env {
        cmd.env(k, v);
    }
    cmd.stdin(if stdin.is_some() {
        Stdio::piped()
    } else {
        Stdio::null()
    })
    .stdout(Stdio::piped())
    .stderr(Stdio::piped());
    let mut child = cmd.spawn().expect("spawn amp-harness");
    if let Some(text) = stdin {
        let mut pipe = child.stdin.take().unwrap();
        pipe.write_all(text.as_bytes()).await.unwrap();
        drop(pipe);
    }
    let out = tokio::time::timeout(Duration::from_secs(90), child.wait_with_output())
        .await
        .expect("amp-harness finished within 90 s")
        .expect("run amp-harness");
    Output {
        code: out.status.code().unwrap_or(-1),
        stdout: String::from_utf8(out.stdout).unwrap(),
        stderr: String::from_utf8(out.stderr).unwrap(),
    }
}

fn config_path(dir: &Path) -> std::path::PathBuf {
    dir.join("home/.config/amp-harness/config.toml")
}

fn config(dir: &Path) -> String {
    std::fs::read_to_string(config_path(dir)).unwrap_or_default()
}

fn write_ca(dir: &Path, pki: &Pki) -> String {
    let path = dir.join("corp-root.pem");
    std::fs::write(&path, &pki.ca_pem).unwrap();
    path.display().to_string()
}

fn no_key(out: &Output) {
    assert!(
        !out.stdout.contains(KEY) && !out.stderr.contains(KEY),
        "key printed:\n{}\n{}",
        out.stdout,
        out.stderr
    );
}

/// No rustls/reqwest wording outside a `cause:` line.
fn plain(text: &str) {
    for raw in [
        "UnknownIssuer",
        "invalid peer certificate",
        "error sending request",
        "client error (Connect)",
        "NotValidForName",
    ] {
        assert!(
            !text
                .lines()
                .any(|l| l.contains(raw) && !l.contains("cause:")),
            "raw {raw:?} in:\n{text}"
        );
    }
}

fn connect_args<'a>(base_url: &'a str, extra: &[&'a str]) -> Vec<&'a str> {
    let mut args = vec![
        "connect",
        "example",
        "--base-url",
        base_url,
        "--role",
        "manager=orchestrator",
        "--role",
        "workers=engineer",
    ];
    args.extend_from_slice(extra);
    args
}

// --- tests --------------------------------------------------------------------------------------------

/// The QA case: connect to a private-CA endpoint with the key in the environment and no CA.
#[tokio::test]
async fn connect_names_an_unknown_ca_and_the_ca_file_fix() {
    let pki = good_pki();
    let endpoint = spawn_https_provider(&pki, true).await;
    let dir = workdir("provider-tls-connect", None);
    let env = [("EXAMPLE_API_KEY", KEY)];

    let out = run(&dir, &connect_args(&endpoint.url, &[]), &env, None).await;
    assert_ne!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    let err = &out.stderr;
    let sentence = format!(
        "{}'s certificate isn't trusted: it's signed by a CA this machine doesn't know.",
        endpoint.host
    );
    assert!(err.contains(&sentence), "{err}");
    assert!(
        err.contains("`amp-harness connect example --ca-file /path/corp-root.pem`"),
        "{err}"
    );
    assert!(!err.contains("--model"), "no --model hint for TLS: {err}");
    assert!(!err.contains("cause:"), "{err}");
    assert!(!err.contains("keeping it"), "{err}");
    // One sentence and its fix, said once.
    assert_eq!(err.matches(&sentence).count(), 1, "{err}");
    plain(err);
    no_key(&out);
    assert!(
        !config(&dir).contains("[providers.example]"),
        "nothing stored: {}",
        config(&dir)
    );

    // --verbose keeps the full cause.
    let out = run(
        &dir,
        &connect_args(&endpoint.url, &["--verbose"]),
        &env,
        None,
    )
    .await;
    assert_ne!(out.code, 0);
    assert!(out.stderr.contains(&sentence), "{}", out.stderr);
    let cause = out
        .stderr
        .lines()
        .find(|l| l.trim_start().starts_with("cause:"))
        .unwrap_or_else(|| panic!("no cause line:\n{}", out.stderr));
    assert!(cause.contains("UnknownIssuer"), "{cause}");
    no_key(&out);

    // The fix works: with the CA bundle it connects and keeps the CA file.
    let ca = write_ca(&dir, &pki);
    let out = run(
        &dir,
        &connect_args(&endpoint.url, &["--ca-file", &ca]),
        &env,
        None,
    )
    .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    assert!(out.stderr.contains("accepted the key"), "{}", out.stderr);
    let text = config(&dir);
    assert!(text.contains("[providers.example]"), "{text}");
    assert!(text.contains(&format!("ca_file = \"{ca}\"")), "{text}");
    no_key(&out);
}

/// An expired certificate and one issued for another host: their own sentence and fix, never
/// `--model` (the CA is given, so only the date or the name is wrong).
#[tokio::test]
async fn connect_names_an_expired_or_misnamed_certificate() {
    for (name, pki, want, fix) in [
        (
            "expired",
            pki(&["localhost", "127.0.0.1"], -30, -1),
            "'s certificate isn't valid now: it has expired",
            "renew it",
        ),
        (
            "misnamed",
            pki(&["other.example.com"], -1, 20),
            "'s certificate isn't trusted: it was issued for another host name.",
            "--base-url",
        ),
    ] {
        let endpoint = spawn_https_provider(&pki, true).await;
        let dir = workdir(&format!("provider-tls-{name}"), None);
        let ca = write_ca(&dir, &pki);
        let out = run(
            &dir,
            &connect_args(&endpoint.url, &["--ca-file", &ca]),
            &[("EXAMPLE_API_KEY", KEY)],
            None,
        )
        .await;
        assert_ne!(out.code, 0, "{name}: {}\n{}", out.stdout, out.stderr);
        let err = &out.stderr;
        assert!(
            err.contains(&format!("{}{want}", endpoint.host)),
            "{name}: {err}"
        );
        assert!(err.contains(fix), "{name}: {err}");
        assert!(!err.contains("--model"), "{name}: {err}");
        plain(err);
        no_key(&out);
    }
}

/// doctor's `models:<id>` says the same sentence and gives the `--ca-file` fix; the raw cause is
/// in the JSON and, in the human report, only with `--verbose`.
#[tokio::test]
async fn doctor_names_an_unknown_ca_for_the_models_check() {
    let pki = good_pki();
    let endpoint = spawn_https_provider(&pki, true).await;
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("provider-tls-doctor", None);
    let env = [("EXAMPLE_API_KEY", KEY), ("AMP_MCP_URL", amp.url.as_str())];
    // Connected with the CA, then the CA is taken away again.
    let ca = write_ca(&dir, &pki);
    let out = run(
        &dir,
        &connect_args(&endpoint.url, &["--ca-file", &ca]),
        &env,
        None,
    )
    .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    let text = config(&dir).replace(&format!("ca_file = \"{ca}\"\n"), "");
    assert!(!text.contains("ca_file"), "{text}");
    std::fs::write(config_path(&dir), text).unwrap();

    let sentence = format!(
        "{}'s certificate isn't trusted: it's signed by a CA this machine doesn't know.",
        endpoint.host
    );
    let out = run(&dir, &["doctor", "--json"], &env, None).await;
    let v = out.json();
    let check = v["checks"]
        .as_array()
        .unwrap()
        .iter()
        .find(|c| c["name"] == "models:example")
        .unwrap_or_else(|| panic!("no models:example: {v}"))
        .clone();
    assert_eq!(check["status"], "fail", "{check}");
    assert_eq!(check["detail"], sentence.as_str(), "{check}");
    let fix = check["fix"].as_str().unwrap();
    assert!(
        fix.contains("`amp-harness connect example --ca-file /path/corp-root.pem`"),
        "{fix}"
    );
    assert!(!fix.contains("--model"), "{fix}");
    no_key(&out);

    let human = run(&dir, &["doctor"], &env, None).await;
    assert!(human.stdout.contains(&sentence), "{}", human.stdout);
    plain(&human.stdout);
    assert!(!human.stdout.contains("cause:"), "{}", human.stdout);
    no_key(&human);

    let verbose = run(&dir, &["doctor", "--verbose"], &env, None).await;
    let cause = verbose
        .stdout
        .lines()
        .find(|l| l.contains("cause:"))
        .unwrap_or_else(|| panic!("no cause line:\n{}", verbose.stdout));
    assert!(cause.contains("UnknownIssuer"), "{cause}");
    no_key(&verbose);
}

/// The first run's own-endpoint path: the endpoint fails without a CA, so it says why in one
/// sentence and asks for the CA bundle; with it, the models are listed and the CA is kept.
#[tokio::test]
async fn the_first_run_asks_for_the_ca_of_an_untrusted_endpoint() {
    let pki = good_pki();
    // Keyless: the first run keeps provider keys only in the OS keychain, which tests never use.
    let endpoint = spawn_https_provider(&pki, false).await;
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("provider-tls-first-run", None);
    let ca = write_ca(&dir, &pki);
    // No project; provider 4 (own endpoint); name; id; the full https address (no "Use …?"); the
    // CA bundle; bearer auth (default); no extra header; no key; keep the suggested mapping.
    let answers = format!("\n4\nExample AI\nexample\n{}\n{ca}\n\n\n\n\n", endpoint.url);
    let out = run(
        &dir,
        &["--snapshot", "100x20", "--no-session"],
        &[
            ("AMP_HARNESS_PROMPTS", "stdin"),
            ("AMP_MCP_URL", amp.url.as_str()),
        ],
        Some(&answers),
    )
    .await;
    assert_eq!(out.code, 0, "{}\n{}", out.stdout, out.stderr);
    let err = &out.stderr;
    for want in [
        format!(
            "{}'s certificate isn't trusted: it's signed by a CA this machine doesn't know.",
            endpoint.host
        ),
        "CA bundle that signed it".to_owned(),
        "It serves 2 models: engineer, orchestrator.".to_owned(),
    ] {
        assert!(err.contains(&want), "missing {want:?}:\n{err}");
    }
    plain(err);
    no_key(&out);
    let text = config(&dir);
    assert!(text.contains("[providers.example]"), "{text}");
    assert!(text.contains(&format!("ca_file = \"{ca}\"")), "{text}");
}
