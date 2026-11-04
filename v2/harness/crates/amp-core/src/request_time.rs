//! Temporal awareness (#2250): every model request carries the current date, time and timezone,
//! so no agent has to run `date` (which the manager's policy denies) to know when it is.
//!
//! [`crate::providers::ConfiguredProvider`] calls [`stamp_messages`] on the copy of the history it
//! sends, before every request of every session (manager chat, `run -p`, workers). Each user
//! message and tool result gets one trailing text block:
//!
//! ```text
//! <current_time>Friday 2026-10-23 08:41:07 -04:00 America/New_York (2026-10-23 12:41:07 UTC)</current_time>
//! ```
//!
//! rendered from that message's own `timestamp`, i.e. when it was sent. The newest message was
//! created just before the request, so the model always sees the current time; older ones show
//! when they happened, which also gives it elapsed time.
//!
//! Why per message and not "now" on the last message only: prompt caching. Anthropic puts the
//! cache breakpoint on the last block of the last user message, and OpenAI-style providers cache
//! the longest matching prefix. A stamp that only the newest message carries would vanish from it
//! on the next request and change the prefix every turn. Rendering every message from its persisted
//! timestamp makes each message's stamp the same on every request (and after a resume), so only the
//! new tail is uncached. The system prompt stays fixed: it only explains the tag (`<amp_context>`
//! `clock:` line).
//!
//! Nothing is persisted: the session JSONL, the TUI transcript, the editor history and AMP see the
//! messages as they were written. A resumed session is stamped again from its stored timestamps,
//! so it never shows a stale "now".

use std::sync::OnceLock;

use chrono::{DateTime, Local, TimeZone, Utc};
use rpi_ai::{
    Message,
    types::{Content, UserContent},
};

/// The tag the stamp is wrapped in.
pub const TAG: &str = "current_time";

/// The `<amp_context>` line that tells the model what the stamp is.
pub const CONTEXT_LINE: &str = "clock: every user message and tool result ends with a \
<current_time> tag added by the harness: the local date, time, UTC offset and timezone when it was \
sent (the newest one is now). Use it for the current date and time instead of running `date`. The \
user does not see it; don't quote it back.";

/// The local timezone's IANA name: `$TZ` when set (it is what [`Local`] uses), else the system's.
pub fn local_zone_name() -> Option<String> {
    static NAME: OnceLock<Option<String>> = OnceLock::new();
    NAME.get_or_init(|| {
        std::env::var("TZ")
            .ok()
            .map(|tz| tz.trim_start_matches(':').trim().to_owned())
            .filter(|tz| !tz.is_empty())
            .or_else(|| iana_time_zone::get_timezone().ok())
    })
    .clone()
}

/// One stamp: `<current_time>Friday 2026-10-23 08:41:07 -04:00 America/New_York (2026-10-23
/// 12:41:07 UTC)</current_time>`. `zone_name` is left out when unknown.
pub fn stamp<Tz: TimeZone>(ts_ms: i64, zone: &Tz, zone_name: Option<&str>) -> String {
    let utc = DateTime::<Utc>::from_timestamp_millis(ts_ms).unwrap_or_default();
    let local = utc.with_timezone(zone).fixed_offset();
    let offset = *local.offset();
    let name = zone_name.map(|n| format!(" {n}")).unwrap_or_default();
    format!(
        "<{TAG}>{} {offset}{name} ({})</{TAG}>",
        local.format("%A %Y-%m-%d %H:%M:%S"),
        utc.format("%Y-%m-%d %H:%M:%S UTC"),
    )
}

/// [`stamp_messages_in`] in the local timezone.
pub fn stamp_messages(messages: &[Message], now_ms: i64) -> Vec<Message> {
    stamp_messages_in(messages, now_ms, &Local, local_zone_name().as_deref())
}

/// `messages` with a stamp appended to every user message and tool result, from the message's own
/// timestamp (`now_ms` for one without a timestamp). Assistant messages are left as they are.
pub fn stamp_messages_in<Tz: TimeZone>(
    messages: &[Message],
    now_ms: i64,
    zone: &Tz,
    zone_name: Option<&str>,
) -> Vec<Message> {
    let at = |ts: i64| stamp(if ts > 0 { ts } else { now_ms }, zone, zone_name);
    messages
        .iter()
        .map(|message| match message {
            Message::User(u) => {
                let mut u = u.clone();
                let s = at(u.timestamp);
                match &mut u.content {
                    UserContent::Text(text) => {
                        text.push_str("\n\n");
                        text.push_str(&s);
                    }
                    UserContent::Blocks(blocks) => blocks.push(Content::text(s)),
                }
                Message::User(u)
            }
            Message::ToolResult(r) => {
                let mut r = r.clone();
                let s = at(r.timestamp);
                r.content.push(Content::text(s));
                Message::ToolResult(r)
            }
            other => other.clone(),
        })
        .collect()
}

