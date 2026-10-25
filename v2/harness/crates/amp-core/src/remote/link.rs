//! The remote link (protocol.md, harness side): one outbound connection to the AMP relay per
//! remote session.
//!
//! [`RemoteLink`] is the handle the chat (and #2376's `/remote` command) uses:
//! [`RemoteLink::enable`] registers a session, [`RemoteLink::disable`] ends it,
//! [`RemoteLink::state`] drives the `REMOTE ON · n devices` badge, [`RemoteLink::take_local_events`]
//! delivers pairing requests awaiting the local y/N and notices, and the device calls list and
//! unpin devices.
//!
//! Inside, one actor task owns the session: it reads the command stream, checks every command
//! (section 5.6), runs the follow-up queue, numbers and signs events (hello, status, chat events
//! from the manager feed, ack, refused, pair_result, bye) on one seq counter (section 6.1),
//! redacts secrets before signing, keeps the idle and lifetime deadlines, and reconnects with
//! backoff. A poster task batches events (~100 ms) and resends what the relay hasn't accepted; a
//! heartbeat task beats every 20 s.

use std::{
    collections::VecDeque,
    path::PathBuf,
    sync::{
        Arc, Mutex,
        atomic::{AtomicI64, Ordering},
    },
    time::Duration,
};

use futures::future::BoxFuture;
use serde_json::{Value, json};
use tokio::sync::{mpsc, oneshot, watch};

use super::crypto::{KeyPair, KeyStore};
use super::envelope::{
    self, Code, Command, CommandContext, EventSigner, Refusal, ReplayState, VerifiedCommand,
};
use super::pairing::{
    self, PairCandidate, PairCheckInput, PairResult, PairingWindow, PinStore, StoredPin,
};
use super::redact::{self, DeltaGuard, Redactor};
use super::relay::{ConnectToken, Frame, RelayClient, RelayError, now_ms};
use crate::chat_event::{ChatEvent, NoticeLevel, PromptOrigin, QueueItem, RunOutcome};
use crate::manager_feed::{FeedEntry, ManagerFeed};

/// At most this many prompts wait in the follow-up queue (section 5.4).
pub const QUEUE_MAX: usize = 5;
/// The ops an M1 harness advertises in `hello.ops`.
pub const OPS: [&str; 3] = ["prompt", "stop", "detach"];

/// Where the session's chat comes from and where remote input goes: the chat's shell.
pub trait RemoteHost: Send + Sync {
    /// Starts `text` now as a user turn with `prompt_id`, never as a slash command. `false` (and
    /// nothing is sent) while a run is busy.
    fn start_prompt(
        &self,
        prompt_id: String,
        text: String,
        origin: PromptOrigin,
    ) -> BoxFuture<'static, bool>;
    /// Stops the running turn and its workers, as Esc / `/abort` do.
    fn stop(&self) -> BoxFuture<'static, ()>;
    /// What `status` reports.
    fn status(&self) -> HostStatus;
}

/// The chat's status for `hello` and `status` (section 6.3).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct HostStatus {
    pub model: String,
    pub busy: bool,
    pub context_pct: Option<i64>,
    pub cost_usd_micros: i64,
    pub project: Option<(i64, String)>,
    pub ticket_id: Option<i64>,
    pub branch: Option<String>,
    pub workers_running: i64,
    // #2388: the rest of what the TUI's header, footer and status line show (section 6.3).
    /// The current ticket's name, board state (`in_progress`, …), epic and story.
    pub ticket_name: Option<String>,
    pub ticket_state: Option<String>,
    pub epic: Option<String>,
    pub story: Option<String>,
    /// The session's token totals (its cost is `cost_usd_micros`).
    pub usage: Option<SessionTokens>,
    /// The thinking level (`off`, `low`, …) and the agent role (`manager`).
    pub thinking: Option<String>,
    pub role: Option<String>,
    /// The working directory. Only its last component leaves the laptop.
    pub cwd: Option<String>,
    /// The project board's counts, while known.
    pub board: Option<BoardCounts>,
    /// Tickets awaiting plan approval: the person's own, and others'.
    pub plan_approval: Option<(i64, i64)>,
    /// The harness's own AMP connection.
    pub amp_connection: Option<AmpConnection>,
}

/// The session's token totals (`status.usage`).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct SessionTokens {
    pub input: i64,
    pub output: i64,
    pub cache_read: i64,
    pub cache_write: i64,
}

/// The board counts the TUI's status line shows (`status.board`).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct BoardCounts {
    pub in_progress: i64,
    pub ready: i64,
    pub question: i64,
    pub scheduled: i64,
}

/// The harness's connection to AMP (`status.amp_connection`), as the TUI header's dot says it.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct AmpConnection {
    /// `connecting`, `connected`, `slow`, `degraded`, `login_needed` or `disconnected`.
    pub state: String,
    /// The latency, while `slow`.
    pub latency_ms: Option<i64>,
}

/// The last component of `path` (`/Users/ada/dev/amp` → `amp`); never the whole path.
pub fn cwd_basename(path: &str) -> Option<String> {
    let trimmed = path.trim().trim_end_matches(['/', '\\']);
    let base = trimmed.rsplit(['/', '\\']).next().unwrap_or("").trim();
    (!base.is_empty()).then(|| cap(base, 64))
}

/// The harness's AMP login: a person's OAuth access token, fetched fresh for register and token
/// refresh (never an `amp_` token), and who that person is.
pub trait TokenSource: Send + Sync {
    fn oauth_token(&self) -> BoxFuture<'static, Result<String, String>>;
    /// The AMP user id of the login (`amp_whoami`); an error for an agent account.
    fn user_id(&self) -> BoxFuture<'static, Result<i64, String>>;
}

/// A fixed token and user (tests).
pub struct FixedToken(pub String, pub i64);

impl TokenSource for FixedToken {
    fn oauth_token(&self) -> BoxFuture<'static, Result<String, String>> {
        let t = self.0.clone();
        Box::pin(async move { Ok(t) })
    }

    fn user_id(&self) -> BoxFuture<'static, Result<i64, String>> {
        let id = self.1;
        Box::pin(async move { Ok(id) })
    }
}

/// Timings; [`LinkTuning::default`] is the protocol's (section 10). Tests shorten them.
#[derive(Debug, Clone, Copy)]
pub struct LinkTuning {
    pub batch_every: Duration,
    pub silence: Duration,
    pub backoff_base: Duration,
    pub backoff_max: Duration,
    pub status_min: Duration,
    pub status_max: Duration,
    pub tick: Duration,
    /// How long `disable` waits for `bye` to reach the relay.
    pub bye_wait: Duration,
    /// At most one `refused` event per device this often (section 5.6 allows one a second).
    pub refused_every: Duration,
}

impl Default for LinkTuning {
    fn default() -> Self {
        Self {
            batch_every: Duration::from_millis(100),
            silence: Duration::from_secs(60),
            backoff_base: Duration::from_secs(1),
            backoff_max: Duration::from_secs(30),
            status_min: Duration::from_secs(1),
            status_max: Duration::from_secs(60),
            tick: Duration::from_millis(250),
            bye_wait: Duration::from_secs(3),
            refused_every: Duration::from_secs(1),
        }
    }
}

/// Which AMP, who, and what this harness is.
#[derive(Debug, Clone)]
pub struct RemoteSetup {
    /// The AMP server's origin (the MCP URL's), checked by [`super::policy::check_server`].
    pub api_base: String,
    /// The web UI's origin, for the pairing link (never the relay's `session_url`).
    pub web_origin: String,
    pub ca_file: Option<PathBuf>,
    pub project_id: Option<i64>,
    pub host: String,
    pub os: String,
    pub version: String,
}

