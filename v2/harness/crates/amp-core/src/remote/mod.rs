//! Remote control, the harness side (#2373): `v2/docs/remote-control/protocol.md`.
//!
//! - [`jcs`]: canonical JSON and the strict parser (section 3.3).
//! - [`crypto`]: base64url, fingerprints, ECDSA P-256 (P1363, high-S accepted), HKDF/HMAC, the
//!   harness key in the keychain (or a 0600 file).
//! - [`envelope`]: the envelope, signed events, and the command check in section 5.6's order.
//! - [`pairing`]: the pairing secret, link, derivations, the section 7.5 check and device pins.
//! - [`redact`]: secret redaction before signing.
//! - [`policy`]: the local off switches and the server/login checks.
//! - [`relay`]: the relay's harness-leg HTTP API.
//! - [`link`]: [`link::RemoteLink`], the outbound link the chat turns on with `/remote`.
//! - `mock_relay` (feature `mock-relay`): an in-process relay for end-to-end tests.

pub mod crypto;
pub mod envelope;
pub mod jcs;
pub mod link;
#[cfg(feature = "mock-relay")]
pub mod mock_relay;
pub mod pairing;
pub mod policy;
pub mod redact;
pub mod relay;

pub use link::{
    Enabled, FixedToken, HostStatus, LinkTuning, LocalEvent, PairingPrompt, RemoteDeps,
    RemoteError, RemoteHost, RemoteLink, RemoteOn, RemoteSetup, RemoteState, TokenSource,
};
