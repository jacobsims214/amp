//! The events waiting for the relay (#2438, protocol.md sections 6.1 and 11).
//!
//! During a relay outage the chat keeps going, so events pile up. The outbox holds at most
//! [`OutboxLimits`] of them (by default the relay's own resync buffer: 200 events, 2 MiB) and
//! drops the oldest beyond that. Events are numbered and signed only when the poster takes them
//! for a batch, so a dropped event never took a seq and the relay still sees contiguous seqs.
//! Once something was dropped, the next batch starts with a signed `notice` whose code is
//! `events_dropped` at the place of the gap, and the poster asks the link for a fresh snapshot
//! (`hello` with its status, and the follow-up queue): the page resyncs from that, the newest
//! events it still gets (the transcript tail) and what follows, instead of a replay of
//! everything the outage held.

use std::{
    collections::VecDeque,
    sync::{Arc, Mutex},
};

use serde_json::Value;

use super::envelope::{EventSigner, MAX_EVENT_BYTES};
use super::relay::now_ms;
use crate::chat_event::{ChatEvent, NoticeLevel};

/// The notice code that marks a gap in the events (section 6.4).
pub const GAP_CODE: &str = "events_dropped";

/// What an unsigned event is counted as beyond its body: the other envelope members and the
/// signature (about 400 bytes) with room to spare.
const ENVELOPE_OVERHEAD: usize = 512;

/// How much the outbox holds before it drops the oldest events.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct OutboxLimits {
    pub max_events: usize,
    pub max_bytes: usize,
}

impl Default for OutboxLimits {
    /// The relay's resync buffer (section 11): holding more would only be replayed past it.
    fn default() -> Self {
        Self {
            max_events: 200,
            max_bytes: 2 * 1024 * 1024,
        }
    }
}

/// What the outbox holds now and has held at most, for tests and diagnostics.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct OutboxStats {
    /// Events not yet accepted by the relay (signed or not), and their bytes.
    pub held_events: usize,
    pub held_bytes: usize,
    /// The most it held at once, this session.
    pub peak_events: usize,
    pub peak_bytes: usize,
    /// Events dropped this session, and the gap notices sent for them.
    pub dropped: u64,
    pub gaps: u64,
}

struct Unsigned {
    op: String,
    device_id: Option<String>,
    body: Value,
    bytes: usize,
}

/// One batch to post.
pub struct Batch {
    pub envelopes: Vec<String>,
}

pub struct Outbox {
    signer: EventSigner,
    limits: OutboxLimits,
    /// Not numbered yet: these can be dropped.
    queued: VecDeque<Unsigned>,
    queued_bytes: usize,
    /// Signed and sent (or about to be) but not accepted: these are kept until the relay has
    /// them, because the relay requires contiguous seqs.
    inflight: VecDeque<(i64, String)>,
    inflight_bytes: usize,
    /// Dropped since the last gap notice.
    dropped: u64,
    /// The seq of a gap notice the relay hasn't accepted yet.
    gap_seq: Option<i64>,
    stats: Arc<Mutex<OutboxStats>>,
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

fn seq_of(env: &str) -> i64 {
    serde_json::from_str::<Value>(env)
        .ok()
        .and_then(|v| v.get("seq").and_then(Value::as_i64))
        .unwrap_or(0)
}

impl Outbox {
    /// `stats` is reset and then kept current.
    pub fn new(signer: EventSigner, limits: OutboxLimits, stats: Arc<Mutex<OutboxStats>>) -> Self {
        *lock(&stats) = OutboxStats::default();
        Self {
            signer,
            limits: OutboxLimits {
                max_events: limits.max_events.max(1),
                max_bytes: limits.max_bytes.max(1),
            },
            queued: VecDeque::new(),
            queued_bytes: 0,
            inflight: VecDeque::new(),
            inflight_bytes: 0,
            dropped: 0,
            gap_seq: None,
            stats,
        }
    }

    /// Nothing waits for the relay.
    pub fn is_empty(&self) -> bool {
        self.queued.is_empty() && self.inflight.is_empty() && self.dropped == 0
    }

    pub fn held_events(&self) -> usize {
        self.queued.len() + self.inflight.len()
    }

