//! AMP in the transcript (M3-4, #2108): `amp_*` tool calls titled the way the web UI names
//! things (`AMP · complete_task #2078`) with a compact result instead of raw JSON, and the inline
//! marker drawn when the current ticket changes (§2 items 4 and 12 of
//! `research/harness-tui-theme-and-metadata.md`).
//!
//! The tool title is plain text; the ticket link is in the header, the inline marker and the
//! command panels. The block itself is [`crate::transcript::ToolBlock`] (word-wrapped, #2176).

use std::{any::Any, sync::Arc};

use amp_core::{
    meta::{AmpMeta, Ticket},
    tickets::Status,
};
use rpi_tui::{Component, visible_width};
use serde_json::Value;

use crate::{
    chrome::{clean, link, shorten},
    theme::palette,
};

/// `amp_complete_task` → `complete_task` (an `amp_amp_` harness prefix is dropped too).
pub fn short_tool(name: &str) -> &str {
    let name = name.strip_prefix("amp_amp_").unwrap_or(name);
    name.strip_prefix("amp_").unwrap_or(name)
}

pub fn is_amp_tool(name: &str) -> bool {
    name.starts_with("amp_")
}

fn id_of(v: &Value) -> Option<i64> {
    match v {
        Value::Number(n) => n.as_i64(),
        Value::String(s) => s.trim().trim_start_matches('#').parse().ok(),
        _ => None,
    }
}

