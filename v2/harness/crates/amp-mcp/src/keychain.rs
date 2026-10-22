//! The one way into the OS keychain (#2165). Every keychain read, write and delete in the harness
//! (provider keys in amp-core, OAuth logins in [`crate::AmpCredentialStore`], setup) goes through
//! here.
//!
//! **One read per item per process.** On macOS, each read by a binary the item does not trust
//! raises its own "allow access" dialog. Before this module, a process resolved the same provider
//! keys many times (every `ProviderSet::build`, doctor, setup) and rmcp loaded the OAuth login on
//! every MCP request, so one run could queue dozens of dialogs. Now the first lookup of a
//! `(service, account)` pair asks the keychain, and the answer is cached for the rest of the process:
//! the secret, "not there", or the error (so a denied or unanswerable item fails once and is not
//! asked again). Lookups of one item are single-flight: the cache lock is held across the keychain
//! call. Our own writes and deletes update the cache, so a key that was just stored is never read
//! back. [`keychain_forget`] drops one entry; the OAuth store uses it before a token refresh, so a
//! refresh token another process rotated is picked up.
//!
//! **No dialog without a terminal.** When nobody can answer a dialog (stdin is not a terminal, or
//! `AMP_HARNESS_KEYCHAIN_PROMPTS=never`), macOS user interaction is turned off around each call. A
//! read that would need approval then fails at once with [`KeychainErrorKind::NeedsApproval`]
//! instead of blocking an agent on a dialog nobody sees.
//!
//! **The harness creates its own items.** On macOS a write deletes any existing item and adds a new
//! one, so the item's creator (the trusted application in its ACL, and its partition list) is this
//! binary. Updating an item in place would keep the old ACL, and an item made by Apple's `security`
//! tool (partition `apple-tool:`) makes every read by amp-harness prompt, "Always Allow" or not.
//!
//! `AMP_HARNESS_KEYCHAIN_TRACE=<file>` appends one line per real keychain call (operation, service,
//! account, outcome; never a secret), so tests can count calls across processes.

use std::{
    collections::HashMap,
    io::{IsTerminal, Write},
    sync::{LazyLock, Mutex},
};

/// `never` (or `0`/`no`/`off`): never let a keychain call show a dialog. `allow`: always let it.
/// Unset: allowed only when stdin is a terminal.
pub const ENV_KEYCHAIN_PROMPTS: &str = "AMP_HARNESS_KEYCHAIN_PROMPTS";
/// Test hook: a file that gets one line per real keychain call.
pub const ENV_KEYCHAIN_TRACE: &str = "AMP_HARNESS_KEYCHAIN_TRACE";
/// What to tell a person whose keychain item cannot be read. One place, so `connect` (#2163) can
/// take it over.
pub const KEYCHAIN_REPAIR_HINT: &str = "run `amp-harness init` in a terminal to store it again through amp-harness, or set the \
     key in the environment instead";

/// Why a keychain call failed.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum KeychainErrorKind {
    /// No usable keychain (no Secret Service, no default keychain, locked store).
    Unavailable,
    /// macOS wants to ask the person, and this run cannot show the dialog.
    NeedsApproval,
    /// The person clicked Deny, or the keychain refused this binary.
    Denied,
    /// Anything else.
    Other,
}

impl KeychainErrorKind {
    fn as_str(self) -> &'static str {
        match self {
            Self::Unavailable => "unavailable",
            Self::NeedsApproval => "needs-approval",
            Self::Denied => "denied",
            Self::Other => "other",
        }
    }
}

/// A keychain failure with a message a person can act on. Never contains a secret.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("{message}")]
pub struct KeychainError {
    pub kind: KeychainErrorKind,
    pub message: String,
}

impl KeychainError {
    pub fn new(kind: KeychainErrorKind, message: impl Into<String>) -> Self {
        Self {
            kind,
            message: message.into(),
        }
    }
}

/// Reads `account` under `service`: `Ok(None)` when there is no such item. At most one real
/// keychain read per item per process (see the module docs).
pub fn keychain_get(service: &str, account: &str) -> Result<Option<String>, KeychainError> {
    KEYCHAIN.get(service, account)
}

/// Stores `secret` as `account` under `service`, as an item this binary creates (macOS: any
/// existing item is deleted first so it gets a fresh ACL). The cache then holds `secret`.
pub fn keychain_set(service: &str, account: &str, secret: &str) -> Result<(), KeychainError> {
    KEYCHAIN.set(service, account, secret)
}

