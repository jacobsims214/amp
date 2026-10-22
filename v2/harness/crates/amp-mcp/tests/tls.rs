//! `--ca-file` / `AMP_CA_FILE`: an HTTPS server signed by a private CA (like the docker-desktop
//! cluster's `amp-local-ca.crt`) is trusted only when that CA is given.

mod common;

use std::{
    net::SocketAddr,
    path::{Path, PathBuf},
    sync::Arc,
};

use amp_mcp::{AmpMcpClient, ClientOptions, McpError, ProtocolMode, load_ca_file};
use common::{ServerKind, TestServer};
use rcgen::{
    BasicConstraints, CertificateParams, CertifiedIssuer, DnType, ExtendedKeyUsagePurpose, IsCa,
    KeyPair, KeyUsagePurpose,
};
use tokio::net::{TcpListener, TcpStream};
use tokio_rustls::{
    TlsAcceptor,
    rustls::{
        self,
        pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer},
    },
    server::TlsStream,
};

/// A per-test directory under cargo's target dir (never /tmp: D13).
fn scratch(name: &str) -> PathBuf {
    let dir = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-mcp-tls")
        .join(name);
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

struct Pki {
    ca_pem: String,
    leaf_der: CertificateDer<'static>,
    leaf_key: PrivateKeyDer<'static>,
}

/// A private CA and a `localhost` / `127.0.0.1` server certificate it signed. Short validity: macOS
/// rejects long-lived TLS server certificates.
fn pki() -> Pki {
    let now = time::OffsetDateTime::now_utc();
    let mut ca = CertificateParams::new(Vec::<String>::new()).unwrap();
    ca.distinguished_name
        .push(DnType::CommonName, "amp-mcp test CA");
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

    Pki {
        ca_pem: ca.pem(),
        leaf_der: leaf_cert.der().clone(),
        leaf_key: PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(leaf_key.serialize_der())),
    }
}

/// TLS on top of a TCP listener, for `axum::serve`. Failed handshakes (an untrusting client) are
/// skipped.
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

struct HttpsServer {
    url: String,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for HttpsServer {
    fn drop(&mut self) {
        self.task.abort();
    }
}

async fn spawn_https(pki: &Pki) -> HttpsServer {
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
    let app = common::router(TestServer::new(ServerKind::Dual), None);
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    HttpsServer {
        url: format!("https://localhost:{port}/mcp"),
        task,
    }
}

#[tokio::test]
async fn private_ca_is_trusted_only_with_the_ca_file() {
    let pki = pki();
    let dir = scratch("trust");
    let ca_path = dir.join("amp-local-ca.crt");
    std::fs::write(&ca_path, &pki.ca_pem).unwrap();
    let server = spawn_https(&pki).await;

    // Without the CA: the certificate is not trusted, so connecting fails.
    let err = AmpMcpClient::connect(ClientOptions::new(&server.url))
        .await
        .expect_err("an untrusted certificate must fail");
    assert!(matches!(err, McpError::Connect { .. }), "{err:?}");

    // With it, in both eras.
    for mode in [ProtocolMode::Auto, ProtocolMode::Legacy] {
        let client = AmpMcpClient::connect(
            ClientOptions::new(&server.url)
                .with_ca_file(&ca_path)
                .with_protocol(mode),
        )
        .await
        .unwrap_or_else(|e| panic!("{mode}: {e:?}"));
        assert_eq!(client.catalog().await.unwrap().len(), 3);
        client.close().await;
    }
}

#[test]
fn bad_ca_files_are_errors() {
    let dir = scratch("bad");
    let missing = dir.join("missing.crt");
    assert!(matches!(
        load_ca_file(&missing),
        Err(McpError::CaFile { path, .. }) if path == missing
    ));
    let not_pem = dir.join("not-pem.crt");
    std::fs::write(&not_pem, "hello").unwrap();
    assert!(matches!(
        load_ca_file(&not_pem),
        Err(McpError::CaFile { .. })
    ));

    let good = dir.join("good.crt");
    std::fs::write(&good, pki().ca_pem).unwrap();
    assert_eq!(load_ca_file(&good).unwrap().len(), 1);
    assert!(amp_mcp::build_http_client(Some(&good)).is_ok());
    assert!(amp_mcp::build_http_client(Some(&missing)).is_err());
}