/// `text` without any stamp blocks (for echoing what a tool returned in tests and faux scripts).
pub fn strip(text: &str) -> String {
    let (open, close) = (format!("<{TAG}>"), format!("</{TAG}>"));
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    while let Some(start) = rest.find(&open) {
        let Some(len) = rest[start..].find(&close) else {
            break;
        };
        out.push_str(&rest[..start]);
        rest = &rest[start + len + close.len()..];
    }
    out.push_str(rest);
    out.trim_end().to_owned()
}

/// The inner text of the newest stamp in `text`, if any.
pub fn last_stamp(text: &str) -> Option<&str> {
    let (open, close) = (format!("<{TAG}>"), format!("</{TAG}>"));
    let start = text.rfind(&open)? + open.len();
    let len = text[start..].find(&close)?;
    Some(&text[start..start + len])
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::FixedOffset;
    use rpi_ai::types::{Api, AssistantMessage, ToolResultMessage, UserMessage};

    const T0: i64 = 1_792_759_267_000; // 2026-10-23 12:41:07 UTC

    fn user(text: &str, ts: i64) -> Message {
        Message::User(UserMessage::new(text, ts))
    }

    fn result(id: &str, ts: i64) -> Message {
        Message::ToolResult(Box::new(ToolResultMessage {
            role: Default::default(),
            tool_call_id: id.into(),
            tool_name: "bash".into(),
            content: vec![Content::text("ok")],
            details: None,
            usage: None,
            added_tool_names: Vec::new(),
            is_error: false,
            timestamp: ts,
        }))
    }

    fn assistant() -> Message {
        Message::Assistant(Box::new(AssistantMessage::empty(
            Api::OpenaiCompletions,
            "p",
            "m",
            0,
        )))
    }

    fn text_of(m: &Message) -> String {
        let blocks = match m {
            Message::User(u) => match &u.content {
                UserContent::Text(t) => return t.clone(),
                UserContent::Blocks(b) => b.clone(),
            },
            Message::ToolResult(r) => r.content.clone(),
            Message::Assistant(a) => a.content.clone(),
        };
        blocks
            .iter()
            .filter_map(|c| match c {
                Content::Text(t) => Some(t.text.clone()),
                _ => None,
            })
            .collect::<Vec<_>>()
            .join("\n")
    }

    fn edt() -> FixedOffset {
        FixedOffset::west_opt(4 * 3600).unwrap()
    }

    #[test]
    fn a_stamp_has_the_local_time_offset_zone_and_utc() {
        assert_eq!(
            stamp(T0, &edt(), Some("America/New_York")),
            "<current_time>Friday 2026-10-23 08:41:07 -04:00 America/New_York \
             (2026-10-23 12:41:07 UTC)</current_time>"
        );
        assert_eq!(
            stamp(T0, &Utc, None),
            "<current_time>Friday 2026-10-23 12:41:07 +00:00 (2026-10-23 12:41:07 UTC)</current_time>"
        );
    }

    #[test]
    fn user_messages_and_tool_results_are_stamped_assistants_are_not() {
        let history = vec![user("hi", T0), assistant(), result("c1", T0 + 60_000)];
        let out = stamp_messages_in(&history, T0 + 120_000, &edt(), Some("America/New_York"));
        assert_eq!(out.len(), 3);
        assert!(text_of(&out[0]).starts_with("hi\n\n<current_time>Friday 2026-10-23 08:41:07"));
        assert_eq!(text_of(&out[1]), "");
        assert!(text_of(&out[2]).contains("08:42:07 -04:00"));
        // The input is untouched (the session keeps what was written).
        assert_eq!(text_of(&history[0]), "hi");
    }

    #[test]
    fn two_turns_a_minute_apart_see_different_times_and_old_stamps_never_change() {
        let mut history = vec![user("first", T0)];
        let first = stamp_messages_in(&history, T0, &edt(), None);
        history.push(assistant());
        history.push(user("second", T0 + 60_000));
        let second = stamp_messages_in(&history, T0 + 60_000, &edt(), None);
        let t1 = last_stamp(&text_of(&first[0])).unwrap().to_owned();
        let t2 = last_stamp(&text_of(&second[2])).unwrap().to_owned();
        assert!(
            t1.contains("08:41:07") && t2.contains("08:42:07"),
            "{t1} / {t2}"
        );
        // Prefix stability (prompt caching): the first message renders the same both times.
        assert_eq!(text_of(&first[0]), text_of(&second[0]));
    }

    #[test]
    fn a_message_without_a_timestamp_gets_the_clock() {
        let out = stamp_messages_in(&[user("x", 0)], T0, &Utc, None);
        assert!(text_of(&out[0]).contains("12:41:07 +00:00"));
    }

    #[test]
    fn strip_and_last_stamp() {
        let s = format!("ok\n{}", stamp(T0, &Utc, None));
        assert_eq!(strip(&s), "ok");
        assert_eq!(strip("plain"), "plain");
        assert!(last_stamp(&s).unwrap().starts_with("Friday"));
        assert_eq!(last_stamp("none"), None);
    }
}
