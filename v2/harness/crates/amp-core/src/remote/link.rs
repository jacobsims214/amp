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
//! (section 5.6), runs the follow-up queue, builds events (hello, status, chat events from the
//! manager feed, ack, refused, pair_result, bye), redacts secrets in them, keeps the idle and
//! lifetime deadlines when the org set them (#2424: by default there are none, and the session
//! lives until a person ends it), and reconnects with backoff for as long as it takes. Events go
//! into the [`Outbox`] (#2438), which holds at most its limits during an outage and drops the
//! oldest; a poster task numbers and signs them on one seq counter (section 6.1) as it batches
//! them (~100 ms), resends what the relay hasn't accepted, and marks a gap with a notice, after
//! which the actor sends a fresh snapshot. A heartbeat task beats every 20 s.

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
use super::live::{self, LiveEntry, LiveRecord};
use super::outbox::{Outbox, OutboxLimits, OutboxStats};
use super::pairing::{
    self, PairCandidate, PairCheckInput, PairResult, PairingWindow, PinStore, StoredPin,
};
use super::pending::{self as pending_ends, PendingEnd, PendingStore};
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

/// Whether the chat is busy for remote devices: a manager reply is running, by the host's word
/// (`host.busy`) or as this link saw it start (`link_busy`, from `RunStart`/`RunEnd` and the
/// prompts it started). Running workers don't count (#2838): `workers_running` is reported
/// apart, and the queue starts the next prompt as soon as the reply ends.
pub(crate) fn reply_busy(host: &HostStatus, link_busy: bool) -> bool {
    host.busy || link_busy
}

/// The chat's status for `hello` and `status` (section 6.3).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct HostStatus {
    pub model: String,
    /// A manager reply is running: a prompt's turn, an automatic wake turn (#2891) or an approval
    /// resume. Never workers: launched workers run outside the reply (`workers_running`), and an
    /// idle manager with workers running takes a prompt at once (#2838).
    pub busy: bool,
    pub context_pct: Option<i64>,
    pub cost_usd_micros: i64,
    pub project: Option<(i64, String)>,
    pub ticket_id: Option<i64>,
    pub branch: Option<String>,
    /// Live workers, reported on their own; they never make the chat `busy` (#2838).
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
    /// The first wait before sending an end the relay didn't hear again (#2396); it doubles up
    /// to a minute.
    pub end_retry: Duration,
    /// The most events held for the relay during an outage; beyond it the oldest are dropped
    /// and the page resyncs (#2438).
    pub outbox: OutboxLimits,
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
            end_retry: Duration::from_secs(5),
            outbox: OutboxLimits::default(),
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
    /// Session ends the relay hasn't heard yet (`<state>/remote-pending-ends.json`, #2396);
    /// `None`: memory only.
    pub pending_ends: Option<PathBuf>,
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
    /// When the session's lifetime ends: `None` (the default, #2424) when it has none.
    pub expires_at_ms: Option<i64>,
    /// When it ends without remote activity: `None` when the org set no idle limit.
    pub idle_deadline_ms: Option<i64>,
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
    /// `None`: no lifetime (#2424).
    pub expires_at_ms: Option<i64>,
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
    /// Tests only: stop as a killed process would (no `bye`, no `DELETE`, the record left).
    #[cfg(feature = "mock-relay")]
    Abandon {
        done: oneshot::Sender<()>,
    },
}

