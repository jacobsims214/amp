//! The protocol's cryptography (protocol.md sections 2, 3 and 7.2): base64url, fingerprints,
//! ECDSA P-256 signatures in the IEEE P1363 form, HKDF/HMAC, the OS CSPRNG, and where the
//! harness key is kept.
//!
//! - Public keys are raw 65-byte uncompressed SEC1 points; compressed points are refused.
//! - Signatures are `r ‖ s`, 64 bytes. Verification accepts high-S signatures (WebCrypto does not
//!   normalize s): `p256` doesn't require low-S, and nothing here adds that check.
//! - Signing uses a hedged nonce (RFC 6979 plus OS randomness).

use std::path::{Path, PathBuf};

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use hmac::{Hmac, Mac};
use p256::ecdsa::{Signature, SigningKey, VerifyingKey, signature::RandomizedSigner};
use rand_core::{OsRng, RngCore};
use sha2::{Digest, Sha256};

/// Signing-input prefix of every envelope (section 3.2).
pub const ENVELOPE_PREFIX: &[u8] = b"amp-rc/1 envelope\n";
/// Signing-input prefix of a pairing request.
pub const PAIR_REQUEST_PREFIX: &[u8] = b"amp-rc/1 pair-request\n";

/// base64url without padding.
pub fn b64u(bytes: &[u8]) -> String {
    URL_SAFE_NO_PAD.encode(bytes)
}

/// Strict base64url: no padding, no non-canonical trailing bits.
pub fn b64u_decode(text: &str) -> Option<Vec<u8>> {
    URL_SAFE_NO_PAD.decode(text).ok()
}

/// `n` bytes from the OS CSPRNG.
pub fn random_bytes(n: usize) -> Vec<u8> {
    let mut out = vec![0u8; n];
    OsRng.fill_bytes(&mut out);
    out
}

/// A fresh nonce: 16 random bytes, base64url (22 chars).
pub fn new_nonce() -> String {
    b64u(&random_bytes(16))
}

pub fn sha256(bytes: &[u8]) -> [u8; 32] {
    Sha256::digest(bytes).into()
}

/// `b64u(SHA-256(raw))`: the 43-char id of a public key (`harness_id`, `device_id`).
pub fn fingerprint(raw_pub: &[u8]) -> String {
    b64u(&sha256(raw_pub))
}

/// The display fingerprint: the first 8 bytes of SHA-256, upper-case hex in groups of 4.
pub fn fingerprint_display(raw_pub: &[u8]) -> String {
    let h = sha256(raw_pub);
    let hex: String = h[..8].iter().map(|b| format!("{b:02X}")).collect();
    hex.as_bytes()
        .chunks(4)
        .map(|c| std::str::from_utf8(c).unwrap_or_default())
        .collect::<Vec<_>>()
        .join("-")
}

/// A raw uncompressed P-256 public key, checked to be on the curve.
pub fn parse_public_key(raw: &[u8]) -> Option<VerifyingKey> {
    if raw.len() != 65 || raw[0] != 0x04 {
        return None;
    }
    VerifyingKey::from_sec1_bytes(raw).ok()
}

/// Verifies a P1363 signature (`sig_b64u`) over `input` with the raw public key `raw_pub`.
/// A signature that isn't exactly 64 bytes, or has r or s outside [1, n−1], fails. High-S passes.
pub fn verify(raw_pub: &[u8], input: &[u8], sig_b64u: &str) -> bool {
    use p256::ecdsa::signature::Verifier as _;
    let Some(key) = parse_public_key(raw_pub) else {
        return false;
    };
    let Some(sig) = b64u_decode(sig_b64u) else {
        return false;
    };
    if sig.len() != 64 {
        return false;
    }
    let Ok(sig) = Signature::from_slice(&sig) else {
        return false;
    };
    key.verify(input, &sig).is_ok()
}

/// HKDF-SHA256 with a 32-byte output.
pub fn hkdf32(ikm: &[u8], salt: &[u8], info: &[u8]) -> [u8; 32] {
    let hk = hkdf::Hkdf::<Sha256>::new(Some(salt), ikm);
    let mut out = [0u8; 32];
    // 32 bytes is far below HKDF-SHA256's limit.
    hk.expand(info, &mut out).expect("32-byte HKDF output");
    out
}