/// The tickets a call names: `task_id`, else up to three of `task_ids` (`#1 #2 #3 +4`).
fn ticket_refs(args: &Value) -> String {
    if let Some(id) = id_of(&args["task_id"]) {
        return format!("#{id}");
    }
    let ids: Vec<i64> = args["task_ids"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(id_of)
        .collect();
    let mut out: Vec<String> = ids.iter().take(3).map(|id| format!("#{id}")).collect();
    if ids.len() > 3 {
        out.push(format!("+{}", ids.len() - 3));
    }
    out.join(" ")
}

/// The tool block's title: `AMP · complete_task #2078`. Plain text (see the module docs).
pub fn tool_title(name: &str, args: &Value) -> String {
    let refs = ticket_refs(args);
    let mut title = format!("AMP · {}", short_tool(name));
    if !refs.is_empty() {
        title.push(' ');
        title.push_str(&refs);
    } else if let Some(n) = args["tasks"].as_array().map(Vec::len) {
        title.push_str(&format!(" {n} ticket{}", if n == 1 { "" } else { "s" }));
    }
    clean(&title)
}

/// What follows the title in the block header: the call's most telling argument, on one line,
/// cleaned and short (empty for none).
pub fn tool_summary(args: &Value) -> String {
    const KEYS: &[&str] = &[
        "query", "path", "name", "title", "state", "reason", "answer", "body", "note",
    ];
    KEYS.iter()
        .find_map(|k| match &args[*k] {
            Value::String(s) if !s.trim().is_empty() => {
                Some(shorten(&clean(s.lines().next().unwrap_or("")), 72))
            }
            _ => None,
        })
        .unwrap_or_default()
}

/// Keys [`result_text`] never lists: the ticket's inherited metadata is noise in a one-line summary.
const SKIPPED_KEYS: &[&str] = &["effective_metadata"];

/// A compact view of an AMP tool's result: an error's text as is; a JSON object as
/// `key: value · ...` (scalars, list lengths); anything else as is. Empty values (null, `""`,
/// `[]`, `{}`) and [`SKIPPED_KEYS`] are left out, and nested records (`task: #67 Card form`) lead,
/// so the part worth reading isn't the part the header cuts.
pub fn result_text(raw: &str, is_error: bool) -> String {
    if is_error {
        return raw.to_owned();
    }
    let Ok(Value::Object(map)) = serde_json::from_str::<Value>(raw.trim()) else {
        return raw.to_owned();
    };
    let mut records = Vec::new();
    let mut parts = Vec::new();
    for (k, v) in &map {
        if SKIPPED_KEYS.contains(&k.as_str()) {
            continue;
        }
        let mut record = false;
        let text = match v {
            Value::Null => continue,
            Value::String(s) if s.is_empty() => continue,
            Value::Array(a) if a.is_empty() => continue,
            Value::Object(o) if o.is_empty() => continue,
            Value::Bool(b) => b.to_string(),
            Value::Number(n) => n.to_string(),
            Value::String(s) if s.chars().count() <= 60 => s.clone(),
            Value::String(s) => shorten(s, 60),
            Value::Array(a) => format!("{} item{}", a.len(), if a.len() == 1 { "" } else { "s" }),
            Value::Object(o) => {
                // A nested record (`task`, `project`): its id and name.
                let id = o.get("id").and_then(Value::as_i64);
                let name = o
                    .get("name")
                    .or_else(|| o.get("title"))
                    .and_then(Value::as_str);
                record = id.is_some();
                match (id, name) {
                    (Some(id), Some(n)) => format!("#{id} {}", shorten(n, 48)),
                    (Some(id), None) => format!("#{id}"),
                    _ => format!("{} field{}", o.len(), if o.len() == 1 { "" } else { "s" }),
                }
            }
        };
        let part = format!("{k}: {}", clean(&text));
        if record {
            records.push(part);
        } else {
            parts.push(part);
        }
    }
    records.append(&mut parts);
    let parts = records;
    if parts.is_empty() {
        "ok".to_owned()
    } else {
        parts.join(" · ")
    }
}

// ------------------------------------------------------------------------------------------------
// The inline ticket marker
// ------------------------------------------------------------------------------------------------

/// What the transcript marker shows.
#[derive(Debug, Clone)]
pub enum MarkerKind {
    /// The current ticket (or its state) changed.
    Ticket(Box<Ticket>),
    /// The ticket couldn't be read (`amp_get_task` failed).
    Unavailable(i64),
    /// No current ticket any more.
    Cleared,
}

/// `── ■ In progress #2108 M3-4 Slash commands ───────`: drawn in the transcript when the current
/// ticket changes (switched, claimed, or its state moved: dispatched, completed, blocked).
pub struct TicketMarker {
    kind: MarkerKind,
    /// The ticket link: (web base for [`amp_core::meta::Instance::ticket_url`], project).
    url: Option<String>,
}

impl TicketMarker {
    pub fn new(kind: MarkerKind, meta: &AmpMeta) -> Self {
        let id = match &kind {
            MarkerKind::Ticket(t) => Some(t.id),
            MarkerKind::Unavailable(id) => Some(*id),
            MarkerKind::Cleared => None,
        };
        let url = match (id, &meta.instance, meta.project_id) {
            (Some(id), Some(inst), Some(pid)) => Some(inst.ticket_url(pid, id)),
            _ => None,
        };
        Self { kind, url }
    }

    pub fn kind(&self) -> &MarkerKind {
        &self.kind
    }
}

impl Component for TicketMarker {
    fn render(&self, width: usize) -> Vec<String> {
        let c = rpi_tui::theme().colors;
        let p = palette();
        let rule = rpi_tui::theme().border_style.chars().horizontal;
        let id_text = |id: i64| {
            let t = rpi_tui::bold(&c.text.fg(&format!("#{id}")));
            match &self.url {
                Some(u) => link(u, &t),
                None => t,
            }
        };
        let (fixed, name) = match &self.kind {
            MarkerKind::Ticket(t) => (
                format!("{} {}", p.state_mark(t.status), id_text(t.id)),
                clean(&t.name),
            ),
            MarkerKind::Unavailable(id) => (
                format!("{} {}", id_text(*id), c.dim.fg("unavailable")),
                String::new(),
            ),
            MarkerKind::Cleared => (c.dim.fg("no current ticket"), String::new()),
        };
        let lead = c.border_muted.fg(&rule.repeat(2));
        let fixed_w = visible_width(&fixed);
        // `── ` + fixed + ` ` + name + ` ` + rule…
        let room = width.saturating_sub(3 + fixed_w + 1 + 3);
        let name = shorten(&name, room);
        let mut line = if 3 + fixed_w > width {
            // Too narrow for the mark: the rule alone.
            c.border_muted.fg(&rule.repeat(width))
        } else {
            format!("{lead} {fixed}")
        };
        let mut used = visible_width(&line);
        if !name.is_empty() && used < width {
            let text = format!(" {}", c.text.fg(&name));
            used += visible_width(&text);
            line.push_str(&text);
        }
        if used + 1 < width {
            line.push(' ');
            line.push_str(&c.border_muted.fg(&rule.repeat(width - used - 1)));
        }
        vec![String::new(), line]
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// The marker for the current snapshot, or `None` while the ticket is still loading.
pub fn marker_for(meta: &AmpMeta) -> Option<Option<(i64, Option<Status>)>> {
    match meta.ticket_id {
        None => Some(None),
        Some(id) => match meta.ticket.get().filter(|t| t.id == id) {
            Some(t) => Some(Some((id, Some(t.status)))),
            None if meta.ticket.error.is_some() => Some(Some((id, None))),
            None => None,
        },
    }
}

/// The marker component for `meta`'s current ticket.
pub fn marker_component(meta: &AmpMeta) -> Arc<TicketMarker> {
    let kind = match meta.ticket_id {
        None => MarkerKind::Cleared,
        Some(id) => match meta.ticket.get().filter(|t| t.id == id) {
            Some(t) => MarkerKind::Ticket(Box::new(t.clone())),
            None => MarkerKind::Unavailable(id),
        },
    };
    Arc::new(TicketMarker::new(kind, meta))
}

/// What the person sees when AMP refused a step only they take, in the web UI (#2171): drawn
/// under the refused tool call.
///
/// ```text
///   ⏸ Waiting for your approval
///     Plan approval is done by a person in the AMP web UI, not by an agent or over MCP: …
///     Approve it in AMP: https://amp.simstech.cloud/project/2/plan   (/open plan)
///     The manager stopped here; nothing else was run.
/// ```
///
/// The server's sentence is untrusted text ([`clean`]); the link is built from the instance's URL
/// helpers when AMP is connected, else the server's link is shown as plain text.
pub struct ApprovalNotice {
    refusal: amp_core::approval::Refusal,
    /// The clickable link (from [`amp_core::meta::Instance`]), and the command that opens it.
    url: Option<(String, &'static str)>,
}

impl ApprovalNotice {
    pub fn new(refusal: amp_core::approval::Refusal, meta: Option<&AmpMeta>) -> Self {
        use amp_core::approval::RefusalKind;
        let url = meta.and_then(|m| {
            let inst = m.instance.as_ref()?;
            let pid = refusal.project_id.or(m.project_id)?;
            Some(match (refusal.kind, refusal.ticket_id) {
                (RefusalKind::PlanApproval, _) => (inst.plan_url(pid), "/open plan"),
                (_, Some(t)) => (inst.ticket_url(pid, t), "/open <id>"),
                _ => (inst.project_url(pid), "/open"),
            })
        });
        Self { refusal, url }
    }

    pub fn refusal(&self) -> &amp_core::approval::Refusal {
        &self.refusal
    }

    /// The clickable link, if AMP is connected.
    pub fn url(&self) -> Option<&str> {
        self.url.as_ref().map(|(u, _)| u.as_str())
    }
}

/// Greedy word wrap of plain text to `width` cells (a word longer than a line is cut).
fn wrap_plain(text: &str, width: usize) -> Vec<String> {
    let width = width.max(1);
    let mut lines = Vec::new();
    let mut line = String::new();
    for word in text.split_whitespace() {
        let w = visible_width(word);
        let used = visible_width(&line);
        if !line.is_empty() && used + 1 + w > width {
            lines.push(std::mem::take(&mut line));
        }
        if line.is_empty() && w > width {
            lines.push(shorten(word, width));
            continue;
        }
        if !line.is_empty() {
            line.push(' ');
        }
        line.push_str(word);
    }
    if !line.is_empty() {
        lines.push(line);
    }
    lines
}

impl Component for ApprovalNotice {
    fn render(&self, width: usize) -> Vec<String> {
        use amp_core::approval::RefusalKind;
        let c = rpi_tui::theme().colors;
        let indent = "    ";
        let room = width.saturating_sub(indent.len());
        let (title, action) = match self.refusal.kind {
            RefusalKind::PlanApproval => ("⏸ Waiting for your approval", "Approve it in AMP:"),
            RefusalKind::PersonInUi => ("⏸ Waiting for you in the AMP web UI", "Do it in AMP:"),
        };
        let mut out = vec![
            String::new(),
            format!(
                "  {}",
                rpi_tui::bold(&c.warning.fg(&shorten(title, width.saturating_sub(2))))
            ),
        ];
        for line in wrap_plain(&clean(&self.refusal.message), room) {
            out.push(format!("{indent}{}", c.muted.fg(&line)));
        }
        match &self.url {
            Some((url, cmd)) => {
                let shown = shorten(url, room.saturating_sub(visible_width(action) + 1));
                let mut line = format!(
                    "{indent}{} {}",
                    c.text.fg(action),
                    link(url, &c.accent.fg(&shown))
                );
                let tail = format!("   ({cmd})");
                if visible_width(&line) + visible_width(&tail) <= width {
                    line.push_str(&c.dim.fg(&tail));
                }
                out.push(line);
            }
            None => {
                if let Some(l) = &self.refusal.link {
                    let text = format!("{action} {}", clean(l));
                    for line in wrap_plain(&text, room) {
                        out.push(format!("{indent}{}", c.text.fg(&line)));
                    }
                }
            }
        }
        for line in wrap_plain(
            "The manager stopped here; nothing else was run. Reply once it's done.",
            room,
        ) {
            out.push(format!("{indent}{}", c.dim.fg(&line)));
        }
        out
    }

    fn invalidate(&self) {}

    fn as_any(&self) -> &dyn Any {
        self
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn titles_name_the_tool_and_its_tickets() {
        assert_eq!(
            tool_title("amp_complete_task", &json!({"task_id": 2078})),
            "AMP · complete_task #2078"
        );
        assert_eq!(
            tool_title("amp_amp_get_task", &json!({"task_id": "#5"})),
            "AMP · get_task #5"
        );
        assert_eq!(
            tool_title("amp_handover_tasks", &json!({"task_ids": [1, 2, 3, 4, 5]})),
            "AMP · handover_tasks #1 #2 #3 +2"
        );
        assert_eq!(
            tool_title("amp_create_tasks", &json!({"tasks": [{}, {}]})),
            "AMP · create_tasks 2 tickets"
        );
        assert_eq!(tool_title("amp_whoami", &json!({})), "AMP · whoami");
    }

    #[test]
    fn the_summary_is_one_clean_line() {
        assert_eq!(tool_summary(&json!({"task_id": 1})), "");
        assert_eq!(
            tool_summary(&json!({"body": "line \"one\"\x1b[31m\nline two"})),
            "line \"one\"[31m"
        );
    }

    #[test]
    fn results_are_compact() {
        assert_eq!(
            result_text(
                r#"{"ok":true,"task_id":2078,"unblocked":[1,2],"task":{"id":5,"name":"x"}}"#,
                false
            ),
            "task: #5 x · ok: true · task_id: 2078 · unblocked: 2 items"
        );
        assert_eq!(result_text("task 9 not found", true), "task 9 not found");
        assert_eq!(result_text("plain text", false), "plain text");
        assert_eq!(result_text("{}", false), "ok");
        assert_eq!(
            result_text(
                r#"{"a":null,"b":"","c":[],"d":{},"effective_metadata":{"x":1}}"#,
                false
            ),
            "ok"
        );
    }

    #[test]
    fn get_task_leads_with_the_task() {
        // amp_get_task's shape: metadata first, the task second (it used to be cut off).
        let plain = r#"{"effective_metadata":{},"task":{"id":67,"project_id":2,"name":"Card form","state":"in_progress","metadata":{}}}"#;
        assert_eq!(result_text(plain, false), "task: #67 Card form");
        let with_comments = r#"{"comments":[],"comments_total":0,"effective_metadata":{"team":"web"},"task":{"id":67,"name":"Card form"}}"#;
        let text = result_text(with_comments, false);
        assert_eq!(text, "task: #67 Card form · comments_total: 0");
        assert!(!text.contains("effective_metadata") && !text.contains("comments:"));
    }
}