/// Everything the link needs.
pub struct RemoteDeps {
    pub setup: RemoteSetup,
    pub tokens: Arc<dyn TokenSource>,
    pub host: Arc<dyn RemoteHost>,
    pub feed: Arc<ManagerFeed>,
    pub keys: KeyStore,
    /// The pin file (`<state>/remote-devices.json`); `None`: memory only.
    pub pins: Option<PathBuf>,
    /// The local log of remote actions (`<state>/remote-actions.log`); `None`: no log.
    pub action_log: Option<PathBuf>,
    pub redactor: Redactor,
    /// The local off switches, re-read on every enable and reconnect ([`super::policy`]).
    pub switches: Arc<dyn Fn() -> Result<(), String> + Send + Sync>,
    pub tuning: LinkTuning,
}

/// The link's state, for the badge.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RemoteState {
    /// Off; `reason` says why it last ended (`None`: never on).
    Off {
        reason: Option<String>,
    },
    /// Registering.
    Starting,
    On(RemoteOn),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RemoteOn {
    pub session_id: String,
    /// The current pairing link, while its secret is open (show it with a QR code).
    pub link: Option<String>,
    pub link_expires_at_ms: Option<i64>,
    /// Live browser event streams (`REMOTE ON · n devices`).
    pub viewers: u32,
    /// Devices pinned for this user.
    pub devices: usize,
    /// The command stream is connected.
    pub online: bool,
    pub expires_at_ms: i64,
    pub idle_deadline_ms: i64,
    pub queue_len: usize,
    /// The harness key's display fingerprint (`29CA-E6B5-00C2-66CB`).
    pub fingerprint: String,
}

/// A pairing request that passed every check: show it and answer `y`/`N` locally.
#[derive(Debug)]
pub struct PairingPrompt {
    pub device_id: String,
    /// Sanitized (no control, ESC or bidi characters).
    pub device_name: String,
    pub email: String,
    pub user_agent: String,
    /// `NNNN-NNNN`: must match the code the browser shows.
    pub code: String,
    /// The prompt times out (answer N) at this time.
    pub expires_at_ms: i64,
    /// Send `true` for y. Dropping it answers N.
    pub answer: oneshot::Sender<bool>,
}

impl PairingPrompt {
    /// The prompt line of section 7.5.
    pub fn question(&self) -> String {
        let who = if self.email.is_empty() {
            String::new()
        } else {
            format!(" ({})", self.email)
        };
        format!(
            "Allow \"{}\"{who} to control this session? code {} [y/N]",
            self.device_name, self.code
        )
    }
}

/// What the link tells the local chat.
#[derive(Debug)]
pub enum LocalEvent {
    Pairing(PairingPrompt),
    Notice { level: NoticeLevel, text: String },
}

/// Why `enable` failed.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("{message}")]
pub struct RemoteError {
    pub message: String,
}

impl RemoteError {
    fn new(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
        }
    }
}

/// What `enable` returns.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Enabled {
    pub session_id: String,
    pub link: String,
    pub link_expires_at_ms: i64,
    pub expires_at_ms: i64,
    pub fingerprint: String,
}

enum Control {
    Disable {
        reason: String,
        done: oneshot::Sender<()>,
    },
    NewPairing {
        reply: oneshot::Sender<Result<(String, i64), String>>,
    },
    DevicesChanged,
}

struct Shared {
    deps: RemoteDeps,
    state: watch::Sender<RemoteState>,
    local_tx: mpsc::UnboundedSender<LocalEvent>,
    local_rx: Mutex<Option<mpsc::UnboundedReceiver<LocalEvent>>>,
    control: Mutex<Option<mpsc::UnboundedSender<Control>>>,
    pins: Mutex<Option<PinStore>>,
    key: Mutex<Option<KeyPair>>,
}

/// The remote link of one chat. Cheap to clone.
#[derive(Clone)]
pub struct RemoteLink {
    shared: Arc<Shared>,
}

impl std::fmt::Debug for RemoteLink {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RemoteLink")
            .field("state", &*self.shared.state.borrow())
            .finish()
    }
}

impl RemoteLink {
    pub fn new(deps: RemoteDeps) -> Self {
        let (state, _) = watch::channel(RemoteState::Off { reason: None });
        let (local_tx, local_rx) = mpsc::unbounded_channel();
        Self {
            shared: Arc::new(Shared {
                deps,
                state,
                local_tx,
                local_rx: Mutex::new(Some(local_rx)),
                control: Mutex::new(None),
                pins: Mutex::new(None),
                key: Mutex::new(None),
            }),
        }
    }

    /// The badge's state; changes are sent as they happen.
    pub fn state(&self) -> watch::Receiver<RemoteState> {
        self.shared.state.subscribe()
    }

    pub fn current(&self) -> RemoteState {
        self.shared.state.borrow().clone()
    }

    pub fn is_on(&self) -> bool {
        !matches!(*self.shared.state.borrow(), RemoteState::Off { .. })
    }

    /// Pairing prompts and notices for the chat (once: the first caller gets the receiver).
    /// Without a taker, every pairing request is answered N.
    pub fn take_local_events(&self) -> Option<mpsc::UnboundedReceiver<LocalEvent>> {
        lock(&self.shared.local_rx).take()
    }