/// Deletes the item. `Ok(true)` if there was one. The cache then holds "not there".
pub fn keychain_delete(service: &str, account: &str) -> Result<bool, KeychainError> {
    KEYCHAIN.delete(service, account)
}

/// Drops the cached answer for one item, so the next [`keychain_get`] asks the keychain again.
pub fn keychain_forget(service: &str, account: &str) {
    KEYCHAIN.forget(service, account);
}

/// How many real keychain reads this process made for one item (tests, diagnostics).
pub fn keychain_reads(service: &str, account: &str) -> u64 {
    KEYCHAIN.reads(service, account)
}

/// Whether a keychain call may show a dialog in this process (see [`ENV_KEYCHAIN_PROMPTS`]).
pub fn keychain_prompts_allowed() -> bool {
    static ALLOWED: LazyLock<bool> = LazyLock::new(|| {
        prompts_allowed_from(
            std::env::var(ENV_KEYCHAIN_PROMPTS).ok().as_deref(),
            std::io::stdin().is_terminal(),
        )
    });
    *ALLOWED
}

fn prompts_allowed_from(setting: Option<&str>, stdin_is_terminal: bool) -> bool {
    match setting.map(|s| s.trim().to_ascii_lowercase()).as_deref() {
        Some("never" | "0" | "no" | "off" | "false") => false,
        Some("allow" | "always" | "1" | "yes" | "on" | "true") => true,
        _ => stdin_is_terminal,
    }
}

type Key = (String, String);

#[derive(Clone)]
enum Cached {
    Found(String),
    Missing,
    Failed(KeychainError),
}

/// The keychain operations the cache sits on (the OS, or a fake in tests).
trait Backend: Send + Sync {
    fn get(&self, service: &str, account: &str) -> Result<Option<String>, KeychainError>;
    fn set(&self, service: &str, account: &str, secret: &str) -> Result<(), KeychainError>;
    fn delete(&self, service: &str, account: &str) -> Result<bool, KeychainError>;
}

#[derive(Default)]
struct State {
    cache: HashMap<Key, Cached>,
    reads: HashMap<Key, u64>,
}

struct Keychain<B> {
    backend: B,
    state: Mutex<State>,
}

static KEYCHAIN: LazyLock<Keychain<OsBackend>> = LazyLock::new(|| Keychain::new(OsBackend));

impl<B: Backend> Keychain<B> {
    fn new(backend: B) -> Self {
        Self {
            backend,
            state: Mutex::default(),
        }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn get(&self, service: &str, account: &str) -> Result<Option<String>, KeychainError> {
        let key = (service.to_owned(), account.to_owned());
        // Held across the keychain call: concurrent lookups of one item make one read.
        let mut state = self.lock();
        let cached = match state.cache.get(&key) {
            Some(cached) => cached.clone(),
            None => {
                *state.reads.entry(key.clone()).or_default() += 1;
                let result = self.backend.get(service, account);
                trace("read", service, account, &outcome(&result));
                let cached = match result {
                    Ok(Some(secret)) => Cached::Found(secret),
                    Ok(None) => Cached::Missing,
                    Err(e) => Cached::Failed(e),
                };
                state.cache.insert(key, cached.clone());
                cached
            }
        };
        match cached {
            Cached::Found(secret) => Ok(Some(secret)),
            Cached::Missing => Ok(None),
            Cached::Failed(e) => Err(e),
        }
    }

    fn set(&self, service: &str, account: &str, secret: &str) -> Result<(), KeychainError> {
        let mut state = self.lock();
        let result = self.backend.set(service, account, secret);
        trace(
            "write",
            service,
            account,
            &outcome(&result.clone().map(|()| None::<()>)),
        );
        let key = (service.to_owned(), account.to_owned());
        match &result {
            Ok(()) => {
                state.cache.insert(key, Cached::Found(secret.to_owned()));
            }
            // What is in the keychain now is unknown: ask again next time.
            Err(_) => {
                state.cache.remove(&key);
            }
        }
        result
    }

    fn delete(&self, service: &str, account: &str) -> Result<bool, KeychainError> {
        let mut state = self.lock();
        let result = self.backend.delete(service, account);
        trace(
            "delete",
            service,
            account,
            &outcome(&result.clone().map(|_| None::<()>)),
        );
        let key = (service.to_owned(), account.to_owned());
        match &result {
            Ok(_) => {
                state.cache.insert(key, Cached::Missing);
            }
            Err(_) => {
                state.cache.remove(&key);
            }
        }
        result
    }

    fn forget(&self, service: &str, account: &str) {
        self.lock()
            .cache
            .remove(&(service.to_owned(), account.to_owned()));
    }

    fn reads(&self, service: &str, account: &str) -> u64 {
        self.lock()
            .reads
            .get(&(service.to_owned(), account.to_owned()))
            .copied()
            .unwrap_or(0)
    }
}

fn outcome<T>(result: &Result<Option<T>, KeychainError>) -> String {
    match result {
        Ok(Some(_)) => "found".to_owned(),
        Ok(None) => "ok".to_owned(),
        Err(e) => format!("error:{}", e.kind.as_str()),
    }
}

fn trace(op: &str, service: &str, account: &str, outcome: &str) {
    let Some(path) = std::env::var_os(ENV_KEYCHAIN_TRACE).filter(|p| !p.is_empty()) else {
        return;
    };
    let line = format!(
        "{op}\t{service}\t{account}\t{outcome}\tpid={}\n",
        std::process::id()
    );
    if let Ok(mut f) = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)
    {
        let _ = f.write_all(line.as_bytes());
    }
}

