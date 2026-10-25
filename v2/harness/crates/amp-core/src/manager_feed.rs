//! The manager's chat as an append-only, sequence-numbered feed of [`ChatEvent`]s (#2370), the
//! manager's counterpart of [`crate::worker_session::WorkerFeed`], for every client that isn't
//! the TUI itself: the remote link (#2373) and later ones.
//!
//! - **Sequence numbers** start at 1 and go up by exactly 1 per event, for the session's whole
//!   life (protocol section 6.1). An event is never replaced or renumbered once it has a number
//!   (unlike `WorkerFeed`, which replaces streaming snapshots).
//! - **Coalescing:** text and thinking deltas of one message are merged for
//!   [`FeedLimits::coalesce`] (about 100 ms) **before** they get a number, up to
//!   [`crate::chat_event::DELTA_MAX`] bytes. Any other event flushes the pending delta first, so
//!   order is kept. [`ManagerFeed::flush_due`] (driven by [`ManagerFeed::spawn_flusher`]) flushes a
//!   delta that has waited long enough while nothing else came.
//! - **Bounded:** the newest [`FeedLimits::max_events`] events within [`FeedLimits::max_bytes`] of
//!   compact JSON are kept; older ones are dropped. [`ManagerFeed::since`] says
//!   [`FeedGap`] when a reader asks for something already dropped: it then resyncs from the
//!   session file ([`crate::chat_event::session_chat_events`]).
//! - **Live:** [`ManagerFeed::subscribe_from`] returns the catch-up and a broadcast receiver under
//!   one lock, so a reader neither misses nor repeats an event between the two. A receiver that
//!   lags (`RecvError::Lagged`) catches up with `since(last seen)`.
//!
//! Inputs: rpi events through [`ManagerFeed::emitter`] (a sink of the session's
//! [`crate::events::FanOutEmitter`]) and the chat's own events (prompts, run outcomes, notices,
//! worker chips) through [`ManagerFeed::push`].

use std::{
    collections::VecDeque,
    sync::{Arc, Mutex, Weak},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

use rpi_agent::{AgentEmitter, AgentEvent};
use tokio::sync::broadcast;

use crate::chat_event::{ChatEvent, ChatMapper, DELTA_MAX};

/// How much the feed keeps and how long a delta waits.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct FeedLimits {
    pub max_events: usize,
    pub max_bytes: usize,
    pub coalesce: Duration,
    /// Live receivers' buffer; a receiver further behind gets `Lagged` and uses `since`.
    pub broadcast: usize,
}

impl Default for FeedLimits {
    fn default() -> Self {
        Self {
            max_events: 2000,
            max_bytes: 8 * 1024 * 1024,
            coalesce: Duration::from_millis(100),
            broadcast: 1024,
        }
    }
}

/// One numbered event.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FeedEntry {
    pub seq: u64,
    /// When it got its number (Unix milliseconds).
    pub at_ms: i64,
    pub event: ChatEvent,
}

/// The reader asked for events the feed has already dropped.
#[derive(Debug, Clone, Copy, PartialEq, Eq, thiserror::Error)]
#[error("events after seq {seen} were dropped: the feed starts at seq {oldest} (last {last})")]
pub struct FeedGap {
    pub seen: u64,
    /// The oldest seq still held.
    pub oldest: u64,
    pub last: u64,
}

#[derive(Debug)]
struct Pending {
    thinking: bool,
    message_id: String,
    delta: String,
    since: Instant,
}

#[derive(Debug, Default)]
struct State {
    entries: VecDeque<(FeedEntry, usize)>,
    bytes: usize,
    last: u64,
    pending: Option<Pending>,
    mapper: ChatMapper,
}

/// The manager's chat feed. Share it as `Arc<ManagerFeed>`.
#[derive(Debug)]
pub struct ManagerFeed {
    state: Mutex<State>,
    tx: broadcast::Sender<FeedEntry>,
    limits: FeedLimits,
}

impl ManagerFeed {
    pub fn new() -> Arc<Self> {
        Self::with_limits(FeedLimits::default())
    }