    fn pins(&self) -> Result<std::sync::MutexGuard<'_, Option<PinStore>>, String> {
        let mut guard = lock(&self.shared.pins);
        if guard.is_none() {
            *guard = Some(match &self.shared.deps.pins {
                Some(path) => PinStore::load(path)?,
                None => PinStore::memory(),
            });
        }
        Ok(guard)
    }

    /// Every pinned device (expired ones included), for `/remote devices`.
    pub fn devices(&self) -> Result<Vec<StoredPin>, String> {
        Ok(self
            .pins()?
            .as_ref()
            .map(|p| p.all().to_vec())
            .unwrap_or_default())
    }

    /// Unpins one device (local only). Returns whether it was pinned.
    pub fn unpin(&self, device_id: &str) -> Result<bool, String> {
        let removed = self
            .pins()?
            .as_mut()
            .map(|p| p.unpin(device_id))
            .transpose()?
            .unwrap_or(false);
        self.notify_devices_changed();
        Ok(removed)
    }

    /// Unpins every device (`/remote devices` → unpin all). Returns how many there were.
    pub fn unpin_all(&self) -> Result<usize, String> {
        let n = self
            .pins()?
            .as_mut()
            .map(PinStore::unpin_all)
            .transpose()?
            .unwrap_or(0);
        self.notify_devices_changed();
        Ok(n)
    }

    fn notify_devices_changed(&self) {
        if let Some(tx) = lock(&self.shared.control).as_ref() {
            let _ = tx.send(Control::DevicesChanged);
        }
    }

    /// A fresh pairing link (`/remote pair`): a new secret replaces the old one. Local only.
    pub async fn new_pairing_link(&self) -> Result<(String, i64), String> {
        let tx = lock(&self.shared.control)
            .clone()
            .ok_or_else(|| "remote control is off".to_owned())?;
        let (reply, rx) = oneshot::channel();
        tx.send(Control::NewPairing { reply })
            .map_err(|_| "remote control is off".to_owned())?;
        rx.await.map_err(|_| "remote control is off".to_owned())?
    }

    /// Ends remote control (`/remote off`, harness exit): `bye`, then `DELETE`. The local session
    /// keeps running.
    pub async fn disable(&self) {
        self.disable_with("harness_ended").await;
    }

    async fn disable_with(&self, reason: &str) {
        let tx = lock(&self.shared.control).clone();
        if let Some(tx) = tx {
            let (done, rx) = oneshot::channel();
            if tx
                .send(Control::Disable {
                    reason: reason.to_owned(),
                    done,
                })
                .is_ok()
            {
                let _ = tokio::time::timeout(Duration::from_secs(10), rx).await;
            }
        }
    }

    /// Turns remote control on (`/remote`): checks the local switches and the login, loads or
    /// creates the harness key, registers the session, opens the command stream and mints the
    /// first pairing link.
    pub async fn enable(&self) -> Result<Enabled, RemoteError> {
        if self.is_on() {
            return Err(RemoteError::new("remote control is already on"));
        }
        let deps = &self.shared.deps;
        (deps.switches)().map_err(RemoteError::new)?;
        if deps.setup.project_id.is_none_or(|id| id <= 0) {
            // The relay binds a session to a project (its org's setting applies).
            return Err(RemoteError::new(
                "remote control needs an AMP project: run the chat in a folder with .amp.json",
            ));
        }
        self.shared.state.send_replace(RemoteState::Starting);
        let result = self.enable_inner().await;
        if let Err(e) = &result {
            self.shared.state.send_replace(RemoteState::Off {
                reason: Some(e.message.clone()),
            });
        }
        result
    }

    async fn enable_inner(&self) -> Result<Enabled, RemoteError> {
        let deps = &self.shared.deps;
        let stored = lock(&self.shared.key).clone();
        let key = match stored {
            Some(k) => k,
            None => {
                let store = deps.keys.clone();
                let key = tokio::task::spawn_blocking(move || store.load_or_create())
                    .await
                    .map_err(|e| RemoteError::new(format!("the harness key: {e}")))?
                    .map_err(|e| RemoteError::new(format!("the harness key: {e}")))?;
                *lock(&self.shared.key) = Some(key.clone());
                key
            }
        };
        let client = RelayClient::new(&deps.setup.api_base, deps.setup.ca_file.as_deref())
            .map_err(RemoteError::new)?;
        let oauth = deps.tokens.oauth_token().await.map_err(RemoteError::new)?;
        super::policy::check_token(&oauth).map_err(RemoteError::new)?;
        let user_id = deps.tokens.user_id().await.map_err(RemoteError::new)?;
        let reg = client
            .register(
                &oauth,
                key.id(),
                &key.public_b64u(),
                &deps.setup.host,
                &deps.setup.os,
                &deps.setup.version,
                deps.setup.project_id,
            )
            .await
            .map_err(|e| RemoteError::new(e.register_message()))?;
        if reg.protocol_version != 1 {
            let _ = client
                .end(&reg.session_id, &reg.connect_token, "harness_ended")
                .await;
            return Err(RemoteError::new(
                "This AMP server speaks another remote control version; update amp-harness.",
            ));
        }
        {
            let mut pins = self.pins().map_err(RemoteError::new)?;
            if let Some(p) = pins.as_mut() {
                p.reload().map_err(RemoteError::new)?;
            }
        }
        let token = ConnectToken::default();
        token.set(&reg.connect_token);
        let now = now_ms();
        let window = PairingWindow::new(now);
        let link = pairing::pairing_link(
            &deps.setup.web_origin,
            &reg.session_id,
            window.secret(),
            key.id(),
        );
        // The pairing secret and the tokens are secrets too.
        deps.redactor.add_values([
            oauth.clone(),
            reg.connect_token.clone(),
            crate::remote::crypto::b64u(window.secret()),
        ]);
        if let Err(e) = client
            .open_pairing(
                &reg.session_id,
                &token.get(),
                window.id(),
                window.expires_at_ms(),
            )
            .await
        {
            // Don't leave a registered session behind.
            let _ = client
                .end(&reg.session_id, &token.get(), "harness_ended")
                .await;
            return Err(RemoteError::new(format!(
                "AMP refused the pairing window: {e}"
            )));
        }
        let (ctl_tx, ctl_rx) = mpsc::unbounded_channel();
        *lock(&self.shared.control) = Some(ctl_tx);
        let enabled = Enabled {
            session_id: reg.session_id.clone(),
            link: link.clone(),
            link_expires_at_ms: window.expires_at_ms(),
            expires_at_ms: reg.expires_at_ms,
            fingerprint: key.display_fingerprint(),
        };
        let actor = Actor::new(
            self.clone(),
            key,
            client,
            reg,
            token,
            (window, link),
            user_id,
            ctl_rx,
        );
        tokio::spawn(actor.run());
        Ok(enabled)
    }

    fn notice(&self, level: NoticeLevel, text: impl Into<String>) {
        let _ = self.shared.local_tx.send(LocalEvent::Notice {
            level,
            text: text.into(),
        });
    }
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

/// What the actor hears from its tasks.
enum Inbound {
    StreamUp(u64),
    Frame(u64, Frame),
    StreamDown(u64, Option<RelayError>),
    /// The poster or heartbeat hit a terminal answer.
    Fatal(String, Option<String>),
    TokenExpired,
    Viewers(u32),
}

/// A prompt waiting in the follow-up queue.
#[derive(Debug, Clone)]
struct Queued {
    prompt_id: String,
    text: String,
    origin: PromptOrigin,
}

/// A token bucket.
#[derive(Debug)]
struct Bucket {
    tokens: f64,
    per_ms: f64,
    burst: f64,
    at: i64,
}

impl Bucket {
    fn new(per_min: f64, burst: f64) -> Self {
        Self {
            tokens: burst,
            per_ms: per_min / 60_000.0,
            burst,
            at: now_ms(),
        }
    }

    fn take(&mut self, now: i64) -> bool {
        self.tokens = (self.tokens + (now - self.at).max(0) as f64 * self.per_ms).min(self.burst);
        self.at = now;
        if self.tokens >= 1.0 {
            self.tokens -= 1.0;
            true
        } else {
            false
        }
    }
}

enum PosterMsg {
    Envelope(String),
    Flush(oneshot::Sender<()>),
}

struct Actor {
    link: RemoteLink,
    key: KeyPair,
    client: RelayClient,
    user_id: i64,
    session_id: String,
    token: ConnectToken,
    token_expires_ms: i64,
    /// No token refresh before this (a failed refresh backs off).
    refresh_after_ms: i64,
    /// The person was told their login was refused (once).
    told_login: bool,
    signer: EventSigner,
    replay: ReplayState,
    window: Option<PairingWindow>,
    window_link: Option<String>,
    pending_pair: Option<(PairCandidate, oneshot::Receiver<bool>)>,
    queue: VecDeque<Queued>,
    queued_ids: u64,
    prompts: Bucket,
    ops: Bucket,
    refused_at: std::collections::HashMap<String, i64>,
    busy: bool,
    feed_seen: u64,
    guard: DeltaGuard,
    viewers: u32,
    online: bool,
    started_at_ms: i64,
    expires_at_ms: i64,
    idle_timeout_ms: i64,
    last_activity_ms: i64,
    pin_days: i64,
    heartbeat_every: Duration,
    last_status: Option<Value>,
    last_status_ms: i64,
    stream_gen: u64,
    backoff: Duration,
    reconnect_at: Option<tokio::time::Instant>,
    last_byte_ms: Arc<AtomicI64>,
    inbound_tx: mpsc::UnboundedSender<Inbound>,
    inbound_rx: mpsc::UnboundedReceiver<Inbound>,
    poster: mpsc::UnboundedSender<PosterMsg>,
    poster_rx: Option<mpsc::UnboundedReceiver<PosterMsg>>,
    control: mpsc::UnboundedReceiver<Control>,
    tasks: Vec<tokio::task::JoinHandle<()>>,
}