/// The real keychain, through keyring.
struct OsBackend;

impl Backend for OsBackend {
    fn get(&self, service: &str, account: &str) -> Result<Option<String>, KeychainError> {
        let _quiet = quiet();
        match entry(service, account)?.get_password() {
            Ok(secret) => Ok(Some(secret)),
            Err(keyring::Error::NoEntry) => Ok(None),
            Err(e) => Err(classify("read", service, account, e)),
        }
    }

    fn set(&self, service: &str, account: &str, secret: &str) -> Result<(), KeychainError> {
        let _quiet = quiet();
        let entry = entry(service, account)?;
        // macOS: updating an item in place keeps its ACL and partition list, and first reads it
        // (which can prompt). A fresh item is created by, and trusted for, this binary.
        if cfg!(target_os = "macos") {
            match entry.delete_credential() {
                Ok(()) | Err(keyring::Error::NoEntry) => {}
                // An item another program made (Apple's `security` tool: partition `apple-tool:`)
                // can't be deleted by this binary without a dialog. Apple's own tool deletes
                // items by service and account without asking, whoever made them.
                Err(e) if security_tool_delete(service, account) => {
                    tracing::debug!(error = %e, "replaced a keychain item another program made");
                }
                Err(e) => return Err(classify("replace", service, account, e)),
            }
        }
        entry
            .set_password(secret)
            .map_err(|e| classify("write", service, account, e))
    }

    fn delete(&self, service: &str, account: &str) -> Result<bool, KeychainError> {
        let _quiet = quiet();
        match entry(service, account)?.delete_credential() {
            Ok(()) => Ok(true),
            Err(keyring::Error::NoEntry) => Ok(false),
            Err(e) => Err(classify("delete", service, account, e)),
        }
    }
}

/// macOS: `/usr/bin/security delete-generic-password -s SERVICE -a ACCOUNT`. True when the item
/// is gone afterwards. No secret goes near the command line.
fn security_tool_delete(service: &str, account: &str) -> bool {
    if !cfg!(target_os = "macos") {
        return false;
    }
    let status = std::process::Command::new("/usr/bin/security")
        .args(["delete-generic-password", "-s", service, "-a", account])
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status();
    trace(
        "delete-via-security",
        service,
        account,
        match &status {
            Ok(s) if s.success() => "ok",
            _ => "error:other",
        },
    );
    // Exit 44: the item could not be found (already gone).
    matches!(status, Ok(s) if s.success() || s.code() == Some(44))
}

fn entry(service: &str, account: &str) -> Result<keyring::Entry, KeychainError> {
    keyring::Entry::new(service, account).map_err(|e| classify("open", service, account, e))
}

/// macOS: turns keychain dialogs off for the duration of one call when nobody can answer them.
#[cfg(target_os = "macos")]
fn quiet() -> Option<security_framework::os::macos::keychain::KeychainUserInteractionLock> {
    if keychain_prompts_allowed() {
        return None;
    }
    security_framework::os::macos::keychain::SecKeychain::disable_user_interaction().ok()
}

