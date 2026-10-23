//! Tickets as the harness shows them: AMP's task records plus the web UI's state words.
//!
//! AMP stores four states (`backlog`, `in_progress`, `blocked`, `completed`), but the web UI shows
//! six, because "blocked" means three different things (`v2/ui/src/lib/ticketState.ts`):
//!
//! | status        | label        | AMP state                                           |
//! |---------------|--------------|-----------------------------------------------------|
//! | `ready`       | Ready        | backlog                                             |
//! | `in_progress` | In progress  | in_progress                                         |
//! | `waiting`     | Waiting      | blocked, no reason (unfinished dependencies)        |
//! | `question`    | Question     | blocked with a reason (an agent asked a person)     |
//! | `scheduled`   | Scheduled    | blocked with reason `scheduled:…` (or `scheduled`)  |
//! | `done`        | Done         | completed                                           |
//!
//! The CLI, `--json` output and (M3) the TUI use these so a ticket reads the same everywhere.

use std::{fmt, str::FromStr};

use serde::{Deserialize, Serialize};

/// The web UI's display state.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Status {
    // Declaration order is the CLI's listing order: what needs a person first.
    Question,
    InProgress,
    Ready,
    Scheduled,
    Waiting,
    Done,
}

impl Status {
    pub const ALL: [Status; 6] = [
        Self::Question,
        Self::InProgress,
        Self::Ready,
        Self::Scheduled,
        Self::Waiting,
        Self::Done,
    ];

    /// Derives the display state like the web UI's `displayState`.
    pub fn of(state: &str, block_reason: Option<&str>) -> Self {
        match state {
            "completed" => Self::Done,
            "in_progress" => Self::InProgress,
            "scheduled" => Self::Scheduled,
            "blocked" => match block_reason.unwrap_or("") {
                r if r.starts_with("scheduled:") => Self::Scheduled,
                "" => Self::Waiting,
                _ => Self::Question,
            },
            _ => Self::Ready,
        }
    }

    /// The snake_case word used in `--json` and `--state`.
    pub fn word(self) -> &'static str {
        match self {
            Self::Ready => "ready",
            Self::InProgress => "in_progress",
            Self::Waiting => "waiting",
            Self::Question => "question",
            Self::Scheduled => "scheduled",
            Self::Done => "done",
        }
    }

    /// The web UI's label.
    pub fn label(self) -> &'static str {
        match self {
            Self::Ready => "Ready",
            Self::InProgress => "In progress",
            Self::Waiting => "Waiting",
            Self::Question => "Question",
            Self::Scheduled => "Scheduled",
            Self::Done => "Done",
        }
    }
}

impl fmt::Display for Status {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.label())
    }
}

/// A `--state` filter: one display state, or an AMP stored state that covers several.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StatusFilter(Vec<Status>);

impl StatusFilter {
    pub fn matches(&self, status: Status) -> bool {
        self.0.contains(&status)
    }

    pub fn includes_done(&self) -> bool {
        self.matches(Status::Done)
    }
}

impl FromStr for StatusFilter {
    type Err = String;

    /// Accepts the UI words and labels (`question`, `In progress`, `in-progress`) and AMP's stored
    /// states (`backlog`, `blocked`, `completed`). Comma-separated values combine.
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let mut out = Vec::new();
        for part in s.split(',').map(str::trim).filter(|p| !p.is_empty()) {
            let key = part.to_ascii_lowercase().replace(['-', ' '], "_");
            let add: &[Status] = match key.as_str() {
                "ready" | "backlog" => &[Status::Ready],
                "in_progress" | "prog" | "running" => &[Status::InProgress],
                "waiting" | "wait" => &[Status::Waiting],
                "question" | "questions" | "flag" => &[Status::Question],
                "scheduled" | "sched" => &[Status::Scheduled],
                "done" | "completed" => &[Status::Done],
                "blocked" => &[Status::Waiting, Status::Question, Status::Scheduled],
                "open" => &[
                    Status::Question,
                    Status::InProgress,
                    Status::Ready,
                    Status::Scheduled,
                    Status::Waiting,
                ],
                "all" => &Status::ALL,
                _ => {
                    return Err(format!(
                        "unknown state {part:?} (use ready, in_progress, waiting, question, \
                         scheduled, done, blocked, open or all)"
                    ));
                }
            };
            for status in add {
                if !out.contains(status) {
                    out.push(*status);
                }
            }
        }
        if out.is_empty() {
            return Err("empty state filter".to_owned());
        }
        Ok(Self(out))
    }
}