impl Actor {
    #[allow(clippy::too_many_arguments)]
    fn new(
        link: RemoteLink,
        key: KeyPair,
        client: RelayClient,
        reg: super::relay::Registered,
        token: ConnectToken,
        (window, window_link): (PairingWindow, String),
        user_id: i64,
        control: mpsc::UnboundedReceiver<Control>,
    ) -> Self {
        let (inbound_tx, inbound_rx) = mpsc::unbounded_channel();
        let (poster, poster_rx) = mpsc::unbounded_channel();
        let now = now_ms();
        Self {
            signer: EventSigner::new(key.clone(), reg.session_id.clone()),
            key,
            client,
            user_id,
            session_id: reg.session_id.clone(),
            token,
            token_expires_ms: reg.connect_token_expires_at_ms,
            refresh_after_ms: 0,
            told_login: false,
            replay: ReplayState::default(),
            window: Some(window),
            window_link: Some(window_link),
            pending_pair: None,
            queue: VecDeque::new(),
            queued_ids: 0,
            prompts: Bucket::new(20.0, 5.0),
            ops: Bucket::new(120.0, 120.0),
            refused_at: Default::default(),
            busy: false,
            feed_seen: link.shared.deps.feed.last_seq(),
            guard: DeltaGuard::default(),
            viewers: 0,
            online: false,
            started_at_ms: reg.started_at_ms,
            expires_at_ms: reg.expires_at_ms,
            idle_timeout_ms: reg.idle_timeout_s.max(1) * 1000,
            last_activity_ms: now,
            pin_days: reg.device_pin_max_days,
            heartbeat_every: Duration::from_secs(reg.heartbeat_interval_s.clamp(1, 60) as u64),
            last_status: None,
            last_status_ms: 0,
            stream_gen: 0,
            backoff: link.shared.deps.tuning.backoff_base,
            reconnect_at: None,
            last_byte_ms: Arc::new(AtomicI64::new(now)),
            inbound_tx,
            inbound_rx,
            poster,
            poster_rx: Some(poster_rx),
            control,
            tasks: Vec::new(),
            link,
        }
    }

    fn deps(&self) -> &RemoteDeps {
        &self.link.shared.deps
    }

    fn tuning(&self) -> LinkTuning {
        self.deps().tuning
    }

    fn active_pins(&self) -> Vec<envelope::DevicePin> {
        let user = self.user_id;
        self.link
            .pins()
            .ok()
            .and_then(|p| p.as_ref().map(|p| p.active(user, now_ms())))
            .unwrap_or_default()
    }

    fn publish_state(&self) {
        let now = now_ms();
        let link = self
            .window
            .as_ref()
            .filter(|w| w.is_open(now))
            .and(self.window_link.clone());
        self.link
            .shared
            .state
            .send_replace(RemoteState::On(RemoteOn {
                session_id: self.session_id.clone(),
                link_expires_at_ms: link
                    .as_ref()
                    .and_then(|_| self.window.as_ref().map(PairingWindow::expires_at_ms)),
                link,
                viewers: self.viewers,
                devices: self.active_pins().len(),
                online: self.online,
                expires_at_ms: self.expires_at_ms,
                idle_deadline_ms: self.last_activity_ms + self.idle_timeout_ms,
                queue_len: self.queue.len(),
                fingerprint: self.key.display_fingerprint(),
            }));
    }

    fn log_action(&self, op: &str, device_id: Option<&str>, result: &str) {
        let Some(path) = &self.deps().action_log else {
            return;
        };
        let line = json!({
            "at_ms": now_ms(), "session_id": self.session_id, "op": op,
            "device_id": device_id, "result": result,
        });
        append_private(path, &format!("{line}\n"));
    }

    /// Signs one event (after redaction) and hands it to the poster.
    fn send(&mut self, op: &str, device_id: Option<&str>, mut body: Value) {
        self.deps().redactor.redact_value(&mut body);
        match self.signer.sign(op, device_id, body, now_ms()) {
            Ok(env) => {
                // Section 6.5: an event never exceeds 1 MiB (the feed's caps keep it far below).
                if env.len() > envelope::MAX_EVENT_BYTES {
                    tracing::warn!(op, "remote: an event over 1 MiB was dropped");
                    return;
                }
                let _ = self.poster.send(PosterMsg::Envelope(env));
            }
            Err(e) => tracing::warn!(op, error = %e, "remote: an event could not be signed"),
        }
    }

    fn status_body(&self) -> Value {
        let s = self.deps().host.status();
        let devices: Vec<Value> = self
            .active_pins()
            .iter()
            .map(|p| json!({"device_id": p.device_id, "name": p.name}))
            .collect();
        json!({
            "model": s.model,
            "busy": s.busy || self.busy,
            "context_pct": s.context_pct,
            "cost_usd_micros": s.cost_usd_micros,
            "project": s.project.map(|(id, name)| json!({"id": id, "name": name})),
            "ticket_id": s.ticket_id,
            "branch": s.branch,
            "queue_len": self.queue.len(),
            "workers_running": s.workers_running,
            "devices": devices,
            // #2388 (optional within v1: an older harness leaves these out).
            "ticket_name": s.ticket_name.as_deref().map(|t| cap(t, 200)),
            "ticket_state": s.ticket_state,
            "epic": s.epic.as_deref().map(|t| cap(t, 200)),
            "story": s.story.as_deref().map(|t| cap(t, 200)),
            "usage": s.usage.map(|u| json!({
                "input_tokens": u.input, "output_tokens": u.output,
                "cache_read_tokens": u.cache_read, "cache_write_tokens": u.cache_write,
            })),
            "thinking": s.thinking,
            "role": s.role,
            "cwd": s.cwd.as_deref().and_then(cwd_basename),
            "board": s.board.map(|b| json!({
                "in_progress": b.in_progress, "ready": b.ready,
                "question": b.question, "scheduled": b.scheduled,
            })),
            "plan_approval": s.plan_approval.map(|(yours, others)| json!({
                "yours": yours, "others": others,
            })),
            "amp_connection": s.amp_connection.map(|c| json!({
                "state": c.state, "latency_ms": c.latency_ms,
            })),
        })
    }

    fn send_hello(&mut self) {
        let setup = &self.deps().setup;
        let body = json!({
            "harness": {
                "harness_id": self.key.id(), "harness_pub": self.key.public_b64u(),
                "host": cap(&setup.host, 64), "os": setup.os, "version": cap(&setup.version, 64),
            },
            "user_id": self.user_id,
            "ops": OPS,
            "started_at_ms": self.started_at_ms,
            "expires_at_ms": self.expires_at_ms,
            "idle_timeout_s": self.idle_timeout_ms / 1000,
            "status": self.status_body(),
        });
        self.send("hello", None, body);
        self.last_status = Some(self.status_body());
        self.last_status_ms = now_ms();
    }

    fn maybe_status(&mut self, force: bool) {
        let now = now_ms();
        let t = self.tuning();
        if !force && now - self.last_status_ms < t.status_min.as_millis() as i64 {
            return;
        }
        let body = self.status_body();
        let stale = now - self.last_status_ms >= t.status_max.as_millis() as i64;
        if force || stale || self.last_status.as_ref() != Some(&body) {
            self.send("status", None, body.clone());
            self.last_status = Some(body);
            self.last_status_ms = now;
        }
    }

    fn start_tasks(&mut self) {
        // Poster.
        if let Some(rx) = self.poster_rx.take() {
            let poster = Poster {
                client: self.client.clone(),
                session_id: self.session_id.clone(),
                token: self.token.clone(),
                every: self.tuning().batch_every,
                inbound: self.inbound_tx.clone(),
            };
            self.tasks.push(tokio::spawn(poster.run(rx)));
        }
        // Heartbeat.
        let client = self.client.clone();
        let sid = self.session_id.clone();
        let token = self.token.clone();
        let every = self.heartbeat_every;
        let inbound = self.inbound_tx.clone();
        self.tasks.push(tokio::spawn(async move {
            let mut ticker = tokio::time::interval(every);
            ticker.tick().await;
            loop {
                ticker.tick().await;
                match client.heartbeat(&sid, &token.get()).await {
                    Ok(v) => {
                        if let Some(n) = v.get("viewers").and_then(Value::as_u64) {
                            let _ = inbound.send(Inbound::Viewers(n as u32));
                        }
                    }
                    Err(e) => {
                        if let Some(reason) = e.ended_reason() {
                            let _ = inbound.send(Inbound::Fatal(reason, None));
                            return;
                        }
                        if e.status == 401 {
                            let _ = inbound.send(Inbound::TokenExpired);
                        }
                    }
                }
            }
        }));
        self.open_stream();
    }

