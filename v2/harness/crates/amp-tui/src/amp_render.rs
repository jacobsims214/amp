//! AMP in the transcript (M3-4, #2108): `amp_*` tool calls titled the way the web UI names
//! things (`complete_task #2078`) with a one-line human summary of the result (#2183,
//! [`result_summary`]: `3 tickets: 1 ready, 1 waiting`) instead of raw JSON, and the inline
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
/// The tickets a call names: its `task_id`, else its `task_ids`.
pub fn ticket_ids(args: &Value) -> Vec<i64> {
    if let Some(id) = id_of(&args["task_id"]) {
        return vec![id];
    }
    args["task_ids"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(id_of)
        .collect()
}

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

/// The tool line's title: `complete_task #2078` (#2183: one line per call, no `AMP ·` prefix).
/// Plain text (see the module docs).
pub fn tool_title(name: &str, args: &Value) -> String {
    let refs = ticket_refs(args);
    let mut title = short_tool(name).to_owned();
    if !refs.is_empty() {
        title.push(' ');
        title.push_str(&refs);
    } else if let Some(n) = args["tasks"].as_array().map(Vec::len) {
        title.push_str(&format!(" {n} ticket{}", if n == 1 { "" } else { "s" }));
    }
    clean(&title)
}

/// The spinner line while the model is still writing a tool call's arguments (#2384): a large
/// `amp_create_tasks` streams for tens of seconds before it runs, and "Working…" alone looked
/// stuck. `json` is the arguments streamed so far (partial JSON): `Writing create_tasks 7
/// tickets · 18 KB ·` — the tool line's title as far as it can be read, and the size once it
/// passes 1 KB. The spinner adds the elapsed time after it.
pub fn streaming_line(name: &str, json: &str) -> String {
    let mut args = rpi_ai::json_parse::parse_streaming_json(Some(json));
    // The parse gives up on a key cut in half (`{"tasks": [{…}, {"na`), which is most of the
    // time: count the elements of the top-level lists as far as they have started instead.
    if let Value::Object(map) = &mut args {
        for (key, n) in started_lists(json) {
            let longer = map
                .get(&key)
                .and_then(Value::as_array)
                .is_none_or(|a| a.len() < n);
            if longer {
                map.insert(key, Value::Array(vec![Value::Null; n]));
            }
        }
    }
    let mut line = format!("Writing {}", tool_title(name, &args));
    let kb = json.len() / 1024;
    if kb > 0 {
        line.push_str(&format!(" · {kb} KB"));
    }
    // The spinner puts the elapsed time right after: `… · 5 KB · 7s`.
    line.push_str(" ·");
    line
}

/// The top-level lists of a (partial) JSON object and how many elements each has begun.
fn started_lists(json: &str) -> Vec<(String, usize)> {
    let mut lists: Vec<(String, usize)> = Vec::new();
    // The open objects and lists, outermost first.
    let mut open: Vec<char> = Vec::new();
    let (mut in_string, mut escaped) = (false, false);
    let (mut string, mut key) = (String::new(), String::new());
    // Inside a top-level list, before its next element.
    let mut expecting = false;
    for c in json.chars() {
        if in_string {
            match c {
                _ if escaped => escaped = false,
                '\\' => escaped = true,
                '"' => in_string = false,
                _ => string.push(c),
            }
            if !in_string && open.len() == 1 {
                key = std::mem::take(&mut string);
            }
            continue;
        }
        if c.is_whitespace() {
            continue;
        }
        let in_list = open.len() == 2 && open[1] == '[';
        if in_list && expecting && c != ']' {
            if let Some(last) = lists.last_mut() {
                last.1 += 1;
            }
            expecting = false;
        }
        match c {
            '"' => {
                in_string = true;
                string.clear();
            }
            '{' | '[' => {
                if open.len() == 1 && c == '[' {
                    lists.push((std::mem::take(&mut key), 0));
                    expecting = true;
                }
                open.push(c);
            }
            '}' | ']' => {
                open.pop();
                expecting = false;
            }
            ',' if in_list => expecting = true,
            _ => {}
        }
    }
    lists.retain(|(key, _)| !key.is_empty());
    lists
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
// Human summaries (#2183)
// ------------------------------------------------------------------------------------------------

/// `1 ticket`, `3 tickets`.
fn count(n: usize, one: &str, many: &str) -> String {
    format!("{n} {}", if n == 1 { one } else { many })
}

/// A bucket's size: a list's length or a number.
fn size(v: &Value) -> usize {
    match v {
        Value::Array(a) => a.len(),
        Value::Number(n) => n.as_u64().unwrap_or(0) as usize,
        _ => 0,
    }
}

/// `in progress`, `question`, `done`: a ticket's state in the web UI's words.
fn state_word(task: &Value) -> Option<String> {
    let state = task["state"].as_str()?;
    let reason = task["block_reason"].as_str().filter(|r| !r.is_empty());
    Some(Status::of(state, reason).label().to_lowercase())
}

/// `N tickets: 1 ready, 2 in progress, ...` from an `amp_list_tasks` result (compact or full).
fn list_tasks_summary(v: &Value) -> Option<String> {
    let total = v["count"].as_u64()? as usize;
    if total == 0 {
        return Some("no tickets".to_owned());
    }
    let questions = size(&v["questions"]);
    // Full format: `blocked` holds the questions too; compact: `waiting` is a number.
    let waiting = match &v["blocked"] {
        Value::Array(a) => a.len().saturating_sub(questions),
        _ => size(&v["waiting"]),
    };
    let buckets = [
        (size(&v["ready_to_dispatch"]), "ready"),
        (size(&v["in_progress"]), "in progress"),
        (questions, "question"),
        (waiting, "waiting"),
        (size(&v["scheduled"]), "scheduled"),
        (size(&v["completed"]), "done"),
    ];
    let parts: Vec<String> = buckets
        .iter()
        .filter(|(n, _)| *n > 0)
        .map(|(n, word)| match (*word, *n) {
            ("question", n) => count(n, "question", "questions"),
            (w, n) => format!("{n} {w}"),
        })
        .collect();
    let mut out = count(total, "ticket", "tickets");
    let people = size(&v["waiting_on_people"]);
    if !parts.is_empty() {
        out.push_str(": ");
        out.push_str(&parts.join(", "));
    }
    if people > 0 {
        out.push_str(&format!(" · {people} for people"));
    }
    Some(out)
}

/// `2 epics, 5 stories, 14 tickets`, or one epic by name.
fn get_plan_summary(v: &Value) -> Option<String> {
    let epics = v["epics"].as_array()?;
    let stories: usize = epics.iter().map(|e| size(&e["stories"])).sum();
    let tickets = v["tickets"].as_u64().unwrap_or(0) as usize;
    let tail = format!(
        "{}, {}",
        count(stories, "story", "stories"),
        count(tickets, "ticket", "tickets")
    );
    Some(match epics.as_slice() {
        [one] if one["name"].is_string() => {
            format!("{}: {tail}", shorten(&clean(one["name"].as_str()?), 40))
        }
        _ => format!("{}, {tail}", count(epics.len(), "epic", "epics")),
    })
}

/// `Card form · in progress · 3 comments` (the title already names `#67`).
fn get_task_summary(v: &Value) -> Option<String> {
    let task = &v["task"];
    let name = task["name"].as_str()?;
    let mut parts = vec![shorten(&clean(name), 60)];
    if let Some(state) = state_word(task) {
        parts.push(state);
    }
    if let Some(n) = v["comments_total"].as_u64().filter(|n| *n > 0) {
        parts.push(count(n as usize, "comment", "comments"));
    }
    Some(parts.join(" · "))
}

/// `3 results: architecture/auth.md, ...` (each document once).
fn kb_search_summary(v: &Value) -> Option<String> {
    let results = v["results"].as_array()?;
    if results.is_empty() {
        return Some("no results".to_owned());
    }
    let mut paths: Vec<&str> = Vec::new();
    for r in results {
        if let Some(p) = r["path"].as_str()
            && !paths.contains(&p)
        {
            paths.push(p);
        }
    }
    let mut out = count(results.len(), "result", "results");
    if !paths.is_empty() {
        out.push_str(": ");
        out.push_str(&clean(&paths.join(", ")));
    }
    Some(out)
}

/// `created 2 tickets: #141 #142`.
fn create_tasks_summary(v: &Value) -> Option<String> {
    let created = v["created"].as_array()?;
    let ids: Vec<String> = created
        .iter()
        .filter_map(|t| id_of(&t["id"]))
        .map(|id| format!("#{id}"))
        .collect();
    let mut out = format!("created {}", count(created.len(), "ticket", "tickets"));
    if !ids.is_empty() {
        let shown: Vec<&str> = ids.iter().take(6).map(String::as_str).collect();
        out.push_str(&format!(": {}", shown.join(" ")));
        if ids.len() > 6 {
            out.push_str(&format!(" +{}", ids.len() - 6));
        }
    }
    if let Some(n) = v["warnings"].as_array().map(Vec::len).filter(|n| *n > 0) {
        out.push_str(&format!(" · {}", count(n, "warning", "warnings")));
    }
    Some(out)
}

/// `plan OK · 3 ready now · 2 awaiting approval`, or `2 issues · ...`.
fn check_plan_summary(v: &Value) -> Option<String> {
    let ok = v["ok"].as_bool()?;
    let issues = size(&v["issues"]);
    let mut parts = vec![if ok && issues == 0 {
        "plan OK".to_owned()
    } else if ok {
        format!("plan OK, {}", count(issues, "note", "notes"))
    } else {
        format!("NO-GO: {}", count(issues, "issue", "issues"))
    }];
    let ready = size(&v["ready_now"]);
    if ready > 0 {
        parts.push(format!("{ready} ready now"));
    }
    let awaiting = size(&v["awaiting_approval"]);
    if awaiting > 0 {
        // #2198: per-ticket approval says which are the caller's own.
        match v["awaiting_yours"].as_array().map(Vec::len) {
            Some(yours) if v["awaiting_approval_tickets"].is_array() => {
                let others = awaiting.saturating_sub(yours);
                parts.push(match (yours, others) {
                    (_, 0) => format!("{yours} of yours awaiting approval"),
                    (0, _) => format!("{others} others' awaiting their owners' approval"),
                    _ => format!("{yours} of yours awaiting approval, {others} others'"),
                });
            }
            _ => parts.push(format!("{awaiting} awaiting approval")),
        }
    }
    Some(parts.join(" · "))
}

/// `3 artifacts: 2 approved, 1 in review`.
fn list_artifacts_summary(v: &Value) -> Option<String> {
    let list = v["artifacts"].as_array()?;
    if list.is_empty() {
        return Some("no artifacts".to_owned());
    }
    let mut by_status: Vec<(String, usize)> = Vec::new();
    for a in list {
        let status = a["status"].as_str().unwrap_or("").replace('_', " ");
        match by_status.iter_mut().find(|(s, _)| *s == status) {
            Some((_, n)) => *n += 1,
            None => by_status.push((status, 1)),
        }
    }
    let mut out = count(list.len(), "artifact", "artifacts");
    let parts: Vec<String> = by_status
        .iter()
        .filter(|(s, _)| !s.is_empty())
        .map(|(s, n)| format!("{n} {}", clean(s)))
        .collect();
    if !parts.is_empty() {
        out.push_str(": ");
        out.push_str(&parts.join(", "));
    }
    Some(out)
}

/// `#2 Name (CODE)`: a project record.
fn project_text(p: &Value) -> Option<String> {
    let name = clean(p["name"].as_str()?);
    let mut out = match id_of(&p["id"]) {
        Some(id) => format!("#{id} {}", shorten(&name, 48)),
        None => shorten(&name, 48),
    };
    if let Some(code) = p["code"].as_str().filter(|c| !c.is_empty()) {
        out.push_str(&format!(" ({})", clean(code)));
    }
    Some(out)
}

/// One line saying what an AMP tool call did, in words (#2183): `3 tickets: 1 ready, 1 waiting`,
/// `Card form · in progress`, `created 2 tickets: #141 #142`. The 15 contract tools
/// ([`amp_core::tools::CONTRACT_TOOLS`]) plus `kb_search` and `create_tasks` have their own;
/// any other tool (or a result that isn't the expected shape) falls back to [`result_text`]'s
/// `key: value` list. An error is its message's first line, without the transport's prefix.
pub fn result_summary(name: &str, raw: &str, is_error: bool) -> String {
    if is_error {
        return error_line(raw);
    }
    let fallback = || first_line(&result_text(raw, false));
    let Ok(v) = serde_json::from_str::<Value>(raw.trim()) else {
        return fallback();
    };
    if !v.is_object() {
        return fallback();
    }
    let say = |s: &str| Some(s.to_owned());
    let summary = match short_tool(name) {
        "list_tasks" => list_tasks_summary(&v),
        "get_plan" => get_plan_summary(&v),
        "get_task" => get_task_summary(&v),
        "kb_search" => kb_search_summary(&v),
        "create_tasks" => create_tasks_summary(&v),
        "check_plan" => check_plan_summary(&v),
        "list_artifacts" => list_artifacts_summary(&v),
        "whoami" => v["email"].as_str().map(|email| {
            let mut out = clean(email);
            if let Some(n) = v["orgs"].as_array().map(Vec::len).filter(|n| *n > 0) {
                out.push_str(&format!(" · {}", count(n, "org", "orgs")));
            }
            if v["global_admin"].as_bool() == Some(true) {
                out.push_str(" · admin");
            }
            out
        }),
        "get_project" => {
            project_text(&v["project"]).map(|p| match v["project"]["state"].as_str() {
                Some(s) if !s.is_empty() => format!("{p} · {}", clean(s)),
                _ => p,
            })
        }
        "list_projects" => v["projects"]
            .as_array()
            .map(|p| count(p.len(), "project", "projects")),
        "create_project" => project_text(&v).map(|p| format!("created {p}")),
        "dispatch_task" => match v["dispatched_to"].as_str().filter(|a| !a.is_empty()) {
            Some(agent) => Some(format!("dispatched to {}", clean(agent))),
            None => say("dispatched"),
        },
        // AMP #2801/#2802: the worker starting its ticket. It says so once; a second claim (or
        // one with no token) anyway started it, so it reads the same.
        "claim_task" => say(if v["already_started"].as_bool() == Some(true) {
            "started (already started)"
        } else {
            "started"
        }),
        "complete_task" => {
            let mut out = "done".to_owned();
            if v["run"].is_object() {
                out.push_str(" · run recorded");
            } else if let Some(e) = v["run_error"].as_str() {
                out.push_str(&format!(" · run not recorded: {}", clean(e)));
            }
            Some(out)
        }
        "block_task" => say("blocked: the question is on the ticket"),
        "report_run" => Some(match v["outcome"].as_str().filter(|o| !o.is_empty()) {
            Some(o) => format!("run recorded: {}", clean(o)),
            None => "run recorded".to_owned(),
        }),
        "answer_question" => Some(match v["state"].as_str() {
            Some("backlog") => "answered · ready to dispatch".to_owned(),
            Some("blocked") => "answered · still waits on dependencies".to_owned(),
            _ => "answered".to_owned(),
        }),
        "kb_write" => v["path"].as_str().map(|p| format!("wrote {}", clean(p))),
        _ => None,
    };
    summary.unwrap_or_else(fallback)
}

/// The first non-empty line of `text`, cleaned.
fn first_line(text: &str) -> String {
    clean(text.lines().find(|l| !l.trim().is_empty()).unwrap_or(""))
}

/// An error's message without the transport's wrapping (`tool error: amp_x: tools/call amp_x:
/// Mcp error: -32603: ` before AMP's own sentence).
pub fn error_text(raw: &str) -> &str {
    let mut text = raw.trim();
    if let Some(at) = text.find("Mcp error: ") {
        text = &text[at + "Mcp error: ".len()..];
        // The JSON-RPC code, `-32603: `.
        if let Some((code, rest)) = text.split_once(": ")
            && !code.is_empty()
            && code
                .trim_start_matches('-')
                .chars()
                .all(|c| c.is_ascii_digit())
        {
            text = rest;
        }
    }
    text
}

/// [`error_text`]'s first line, cleaned.
pub fn error_line(raw: &str) -> String {
    first_line(error_text(raw))
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

/// A refused call's own row: the [`ApprovalNotice`] under it carries the server's sentence.
pub use amp_core::approval::REFUSED_LINE;

/// What the person sees when AMP refused a step only they take, in the web UI (#2171): drawn
/// under the refused tool call.
///
/// ```text
///   ⏸ Waiting for your approval          (per-ticket servers, #2198: "… #2203 is yours", or
///                                         "Waiting for its owner's approval: #2203 is someone else's")
///     Plan approval is done by a person in the AMP web UI, not by an agent or over MCP: …
///     Approve it in AMP: https://amp.simstech.cloud/project/2/my-work?approve=all   (/open plan)
///     The manager stopped here; nothing else was run.
/// ```
///
/// The server's sentence is untrusted text ([`clean`]); the link is built from the instance's URL
/// helpers when AMP is connected, else the server's link is shown as plain text.
pub struct ApprovalNotice {
    refusal: amp_core::approval::Refusal,
    /// The clickable link (from [`amp_core::meta::Instance`]), and the command that opens it.
    url: Option<(String, &'static str)>,
    /// Whose approval a plan-gate refusal waits for (#2198).
    whose: Whose,
}

/// Whose approval a refused ticket waits for.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Whose {
    /// The person at the keyboard: their own ticket, or (before #2198) the whole plan.
    Yours,
    /// Someone else's ticket: its owner approves it, or a project lead on their behalf.
    Owners,
    /// Per-ticket approval, but the ticket isn't in the last plan check: its owner's.
    Unknown,
}

impl ApprovalNotice {
    pub fn new(refusal: amp_core::approval::Refusal, meta: Option<&AmpMeta>) -> Self {
        use amp_core::approval::RefusalKind;
        let whose = if !refusal.per_ticket {
            Whose::Yours
        } else {
            let plan = meta.and_then(|m| m.plan.get());
            match (plan, refusal.ticket_id) {
                (Some(plan), Some(t)) => match plan.whose(t) {
                    Some(a) if a.yours => Whose::Yours,
                    Some(_) => Whose::Owners,
                    None => Whose::Unknown,
                },
                _ => Whose::Unknown,
            }
        };
        let url = meta.and_then(|m| {
            let inst = m.instance.as_ref()?;
            let pid = refusal.project_id.or(m.project_id)?;
            Some(match (refusal.kind, refusal.ticket_id) {
                (RefusalKind::PlanApproval, _) => (
                    // The refusal's own link says which page this AMP approves on (#2495).
                    inst.approve_url(
                        pid,
                        refusal
                            .link
                            .as_deref()
                            .or_else(|| m.plan.get().and_then(|p| p.approve_url.as_deref())),
                    ),
                    "/open plan",
                ),
                (_, Some(t)) => (inst.ticket_url(pid, t), "/open <id>"),
                _ => (inst.project_url(pid), "/open"),
            })
        });
        Self {
            refusal,
            url,
            whose,
        }
    }

    /// Whose approval the refusal waits for.
    pub fn whose(&self) -> Whose {
        self.whose
    }

    pub fn refusal(&self) -> &amp_core::approval::Refusal {
        &self.refusal
    }

    /// The clickable link, if AMP is connected.
    pub fn url(&self) -> Option<&str> {
        self.url.as_ref().map(|(u, _)| u.as_str())
    }
}

/// The server's sentence without its link (#2226): the notice's own link line follows, built
/// from the instance's web URL, so a relative `/project/…` isn't shown twice. "… on their behalf):
/// /project/71/my-work?approve=all. Agents …" reads "… on their behalf). Agents …".
fn without_link(message: &str, link: Option<&str>) -> String {
    let Some(link) = link.filter(|l| !l.is_empty()) else {
        return message.to_owned();
    };
    message
        .replace(&format!(": {link}"), "")
        .replace(&format!(" {link}"), "")
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
        let ticket = self
            .refusal
            .ticket_id
            .map_or_else(|| "this ticket".to_owned(), |t| format!("#{t}"));
        let (title, action) = match (self.refusal.kind, self.whose) {
            (RefusalKind::PersonInUi, _) => (
                "⏸ Waiting for you in the AMP web UI".to_owned(),
                "Do it in AMP:",
            ),
            (RefusalKind::PlanApproval, Whose::Yours) if self.refusal.per_ticket => (
                format!("⏸ Waiting for your approval: {ticket} is yours"),
                "Approve it in AMP:",
            ),
            (RefusalKind::PlanApproval, Whose::Yours) => (
                "⏸ Waiting for your approval".to_owned(),
                "Approve it in AMP:",
            ),
            (RefusalKind::PlanApproval, Whose::Owners) => (
                format!(
                    "⏸ Waiting for its owner's approval: {ticket} is someone else's (a project \
                     lead may approve it on their behalf)"
                ),
                "Approval in AMP:",
            ),
            (RefusalKind::PlanApproval, Whose::Unknown) => (
                format!(
                    "⏸ Waiting for {ticket}'s owner to approve it (yours if it's your ticket; a \
                     project lead may approve it on their behalf)"
                ),
                "Approve it in AMP:",
            ),
        };
        let mut out = vec![String::new()];
        for line in wrap_plain(&title, width.saturating_sub(2)) {
            out.push(format!(
                "  {}",
                crate::density::strong(&c.warning.fg(&line))
            ));
        }
        let sentence = without_link(&self.refusal.message, self.refusal.link.as_deref());
        for line in wrap_plain(&clean(&sentence), room) {
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
            "complete_task #2078"
        );
        assert_eq!(
            tool_title("amp_amp_get_task", &json!({"task_id": "#5"})),
            "get_task #5"
        );
        assert_eq!(
            tool_title("amp_handover_tasks", &json!({"task_ids": [1, 2, 3, 4, 5]})),
            "handover_tasks #1 #2 #3 +2"
        );
        assert_eq!(
            tool_title("amp_create_tasks", &json!({"tasks": [{}, {}]})),
            "create_tasks 2 tickets"
        );
        assert_eq!(tool_title("amp_whoami", &json!({})), "whoami");
    }

    #[test]
    fn streaming_line_counts_the_tickets_written_so_far() {
        assert_eq!(
            streaming_line("amp_create_tasks", ""),
            "Writing create_tasks ·"
        );
        assert_eq!(
            streaming_line(
                "amp_create_tasks",
                r#"{"project_id": 2, "tasks": [{"ref": "a"}, {"ref": "b"}, {"na"#
            ),
            "Writing create_tasks 3 tickets ·"
        );
        let big = format!(r#"{{"tasks": [{{"description": "{}"}}"#, "x".repeat(3000));
        assert_eq!(
            streaming_line("amp_create_tasks", &big),
            "Writing create_tasks 1 ticket · 2 KB ·"
        );
        assert_eq!(
            streaming_line("write", r#"{"path": "a.md", "con"#),
            "Writing write ·"
        );
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
    #[test]
    fn list_tasks_reads_as_a_sentence() {
        // The compact board from the review (#2175 finding 8).
        let compact = r#"{"ready_to_dispatch":[141],"in_progress":[],"questions":[],"waiting":1,"scheduled":0,"completed":1,"quiet":[],"waiting_on_people":[],"by_epic":{"Repo hygiene":[{"id":141,"name":"x","state":"ready"}]},"count":3,"owner_filter":"mine"}"#;
        assert_eq!(
            result_summary("amp_list_tasks", compact, false),
            "3 tickets: 1 ready, 1 waiting, 1 done"
        );
        // format=full: blocked holds the questions too.
        let full = r#"{"ready_to_dispatch":[],"in_progress":[{"id":1}],"blocked":[{"id":2},{"id":3}],"scheduled":[{"id":4}],"questions":[{"id":2}],"completed":[],"waiting_on_people":[{"id":9}],"count":5,"owner_filter":"any"}"#;
        assert_eq!(
            result_summary("amp_amp_list_tasks", full, false),
            "5 tickets: 1 in progress, 1 question, 1 waiting, 1 scheduled · 1 for people"
        );
        assert_eq!(
            result_summary(
                "amp_list_tasks",
                r#"{"count":0,"owner_filter":"mine"}"#,
                false
            ),
            "no tickets"
        );
    }

    #[test]
    fn get_plan_get_task_kb_search_and_create_tasks_read_as_sentences() {
        let plan = r#"{"project_id":2,"tickets":14,"epics":[{"id":1,"name":"A","stories":[{"id":1},{"id":2}]},{"id":2,"name":"B","stories":[{"id":3}]}]}"#;
        assert_eq!(
            result_summary("amp_get_plan", plan, false),
            "2 epics, 3 stories, 14 tickets"
        );
        let one = r#"{"project_id":2,"tickets":1,"epics":[{"id":1,"name":"Harness M4","stories":[{"id":1}]}]}"#;
        assert_eq!(
            result_summary("amp_get_plan", one, false),
            "Harness M4: 1 story, 1 ticket"
        );

        let task = r#"{"comments":[{}],"comments_total":4,"effective_metadata":{},"task":{"id":67,"name":"Card form","state":"blocked","block_reason":"Stripe or Adyen?"}}"#;
        assert_eq!(
            result_summary("amp_get_task", task, false),
            "Card form · question · 4 comments"
        );
        let done =
            r#"{"effective_metadata":{},"task":{"id":67,"name":"Card form","state":"completed"}}"#;
        assert_eq!(
            result_summary("amp_get_task", done, false),
            "Card form · done"
        );

        let kb = r#"{"kb":{"scope":"project"},"count":3,"results":[{"path":"a/x.md"},{"path":"a/x.md"},{"path":"b/y.md"}]}"#;
        assert_eq!(
            result_summary("amp_kb_search", kb, false),
            "3 results: a/x.md, b/y.md"
        );
        assert_eq!(
            result_summary("amp_kb_search", r#"{"count":0,"results":[]}"#, false),
            "no results"
        );

        let created = r#"{"count":2,"created":[{"ref":"a","id":141,"state":"backlog"},{"ref":"b","id":142,"state":"blocked"}],"warnings":["x"]}"#;
        assert_eq!(
            result_summary("amp_create_tasks", created, false),
            "created 2 tickets: #141 #142 · 1 warning"
        );
    }

    #[test]
    fn the_other_contract_tools_have_summaries() {
        let cases = [
            (
                "amp_whoami",
                r#"{"user_id":7,"email":"dev@example.com","account":"human","orgs":[{},{}],"global_admin":true}"#,
                "dev@example.com · 2 orgs · admin",
            ),
            (
                "amp_get_project",
                r#"{"project":{"id":2,"name":"AMP","code":"AMP-V2","state":"active"}}"#,
                "#2 AMP (AMP-V2) · active",
            ),
            (
                "amp_list_projects",
                r#"{"projects":[{},{},{}]}"#,
                "3 projects",
            ),
            (
                "amp_create_project",
                r#"{"id":45,"name":"Play","code":"PLAY"}"#,
                "created #45 Play (PLAY)",
            ),
            (
                "amp_check_plan",
                r#"{"ok":true,"issues":[],"ready_now":[1,2],"awaiting_approval":[1,2,3],"summary":"…"}"#,
                "plan OK · 2 ready now · 3 awaiting approval",
            ),
            (
                "amp_check_plan",
                r#"{"ok":false,"issues":[{},{}],"ready_now":[],"awaiting_approval":[]}"#,
                "NO-GO: 2 issues",
            ),
            (
                "amp_dispatch_task",
                r#"{"task_id":5,"dispatched_to":"amp:amp-worker-backend"}"#,
                "dispatched to amp:amp-worker-backend",
            ),
            (
                "amp_claim_task",
                r#"{"task":{"id":5},"started_at":"2026-10-29T15:41:00Z","phase":"started","dispatch_id":"d_x","already_started":false}"#,
                "started",
            ),
            (
                "amp_claim_task",
                r#"{"task":{"id":5},"already_started":true}"#,
                "started (already started)",
            ),
            (
                "amp_complete_task",
                r#"{"task_id":5,"state":"completed","run":{"id":1}}"#,
                "done · run recorded",
            ),
            (
                "amp_complete_task",
                r#"{"task_id":5,"state":"completed"}"#,
                "done",
            ),
            (
                "amp_block_task",
                r#"{"task_id":5,"state":"blocked"}"#,
                "blocked: the question is on the ticket",
            ),
            (
                "amp_report_run",
                r#"{"id":3,"task_id":5,"outcome":"completed"}"#,
                "run recorded: completed",
            ),
            (
                "amp_answer_question",
                r#"{"task_id":5,"state":"backlog","next":"…"}"#,
                "answered · ready to dispatch",
            ),
            (
                "amp_list_artifacts",
                r#"{"artifacts":[{"status":"approved"},{"status":"in_review"},{"status":"approved"}],"count":3}"#,
                "3 artifacts: 2 approved, 1 in review",
            ),
            (
                "amp_kb_write",
                r#"{"path":"architecture/x.md","title":"X"}"#,
                "wrote architecture/x.md",
            ),
        ];
        for (tool, raw, want) in cases {
            assert_eq!(result_summary(tool, raw, false), want, "{tool}");
        }
        // #2198: per-ticket approval says which are the caller's own.
        let mine = r#"{"ok":true,"issues":[],"ready_now":[],"awaiting_approval":[1,2,3],"awaiting_approval_tickets":[{"task_id":1,"yours":true},{"task_id":2,"yours":true},{"task_id":3,"yours":false}],"awaiting_yours":[1,2]}"#;
        assert_eq!(
            result_summary("amp_check_plan", mine, false),
            "plan OK · 2 of yours awaiting approval, 1 others'"
        );
        let theirs = r#"{"ok":true,"issues":[],"awaiting_approval":[3],"awaiting_approval_tickets":[{"task_id":3,"yours":false}],"awaiting_yours":[]}"#;
        assert_eq!(
            result_summary("amp_check_plan", theirs, false),
            "plan OK · 1 others' awaiting their owners' approval"
        );
        // Every contract tool has its own summary, not the raw keys.
        for tool in amp_core::tools::CONTRACT_TOOLS {
            assert!(
                cases.iter().any(|(t, _, _)| t == tool)
                    || ["amp_list_tasks", "amp_get_plan", "amp_get_task"].contains(tool),
                "{tool} has no summary test"
            );
        }
    }

    #[test]
    fn unknown_tools_and_shapes_fall_back_to_keys_and_errors_lose_the_transport() {
        assert_eq!(
            result_summary("amp_velocity", r#"{"done":3,"per_day":1.5}"#, false),
            "done: 3 · per_day: 1.5"
        );
        // A contract tool whose result isn't the expected shape.
        assert_eq!(result_summary("amp_get_task", r#"{"x":1}"#, false), "x: 1");
        assert_eq!(
            result_summary("amp_get_task", "not json", false),
            "not json"
        );
        assert_eq!(
            result_summary(
                "amp_dispatch_task",
                "tool error: amp_dispatch_task: tools/call amp_dispatch_task: Mcp error: -32603: task 9 not found\nmore",
                true
            ),
            "task 9 not found"
        );
        assert_eq!(error_line("line one\nline two"), "line one");
        assert_eq!(error_text("Mcp error: -32603: no"), "no");
    }
}