    pub fn held_bytes(&self) -> usize {
        self.queued_bytes + self.inflight_bytes
    }

    /// Adds one event (already redacted). Over the limits, the oldest unsigned events go.
    pub fn push(&mut self, op: &str, device_id: Option<&str>, body: Value) {
        let bytes = serde_json::to_string(&body).map_or(0, |s| s.len()) + ENVELOPE_OVERHEAD;
        self.queued.push_back(Unsigned {
            op: op.to_owned(),
            device_id: device_id.map(str::to_owned),
            body,
            bytes,
        });
        self.queued_bytes += bytes;
        let mut dropped = 0;
        while (self.held_events() > self.limits.max_events
            || self.held_bytes() > self.limits.max_bytes)
            && let Some(old) = self.queued.pop_front()
        {
            self.queued_bytes -= old.bytes;
            dropped += 1;
        }
        self.dropped += dropped;
        self.update(|s| s.dropped += dropped);
    }

    /// The next batch (≤ `max_events`, ≤ `max_bytes` of envelopes plus separators): the
    /// unaccepted signed events first; when there are none, the next queued events are signed,
    /// after a gap notice when events were dropped.
    pub fn next_batch(&mut self, max_events: usize, max_bytes: usize) -> Batch {
        if self.inflight.is_empty() {
            let now = now_ms();
            if self.dropped > 0 {
                // The notice counts against the limits too: make room for it.
                let notice_bytes = 300 + ENVELOPE_OVERHEAD;
                while (self.held_events() + 1 > self.limits.max_events
                    || self.held_bytes() + notice_bytes > self.limits.max_bytes)
                    && let Some(old) = self.queued.pop_front()
                {
                    self.queued_bytes -= old.bytes;
                    self.dropped += 1;
                    self.update(|s| s.dropped += 1);
                }
                let n = self.dropped;
                let text = format!(
                    "{n} chat event{} from the laptop {} dropped while AMP's relay was out of \
                     reach; the transcript has a gap. Showing the latest.",
                    if n == 1 { "" } else { "s" },
                    if n == 1 { "was" } else { "were" },
                );
                let body = serde_json::to_value(ChatEvent::notice(
                    NoticeLevel::Warn,
                    Some(GAP_CODE),
                    &text,
                ))
                .unwrap_or(Value::Null);
                self.sign_into_inflight("chat_event", None, body, now);
                self.gap_seq = self.inflight.back().map(|(s, _)| *s);
                self.dropped = 0;
                self.update(|s| s.gaps += 1);
            }
            // At most half the limits go into one batch: while it waits on an outage, the
            // other half keeps the newest events (the tail the page resyncs with).
            let fill_events = max_events.min((self.limits.max_events / 2).max(1));
            let fill_bytes = max_bytes.min((self.limits.max_bytes / 2).max(1));
            let mut est = self.inflight_bytes;
            while let Some(next) = self.queued.front() {
                if self.inflight.len() >= fill_events
                    || (!self.inflight.is_empty() && est + next.bytes > fill_bytes)
                {
                    break;
                }
                let Some(u) = self.queued.pop_front() else {
                    break;
                };
                self.queued_bytes -= u.bytes;
                est += u.bytes;
                self.sign_into_inflight(&u.op, u.device_id.as_deref(), u.body, now);
            }
        }
        let mut envelopes = Vec::new();
        let mut bytes = 0;
        for (_, e) in &self.inflight {
            if envelopes.len() >= max_events
                || (!envelopes.is_empty() && bytes + e.len() > max_bytes)
            {
                break;
            }
            bytes += e.len() + 1;
            envelopes.push(e.clone());
        }
        self.update(|_| {});
        Batch { envelopes }
    }

    fn sign_into_inflight(&mut self, op: &str, device_id: Option<&str>, body: Value, now: i64) {
        match self
            .signer
            .sign_at_most(op, device_id, body, now, MAX_EVENT_BYTES)
        {
            Ok(Some(env)) => {
                self.inflight_bytes += env.len();
                self.inflight.push_back((seq_of(&env), env));
            }
            // Section 6.5: an event never exceeds 1 MiB (the feed's caps keep it far below).
            Ok(None) => tracing::warn!(op, "remote: an event over 1 MiB was dropped"),
            Err(e) => tracing::warn!(op, error = %e, "remote: an event could not be signed"),
        }
    }