    fn open_stream(&mut self) {
        self.stream_gen += 1;
        let gen_ = self.stream_gen;
        let client = self.client.clone();
        let sid = self.session_id.clone();
        let token = self.token.get();
        let inbound = self.inbound_tx.clone();
        let last = Arc::clone(&self.last_byte_ms);
        self.tasks.push(tokio::spawn(async move {
            let up = inbound.clone();
            let frames = inbound.clone();
            let result = client
                .command_stream(
                    &sid,
                    &token,
                    last,
                    move || {
                        let _ = up.send(Inbound::StreamUp(gen_));
                    },
                    move |f| frames.send(Inbound::Frame(gen_, f)).is_ok(),
                )
                .await;
            let _ = inbound.send(Inbound::StreamDown(gen_, result.err()));
        }));
    }

    async fn run(mut self) {
        self.start_tasks();
        self.publish_state();
        let feed = Arc::clone(&self.deps().feed);
        let (catch_up, mut feed_rx) = feed.subscribe_from(self.feed_seen);
        if let Ok(entries) = catch_up {
            for e in entries {
                self.on_feed(e).await;
            }
        }
        let mut ticker = tokio::time::interval(self.tuning().tick);
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        let end: Option<(String, bool)> = loop {
            let pair_wait = async {
                match self.pending_pair.as_mut() {
                    Some((_, rx)) => Some(rx.await.unwrap_or(false)),
                    None => std::future::pending().await,
                }
            };
            let reconnect = async {
                match self.reconnect_at {
                    Some(at) => tokio::time::sleep_until(at).await,
                    None => std::future::pending().await,
                }
            };
            tokio::select! {
                msg = self.control.recv() => match msg {
                    Some(Control::Disable { reason, done }) => {
                        self.finish(&reason, true).await;
                        let _ = done.send(());
                        return;
                    }
                    Some(Control::NewPairing { reply }) => {
                        let r = self.new_window().await;
                        let _ = reply.send(r);
                    }
                    Some(Control::DevicesChanged) => {
                        self.maybe_status(true);
                        self.publish_state();
                    }
                    None => break Some(("harness_ended".to_owned(), true)),
                },
                inbound = self.inbound_rx.recv() => {
                    let Some(inbound) = inbound else { break Some(("harness_ended".into(), true)) };
                    if let Some(end) = self.on_inbound(inbound).await {
                        break Some(end);
                    }
                }
                entry = feed_rx.recv() => match entry {
                    Ok(e) => self.on_feed(e).await,
                    Err(tokio::sync::broadcast::error::RecvError::Lagged(_)) => {
                        self.catch_up_feed().await;
                    }
                    Err(tokio::sync::broadcast::error::RecvError::Closed) => {
                        break Some(("harness_ended".into(), true));
                    }
                },
                answer = pair_wait => {
                    if let Some(answer) = answer {
                        self.on_pair_answer(answer);
                    }
                }
                _ = reconnect => {
                    self.reconnect_at = None;
                    if let Some(end) = self.reconnect().await {
                        break Some(end);
                    }
                }
                _ = ticker.tick() => {
                    if let Some(end) = self.on_tick().await {
                        break Some(end);
                    }
                }
            }
        };
        if let Some((reason, tell_relay)) = end {
            self.finish(&reason, tell_relay).await;
        }
    }

    async fn catch_up_feed(&mut self) {
        let feed = Arc::clone(&self.deps().feed);
        match feed.since(self.feed_seen) {
            Ok(entries) => {
                for e in entries {
                    self.on_feed(e).await;
                }
            }
            Err(gap) => {
                // M1: no resync (section 11); say so and go on from the newest event.
                self.feed_seen = gap.last;
                let body = serde_json::to_value(ChatEvent::notice(
                    NoticeLevel::Warn,
                    Some("events_dropped"),
                    "Some of the laptop's chat events were dropped; the transcript has a gap.",
                ))
                .unwrap_or(Value::Null);
                self.send("chat_event", None, body);
            }
        }
    }

    async fn on_tick(&mut self) -> Option<(String, bool)> {
        let now = now_ms();
        if now >= self.expires_at_ms {
            return Some(("expired".into(), true));
        }
        if now >= self.last_activity_ms + self.idle_timeout_ms {
            return Some(("idle".into(), true));
        }
        // A delta whose last word waits for more text: send it once the feed is quiet.
        // (The feed numbers a quiet delta after ~100 ms, so a word still growing arrives sooner.)
        if self.guard.is_stale(Duration::from_millis(400)) {
            self.flush_guard();
        }
        // Silence: no byte (not even a keepalive) for too long.
        if self.online
            && now - self.last_byte_ms.load(Ordering::Relaxed)
                > self.tuning().silence.as_millis() as i64
        {
            self.stream_down(None);
        }
        // Token refresh at two thirds of its life.
        let left = self.token_expires_ms - now;
        if left < 5 * 60 * 1000 && now >= self.refresh_after_ms {
            self.refresh_token().await;
        }
        // The pairing window's expiry (the prompt answers N).
        if let Some((cand, _)) = &self.pending_pair
            && self
                .window
                .as_ref()
                .is_none_or(|w| now >= w.expires_at_ms())
        {
            let pid = cand.pairing_id.clone();
            let dev = cand.device_id.clone();
            self.pending_pair = None;
            if let Some(w) = self.window.as_mut() {
                w.consume();
            }
            self.send_pair_result(PairResult {
                pairing_id: pid,
                device_id: Some(dev.clone()),
                result: "expired",
                code: Some(Code::PairingExpired),
                tries_left: None,
                pinned_until_ms: None,
            });
            self.log_action("pair", Some(&dev), "expired");
            self.publish_state();
        }
        // A queued prompt waiting for a run that ended without our noticing (a server switch).
        if !self.queue.is_empty() && !self.busy && !self.deps().host.status().busy {
            self.start_next().await;
        }
        self.maybe_status(false);
        None
    }

    async fn refresh_token(&mut self) {
        let oauth = match self.deps().tokens.oauth_token().await {
            Ok(t) => t,
            Err(e) => {
                tracing::debug!(error = %e, "remote: no OAuth token for the refresh");
                return;
            }
        };
        match self.client.refresh(&self.session_id, &oauth).await {
            Ok(r) => {
                self.deps()
                    .redactor
                    .add_values([oauth, r.connect_token.clone()]);
                self.token.set(&r.connect_token);
                self.token_expires_ms = r.connect_token_expires_at_ms;
                self.refresh_after_ms = 0;
                self.told_login = false;
            }
            Err(e) => {
                self.refresh_after_ms = now_ms() + 30_000;
                if e.status == 401 && !self.told_login {
                    // A 401 on refresh is the OAuth login (#2374): log in again. The link keeps
                    // trying, so `amp-harness login` in another terminal heals it.
                    self.told_login = true;
                    self.link.notice(
                        NoticeLevel::Warn,
                        "remote control: AMP did not accept your login; run `amp-harness login`",
                    );
                }
                if let Some(reason) = e.ended_reason() {
                    let _ = self.inbound_tx.send(Inbound::Fatal(reason, None));
                } else if e.status == 403 {
                    // The org switch went off (section 12.3).
                    let _ = self.inbound_tx.send(Inbound::Fatal(
                        "policy_off".into(),
                        Some(e.register_message()),
                    ));
                }
            }
        }
    }

    fn stream_down(&mut self, err: Option<RelayError>) {
        // Ignore the old reader from now on.
        self.stream_gen += 1;
        self.online = false;
        if let Some(e) = &err {
            tracing::debug!(error = %e, "remote: the command stream is down");
        }
        let t = self.tuning();
        let jitter = 0.8 + (crate::remote::crypto::random_bytes(1)[0] as f64 / 255.0) * 0.4;
        let delay = self.backoff.mul_f64(jitter);
        self.backoff = (self.backoff * 2).min(t.backoff_max);
        self.reconnect_at = Some(tokio::time::Instant::now() + delay);
        self.publish_state();
    }