struct Shared {
    deps: RemoteDeps,
    state: watch::Sender<RemoteState>,
    local_tx: mpsc::UnboundedSender<LocalEvent>,
    local_rx: Mutex<Option<mpsc::UnboundedReceiver<LocalEvent>>>,
    control: Mutex<Option<mpsc::UnboundedSender<Control>>>,
    pins: Mutex<Option<PinStore>>,
    key: Mutex<Option<KeyPair>>,
    /// Ends to send again (#2396), and one sender at a time.
    pending: Mutex<PendingStore>,
    sending_pending: tokio::sync::Mutex<()>,
    retrying: std::sync::atomic::AtomicBool,
    /// The current session's outbox, as numbers (#2438).
    outbox_stats: Arc<Mutex<OutboxStats>>,
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
        let pending = Mutex::new(PendingStore::new(deps.pending_ends.clone()));
        Self {
            shared: Arc::new(Shared {
                pending,
                sending_pending: tokio::sync::Mutex::new(()),
                retrying: std::sync::atomic::AtomicBool::new(false),
                outbox_stats: Arc::new(Mutex::new(OutboxStats::default())),
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

    /// What the current session holds for the relay, and the most it held (#2438).
    pub fn outbox_stats(&self) -> OutboxStats {
        *lock(&self.shared.outbox_stats)
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

    /// Tests only (#2424): stops the link as a killed process would. Nothing reaches the relay,
    /// which keeps the session live and offline, and the session's record stays on disk with no
    /// lock on it, as after a crash or a reboot.
    #[cfg(feature = "mock-relay")]
    pub async fn abandon_for_tests(&self) {
        let tx = lock(&self.shared.control).clone();
        if let Some(tx) = tx {
            let (done, rx) = oneshot::channel();
            if tx.send(Control::Abandon { done }).is_ok() {
                let _ = tokio::time::timeout(Duration::from_secs(10), rx).await;
            }
        }
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
        // Sessions a chat on this computer left behind when it was killed (#2424): turning
        // remote on again here ends them, so they don't stay listed forever.
        self.end_orphans();
        // Ends the relay missed earlier (#2396) go out too, now that it may be back.
        self.retry_pending_ends();
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
            expires_at_ms: reg.expires_at(),
            fingerprint: key.display_fingerprint(),
        };
        // #2424: hold this session's record while the process lives (see [`live`]).
        let record = self.live_dir().and_then(|dir| {
            live::claim(
                &dir,
                &LiveEntry {
                    v: 1,
                    api_base: deps.setup.api_base.clone(),
                    session_id: reg.session_id.clone(),
                    harness_id: key.id().to_owned(),
                    started_at_ms: reg.started_at_ms,
                },
            )
            .map_err(|e| tracing::warn!(error = %e, "remote: no live session record"))
            .ok()
        });
        let mut actor = Actor::new(
            self.clone(),
            key,
            client,
            reg,
            token,
            (window, link),
            user_id,
            ctl_rx,
        );
        actor.live = record;
        tokio::spawn(actor.run());
        Ok(enabled)
    }

    /// The harness key, loaded once (from the store, creating it if there is none).
    async fn harness_key(&self) -> Result<KeyPair, String> {
        if let Some(k) = lock(&self.shared.key).clone() {
            return Ok(k);
        }
        let store = self.shared.deps.keys.clone();
        let key = tokio::task::spawn_blocking(move || store.load_or_create())
            .await
            .map_err(|e| format!("the harness key: {e}"))??;
        *lock(&self.shared.key) = Some(key.clone());
        Ok(key)
    }

    /// Where running sessions keep their records (`<state>/remote-live/`, next to the pending
    /// ends): `None` when the link keeps its state in memory.
    fn live_dir(&self) -> Option<PathBuf> {
        let file = self.shared.deps.pending_ends.as_ref()?;
        Some(file.parent()?.join(live::LIVE_DIR))
    }

    /// Records the end of every session a dead chat on this computer left behind for this AMP
    /// server (#2424, see [`live`]). [`Self::retry_pending_ends`] sends them.
    fn end_orphans(&self) {
        let Some(dir) = self.live_dir() else {
            return;
        };
        let now = now_ms();
        for orphan in live::take_orphans(&dir, &self.shared.deps.setup.api_base) {
            let end = PendingEnd {
                api_base: orphan.api_base,
                session_id: orphan.session_id.clone(),
                harness_id: orphan.harness_id,
                reason: "harness_ended".to_owned(),
                until_ms: now + NO_LIMIT_PENDING_MS,
            };
            if let Err(e) = lock(&self.shared.pending).add(end, now) {
                tracing::warn!(error = %e, "remote: a left-behind session's end was not saved");
            }
            self.log_line("end_orphan", &orphan.session_id);
        }
    }

    fn log_line(&self, op: &str, session_id: &str) {
        if let Some(path) = &self.shared.deps.action_log {
            let line = json!({
                "at_ms": now_ms(), "session_id": session_id, "op": op,
                "device_id": null, "result": "recorded",
            });
            append_private(path, &format!("{line}\n"));
        }
    }

    /// The session ends recorded for this AMP server that the relay hasn't heard yet (#2396).
    pub fn pending_ends(&self) -> Vec<PendingEnd> {
        let api_base = &self.shared.deps.setup.api_base;
        lock(&self.shared.pending)
            .load(now_ms())
            .into_iter()
            .filter(|e| &e.api_base == api_base)
            .collect()
    }

    /// Sends every recorded end for this AMP server once (#2396): a token refresh signed with
    /// the harness key, then `DELETE`. Returns how many are still waiting for the relay. Reads no
    /// key and asks for no login when nothing is waiting.
    pub async fn send_pending_ends(&self) -> usize {
        let _one = self.shared.sending_pending.lock().await;
        let ends = self.pending_ends();
        if ends.is_empty() {
            return 0;
        }
        let deps = &self.shared.deps;
        let mut settled: Vec<String> = Vec::new();
        let left = 'send: {
            let key = match self.harness_key().await {
                Ok(k) => k,
                Err(e) => {
                    tracing::debug!(error = %e, "remote: no harness key for the pending ends");
                    break 'send ends.len();
                }
            };
            let client = match RelayClient::new(&deps.setup.api_base, deps.setup.ca_file.as_deref())
            {
                Ok(c) => c,
                Err(e) => {
                    tracing::debug!(error = %e, "remote: no relay client for the pending ends");
                    break 'send ends.len();
                }
            };
            let oauth = match deps.tokens.oauth_token().await {
                Ok(t) => t,
                Err(e) => {
                    tracing::debug!(error = %e, "remote: no login for the pending ends");
                    break 'send ends.len();
                }
            };
            deps.redactor.add_values([oauth.clone()]);
            let mut left = 0;
            for end in &ends {
                // Only the key the session was registered with can prove the end.
                let result = if end.harness_id != key.id() {
                    pending_ends::Outcome::GiveUp
                } else {
                    match client.refresh(&end.session_id, &oauth, &key).await {
                        Ok(r) => {
                            deps.redactor.add_values([r.connect_token.clone()]);
                            match client
                                .end(&end.session_id, &r.connect_token, &end.reason)
                                .await
                            {
                                Ok(()) => pending_ends::Outcome::Done,
                                Err(e) => pending_ends::outcome(&e),
                            }
                        }
                        Err(e) => pending_ends::outcome(&e),
                    }
                };
                match result {
                    pending_ends::Outcome::Retry => left += 1,
                    done => {
                        let ok = done == pending_ends::Outcome::Done;
                        if let Some(path) = &deps.action_log {
                            let line = json!({
                                "at_ms": now_ms(), "session_id": end.session_id, "op": "end_late",
                                "device_id": null, "result": if ok { "ok" } else { "gave_up" },
                            });
                            append_private(path, &format!("{line}\n"));
                        }
                        settled.push(end.session_id.clone());
                    }
                }
            }
            left
        };
        if !settled.is_empty()
            && let Err(e) = lock(&self.shared.pending).remove(&settled, now_ms())
        {
            tracing::warn!(error = %e, "remote: the pending ends file could not be written");
        }
        left
    }

    /// Sends the recorded ends in the background until the relay has heard them all (or their
    /// deadlines pass), waiting `end_retry`, doubling up to a minute, between tries. One loop
    /// at a time; the chat calls it at start, and `/remote` and a missed end call it too.
    pub fn retry_pending_ends(&self) {
        use std::sync::atomic::Ordering::SeqCst;
        if self.pending_ends().is_empty() || tokio::runtime::Handle::try_current().is_err() {
            return;
        }
        if self.shared.retrying.swap(true, SeqCst) {
            return;
        }
        let link = self.clone();
        tokio::spawn(async move {
            let mut wait = link.shared.deps.tuning.end_retry;
            loop {
                if link.send_pending_ends().await == 0 {
                    break;
                }
                tokio::time::sleep(wait).await;
                wait = (wait * 2).min(Duration::from_secs(60));
            }
            link.shared.retrying.store(false, SeqCst);
            // An end recorded while the loop was finishing.
            if !link.pending_ends().is_empty() {
                link.retry_pending_ends();
            }
        });
    }

    fn notice(&self, level: NoticeLevel, text: impl Into<String>) {
        let _ = self.shared.local_tx.send(LocalEvent::Notice {
            level,
            text: text.into(),
        });
    }
}

/// What to tell the person when the relay refuses a token refresh's proof (protocol 8.2), or
/// `None` for an answer that isn't one (a replayed nonce is a retry's, and any other failure is
/// retried quietly). `local_now_ms` is this computer's clock, `token_left_ms` how long the current
/// connect token still works.
pub(crate) fn refused_refresh_notice(
    e: &RelayError,
    local_now_ms: i64,
    token_left_ms: i64,
) -> Option<String> {
    if e.status != 400 && e.status != 403 {
        return None;
    }
    let code = e.code.as_deref()?;
    let stops = if token_left_ms > 60_000 {
        format!(
            "remote control stops when its current token runs out, in about {} min",
            token_left_ms / 60_000
        )
    } else {
        "remote control stops now".to_owned()
    };
    match code {
        "stale_ts" => {
            let off = match e.body.get("now_ms").and_then(Value::as_i64) {
                Some(relay_now) => {
                    let d = local_now_ms - relay_now;
                    let secs = d.unsigned_abs().div_ceil(1000);
                    let amount = if secs >= 120 {
                        format!("{} min", secs.div_ceil(60))
                    } else {
                        format!("{secs} s")
                    };
                    format!(
                        "this computer's clock is {amount} {} AMP's",
                        if d > 0 { "ahead of" } else { "behind" }
                    )
                }
                None => "this computer's clock is more than a minute off AMP's".to_owned(),
            };
            Some(format!(
                "Remote control can't renew its token: {off}. Set the clock automatically \
                 (System Settings > General > Date & Time); until then {stops}."
            ))
        }
        "bad_signature"
        | "wrong_harness"
        | "wrong_session"
        | "malformed"
        | "unsupported_version"
        | "unsupported_alg" => Some(format!(
            "Remote control can't renew its token: AMP refused this session's key ({code}); \
             {stops}. Run /remote again to start a new session."
        )),
        _ => None,
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
    /// The poster sent a gap notice: send a fresh snapshot (#2438).
    Resync,
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
    /// The refusal code of a token refresh the person was told about (once per code, until a
    /// refresh works again).
    told_refresh: Option<String>,
    /// Events for the relay, numbered and signed when the poster takes them (#2438).
    outbox: Arc<Mutex<Outbox>>,
    outbox_wake: Arc<tokio::sync::Notify>,
    replay: ReplayState,
    window: Option<PairingWindow>,
    window_link: Option<String>,
    pending_pair: Option<(PairCandidate, oneshot::Receiver<bool>)>,
    queue: VecDeque<Queued>,
    queued_ids: u64,
    prompts: Bucket,
    ops: Bucket,
    refused_at: std::collections::HashMap<String, i64>,
    /// A manager reply is running, as this link saw it: set when it starts a prompt and on
    /// `RunStart`, cleared on `RunEnd` (the shell's reply events; workers send none).
    busy: bool,
    feed_seen: u64,
    guard: DeltaGuard,
    viewers: u32,
    online: bool,
    started_at_ms: i64,
    /// `None`: no lifetime; `idle_timeout_ms` `None`: no idle limit (the defaults, #2424).
    expires_at_ms: Option<i64>,
    idle_timeout_ms: Option<i64>,
    last_activity_ms: i64,
    /// This session's record while the process lives (#2424).
    live: Option<LiveRecord>,
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
            outbox: Arc::new(Mutex::new(Outbox::new(
                EventSigner::new(key.clone(), reg.session_id.clone()),
                link.shared.deps.tuning.outbox,
                Arc::clone(&link.shared.outbox_stats),
            ))),
            outbox_wake: Arc::new(tokio::sync::Notify::new()),
            key,
            client,
            user_id,
            session_id: reg.session_id.clone(),
            token,
            token_expires_ms: reg.connect_token_expires_at_ms,
            refresh_after_ms: 0,
            told_login: false,
            told_refresh: None,
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
            expires_at_ms: reg.expires_at(),
            idle_timeout_ms: reg.idle_timeout_ms(),
            last_activity_ms: now,
            live: None,
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
                idle_deadline_ms: self.idle_deadline(),
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

    /// Hands one event (after redaction) to the poster, which numbers and signs it (#2438).
    fn send(&mut self, op: &str, device_id: Option<&str>, mut body: Value) {
        self.deps().redactor.redact_value(&mut body);
        lock(&self.outbox).push(op, device_id, body);
        self.outbox_wake.notify_one();
    }

    /// Events were dropped (an outage outlasted the outbox, or the feed's own gap): send what
    /// the page needs to resync, a fresh `hello` with its status and the follow-up queue. The
    /// newest events follow as they come (section 11, #2438).
    fn resync(&mut self) {
        self.send_hello();
        self.push_queue_event();
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
            "busy": reply_busy(&s, self.busy),
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
            // null: no lifetime / no idle limit (#2424).
            "expires_at_ms": self.expires_at_ms,
            "idle_timeout_s": self.idle_timeout_ms.map(|ms| ms / 1000),
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
                outbox: Arc::clone(&self.outbox),
                wake: Arc::clone(&self.outbox_wake),
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
                    #[cfg(feature = "mock-relay")]
                    Some(Control::Abandon { done }) => {
                        for t in self.tasks.drain(..) {
                            t.abort();
                        }
                        // Closing the file drops the lock; the record stays, as after a crash.
                        drop(self.live.take());
                        *lock(&self.link.shared.control) = None;
                        self.link.shared.state.send_replace(RemoteState::Off {
                            reason: Some("abandoned".into()),
                        });
                        let _ = done.send(());
                        return;
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
                // The feed dropped events before we read them: mark the gap and resync, then go
                // on from the newest event (section 11).
                self.feed_seen = gap.last;
                let body = serde_json::to_value(ChatEvent::notice(
                    NoticeLevel::Warn,
                    Some("events_dropped"),
                    "Some of the laptop's chat events were dropped; the transcript has a gap.",
                ))
                .unwrap_or(Value::Null);
                self.send("chat_event", None, body);
                self.resync();
            }
        }
    }

    /// When the session ends without remote activity, if the org set an idle limit.
    fn idle_deadline(&self) -> Option<i64> {
        self.idle_timeout_ms
            .map(|t| self.last_activity_ms.saturating_add(t))
    }

    /// `expired` or `idle` once a limit the org set has passed; never without limits (#2424).
    fn time_limit_passed(&self, now: i64) -> Option<(String, bool)> {
        if self.expires_at_ms.is_some_and(|t| now >= t) {
            return Some(("expired".into(), true));
        }
        if self.idle_deadline().is_some_and(|t| now >= t) {
            return Some(("idle".into(), true));
        }
        None
    }

    async fn on_tick(&mut self) -> Option<(String, bool)> {
        let now = now_ms();
        if let Some(end) = self.time_limit_passed(now) {
            return Some(end);
        }
        // A delta whose last word waits for more text: send it once the feed is quiet.
        // (The feed numbers a quiet delta after ~100 ms, so a word still growing arrives sooner.)
        // A tail that could start a known secret stays held (#2399, N4a).
        if self.guard.is_stale(Duration::from_millis(400)) {
            let redactor = self.deps().redactor.clone();
            if let Some((message_id, thinking, text)) = self.guard.flush_quiet(&redactor) {
                self.send_delta(&message_id, thinking, text);
            }
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
        if !self.queue.is_empty() && !reply_busy(&self.deps().host.status(), self.busy) {
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
        match self
            .client
            .refresh(&self.session_id, &oauth, &self.key)
            .await
        {
            Ok(r) => {
                self.deps()
                    .redactor
                    .add_values([oauth, r.connect_token.clone()]);
                self.token.set(&r.connect_token);
                self.token_expires_ms = r.connect_token_expires_at_ms;
                self.refresh_after_ms = 0;
                self.told_login = false;
                self.told_refresh = None;
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
                } else if session_gone(&e) {
                    // The relay doesn't know the session (a reset database): it can't come back.
                    let _ = self.inbound_tx.send(Inbound::Fatal(
                        "harness_ended".into(),
                        Some(
                            "AMP no longer has this remote session; /remote starts a new one."
                                .into(),
                        ),
                    ));
                } else if let Some(text) =
                    refused_refresh_notice(&e, now_ms(), self.token_expires_ms - now_ms())
                {
                    // The relay refused the harness key's proof (#2399, re-review #2394 N5): a
                    // clock that's off, or a key it doesn't know. Retrying won't help, and
                    // without a word remote control would just stop when the token runs out.
                    let code = e.code.clone().unwrap_or_default();
                    if self.told_refresh.as_deref() != Some(code.as_str()) {
                        self.told_refresh = Some(code);
                        self.link.notice(NoticeLevel::Warn, text.clone());
                        let body = serde_json::to_value(ChatEvent::notice(
                            NoticeLevel::Warn,
                            Some("token_refresh_refused"),
                            &text,
                        ))
                        .unwrap_or(Value::Null);
                        self.send("chat_event", None, body);
                    }
                } else if e.status == 403 && e.code.as_deref() == Some("remote_disabled") {
                    // The org switch went off (section 12.3). Another 403 (a refused proof,
                    // #2390) is retried like any other failure.
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
        // With no limits (the default, #2424) there is no deadline: it keeps trying, however long
        // the laptop slept or the relay was away.
        if let Some(end) = self.time_limit_passed(now_ms()) {
            return Some(end);
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
                // The relay's own 404 says the session is gone. A 404 or a page that isn't the
                // relay's means it isn't there right now (switched off, a rollout): a pause, so
                // keep trying (#2424).
                if err.as_ref().is_some_and(session_gone) {
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
            Inbound::Resync => self.resync(),
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
        // Deltas: hold a trailing word, or a tail a known value starts with, back so a split
        // secret is redacted whole.
        match &event {
            ChatEvent::TextDelta { message_id, delta }
            | ChatEvent::ThinkingDelta { message_id, delta } => {
                let thinking = matches!(event, ChatEvent::ThinkingDelta { .. });
                let redactor = self.deps().redactor.clone();
                let (flushed, now) = self.guard.push(message_id, thinking, delta, &redactor);
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
        // A session-only pin lasts the session: its lifetime, or the longest pin without one.
        let session_end = self
            .expires_at_ms
            .unwrap_or(now + pairing::MAX_PIN_DAYS * pairing::DAY_MS);
        let until = pairing::pin_until(now, self.pin_days, session_end);
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
        let mut late_end = false;
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
                // #2396: the relay is out of reach (or the token lapsed during an outage).
                // Remote control is off here now; the relay hears it when it's back. An idle or
                // expired session needs nothing: the relay ends it by itself.
                // With no limits (#2424) the relay never ends it by itself: keep trying for
                // NO_LIMIT_PENDING_MS.
                let until_ms = [self.expires_at_ms, self.idle_deadline()]
                    .into_iter()
                    .flatten()
                    .min()
                    .unwrap_or(now_ms() + NO_LIMIT_PENDING_MS);
                if pending_ends::outcome(&e) == pending_ends::Outcome::Retry
                    && matches!(delete_reason, "harness_ended" | "detached")
                    && until_ms > now_ms()
                {
                    let end = PendingEnd {
                        api_base: self.deps().setup.api_base.clone(),
                        session_id: self.session_id.clone(),
                        harness_id: self.key.id().to_owned(),
                        reason: delete_reason.to_owned(),
                        until_ms,
                    };
                    match lock(&self.link.shared.pending).add(end, now_ms()) {
                        Ok(()) => late_end = true,
                        Err(e) => {
                            tracing::warn!(error = %e, "remote: the pending end was not saved")
                        }
                    }
                }
            }
        }
        for t in self.tasks.drain(..) {
            t.abort();
        }
        // Ended here (or its end is a pending end now): nothing is left behind (#2424).
        if let Some(record) = self.live.take() {
            record.release();
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
        let text = end_notice(
            reason,
            self.idle_timeout_ms,
            self.expires_at_ms.map(|t| (t - self.started_at_ms).max(0)),
        );
        self.link.notice(NoticeLevel::Info, text);
        if late_end {
            self.link.notice(
                NoticeLevel::Warn,
                "AMP's relay couldn't be reached, so AMP still lists this remote session; the \
                 harness tells it the session ended once the relay is back (or the next time it \
                 starts).",
            );
        }
        self.link.shared.state.send_replace(RemoteState::Off {
            reason: Some(reason.to_owned()),
        });
        if late_end {
            self.link.retry_pending_ends();
        }
    }
}

/// How long a missed end is retried when the session has no limit (#2424): the relay never ends
/// such a session by itself, so the end is worth sending for as long as a device stays trusted.
pub const NO_LIMIT_PENDING_MS: i64 = 30 * 24 * 3600 * 1000;

/// Whether a command stream error says the session is gone for good: the relay's own `404
/// not_found`. A bare 404, or an answer that isn't the relay's, is a relay that isn't there right
/// now (#2424: the relay's kill switch pauses a session, it doesn't end it).
fn session_gone(e: &RelayError) -> bool {
    e.status == 404 && e.code.as_deref() == Some("not_found")
}

/// What the person is told when remote control ends, one sentence per end reason (protocol 12.2).
/// `idle_timeout_ms` and `lifetime_ms` are the org's limits, when it set them (#2424). Each reason
/// has its own words (#2455): losing access to the project is not a Revoke all. A reason this
/// harness doesn't know yet (a newer AMP) is named as it is.
fn end_notice(reason: &str, idle_timeout_ms: Option<i64>, lifetime_ms: Option<i64>) -> String {
    match reason {
        "idle" => format!(
            "Remote control turned off after {} without remote activity. That is the \
             organization's limit.",
            human_duration(idle_timeout_ms.unwrap_or(0))
        ),
        "expired" => format!(
            "Remote control turned off: {} since it was turned on (the organization's limit).",
            human_duration(lifetime_ms.unwrap_or(0))
        ),
        "detached" => "Remote control turned off from the paired device (detach).".to_owned(),
        "harness_ended" => "Remote control is off.".to_owned(),
        "disconnected" => "Remote control turned off from the web app (Disconnect).".to_owned(),
        "revoked" => {
            "Remote control turned off: all remote sessions were revoked in AMP.".to_owned()
        }
        "access_lost" => {
            "Remote control ended: you no longer have access to this project.".to_owned()
        }
        "deactivated" => "Remote control ended: your AMP account was deactivated.".to_owned(),
        "policy_off" => "Remote control turned off by the organization's setting.".to_owned(),
        "killed" => "Remote control ended: an AMP admin ended this session.".to_owned(),
        other => format!("Remote control turned off ({other})."),
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

/// Batches events (about every 100 ms, ≤ 64 / 2 MiB a batch) from the outbox and posts them in
/// order, keeping each one until the relay has accepted it. The outbox is shared with the actor,
/// so events keep arriving (and the oldest are dropped past its limits) while a post waits on
/// an outage (#2438).
struct Poster {
    client: RelayClient,
    session_id: String,
    token: ConnectToken,
    every: Duration,
    inbound: mpsc::UnboundedSender<Inbound>,
    outbox: Arc<Mutex<Outbox>>,
    wake: Arc<tokio::sync::Notify>,
}

const BATCH_EVENTS: usize = 64;
const BATCH_BYTES: usize = 2 * 1024 * 1024 - 64;

impl Poster {
    async fn run(self, mut rx: mpsc::UnboundedReceiver<PosterMsg>) {
        let mut flushes: Vec<oneshot::Sender<()>> = Vec::new();
        let mut retry = Duration::from_millis(250);
        loop {
            // Wait for something to send.
            if lock(&self.outbox).is_empty() {
                tokio::select! {
                    _ = self.wake.notified() => {}
                    msg = rx.recv() => match msg {
                        // The event a flush follows (`bye`) may have arrived with it.
                        Some(PosterMsg::Flush(done)) if lock(&self.outbox).is_empty() => {
                            let _ = done.send(());
                            continue;
                        }
                        Some(PosterMsg::Flush(done)) => flushes.push(done),
                        None => return,
                    },
                }
            }
            // Gather for one batch interval.
            let deadline = tokio::time::Instant::now() + self.every;
            tokio::select! {
                msg = rx.recv() => match msg {
                    Some(PosterMsg::Flush(done)) => flushes.push(done),
                    None => return,
                },
                _ = tokio::time::sleep_until(deadline) => {}
            }
            loop {
                let batch = lock(&self.outbox).next_batch(BATCH_EVENTS, BATCH_BYTES);
                if batch.envelopes.is_empty() {
                    break;
                }
                match self
                    .client
                    .post_events(&self.session_id, &self.token.get(), &batch.envelopes)
                    .await
                {
                    Ok(a) => {
                        if lock(&self.outbox).accepted(a.accepted_through) {
                            let _ = self.inbound.send(Inbound::Resync);
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
                        {
                            let mut outbox = lock(&self.outbox);
                            if outbox.accepted(through) {
                                let _ = self.inbound.send(Inbound::Resync);
                            }
                            let expected = e.body.get("expected_seq").and_then(Value::as_i64);
                            if expected
                                .is_some_and(|x| outbox.first_unaccepted().is_some_and(|s| s > x))
                            {
                                // The relay lost events we no longer hold (a failover): it takes
                                // the next batch as a new start (section 11), so drop nothing more.
                                tracing::debug!("remote: the relay's buffer starts over");
                            }
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
                    Err(e) if e.status == 429 => {
                        // Over the session's event byte rate (#2390): wait as told, keep the batch.
                        let told = e
                            .body
                            .get("retry_after_s")
                            .and_then(Value::as_u64)
                            .unwrap_or(1)
                            .clamp(1, 30);
                        tokio::time::sleep(Duration::from_secs(told).max(retry)).await;
                        retry = (retry * 2).min(Duration::from_secs(5));
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
                        // Network trouble: keep the batch and try again. New events wait in
                        // the outbox, within its limits.
                        tokio::time::sleep(retry).await;
                        retry = (retry * 2).min(Duration::from_secs(5));
                    }
                }
                // A flush asked for meanwhile is answered once the outbox is empty.
                while let Ok(PosterMsg::Flush(done)) = rx.try_recv() {
                    flushes.push(done);
                }
            }
            for f in flushes.drain(..) {
                let _ = f.send(());
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// #2838: `busy` is a running manager reply (a wake turn is one), never running workers.
    #[test]
    fn busy_is_the_managers_reply_not_its_workers() {
        let idle_with_workers = HostStatus {
            busy: false,
            workers_running: 4,
            ..HostStatus::default()
        };
        assert!(!reply_busy(&idle_with_workers, false));
        let replying = HostStatus {
            busy: true,
            ..idle_with_workers.clone()
        };
        assert!(reply_busy(&replying, false));
        // The link saw the reply (or a wake turn) start before the host's status caught up.
        assert!(reply_busy(&idle_with_workers, true));
        assert!(reply_busy(&replying, true));
        assert!(!reply_busy(&HostStatus::default(), false));
    }

    /// #2455: every end reason has its own words; losing project access isn't a Revoke all.
    #[test]
    fn end_notice_per_reason() {
        assert_eq!(
            end_notice("access_lost", None, None),
            "Remote control ended: you no longer have access to this project."
        );
        assert_eq!(
            end_notice("deactivated", None, None),
            "Remote control ended: your AMP account was deactivated."
        );
        assert_eq!(
            end_notice("revoked", None, None),
            "Remote control turned off: all remote sessions were revoked in AMP."
        );
        assert_eq!(
            end_notice("idle", Some(30 * 60_000), None),
            "Remote control turned off after 30 minutes without remote activity. That is the \
             organization's limit."
        );
        assert_eq!(
            end_notice("expired", None, Some(3_600_000)),
            "Remote control turned off: 1 hour since it was turned on (the organization's limit)."
        );
        let reasons = [
            "idle",
            "expired",
            "detached",
            "harness_ended",
            "disconnected",
            "revoked",
            "access_lost",
            "deactivated",
            "policy_off",
            "killed",
        ];
        let texts: std::collections::HashSet<String> =
            reasons.iter().map(|r| end_notice(r, None, None)).collect();
        assert_eq!(texts.len(), reasons.len(), "two reasons share a sentence");
        for r in reasons {
            assert!(
                !end_notice(r, None, None).contains(&format!("({r})")),
                "{r} falls through to the generic text"
            );
        }
        assert_eq!(
            end_notice("something_new", None, None),
            "Remote control turned off (something_new)."
        );
    }

    fn refused(status: u16, code: &str, body: Value) -> RelayError {
        RelayError {
            status,
            code: Some(code.to_owned()),
            message: String::new(),
            body: Box::new(body),
        }
    }

    /// #2399 (N5): what the person is told when a token refresh is refused, and when not.
    #[test]
    fn refused_refresh_notices() {
        let now = 1_800_000_000_000;
        let ahead = refused(400, "stale_ts", json!({"now_ms": now - 75_000}));
        let text = refused_refresh_notice(&ahead, now, 10 * 60_000).unwrap();
        assert!(text.contains("clock is 75 s ahead of AMP's"), "{text}");
        assert!(text.contains("in about 10 min"), "{text}");
        let behind = refused(400, "stale_ts", json!({"now_ms": now + 3 * 3_600_000}));
        let text = refused_refresh_notice(&behind, now, 30_000).unwrap();
        assert!(text.contains("clock is 180 min behind AMP's"), "{text}");
        assert!(text.contains("remote control stops now"), "{text}");
        let unknown = refused(400, "stale_ts", json!({}));
        assert!(
            refused_refresh_notice(&unknown, now, 0)
                .unwrap()
                .contains("more than a minute off")
        );
        for code in ["bad_signature", "wrong_harness"] {
            let status = if code == "bad_signature" { 403 } else { 400 };
            let text = refused_refresh_notice(&refused(status, code, json!({})), now, 0).unwrap();
            assert!(text.contains(code) && text.contains("/remote"), "{text}");
        }
        // A retry's own replay, policy, login and outages are handled elsewhere (or retried).
        for (status, code) in [
            (400, "replayed_nonce"),
            (403, "remote_disabled"),
            (401, "unauthenticated"),
            (503, "relay_unavailable"),
            (429, "rate_limited"),
        ] {
            assert_eq!(
                refused_refresh_notice(&refused(status, code, json!({})), now, 0),
                None,
                "{code}"
            );
        }
    }
}