    /// The relay has every event through `seq`. `true` when that took in a gap notice: the
    /// relay is reachable again, so the link sends the page a fresh snapshot now (sent earlier,
    /// during the outage, it could be dropped itself).
    pub fn accepted(&mut self, seq: i64) -> bool {
        while let Some((s, e)) = self.inflight.front() {
            if *s > seq {
                break;
            }
            self.inflight_bytes -= e.len();
            self.inflight.pop_front();
        }
        self.update(|_| {});
        let resync = self.gap_seq.is_some_and(|g| g <= seq);
        if resync {
            self.gap_seq = None;
        }
        resync
    }

    /// The seq of the oldest event the relay hasn't accepted.
    pub fn first_unaccepted(&self) -> Option<i64> {
        self.inflight.front().map(|(s, _)| *s)
    }

    fn update(&self, f: impl FnOnce(&mut OutboxStats)) {
        let mut s = lock(&self.stats);
        f(&mut s);
        s.held_events = self.held_events();
        s.held_bytes = self.held_bytes();
        s.peak_events = s.peak_events.max(s.held_events);
        s.peak_bytes = s.peak_bytes.max(s.held_bytes);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::remote::crypto::KeyPair;
    use serde_json::json;

    fn outbox(max_events: usize, max_bytes: usize) -> (Outbox, Arc<Mutex<OutboxStats>>) {
        let stats = Arc::new(Mutex::new(OutboxStats::default()));
        let signer = EventSigner::new(KeyPair::generate(), "rs_AAAAAAAAAAAAAAAAAAAAAA");
        let o = Outbox::new(
            signer,
            OutboxLimits {
                max_events,
                max_bytes,
            },
            Arc::clone(&stats),
        );
        (o, stats)
    }

    fn notice(i: usize) -> Value {
        json!({"type": "notice", "level": "info", "code": null, "text": format!("event {i}")})
    }

    fn parse(e: &str) -> Value {
        serde_json::from_str(e).unwrap()
    }

    /// The relay takes every batch: the envelopes in order, and whether a resync was asked for.
    fn drain(o: &mut Outbox) -> (Vec<Value>, bool) {
        let (mut all, mut resync) = (Vec::new(), false);
        loop {
            let b = o.next_batch(64, 2 * 1024 * 1024);
            let Some(last) = b.envelopes.last() else {
                return (all, resync);
            };
            resync |= o.accepted(parse(last)["seq"].as_i64().unwrap());
            all.extend(b.envelopes.iter().map(|e| parse(e)));
        }
    }

    /// A long outage: the first batch is in flight and never accepted, a chatty agent goes on.
    /// The outbox never holds more than its limits, by count and by bytes.
    #[test]
    fn a_long_outage_stays_under_the_cap() {
        let (mut o, stats) = outbox(50, 64 * 1024);
        for i in 0..10 {
            o.push("chat_event", None, notice(i));
        }
        let first = o.next_batch(64, 2 * 1024 * 1024);
        assert_eq!(first.envelopes.len(), 10);
        for i in 10..5000 {
            o.push("chat_event", None, notice(i));
            // The poster retries the same batch while the relay is away.
            let again = o.next_batch(64, 2 * 1024 * 1024);
            assert_eq!(
                again.envelopes, first.envelopes,
                "the batch in flight is kept"
            );
            assert!(o.held_events() <= 50, "{} events held", o.held_events());
        }
        let s = *stats.lock().unwrap();
        assert!(s.peak_events <= 50, "{s:?}");
        assert!(s.peak_bytes <= 64 * 1024, "{s:?}");
        assert_eq!(s.dropped, 5000 - 50, "{s:?}");

        // By bytes: big events fill the byte budget long before the count.
        let (mut o, stats) = outbox(1000, 64 * 1024);
        let big = "x".repeat(8 * 1024);
        for _ in 0..200 {
            o.push(
                "chat_event",
                None,
                json!({"type": "notice", "level": "info", "code": null, "text": big}),
            );
        }
        let s = *stats.lock().unwrap();
        assert!(s.peak_bytes <= 64 * 1024 && s.held_events < 10, "{s:?}");
    }

    /// After the outage the relay gets contiguous seqs: the batch it missed, a gap notice where
    /// the dropped events were, then the newest events (the tail), and a resync is asked for.
    #[test]
    fn after_the_outage_the_seqs_are_contiguous_and_the_gap_is_marked() {
        let (mut o, stats) = outbox(20, 1024 * 1024);
        for i in 0..5 {
            o.push("chat_event", None, notice(i));
        }
        let first = o.next_batch(64, 2 * 1024 * 1024);
        for i in 5..100 {
            o.push("chat_event", None, notice(i));
        }
        // The relay is back and takes the first batch.
        let last = parse(first.envelopes.last().unwrap())["seq"]
            .as_i64()
            .unwrap();
        assert!(!o.accepted(last), "no gap in the first batch");
        let (envs, resync) = drain(&mut o);
        assert!(resync, "a snapshot is asked for");
        let seqs: Vec<i64> = envs.iter().map(|e| e["seq"].as_i64().unwrap()).collect();
        assert_eq!(
            seqs,
            ((last + 1)..=(last + seqs.len() as i64)).collect::<Vec<_>>()
        );
        assert_eq!(envs[0]["body"]["code"], GAP_CODE, "{}", envs[0]);
        assert!(
            envs[0]["body"]["text"]
                .as_str()
                .unwrap()
                .starts_with("80 chat events"),
            "{}",
            envs[0]
        );
        // The tail: the newest 15 (20 held minus the 5 in flight), oldest dropped.
        let texts: Vec<&str> = envs[1..]
            .iter()
            .map(|e| e["body"]["text"].as_str().unwrap())
            .collect();
        assert_eq!(texts.first(), Some(&"event 85"));
        assert_eq!(texts.last(), Some(&"event 99"));
        assert_eq!(texts.len(), 15);
        assert!(o.is_empty());
        assert_eq!(stats.lock().unwrap().gaps, 1);
        // No more gaps: no more notices.
        o.push("chat_event", None, notice(100));
        let b = o.next_batch(64, 2 * 1024 * 1024);
        assert_eq!(b.envelopes.len(), 1);
        assert!(!o.accepted(parse(&b.envelopes[0])["seq"].as_i64().unwrap()));
    }

    /// Events dropped before anything was in flight: the gap notice itself stays within the cap.
    #[test]
    fn the_gap_notice_counts_against_the_cap() {
        let (mut o, stats) = outbox(10, 1024 * 1024);
        for i in 0..100 {
            o.push("chat_event", None, notice(i));
        }
        let first_batch = o.next_batch(64, 2 * 1024 * 1024).envelopes.len();
        assert!(
            first_batch <= 5,
            "a batch takes at most half the limits: {first_batch}"
        );
        assert!(o.held_events() <= 10);
        // The relay is back: the batch in flight goes again, then the rest.
        let (envs, resync) = drain(&mut o);
        assert!(resync);
        assert_eq!(envs.len(), 10, "the notice and the newest 9");
        assert_eq!(envs.last().unwrap()["body"]["text"], "event 99");
        let first = &envs[0];
        assert!(
            first["body"]["text"]
                .as_str()
                .unwrap()
                .starts_with("91 chat events"),
            "{first}"
        );
        let s = *stats.lock().unwrap();
        assert!(s.peak_events <= 10 && s.dropped == 91, "{s:?}");
    }

    /// An event too big for the wire takes no seq, so the next one follows on.
    #[test]
    fn an_oversized_event_leaves_no_seq_gap() {
        let (mut o, _) = outbox(10, 8 * 1024 * 1024);
        o.push("chat_event", None, notice(0));
        o.push(
            "chat_event",
            None,
            json!({"type": "notice", "level": "info", "code": null, "text": "y".repeat(MAX_EVENT_BYTES)}),
        );
        o.push("chat_event", None, notice(2));
        let b = o.next_batch(64, 4 * 1024 * 1024);
        let seqs: Vec<i64> = b
            .envelopes
            .iter()
            .map(|e| parse(e)["seq"].as_i64().unwrap())
            .collect();
        assert_eq!(seqs, vec![1, 2]);
    }
}