    /// Section 12.3: re-check the switch, the deadlines, the token and the pins, then reopen.
    async fn reconnect(&mut self) -> Option<(String, bool)> {
        let now = now_ms();
        if now >= self.expires_at_ms {
            return Some(("expired".into(), true));
        }
        if now >= self.last_activity_ms + self.idle_timeout_ms {
            return Some(("idle".into(), true));
        }
        if let Err(why) = (self.deps().switches)() {
            self.link.notice(NoticeLevel::Warn, why);
            return Some(("harness_ended".into(), true));
        }
        self.refresh_token().await;
        if let Ok(mut pins) = self.link.pins()
            && let Some(p) = pins.as_mut()
            && let Err(e) = p.reload()
        {
            tracing::warn!(error = %e, "remote: the pin file could not be read again");
        }
        self.open_stream();
        None
    }

    async fn on_inbound(&mut self, inbound: Inbound) -> Option<(String, bool)> {
        match inbound {
            Inbound::StreamUp(g) if g == self.stream_gen => {
                self.online = true;
                self.backoff = self.tuning().backoff_base;
                self.send_hello();
                self.publish_state();
            }
            Inbound::StreamDown(g, err) if g == self.stream_gen => {
                if let Some(reason) = err.as_ref().and_then(RelayError::ended_reason) {
                    return Some((reason, false));
                }
                if err.as_ref().is_some_and(|e| e.status == 401) {
                    self.refresh_token().await;
                }
                if err.as_ref().is_some_and(|e| e.status == 404) {
                    return Some(("harness_ended".into(), false));
                }
                self.stream_down(err);
            }
            Inbound::Frame(g, frame) if g == self.stream_gen => return self.on_frame(frame).await,
            Inbound::StreamUp(_) | Inbound::StreamDown(..) | Inbound::Frame(..) => {}
            Inbound::Fatal(reason, message) => {
                if let Some(m) = message {
                    self.link.notice(NoticeLevel::Warn, m);
                }
                let tell = reason == "bad_batch";
                return Some((if tell { "harness_ended".into() } else { reason }, tell));
            }
            Inbound::TokenExpired => self.refresh_token().await,
            Inbound::Viewers(n) => self.set_viewers(n),
        }
        None
    }

    fn set_viewers(&mut self, n: u32) {
        if n > self.viewers {
            // A new viewer attach is remote activity (section 10).
            self.last_activity_ms = now_ms();
        }
        if n != self.viewers {
            self.viewers = n;
            self.publish_state();
        }
    }

    async fn on_frame(&mut self, frame: Frame) -> Option<(String, bool)> {
        match frame {
            Frame::Command(text) => return self.on_command(text.as_bytes()).await,
            Frame::PairRequest(data) => self.on_pair_request(data.as_bytes()),
            Frame::Viewers(n) => self.set_viewers(n),
            Frame::DeviceRevoked { device_id, all } => {
                // #2376: the chat says which device AMP revoked.
                let name = device_id.as_deref().and_then(|d| {
                    self.link
                        .devices()
                        .ok()?
                        .into_iter()
                        .find(|p| p.device_id == d)
                        .map(|p| p.name)
                });
                let text = match (all, name) {
                    (true, _) => "Remote control: AMP revoked every paired device; each must pair \
                                  again (/remote pair)."
                        .to_owned(),
                    (false, Some(n)) => format!(
                        "Remote control: AMP revoked \"{n}\"; its commands are refused from now on."
                    ),
                    (false, None) => "Remote control: AMP revoked a paired device.".to_owned(),
                };
                self.link.notice(NoticeLevel::Info, text);
                if let Ok(mut pins) = self.link.pins()
                    && let Some(p) = pins.as_mut()
                {
                    let r = if all {
                        p.unpin_all().map(|_| ())
                    } else if let Some(d) = &device_id {
                        p.unpin(d).map(|_| ())
                    } else {
                        Ok(())
                    };
                    if let Err(e) = r {
                        tracing::warn!(error = %e, "remote: unpin failed");
                    }
                }
                self.log_action(
                    "device_revoked",
                    device_id.as_deref(),
                    if all { "all" } else { "one" },
                );
                self.maybe_status(true);
                self.publish_state();
            }
            // Another stream of this session took over: not a session end. Reconnect (which
            // replaces it back, since only the connect token's holder can open one).
            Frame::End(reason) if reason == "replaced" => self.stream_down(None),
            Frame::End(reason) => return Some((reason, false)),
            Frame::Other(_) => {}
        }
        None
    }

    fn refuse(&mut self, r: &Refusal) {
        self.log_action(
            r.op.as_deref().unwrap_or("?"),
            r.device_id.as_deref(),
            r.code.as_str(),
        );
        // At most one refused event per second per device.
        let key = r.device_id.clone().unwrap_or_default();
        let now = now_ms();
        let every = self.tuning().refused_every.as_millis() as i64;
        if self.refused_at.get(&key).is_some_and(|t| now - t < every) {
            return;
        }
        self.refused_at.insert(key, now);
        let device = r.device_id.clone();
        self.send("refused", device.as_deref(), envelope::refused_body(r));
    }

    async fn on_command(&mut self, text: &[u8]) -> Option<(String, bool)> {
        let pins = self.active_pins();
        let ctx = CommandContext {
            now_ms: now_ms(),
            user_id: self.user_id,
            session_id: &self.session_id,
            harness_id: self.key.id(),
            pins: &pins,
        };
        let cmd = match envelope::check_command(text, &ctx, &mut self.replay) {
            Ok(c) => c,
            Err(r) => {
                self.refuse(&r);
                return None;
            }
        };
        // Step 14: rate limits.
        let now = now_ms();
        let allowed = match cmd.command {
            Command::Prompt { .. } => self.prompts.take(now),
            Command::Stop | Command::Detach => self.ops.take(now),
        };
        if !allowed {
            self.refuse(&Refusal {
                code: Code::RateLimited,
                device_id: Some(cmd.device_id.clone()),
                seq: Some(cmd.seq),
                op: Some(cmd.command.op().into()),
                last_seq: None,
            });
            return None;
        }
        self.last_activity_ms = now;
        self.act(cmd).await
    }

    async fn act(&mut self, cmd: VerifiedCommand) -> Option<(String, bool)> {
        let VerifiedCommand {
            device_id,
            device_name,
            seq,
            command,
        } = cmd;
        match command {
            Command::Prompt { text, .. } => {
                let origin = PromptOrigin::Remote {
                    device_id: device_id.clone(),
                    device_name,
                };
                if self.queue.len() >= QUEUE_MAX {
                    self.refuse(&Refusal {
                        code: Code::QueueFull,
                        device_id: Some(device_id),
                        seq: Some(seq),
                        op: Some("prompt".into()),
                        last_seq: None,
                    });
                    return None;
                }
                self.queued_ids += 1;
                let prompt_id = format!("r_{}", self.queued_ids);
                let started = self.queue.is_empty()
                    && !self.busy
                    && self
                        .deps()
                        .host
                        .start_prompt(prompt_id.clone(), text.clone(), origin.clone())
                        .await;
                if started {
                    self.busy = true;
                    self.send(
                        "ack",
                        Some(&device_id),
                        envelope::ack_body(
                            &device_id,
                            seq,
                            "prompt",
                            "started",
                            Some(&prompt_id),
                            None,
                        ),
                    );
                    self.log_action("prompt", Some(&device_id), "started");
                } else {
                    self.queue.push_back(Queued {
                        prompt_id: prompt_id.clone(),
                        text,
                        origin,
                    });
                    let pos = self.queue.len();
                    self.send(
                        "ack",
                        Some(&device_id),
                        envelope::ack_body(
                            &device_id,
                            seq,
                            "prompt",
                            "queued",
                            Some(&prompt_id),
                            Some(pos),
                        ),
                    );
                    self.log_action("prompt", Some(&device_id), "queued");
                    self.push_queue_event();
                }
                self.publish_state();
            }
            Command::Stop => {
                self.deps().host.stop().await;
                let had = !self.queue.is_empty();
                self.queue.clear();
                self.send(
                    "ack",
                    Some(&device_id),
                    envelope::ack_body(&device_id, seq, "stop", "done", None, None),
                );
                if had {
                    self.push_queue_event();
                }
                self.log_action("stop", Some(&device_id), "done");
                self.publish_state();
            }
            Command::Detach => {
                self.send(
                    "ack",
                    Some(&device_id),
                    envelope::ack_body(&device_id, seq, "detach", "done", None, None),
                );
                self.log_action("detach", Some(&device_id), "done");
                return Some(("detached".into(), true));
            }
        }
        None
    }