/// HMAC-SHA256.
pub fn hmac_sha256(key: &[u8], msg: &[u8]) -> [u8; 32] {
    let mut mac = <Hmac<Sha256> as Mac>::new_from_slice(key).expect("HMAC takes any key size");
    mac.update(msg);
    mac.finalize().into_bytes().into()
}

/// Constant-time check of an HMAC-SHA256 tag.
pub fn hmac_sha256_verify(key: &[u8], msg: &[u8], tag: &[u8]) -> bool {
    let mut mac = <Hmac<Sha256> as Mac>::new_from_slice(key).expect("HMAC takes any key size");
    mac.update(msg);
    mac.verify_slice(tag).is_ok()
}

/// A P-256 key pair: the harness key (one per install), or a device key in tests.
#[derive(Clone)]
pub struct KeyPair {
    signing: SigningKey,
    public_raw: Vec<u8>,
    id: String,
}

impl std::fmt::Debug for KeyPair {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("KeyPair").field("id", &self.id).finish()
    }
}

impl KeyPair {
    /// A new random key.
    pub fn generate() -> Self {
        Self::from_signing(SigningKey::random(&mut OsRng))
    }

    /// The key with this 32-byte private scalar, if it is a valid one.
    pub fn from_scalar(scalar: &[u8]) -> Option<Self> {
        SigningKey::from_slice(scalar).ok().map(Self::from_signing)
    }

    fn from_signing(signing: SigningKey) -> Self {
        let public_raw = signing
            .verifying_key()
            .to_encoded_point(false)
            .as_bytes()
            .to_vec();
        let id = fingerprint(&public_raw);
        Self {
            signing,
            public_raw,
            id,
        }
    }

    /// The private scalar (for the keychain only).
    pub fn scalar(&self) -> Vec<u8> {
        self.signing.to_bytes().to_vec()
    }

    pub fn public_raw(&self) -> &[u8] {
        &self.public_raw
    }

    pub fn public_b64u(&self) -> String {
        b64u(&self.public_raw)
    }

    /// The fingerprint: `harness_id` or `device_id`.
    pub fn id(&self) -> &str {
        &self.id
    }

    pub fn display_fingerprint(&self) -> String {
        fingerprint_display(&self.public_raw)
    }

    /// A P1363 signature over `input`, base64url.
    pub fn sign(&self, input: &[u8]) -> String {
        let sig: Signature = self.signing.sign_with_rng(&mut OsRng, input);
        b64u(&sig.to_bytes())
    }
}

/// Where the harness key lives (protocol.md 3.1): the OS keychain, or a 0600 file where the
/// harness keeps its logins in a file too. Tests use [`KeyStore::Memory`] or a file under a
/// temporary HOME.
#[derive(Debug, Clone)]
pub enum KeyStore {
    Keychain { service: String },
    File { path: PathBuf },
    Memory(std::sync::Arc<std::sync::Mutex<Option<Vec<u8>>>>),
}

/// The keychain account the harness key is kept under.
pub const HARNESS_KEY_ACCOUNT: &str = "remote-control:harness-key";
/// The file the key is kept in when logins are kept in a file (next to `credentials.json`).
pub const HARNESS_KEY_FILE: &str = "remote-harness-key.json";

#[derive(serde::Serialize, serde::Deserialize)]
struct KeyFile {
    v: u32,
    scalar_b64u: String,
}

impl KeyStore {
    /// The store the harness's logins use ([`amp_mcp::CredentialBackend::resolve`]).
    pub fn resolve() -> Result<Self, String> {
        match amp_mcp::CredentialBackend::resolve().map_err(|e| e.to_string())? {
            amp_mcp::CredentialBackend::Keychain { service } => Ok(Self::Keychain { service }),
            amp_mcp::CredentialBackend::File { path } => Ok(Self::File {
                path: path
                    .parent()
                    .unwrap_or(Path::new("."))
                    .join(HARNESS_KEY_FILE),
            }),
        }
    }

    pub fn memory() -> Self {
        Self::Memory(Default::default())
    }

    pub fn describe(&self) -> String {
        match self {
            Self::Keychain { service } => format!("keychain (service {service})"),
            Self::File { path } => format!("file {}", path.display()),
            Self::Memory(_) => "memory".to_owned(),
        }
    }