    pub fn with_limits(limits: FeedLimits) -> Arc<Self> {
        let (tx, _) = broadcast::channel(limits.broadcast.max(1));
        Arc::new(Self {
            state: Mutex::new(State {
                mapper: ChatMapper::live(),
                ..State::default()
            }),
            tx,
            limits,
        })
    }

    pub fn limits(&self) -> FeedLimits {
        self.limits
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Adds one chat event (a delta may wait to be merged with the next one).
    pub fn push(&self, event: ChatEvent) {
        let mut s = self.lock();
        self.push_locked(&mut s, event, Instant::now());
    }

    /// Maps one rpi event and adds what it shows.
    pub fn push_agent(&self, event: &AgentEvent) {
        let mut s = self.lock();
        let now = Instant::now();
        for e in s.mapper.map(event) {
            self.push_locked(&mut s, e, now);
        }
    }

    fn push_locked(&self, s: &mut State, event: ChatEvent, now: Instant) {
        let (thinking, message_id, delta) = match event {
            ChatEvent::TextDelta { message_id, delta } => (false, message_id, delta),
            ChatEvent::ThinkingDelta { message_id, delta } => (true, message_id, delta),
            other => {
                self.flush_locked(s);
                self.append(s, other);
                return;
            }
        };
        if let Some(p) = &mut s.pending
            && p.thinking == thinking
            && p.message_id == message_id
            && p.delta.len() + delta.len() <= DELTA_MAX
            && now.duration_since(p.since) < self.limits.coalesce
        {
            p.delta.push_str(&delta);
            return;
        }
        self.flush_locked(s);
        s.pending = Some(Pending {
            thinking,
            message_id,
            delta,
            since: now,
        });
    }

    fn flush_locked(&self, s: &mut State) {
        if let Some(p) = s.pending.take() {
            let event = if p.thinking {
                ChatEvent::ThinkingDelta {
                    message_id: p.message_id,
                    delta: p.delta,
                }
            } else {
                ChatEvent::TextDelta {
                    message_id: p.message_id,
                    delta: p.delta,
                }
            };
            self.append(s, event);
        }
    }

    fn append(&self, s: &mut State, event: ChatEvent) {
        s.last += 1;
        let entry = FeedEntry {
            seq: s.last,
            at_ms: now_ms(),
            event,
        };
        let size = serde_json::to_string(&entry.event).map_or(0, |j| j.len());
        // Sent under the lock, so receivers see seqs in order; `send` never blocks.
        let _ = self.tx.send(entry.clone());
        s.entries.push_back((entry, size));
        s.bytes += size;
        while s.entries.len() > 1
            && (s.entries.len() > self.limits.max_events || s.bytes > self.limits.max_bytes)
        {
            if let Some((_, n)) = s.entries.pop_front() {
                s.bytes -= n;
            }
        }
    }

    /// Numbers the pending delta now.
    pub fn flush(&self) {
        let mut s = self.lock();
        self.flush_locked(&mut s);
    }

    /// Numbers the pending delta if it has waited [`FeedLimits::coalesce`]; whether it did.
    pub fn flush_due(&self, now: Instant) -> bool {
        let mut s = self.lock();
        let due = s
            .pending
            .as_ref()
            .is_some_and(|p| now.duration_since(p.since) >= self.limits.coalesce);
        if due {
            self.flush_locked(&mut s);
        }
        due
    }

    /// A task that calls [`Self::flush_due`] every [`FeedLimits::coalesce`] until the feed is
    /// dropped. Needs a tokio runtime.
    pub fn spawn_flusher(self: &Arc<Self>) -> tokio::task::JoinHandle<()> {
        let feed: Weak<Self> = Arc::downgrade(self);
        let every = self.limits.coalesce.max(Duration::from_millis(10));
        tokio::spawn(async move {
            let mut tick = tokio::time::interval(every);
            tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                tick.tick().await;
                match feed.upgrade() {
                    Some(f) => {
                        f.flush_due(Instant::now());
                    }
                    None => break,
                }
            }
        })
    }

    /// The last seq given out (0: none yet).
    pub fn last_seq(&self) -> u64 {
        self.lock().last
    }

    /// The oldest seq still held, if any.
    pub fn oldest_seq(&self) -> Option<u64> {
        self.lock().entries.front().map(|(e, _)| e.seq)
    }