    /// The queue changed: a `queue` chat event through the feed, so the local chat and every
    /// viewer see it in order.
    fn push_queue_event(&self) {
        let items = self
            .queue
            .iter()
            .map(|q| QueueItem::new(q.prompt_id.clone(), &q.text, &q.origin))
            .collect();
        self.deps().feed.push(ChatEvent::Queue { items });
    }

    async fn start_next(&mut self) {
        let Some(next) = self.queue.pop_front() else {
            return;
        };
        let started = self
            .deps()
            .host
            .start_prompt(
                next.prompt_id.clone(),
                next.text.clone(),
                next.origin.clone(),
            )
            .await;
        if started {
            self.busy = true;
            self.push_queue_event();
        } else {
            self.queue.push_front(next);
        }
        self.publish_state();
    }

    fn flush_guard(&mut self) {
        if let Some((message_id, thinking, text)) = self.guard.flush() {
            self.send_delta(&message_id, thinking, text);
        }
    }

    fn send_delta(&mut self, message_id: &str, thinking: bool, delta: String) {
        if delta.is_empty() {
            return;
        }
        let event = if thinking {
            ChatEvent::ThinkingDelta {
                message_id: message_id.to_owned(),
                delta,
            }
        } else {
            ChatEvent::TextDelta {
                message_id: message_id.to_owned(),
                delta,
            }
        };
        if let Ok(body) = serde_json::to_value(&event) {
            self.send("chat_event", None, body);
        }
    }

    async fn on_feed(&mut self, entry: FeedEntry) {
        if entry.seq <= self.feed_seen {
            return;
        }
        self.feed_seen = entry.seq;
        let event = entry.event;
        // Deltas: hold a trailing word back so a split secret is redacted whole.
        match &event {
            ChatEvent::TextDelta { message_id, delta }
            | ChatEvent::ThinkingDelta { message_id, delta } => {
                let thinking = matches!(event, ChatEvent::ThinkingDelta { .. });
                let (flushed, now) = self.guard.push(message_id, thinking, delta);
                if let Some((id, th, text)) = flushed {
                    self.send_delta(&id, th, text);
                }
                let id = message_id.clone();
                self.send_delta(&id, thinking, now);
                return;
            }
            _ => self.flush_guard(),
        }
        let event = match event {
            ChatEvent::ToolStart {
                message_id,
                tool_call_id,
                name,
                arguments_json,
                truncated,
            } => ChatEvent::ToolStart {
                arguments_json: if truncated {
                    redact::drop_partial_tail(&arguments_json).to_owned()
                } else {
                    arguments_json
                },
                message_id,
                tool_call_id,
                name,
                truncated,
            },
            ChatEvent::ToolEnd {
                tool_call_id,
                name,
                is_error,
                text,
                truncated,
                full_bytes,
                refused_line,
            } => ChatEvent::ToolEnd {
                text: if truncated {
                    redact::drop_partial_tail(&text).to_owned()
                } else {
                    text
                },
                tool_call_id,
                name,
                is_error,
                truncated,
                full_bytes,
                refused_line,
            },
            other => other,
        };
        let run_end = match &event {
            ChatEvent::RunStart { .. } => {
                self.busy = true;
                None
            }
            ChatEvent::RunEnd { outcome, .. } => {
                self.busy = false;
                Some(*outcome)
            }
            _ => None,
        };
        if let Ok(body) = serde_json::to_value(&event) {
            self.send("chat_event", None, body);
        }
        match run_end {
            // A stop (remote or local Esc / /abort) clears the queue (section 5.4).
            Some(RunOutcome::Aborted) => {
                if !self.queue.is_empty() {
                    self.queue.clear();
                    self.push_queue_event();
                    self.publish_state();
                }
            }
            Some(_) => self.start_next().await,
            None => {}
        }
        if run_end.is_some()
            || matches!(
                event,
                ChatEvent::RunStart { .. } | ChatEvent::WorkerStatus { .. }
            )
        {
            self.maybe_status(false);
        }
    }

    fn on_pair_request(&mut self, data: &[u8]) {
        let harness_pub = self.key.public_raw().to_vec();
        let input = PairCheckInput {
            now_ms: now_ms(),
            session_id: &self.session_id,
            harness_id: self.key.id(),
            harness_pub: &harness_pub,
            user_id: self.user_id,
        };
        let Some(window) = self.window.as_mut() else {
            return;
        };
        match window.check(data, &input) {
            Err(result) => {
                let dev = result.device_id.clone();
                self.log_action(
                    "pair",
                    dev.as_deref(),
                    result.code.map_or(result.result, Code::as_str),
                );
                self.send_pair_result(result);
                self.publish_state();
            }
            Ok(cand) => {
                let (answer, rx) = oneshot::channel();
                let prompt = PairingPrompt {
                    device_id: cand.device_id.clone(),
                    device_name: cand.device_name.clone(),
                    email: cand.email.clone(),
                    user_agent: cand.user_agent.clone(),
                    code: cand.code.clone(),
                    expires_at_ms: window.expires_at_ms(),
                    answer,
                };
                // Nobody to ask: dropping the prompt answers N.
                let _ = self.link.shared.local_tx.send(LocalEvent::Pairing(prompt));
                self.pending_pair = Some((cand, rx));
            }
        }
    }

    fn send_pair_result(&mut self, r: PairResult) {
        let dev = r.device_id.clone();
        self.send("pair_result", dev.as_deref(), r.body());
    }

    fn on_pair_answer(&mut self, yes: bool) {
        let Some((cand, _)) = self.pending_pair.take() else {
            return;
        };
        if let Some(w) = self.window.as_mut() {
            w.consume();
        }
        if !yes {
            self.send_pair_result(PairResult {
                pairing_id: cand.pairing_id,
                device_id: Some(cand.device_id.clone()),
                result: "rejected",
                code: Some(Code::Rejected),
                tries_left: None,
                pinned_until_ms: None,
            });
            self.log_action("pair", Some(&cand.device_id), "rejected");
            self.publish_state();
            return;
        }
        let now = now_ms();
        let until = pairing::pin_until(now, self.pin_days, self.expires_at_ms);
        let pin = StoredPin {
            device_id: cand.device_id.clone(),
            device_pub: crate::remote::crypto::b64u(&cand.device_pub),
            name: cand.device_name.clone(),
            user_id: self.user_id,
            paired_at_ms: now,
            pinned_until_ms: until,
            session_only: self.pin_days <= 0,
        };
        let saved = self
            .link
            .pins()
            .and_then(|mut p| p.as_mut().map(|p| p.pin(pin, now)).unwrap_or(Ok(())));
        if let Err(e) = saved {
            self.link.notice(
                NoticeLevel::Error,
                format!("remote: the paired device could not be saved: {e}"),
            );
        }
        self.send_pair_result(PairResult {
            pairing_id: cand.pairing_id,
            device_id: Some(cand.device_id.clone()),
            result: "accepted",
            code: None,
            tries_left: None,
            pinned_until_ms: Some(until),
        });
        self.log_action("pair", Some(&cand.device_id), "accepted");
        self.last_activity_ms = now;
        self.maybe_status(true);
        self.publish_state();
    }