/// An AMP task record as `amp_list_tasks` (format `full`) and `amp_get_task` return it. Every field
/// but `id` and `name` has a default, so a newer or older server's record still parses.
#[derive(Debug, Clone, Deserialize)]
pub struct AmpTask {
    pub id: i64,
    #[serde(default)]
    pub project_id: Option<i64>,
    #[serde(default)]
    pub epic_id: Option<i64>,
    #[serde(default)]
    pub story_id: Option<i64>,
    pub name: String,
    #[serde(default, deserialize_with = "null_as_default")]
    pub description: String,
    #[serde(default, deserialize_with = "null_as_default")]
    pub acceptance_criteria: String,
    #[serde(default, deserialize_with = "null_as_default")]
    pub state: String,
    #[serde(default, deserialize_with = "null_as_default")]
    pub priority: String,
    #[serde(default, deserialize_with = "null_as_default")]
    pub dependency_ids: Vec<i64>,
    #[serde(default, deserialize_with = "null_as_default")]
    pub blocked_by_ids: Vec<i64>,
    #[serde(default)]
    pub assigned_to: Option<String>,
    #[serde(default)]
    pub block_reason: Option<String>,
    #[serde(default)]
    pub start_at: Option<String>,
    #[serde(default)]
    pub owner_user_id: Option<i64>,
    #[serde(default)]
    pub performer: Option<String>,
    #[serde(default)]
    pub dispatched_at: Option<String>,
    #[serde(default)]
    pub completed_at: Option<String>,
    #[serde(default)]
    pub created_at: Option<String>,
    #[serde(default)]
    pub updated_at: Option<String>,
}

/// A human ticket from `amp_list_tasks`' `waiting_on_people` (AMP lists open human tickets there
/// rather than in the state buckets).
#[derive(Debug, Clone, Deserialize)]
pub struct PersonWait {
    pub id: i64,
    pub name: String,
    #[serde(default)]
    pub owner: Option<String>,
    /// `ready` or `in_progress`.
    #[serde(default, deserialize_with = "null_as_default")]
    pub state: String,
}

/// One row of `tickets list`, and the summary part of `tickets get`. This is the `--json` contract:
/// add fields, never rename or remove them.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct TicketRow {
    pub id: i64,
    pub name: String,
    /// The web UI's state word.
    pub status: Status,
    /// The web UI's label for `status`.
    pub status_label: &'static str,
    /// AMP's stored state (`backlog`, `in_progress`, `blocked`, `completed`).
    pub state: String,
    pub priority: Option<String>,
    /// `agent` or `human`.
    pub performer: Option<String>,
    /// The agent type for agent tickets.
    pub assigned_to: Option<String>,
    pub owner_user_id: Option<i64>,
    /// The owner's email; AMP only gives it for human tickets in a list.
    pub owner: Option<String>,
    pub epic_id: Option<i64>,
    pub story_id: Option<i64>,
    /// Unfinished dependencies (waiting tickets).
    pub blocked_by_ids: Vec<i64>,
    /// The question for a person (status `question`).
    pub question: Option<String>,
    /// When a scheduled ticket may start.
    pub start_at: Option<String>,
}

fn non_empty(s: &str) -> Option<String> {
    let s = s.trim();
    (!s.is_empty()).then(|| s.to_owned())
}

impl From<&AmpTask> for TicketRow {
    fn from(t: &AmpTask) -> Self {
        let status = Status::of(&t.state, t.block_reason.as_deref());
        Self {
            id: t.id,
            name: t.name.clone(),
            status,
            status_label: status.label(),
            state: t.state.clone(),
            priority: non_empty(&t.priority),
            performer: t.performer.as_deref().and_then(non_empty),
            assigned_to: t.assigned_to.as_deref().and_then(non_empty),
            owner_user_id: t.owner_user_id,
            owner: None,
            epic_id: t.epic_id,
            story_id: t.story_id,
            blocked_by_ids: t.blocked_by_ids.clone(),
            question: match status {
                Status::Question => t.block_reason.as_deref().and_then(non_empty),
                _ => None,
            },
            start_at: match status {
                Status::Scheduled => t.start_at.clone(),
                _ => None,
            },
        }
    }
}

impl From<&PersonWait> for TicketRow {
    fn from(p: &PersonWait) -> Self {
        let (status, state) = match p.state.as_str() {
            "in_progress" => (Status::InProgress, "in_progress"),
            _ => (Status::Ready, "backlog"),
        };
        Self {
            id: p.id,
            name: p.name.clone(),
            status,
            status_label: status.label(),
            state: state.to_owned(),
            priority: None,
            performer: Some("human".to_owned()),
            assigned_to: None,
            owner_user_id: None,
            owner: p.owner.as_deref().and_then(non_empty),
            epic_id: None,
            story_id: None,
            blocked_by_ids: Vec::new(),
            question: None,
            start_at: None,
        }
    }
}

/// Go marshals a nil slice as `null`; read that (and any other `null`) as the default.
fn null_as_default<'de, D, T>(deserializer: D) -> Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Ok(Option::<T>::deserialize(deserializer)?.unwrap_or_default())
}