#[cfg(not(target_os = "macos"))]
fn quiet() -> Option<()> {
    None
}

/// macOS Security framework status codes we explain.
const ERR_SEC_INTERACTION_NOT_ALLOWED: i32 = -25308;
const ERR_SEC_AUTH_FAILED: i32 = -25293;
const ERR_SEC_USER_CANCELED: i32 = -128;

fn platform_code(e: &keyring::Error) -> Option<i32> {
    #[cfg(target_os = "macos")]
    {
        let inner: &(dyn std::error::Error + 'static) = match e {
            keyring::Error::PlatformFailure(inner) | keyring::Error::NoStorageAccess(inner) => {
                inner.as_ref()
            }
            _ => return None,
        };
        inner
            .downcast_ref::<security_framework::base::Error>()
            .map(|err| err.code())
    }
    #[cfg(not(target_os = "macos"))]
    {
        let _ = e;
        None
    }
}

fn classify(op: &str, service: &str, account: &str, e: keyring::Error) -> KeychainError {
    let item = format!("keychain item `{account}` (service {service})");
    let code = platform_code(&e);
    let needs_approval = |code: i32| {
        KeychainError::new(
            KeychainErrorKind::NeedsApproval,
            format!(
                "macOS would have to ask before amp-harness can {op} {item} (it was made by \
                 another program, such as `security add-generic-password`, or by an earlier \
                 amp-harness build), and this run cannot show the dialog. Not asking \
                 (OSStatus {code}): {KEYCHAIN_REPAIR_HINT}"
            ),
        )
    };
    match code {
        Some(code @ ERR_SEC_INTERACTION_NOT_ALLOWED) => return needs_approval(code),
        // With dialogs turned off, macOS reports an item this binary is not trusted for as an
        // authorization failure rather than "interaction not allowed".
        Some(code @ ERR_SEC_AUTH_FAILED) if !keychain_prompts_allowed() => {
            return needs_approval(code);
        }
        Some(code @ (ERR_SEC_USER_CANCELED | ERR_SEC_AUTH_FAILED)) => {
            return KeychainError::new(
                KeychainErrorKind::Denied,
                format!(
                    "access to {item} was denied (OSStatus {code}: Deny was clicked, or the item \
                     was made by another program such as `security add-generic-password`). Not \
                     asking again in this run: {KEYCHAIN_REPAIR_HINT}"
                ),
            );
        }
        _ => {}
    }
    match e {
        keyring::Error::NoStorageAccess(inner) => KeychainError::new(
            KeychainErrorKind::Unavailable,
            format!("the keychain cannot be used to {op} {item}: {inner}"),
        ),
        keyring::Error::NoDefaultStore | keyring::Error::NotSupportedByStore(_) => {
            KeychainError::new(
                KeychainErrorKind::Unavailable,
                format!("no usable keychain to {op} {item}: {e}"),
            )
        }
        // These carry the stored bytes: never format them.
        keyring::Error::BadEncoding(_) | keyring::Error::BadDataFormat(..) => KeychainError::new(
            KeychainErrorKind::Other,
            format!("{item} does not hold readable text: {KEYCHAIN_REPAIR_HINT}"),
        ),
        other => KeychainError::new(
            KeychainErrorKind::Other,
            format!("keychain {op} of {item} failed: {other}"),
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    /// A keychain in memory that counts calls; `fail` makes reads of one account fail.
    #[derive(Default, Clone)]
    struct Fake {
        items: Arc<Mutex<HashMap<Key, String>>>,
        calls: Arc<AtomicUsize>,
        fail: Option<(String, KeychainErrorKind)>,
    }

    impl Backend for Fake {
        fn get(&self, service: &str, account: &str) -> Result<Option<String>, KeychainError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            // Slow enough that concurrent lookups overlap.
            std::thread::sleep(std::time::Duration::from_millis(20));
            if let Some((acct, kind)) = &self.fail
                && acct == account
            {
                return Err(KeychainError::new(*kind, "nope"));
            }
            Ok(self
                .items
                .lock()
                .unwrap()
                .get(&(service.to_owned(), account.to_owned()))
                .cloned())
        }
        fn set(&self, service: &str, account: &str, secret: &str) -> Result<(), KeychainError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            self.items
                .lock()
                .unwrap()
                .insert((service.to_owned(), account.to_owned()), secret.to_owned());
            Ok(())
        }
        fn delete(&self, service: &str, account: &str) -> Result<bool, KeychainError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            Ok(self
                .items
                .lock()
                .unwrap()
                .remove(&(service.to_owned(), account.to_owned()))
                .is_some())
        }
    }

    #[test]
    fn each_item_is_read_once_found_or_not() {
        let fake = Fake::default();
        fake.items
            .lock()
            .unwrap()
            .insert(("s".into(), "provider:openrouter".into()), "k1".into());
        let kc = Keychain::new(fake.clone());
        for _ in 0..50 {
            assert_eq!(
                kc.get("s", "provider:openrouter").unwrap().as_deref(),
                Some("k1")
            );
            assert_eq!(kc.get("s", "provider:anthropic").unwrap(), None);
        }
        assert_eq!(kc.reads("s", "provider:openrouter"), 1);
        assert_eq!(kc.reads("s", "provider:anthropic"), 1);
        assert_eq!(fake.calls.load(Ordering::SeqCst), 2);
        // Another service is another item.
        assert_eq!(kc.get("t", "provider:openrouter").unwrap(), None);
        assert_eq!(fake.calls.load(Ordering::SeqCst), 3);
    }

    #[test]
    fn concurrent_lookups_make_one_read() {
        let fake = Fake::default();
        let kc = Arc::new(Keychain::new(fake.clone()));
        let threads: Vec<_> = (0..16)
            .map(|_| {
                let kc = Arc::clone(&kc);
                std::thread::spawn(move || kc.get("s", "provider:openrouter").unwrap())
            })
            .collect();
        for t in threads {
            assert_eq!(t.join().unwrap(), None);
        }
        assert_eq!(fake.calls.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn a_failure_is_cached_and_never_retried() {
        let fake = Fake {
            fail: Some(("provider:openrouter".into(), KeychainErrorKind::Denied)),
            ..Fake::default()
        };
        let kc = Keychain::new(fake.clone());
        for _ in 0..10 {
            let err = kc.get("s", "provider:openrouter").unwrap_err();
            assert_eq!(err.kind, KeychainErrorKind::Denied);
        }
        assert_eq!(fake.calls.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn writes_and_deletes_update_the_cache_without_reading() {
        let fake = Fake::default();
        let kc = Keychain::new(fake.clone());
        assert_eq!(kc.get("s", "a").unwrap(), None);
        kc.set("s", "a", "k2").unwrap();
        assert_eq!(kc.get("s", "a").unwrap().as_deref(), Some("k2"));
        assert!(kc.delete("s", "a").unwrap());
        assert_eq!(kc.get("s", "a").unwrap(), None);
        // One read, one write, one delete; the reads after them came from the cache.
        assert_eq!(kc.reads("s", "a"), 1);
        assert_eq!(fake.calls.load(Ordering::SeqCst), 3);
        // forget makes the next lookup ask again.
        kc.forget("s", "a");
        assert_eq!(kc.get("s", "a").unwrap(), None);
        assert_eq!(kc.reads("s", "a"), 2);
    }

    #[test]
    fn prompts_follow_the_setting_then_the_terminal() {
        assert!(!prompts_allowed_from(Some("never"), true));
        assert!(!prompts_allowed_from(Some(" OFF "), true));
        assert!(prompts_allowed_from(Some("allow"), false));
        assert!(prompts_allowed_from(None, true));
        assert!(!prompts_allowed_from(None, false));
        assert!(!prompts_allowed_from(Some(""), false));
    }

    #[test]
    fn errors_are_actionable_and_never_carry_the_stored_bytes() {
        let e = classify(
            "read",
            "amp-harness",
            "provider:openrouter",
            keyring::Error::BadEncoding(b"sk-or-secret".to_vec()),
        );
        assert!(!e.message.contains("sk-or-secret"), "{}", e.message);
        assert!(e.message.contains("amp-harness init"), "{}", e.message);
        let e = classify(
            "read",
            "amp-harness",
            "provider:openrouter",
            keyring::Error::NoDefaultStore,
        );
        assert_eq!(e.kind, KeychainErrorKind::Unavailable);
    }
}