    async fn new_window(&mut self) -> Result<(String, i64), String> {
        let now = now_ms();
        let window = PairingWindow::new(now);
        self.client
            .open_pairing(
                &self.session_id,
                &self.token.get(),
                window.id(),
                window.expires_at_ms(),
            )
            .await
            .map_err(|e| format!("AMP refused the pairing window: {e}"))?;
        let link = pairing::pairing_link(
            &self.deps().setup.web_origin,
            &self.session_id,
            window.secret(),
            self.key.id(),
        );
        self.deps()
            .redactor
            .add_values([crate::remote::crypto::b64u(window.secret())]);
        let expires = window.expires_at_ms();
        // A pending y/N for the old secret is answered N.
        self.pending_pair = None;
        self.window = Some(window);
        self.window_link = Some(link.clone());
        self.publish_state();
        Ok((link, expires))
    }

    /// Ends the session: `bye` and `DELETE` when the relay should hear it, then everything stops.
    async fn finish(&mut self, reason: &str, tell_relay: bool) {
        self.flush_guard();
        if tell_relay {
            self.send("bye", None, json!({"reason": reason}));
            let (done, rx) = oneshot::channel();
            if self.poster.send(PosterMsg::Flush(done)).is_ok() {
                let _ = tokio::time::timeout(self.tuning().bye_wait, rx).await;
            }
            let delete_reason = match reason {
                "detached" | "idle" | "expired" => reason,
                _ => "harness_ended",
            };
            if let Err(e) = self
                .client
                .end(&self.session_id, &self.token.get(), delete_reason)
                .await
            {
                tracing::debug!(error = %e, "remote: DELETE failed");
            }
        }
        for t in self.tasks.drain(..) {
            t.abort();
        }
        if !self.queue.is_empty() {
            self.queue.clear();
            self.push_queue_event();
        }
        if let Ok(mut pins) = self.link.pins()
            && let Some(p) = pins.as_mut()
        {
            p.end_session();
        }
        self.log_action("end", None, reason);
        *lock(&self.link.shared.control) = None;
        let text = match reason {
            "idle" => format!(
                "Remote control turned off after {} without remote activity.",
                human_duration(self.idle_timeout_ms)
            ),
            "expired" => "Remote control turned off: 12 hours since it was turned on.".to_owned(),
            "detached" => "Remote control turned off from the paired device (detach).".to_owned(),
            "harness_ended" => "Remote control is off.".to_owned(),
            "disconnected" => "Remote control turned off from the web app (Disconnect).".to_owned(),
            "revoked" => {
                "Remote control turned off: all remote sessions were revoked in AMP.".to_owned()
            }
            "policy_off" => "Remote control turned off by the organization's setting.".to_owned(),
            other => format!("Remote control turned off ({other})."),
        };
        self.link.notice(NoticeLevel::Info, text);
        self.link.shared.state.send_replace(RemoteState::Off {
            reason: Some(reason.to_owned()),
        });
    }
}

/// `30 minutes`, `1 hour`, `45 seconds`.
fn human_duration(ms: i64) -> String {
    let s = ms / 1000;
    let (n, unit) = if s >= 3600 && s % 3600 == 0 {
        (s / 3600, "hour")
    } else if s >= 60 {
        (s / 60, "minute")
    } else {
        (s, "second")
    };
    format!("{n} {unit}{}", if n == 1 { "" } else { "s" })
}

fn cap(s: &str, n: usize) -> String {
    s.chars().take(n).collect()
}

/// Appends `line` to `path` (mode 0600).
fn append_private(path: &std::path::Path, line: &str) {
    use std::io::Write as _;
    if let Some(dir) = path.parent() {
        let _ = std::fs::create_dir_all(dir);
    }
    let mut opts = std::fs::OpenOptions::new();
    opts.append(true).create(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        opts.mode(0o600);
    }
    if let Ok(mut f) = opts.open(path) {
        let _ = f.write_all(line.as_bytes());
    }
}

/// Batches signed envelopes (about every 100 ms, ≤ 64 / 2 MiB a batch) and posts them in
/// order, keeping each one until the relay has accepted it.
struct Poster {
    client: RelayClient,
    session_id: String,
    token: ConnectToken,
    every: Duration,
    inbound: mpsc::UnboundedSender<Inbound>,
}

const BATCH_EVENTS: usize = 64;
const BATCH_BYTES: usize = 2 * 1024 * 1024 - 64;

fn seq_of(env: &str) -> i64 {
    serde_json::from_str::<Value>(env)
        .ok()
        .and_then(|v| v.get("seq").and_then(Value::as_i64))
        .unwrap_or(0)
}

impl Poster {
    async fn run(self, mut rx: mpsc::UnboundedReceiver<PosterMsg>) {
        let mut pending: VecDeque<(i64, String)> = VecDeque::new();
        let mut flushes: Vec<oneshot::Sender<()>> = Vec::new();
        let mut retry = Duration::from_millis(250);
        loop {
            // Wait for something to send.
            if pending.is_empty() {
                match rx.recv().await {
                    Some(PosterMsg::Envelope(e)) => pending.push_back((seq_of(&e), e)),
                    Some(PosterMsg::Flush(done)) => {
                        let _ = done.send(());
                        continue;
                    }
                    None => return,
                }
            }
            // Gather for one batch interval.
            let deadline = tokio::time::Instant::now() + self.every;
            loop {
                tokio::select! {
                    msg = rx.recv() => match msg {
                        Some(PosterMsg::Envelope(e)) => pending.push_back((seq_of(&e), e)),
                        Some(PosterMsg::Flush(done)) => { flushes.push(done); break; }
                        None => break,
                    },
                    _ = tokio::time::sleep_until(deadline) => break,
                }
            }
            while !pending.is_empty() {
                let mut batch = Vec::new();
                let mut bytes = 0;
                for (_, e) in pending.iter() {
                    if batch.len() >= BATCH_EVENTS || bytes + e.len() > BATCH_BYTES {
                        break;
                    }
                    bytes += e.len() + 1;
                    batch.push(e.clone());
                }
                match self
                    .client
                    .post_events(&self.session_id, &self.token.get(), &batch)
                    .await
                {
                    Ok(a) => {
                        while pending
                            .front()
                            .is_some_and(|(s, _)| *s <= a.accepted_through)
                        {
                            pending.pop_front();
                        }
                        retry = Duration::from_millis(250);
                    }
                    Err(e) if e.status == 409 => {
                        // seq_gap: drop what the relay already has, resend from there.
                        let through = e
                            .body
                            .get("accepted_through")
                            .and_then(Value::as_i64)
                            .unwrap_or(0);
                        while pending.front().is_some_and(|(s, _)| *s <= through) {
                            pending.pop_front();
                        }
                        let expected = e.body.get("expected_seq").and_then(Value::as_i64);
                        if expected.is_some_and(|x| pending.front().is_some_and(|(s, _)| *s > x)) {
                            // The relay lost events we no longer hold (a failover): it takes
                            // the next batch as a new start (section 11), so drop nothing more.
                            tracing::debug!("remote: the relay's buffer starts over");
                        }
                        tokio::time::sleep(retry).await;
                        retry = (retry * 2).min(Duration::from_secs(5));
                    }
                    Err(e) if e.ended_reason().is_some() => {
                        let _ = self
                            .inbound
                            .send(Inbound::Fatal(e.ended_reason().unwrap_or_default(), None));
                        return;
                    }
                    Err(e) if e.status == 401 => {
                        let _ = self.inbound.send(Inbound::TokenExpired);
                        tokio::time::sleep(retry).await;
                        retry = (retry * 2).min(Duration::from_secs(5));
                    }
                    Err(e) if e.status == 400 || e.status == 413 => {
                        // A bug on one side: end remote with a local notice (section 8.2).
                        let _ = self.inbound.send(Inbound::Fatal(
                            "bad_batch".into(),
                            Some(format!(
                                "remote control stopped: AMP refused an event ({e})"
                            )),
                        ));
                        return;
                    }
                    Err(_) => {
                        // Network trouble: keep everything and try again.
                        tokio::time::sleep(retry).await;
                        retry = (retry * 2).min(Duration::from_secs(5));
                    }
                }
            }
            for f in flushes.drain(..) {
                let _ = f.send(());
            }
        }
    }
}