/// The buckets of `amp_list_tasks` with `format: "full"`.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct FullTaskList {
    #[serde(default, deserialize_with = "null_as_default")]
    pub ready_to_dispatch: Vec<AmpTask>,
    #[serde(default, deserialize_with = "null_as_default")]
    pub in_progress: Vec<AmpTask>,
    /// Waiting and question tickets (questions are listed again under `questions`).
    #[serde(default, deserialize_with = "null_as_default")]
    pub blocked: Vec<AmpTask>,
    #[serde(default, deserialize_with = "null_as_default")]
    pub scheduled: Vec<AmpTask>,
    #[serde(default, deserialize_with = "null_as_default")]
    pub completed: Vec<AmpTask>,
    #[serde(default, deserialize_with = "null_as_default")]
    pub waiting_on_people: Vec<PersonWait>,
    #[serde(default)]
    pub owner_filter: Option<String>,
}

impl FullTaskList {
    /// Every ticket once, sorted by status (questions first) and then id.
    pub fn rows(&self) -> Vec<TicketRow> {
        let mut rows: Vec<TicketRow> = self
            .ready_to_dispatch
            .iter()
            .chain(&self.in_progress)
            .chain(&self.blocked)
            .chain(&self.scheduled)
            .chain(&self.completed)
            .map(TicketRow::from)
            .chain(self.waiting_on_people.iter().map(TicketRow::from))
            .collect();
        rows.sort_by_key(|r| (r.status, r.id));
        rows.dedup_by_key(|r| r.id);
        rows
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn status_matches_the_web_ui() {
        assert_eq!(Status::of("backlog", None), Status::Ready);
        assert_eq!(Status::of("in_progress", None), Status::InProgress);
        assert_eq!(Status::of("blocked", None), Status::Waiting);
        assert_eq!(Status::of("blocked", Some("")), Status::Waiting);
        assert_eq!(Status::of("blocked", Some("Which DB?")), Status::Question);
        assert_eq!(
            Status::of("blocked", Some("scheduled: 2026-11-01")),
            Status::Scheduled
        );
        assert_eq!(Status::of("scheduled", None), Status::Scheduled);
        assert_eq!(Status::of("completed", None), Status::Done);
        assert_eq!(Status::of("something_new", None), Status::Ready);
        assert_eq!(Status::Question.label(), "Question");
        assert_eq!(Status::InProgress.label(), "In progress");
        assert_eq!(
            serde_json::to_value(Status::InProgress).unwrap(),
            "in_progress"
        );
    }

    #[test]
    fn filters_parse_ui_words_labels_and_amp_states() {
        let f: StatusFilter = "In progress".parse().unwrap();
        assert!(f.matches(Status::InProgress) && !f.matches(Status::Ready));
        let f: StatusFilter = "blocked".parse().unwrap();
        assert!(f.matches(Status::Waiting) && f.matches(Status::Question));
        assert!(!f.includes_done());
        let f: StatusFilter = "ready,done".parse().unwrap();
        assert!(f.matches(Status::Ready) && f.includes_done());
        assert!("all".parse::<StatusFilter>().unwrap().includes_done());
        assert!("nope".parse::<StatusFilter>().is_err());
        assert!("".parse::<StatusFilter>().is_err());
    }

    #[test]
    fn rows_come_from_every_bucket_once_in_status_order() {
        let list: FullTaskList = serde_json::from_value(serde_json::json!({
            "ready_to_dispatch": [{"id": 5, "name": "five", "state": "backlog", "priority": "2", "dependency_ids": null}],
            "in_progress": [{"id": 3, "name": "three", "state": "in_progress", "assigned_to": "amp:amp-worker-backend"}],
            "blocked": [
                {"id": 4, "name": "four", "state": "blocked", "block_reason": "Which DB?"},
                {"id": 6, "name": "six", "state": "blocked", "blocked_by_ids": [3]}
            ],
            "questions": [{"id": 4, "name": "four", "state": "blocked", "block_reason": "Which DB?"}],
            "scheduled": [{"id": 7, "name": "seven", "state": "blocked", "block_reason": "scheduled: later", "start_at": "2026-11-01T00:00:00Z"}],
            "completed": [{"id": 1, "name": "one", "state": "completed"}],
            "waiting_on_people": [{"id": 9, "name": "merge", "owner": "a@b.c", "state": "ready", "waiting_hours": 2}],
            "count": 7
        }))
        .unwrap();
        let rows = list.rows();
        let ids: Vec<_> = rows.iter().map(|r| r.id).collect();
        assert_eq!(ids, [4, 3, 5, 9, 7, 6, 1]);
        assert_eq!(rows[0].question.as_deref(), Some("Which DB?"));
        assert_eq!(rows[3].performer.as_deref(), Some("human"));
        assert_eq!(rows[3].owner.as_deref(), Some("a@b.c"));
        assert_eq!(rows[4].start_at.as_deref(), Some("2026-11-01T00:00:00Z"));
        assert_eq!(rows[5].blocked_by_ids, [3]);
    }
}