    fn load(&self) -> Result<Option<Vec<u8>>, String> {
        match self {
            Self::Keychain { service } => {
                match amp_mcp::keychain_get(service, HARNESS_KEY_ACCOUNT) {
                    Ok(Some(text)) => b64u_decode(text.trim())
                        .map(Some)
                        .ok_or_else(|| "the stored harness key is not base64url".to_owned()),
                    Ok(None) => Ok(None),
                    Err(e) => Err(format!("keychain: {e}")),
                }
            }
            Self::File { path } => match std::fs::read_to_string(path) {
                Ok(text) => {
                    let file: KeyFile = serde_json::from_str(&text)
                        .map_err(|e| format!("{}: {e}", path.display()))?;
                    b64u_decode(&file.scalar_b64u)
                        .map(Some)
                        .ok_or_else(|| format!("{}: the key is not base64url", path.display()))
                }
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
                Err(e) => Err(format!("read {}: {e}", path.display())),
            },
            Self::Memory(cell) => Ok(cell.lock().unwrap_or_else(|e| e.into_inner()).clone()),
        }
    }

    fn save(&self, scalar: &[u8]) -> Result<(), String> {
        match self {
            Self::Keychain { service } => {
                amp_mcp::keychain_set(service, HARNESS_KEY_ACCOUNT, &b64u(scalar))
                    .map_err(|e| format!("keychain: {e}"))
            }
            Self::File { path } => {
                let text = serde_json::to_string(&KeyFile {
                    v: 1,
                    scalar_b64u: b64u(scalar),
                })
                .map_err(|e| e.to_string())?;
                write_private(path, text.as_bytes())
            }
            Self::Memory(cell) => {
                *cell.lock().unwrap_or_else(|e| e.into_inner()) = Some(scalar.to_vec());
                Ok(())
            }
        }
    }

    /// The harness key: the stored one, else a new one, stored before it is used. Blocking
    /// (the keychain): call it from `spawn_blocking`.
    pub fn load_or_create(&self) -> Result<KeyPair, String> {
        if let Some(scalar) = self.load()? {
            return KeyPair::from_scalar(&scalar)
                .ok_or_else(|| "the stored harness key is not a P-256 key".to_owned());
        }
        let key = KeyPair::generate();
        self.save(&key.scalar())?;
        Ok(key)
    }
}

/// Writes `bytes` to `path` with mode 0600 (unix), creating the parent directory, through a
/// temporary file and a rename.
pub fn write_private(path: &Path, bytes: &[u8]) -> Result<(), String> {
    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
    }
    let tmp = path.with_extension("tmp");
    {
        use std::io::Write as _;
        let mut opts = std::fs::OpenOptions::new();
        opts.write(true).create(true).truncate(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt as _;
            opts.mode(0o600);
        }
        let mut f = opts
            .open(&tmp)
            .map_err(|e| format!("write {}: {e}", tmp.display()))?;
        f.write_all(bytes)
            .map_err(|e| format!("write {}: {e}", tmp.display()))?;
    }
    std::fs::rename(&tmp, path).map_err(|e| format!("write {}: {e}", path.display()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sign_verify_round_trip_and_stores() {
        let key = KeyPair::generate();
        let sig = key.sign(b"hello");
        assert_eq!(b64u_decode(&sig).unwrap().len(), 64);
        assert!(verify(key.public_raw(), b"hello", &sig));
        assert!(!verify(key.public_raw(), b"hellp", &sig));
        // Compressed points are refused.
        let compressed = p256::ecdsa::VerifyingKey::from_sec1_bytes(key.public_raw())
            .unwrap()
            .to_encoded_point(true);
        assert!(!verify(compressed.as_bytes(), b"hello", &sig));

        let mem = KeyStore::memory();
        let a = mem.load_or_create().unwrap();
        let b = mem.load_or_create().unwrap();
        assert_eq!(a.id(), b.id());

        let dir = std::env::temp_dir().join(format!("amp-rc-key-{}", std::process::id()));
        let file = KeyStore::File {
            path: dir.join(HARNESS_KEY_FILE),
        };
        let a = file.load_or_create().unwrap();
        let b = file.load_or_create().unwrap();
        assert_eq!(a.id(), b.id());
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = std::fs::metadata(dir.join(HARNESS_KEY_FILE))
                .unwrap()
                .permissions()
                .mode();
            assert_eq!(mode & 0o777, 0o600);
        }
        let _ = std::fs::remove_dir_all(dir);
    }
}