    /// How many events the feed holds.
    pub fn len(&self) -> usize {
        self.lock().entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    fn since_locked(s: &State, seen: u64) -> Result<Vec<FeedEntry>, FeedGap> {
        let oldest = s.entries.front().map_or(s.last + 1, |(e, _)| e.seq);
        if seen < s.last && seen + 1 < oldest {
            return Err(FeedGap {
                seen,
                oldest,
                last: s.last,
            });
        }
        Ok(s.entries
            .iter()
            .filter(|(e, _)| e.seq > seen)
            .map(|(e, _)| e.clone())
            .collect())
    }

    /// The events after `seen` (0: from the start). `Err` when some of them were dropped.
    pub fn since(&self, seen: u64) -> Result<Vec<FeedEntry>, FeedGap> {
        Self::since_locked(&self.lock(), seen)
    }

    /// Live events from now on (a pending delta arrives once it is numbered).
    pub fn subscribe(&self) -> broadcast::Receiver<FeedEntry> {
        self.tx.subscribe()
    }

    /// [`Self::since`]`(seen)` and a receiver of every later event, taken together: nothing is
    /// missed or repeated between them.
    pub fn subscribe_from(
        &self,
        seen: u64,
    ) -> (
        Result<Vec<FeedEntry>, FeedGap>,
        broadcast::Receiver<FeedEntry>,
    ) {
        let s = self.lock();
        let rx = self.tx.subscribe();
        (Self::since_locked(&s, seen), rx)
    }

    /// An emitter that feeds rpi's events in (a sink of the session's fan-out emitter).
    pub fn emitter(self: &Arc<Self>) -> Arc<dyn AgentEmitter> {
        Arc::new(FeedEmitter(Arc::clone(self)))
    }
}

struct FeedEmitter(Arc<ManagerFeed>);

impl AgentEmitter for FeedEmitter {
    fn emit(
        &self,
        event: AgentEvent,
    ) -> std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send + 'static>> {
        self.0.push_agent(&event);
        Box::pin(std::future::ready(()))
    }

    fn try_emit(&self, event: AgentEvent) {
        self.0.push_agent(&event);
    }
}

fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| i64::try_from(d.as_millis()).unwrap_or(i64::MAX))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::chat_event::{NoticeLevel, PromptOrigin};

    fn text(id: &str, d: &str) -> ChatEvent {
        ChatEvent::TextDelta {
            message_id: id.into(),
            delta: d.into(),
        }
    }

    fn notice(t: &str) -> ChatEvent {
        ChatEvent::notice(NoticeLevel::Info, None, t)
    }

    fn slow() -> FeedLimits {
        FeedLimits {
            coalesce: Duration::from_secs(3600),
            ..FeedLimits::default()
        }
    }

    #[test]
    fn seqs_are_contiguous_and_replay_from_any_seq() {
        let feed = ManagerFeed::new();
        assert_eq!(feed.since(0).unwrap(), vec![]);
        feed.push(ChatEvent::user_message("p_1", "hi", &PromptOrigin::Local));
        feed.push(notice("a"));
        feed.push(notice("b"));
        let all = feed.since(0).unwrap();
        assert_eq!(
            all.iter().map(|e| e.seq).collect::<Vec<_>>(),
            [1, 2, 3],
            "starts at 1, +1 each"
        );
        let tail = feed.since(1).unwrap();
        assert_eq!(tail.len(), 2);
        assert_eq!(tail[0].event, notice("a"));
        assert!(feed.since(3).unwrap().is_empty());
        assert!(feed.since(99).unwrap().is_empty());
        assert_eq!(feed.last_seq(), 3);
    }

    #[test]
    fn deltas_coalesce_before_they_get_a_seq() {
        let feed = ManagerFeed::with_limits(slow());
        feed.push(text("msg_1", "Run"));
        feed.push(text("msg_1", "ning"));
        assert_eq!(feed.last_seq(), 0, "still pending");
        feed.push(ChatEvent::ThinkingDelta {
            message_id: "msg_1".into(),
            delta: "t".into(),
        });
        feed.push(notice("n"));
        let all = feed.since(0).unwrap();
        assert_eq!(all[0].event, text("msg_1", "Running"));
        assert!(matches!(all[1].event, ChatEvent::ThinkingDelta { .. }));
        assert_eq!(all[2].event, notice("n"));
        assert_eq!(all.len(), 3);

        // Another message's delta and the size cap both start a new event.
        feed.push(text("msg_2", "a"));
        feed.push(text("msg_3", "b"));
        feed.push(text("msg_3", &"c".repeat(DELTA_MAX)));
        feed.flush();
        let tail: Vec<ChatEvent> = feed
            .since(3)
            .unwrap()
            .into_iter()
            .map(|e| e.event)
            .collect();
        assert_eq!(tail.len(), 3);
        assert_eq!(tail[0], text("msg_2", "a"));
        assert_eq!(tail[1], text("msg_3", "b"));
    }

    #[test]
    fn a_waiting_delta_is_flushed_when_due() {
        let feed = ManagerFeed::with_limits(FeedLimits {
            coalesce: Duration::from_millis(100),
            ..FeedLimits::default()
        });
        feed.push(text("msg_1", "a"));
        assert!(!feed.flush_due(Instant::now()));
        assert!(feed.flush_due(Instant::now() + Duration::from_millis(150)));
        assert_eq!(feed.last_seq(), 1);
        assert!(!feed.flush_due(Instant::now() + Duration::from_secs(1)));
    }

    #[test]
    fn the_feed_is_bounded_and_reports_a_gap() {
        let feed = ManagerFeed::with_limits(FeedLimits {
            max_events: 3,
            ..FeedLimits::default()
        });
        for i in 0..5 {
            feed.push(notice(&i.to_string()));
        }
        assert_eq!(feed.len(), 3);
        assert_eq!(feed.oldest_seq(), Some(3));
        assert_eq!(feed.since(2).unwrap().len(), 3, "seq 3 onwards is all held");
        assert_eq!(
            feed.since(1),
            Err(FeedGap {
                seen: 1,
                oldest: 3,
                last: 5
            })
        );
        assert!(feed.since(5).unwrap().is_empty());

        let by_bytes = ManagerFeed::with_limits(FeedLimits {
            max_bytes: 200,
            ..FeedLimits::default()
        });
        for _ in 0..10 {
            by_bytes.push(notice(&"x".repeat(60)));
        }
        assert!(by_bytes.len() < 10);
        assert!(!by_bytes.is_empty());
    }

    #[tokio::test]
    async fn subscribe_from_misses_and_repeats_nothing() {
        let feed = ManagerFeed::new();
        feed.push(notice("1"));
        feed.push(notice("2"));
        let (catch_up, mut rx) = feed.subscribe_from(1);
        feed.push(notice("3"));
        let catch_up = catch_up.unwrap();
        assert_eq!(catch_up.iter().map(|e| e.seq).collect::<Vec<_>>(), [2]);
        assert_eq!(rx.recv().await.unwrap().seq, 3);
        assert!(rx.try_recv().is_err());
    }

    #[tokio::test]
    async fn rpi_events_reach_the_feed_through_its_emitter() {
        use rpi_agent::AgentToolResult;
        let feed = ManagerFeed::new();
        let emitter = feed.emitter();
        emitter
            .emit(AgentEvent::ToolExecutionEnd {
                tool_call_id: "c".into(),
                tool_name: "bash".into(),
                result: AgentToolResult::text("ok"),
                is_error: false,
            })
            .await;
        emitter.try_emit(AgentEvent::TurnStart);
        let all = feed.since(0).unwrap();
        assert_eq!(all.len(), 1);
        assert_eq!(all[0].event.type_tag(), "tool_end");
    }

    #[tokio::test]
    async fn the_flusher_numbers_a_quiet_delta_and_stops_with_the_feed() {
        let feed = ManagerFeed::with_limits(FeedLimits {
            coalesce: Duration::from_millis(20),
            ..FeedLimits::default()
        });
        let task = feed.spawn_flusher();
        feed.push(text("msg_1", "a"));
        let deadline = Instant::now() + Duration::from_secs(5);
        while feed.last_seq() == 0 && Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert_eq!(feed.last_seq(), 1);
        drop(feed);
        let _ = tokio::time::timeout(Duration::from_secs(5), task).await;
    }
}
