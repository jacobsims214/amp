//! Waiting for a person's plan approval, and resuming the manager when it comes (#2425).
//!
//! The manager plans, asks its person to approve the plan in the AMP web UI, and its turn ends
//! (it presents the plan and stops, or AMP refused a dispatch: [`crate::approval`]). Before #2425
//! the chat then sat idle until the person came back to the terminal and typed. Now the chat
//! waits for the approval itself and starts the manager's next turn when a person has approved.
//!
//! **The harness never approves.** It only watches AMP's answer to `amp_check_plan`, which the
//! metadata poller already reads (no tokens, never through the model), and reacts to an approval
//! a person made in the web UI.
//!
//! **How a pending approval is noticed.** When a manager turn completes, the chat starts an
//! [`ApprovalWait`] if the turn touched the plan ([`touches_plan`]: it planned, checked the plan,
//! or AMP refused a step for want of approval) and asks the poller for a fresh plan read. The
//! first read made after the turn ended *arms* the wait: the tickets to wait for are the person's
//! own awaiting tickets (`awaiting_yours`; every awaiting ticket on servers before per-ticket
//! approval) plus any ticket AMP refused this turn. Nothing awaiting: there is nothing to wait for
//! and the wait ends without a word.
//!
//! **How the approval is seen.** AMP publishes `project.updated` / `task.updated` live events on
//! approval, but its live stream (`/api/events`) is for the browser session (oauth2-proxy in
//! front), not the harness's MCP login, so the harness polls instead: the poller reads the plan
//! every 5 s while tickets await, backing off to 15 s after 2 min and 60 s after 15 min
//! ([`crate::meta::Intervals::plan_pending_after`]), and stops the fast poll once nothing waits.
//! Each fresh read is checked with [`ApprovalWait::check`]:
//! - some waited tickets left the awaiting list **and** the project's last approval
//!   (`plan_approved_at`) moved: [`WaitCheck::Approved`] (resume, naming any still waiting);
//! - they left without a new approval (deleted, completed, re-planned away):
//!   [`WaitCheck::Changed`] (say so; don't resume);
//! - plan approval was turned off for the project: [`WaitCheck::Changed`] too;
//! - otherwise keep waiting.
//!
//! The resume prompt ([`resume_prompt`]) is a user turn the person sees in the transcript.

use std::{collections::BTreeSet, time::Instant};

use crate::approval::{Refusal, RefusalKind};
use crate::meta::{AmpMeta, User};

/// `[manager] auto_resume_on_approval` (default true): the env override.
pub const ENV_AUTO_RESUME: &str = "AMP_HARNESS_AUTO_RESUME";

/// The manager's tools whose call means the turn planned or looked at the plan.
pub const PLAN_TOOLS: &[&str] = &[
    "amp_create_tasks",
    "amp_check_plan",
    "amp_approve_plan",
    "amp_set_task_dependencies",
    "amp_update_task",
];

/// Whether a manager turn that called `tool` touched the plan (a wait may follow it).
pub fn touches_plan(tool: &str) -> bool {
    PLAN_TOOLS.contains(&tool)
}

/// A refusal that waits on a plan approval (not another person-only step, such as marking a
/// person's ticket done, which no approval resumes).
pub fn waits_for_approval(r: &Refusal) -> bool {
    r.kind == RefusalKind::PlanApproval
}

/// One wait for a person's approval.
#[derive(Debug, Clone)]
pub struct ApprovalWait {
    /// The project whose plan waits.
    pub project_id: i64,
    /// When the turn ended: only plan reads made after it count.
    pub started: Instant,
    /// Tickets AMP refused this turn for want of approval (waited for even when they are someone
    /// else's: a project lead may approve them on their owner's behalf).
    refused: BTreeSet<i64>,
    /// The tickets waited for, once armed.
    tickets: BTreeSet<i64>,
    armed: bool,
    /// The project's last approval when the wait began (`plan_approved_at`); `None` when the plan
    /// had never been read (the arming read sets it then).
    baseline: Option<Option<String>>,
    /// The last plan read looked at (its `updated` time), so each read is checked once.
    seen: Option<Instant>,
}

/// What a plan read says about a wait.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WaitCheck {
    /// No fresh read yet: nothing known.
    Unknown,
    /// Armed just now: waiting for these tickets.
    Armed(Vec<i64>),
    /// Nothing waits on the person: no wait.
    Nothing,
    /// Still waiting.
    Waiting,
    /// A person approved: these tickets are approved, these still wait.
    Approved(Approval),
    /// The waited tickets stopped waiting without an approval, or approval was turned off: say
    /// this and stop waiting, without resuming.
    Changed(String),
}

/// An approval a person made in the web UI.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Approval {
    pub approved: Vec<i64>,
    pub still_waiting: Vec<i64>,
    /// `plan_approved_by` (a user id).
    pub by: Option<i64>,
    /// `plan_approved_at` (RFC 3339, as AMP sent it).
    pub at: Option<String>,
}

impl ApprovalWait {
    /// A wait for `project_id`'s plan, begun when the turn ended. `refusals`: the turn's
    /// approval refusals. `meta`: the snapshot then (its `plan_approved_at` is the baseline).
    pub fn new(project_id: i64, refusals: &[Refusal], meta: &AmpMeta, started: Instant) -> Self {
        Self {
            project_id,
            started,
            refused: refusals
                .iter()
                .filter(|r| waits_for_approval(r))
                .filter_map(|r| r.ticket_id)
                .collect(),
            tickets: BTreeSet::new(),
            armed: false,
            baseline: meta.plan.get().map(|p| p.plan_approved_at.clone()),
            seen: None,
        }
    }

    /// Whether a fresh plan read armed it (the tickets are known).
    pub fn armed(&self) -> bool {
        self.armed
    }

    /// The tickets waited for (empty until armed).
    pub fn tickets(&self) -> Vec<i64> {
        self.tickets.iter().copied().collect()
    }

    /// Looks at `meta`'s plan read, if it is one this wait hasn't seen yet and was made after
    /// the turn ended. Pure apart from remembering what it saw.
    pub fn check(&mut self, meta: &AmpMeta) -> WaitCheck {
        if meta.project_id != Some(self.project_id) {
            return WaitCheck::Changed(
                "the session switched to another project, so the chat stopped waiting for this \
                 plan's approval"
                    .to_owned(),
            );
        }
        let Some(updated) = meta.plan.updated else {
            return WaitCheck::Unknown;
        };
        if updated < self.started || self.seen == Some(updated) {
            return WaitCheck::Unknown;
        }
        self.seen = Some(updated);
        let Some(plan) = meta.plan.get() else {
            return WaitCheck::Unknown;
        };
        let required = meta.project.get().map_or(plan.require_plan_approval, |p| {
            p.require_plan_approval || plan.require_plan_approval
        });
        let awaiting: BTreeSet<i64> = plan.awaiting_approval.iter().copied().collect();
        let baseline = self
            .baseline
            .get_or_insert_with(|| plan.plan_approved_at.clone());
        let moved = plan.plan_approved_at.is_some() && plan.plan_approved_at != *baseline;
        if !self.armed {
            self.armed = true;
            self.tickets = plan.yours().into_iter().collect();
            self.tickets.extend(self.refused.iter().copied());
            if self.tickets.is_empty() || !required {
                return WaitCheck::Nothing;
            }
            let gone: Vec<i64> = self.tickets.difference(&awaiting).copied().collect();
            if gone.is_empty() {
                // Nothing of these approved yet: AMP's last approval now is the one to beat.
                self.baseline = Some(plan.plan_approved_at.clone());
                return WaitCheck::Armed(self.tickets());
            }
            if !moved {
                // Refused tickets that no longer wait without a new approval: not waited for.
                self.tickets.retain(|t| awaiting.contains(t));
                return if self.tickets.is_empty() {
                    WaitCheck::Nothing
                } else {
                    WaitCheck::Armed(self.tickets())
                };
            }
            // The person was quick: some were approved by the time of this read.
        }
        if !required {
            return WaitCheck::Changed(
                "plan approval was turned off for this project in AMP, so the chat stopped \
                 waiting. Reply to carry on."
                    .to_owned(),
            );
        }
        let gone: Vec<i64> = self.tickets.difference(&awaiting).copied().collect();
        if gone.is_empty() {
            return WaitCheck::Waiting;
        }
        let still: Vec<i64> = self.tickets.intersection(&awaiting).copied().collect();
        if moved {
            return WaitCheck::Approved(self.approval(gone, still, plan));
        }
        WaitCheck::Changed(format!(
            "{} no longer {} for approval, but AMP recorded no new approval (deleted, done or \
             changed?), so the chat stopped waiting. Reply to carry on.",
            ids(&gone),
            if gone.len() == 1 { "waits" } else { "wait" }
        ))
    }

    fn approval(
        &self,
        approved: Vec<i64>,
        still_waiting: Vec<i64>,
        plan: &crate::meta::PlanApproval,
    ) -> Approval {
        Approval {
            approved,
            still_waiting,
            by: plan.plan_approved_by,
            at: plan.plan_approved_at.clone(),
        }
    }
}

/// `#1, #2`.
pub fn ids(tickets: &[i64]) -> String {
    tickets
        .iter()
        .map(|t| format!("#{t}"))
        .collect::<Vec<_>>()
        .join(", ")
}

/// Who approved: the person's own email when it was them, else `user <id>` (AMP's check_plan
/// gives only the id), else "a person".
pub fn approver(by: Option<i64>, me: Option<&User>) -> String {
    match (by, me) {
        (Some(id), Some(u)) if u.user_id == id && !u.email.is_empty() => u.email.clone(),
        (Some(id), _) => format!("user {id}"),
        (None, _) => "a person".to_owned(),
    }
}

/// `plan_approved_at` as local `HH:MM` (falls back to the text as sent).
pub fn local_time(at: Option<&str>) -> Option<String> {
    let at = at?;
    Some(match chrono::DateTime::parse_from_rfc3339(at) {
        Ok(t) => t.with_timezone(&chrono::Local).format("%H:%M").to_string(),
        Err(_) => at.to_owned(),
    })
}

/// The transcript line when the approval comes: "Plan approved in AMP by ada@example.com at
/// 14:02 (#2201, #2202). The manager carries on."
pub fn approved_line(a: &Approval, me: Option<&User>) -> String {
    let mut line = format!("✓ Plan approved in AMP by {}", approver(a.by, me));
    if let Some(t) = local_time(a.at.as_deref()) {
        line.push_str(&format!(" at {t}"));
    }
    line.push_str(&format!(" ({}).", ids(&a.approved)));
    if !a.still_waiting.is_empty() {
        line.push_str(&format!(" {} still waiting.", ids(&a.still_waiting)));
    }
    line.push_str(" The manager carries on.");
    line
}

/// The user turn that resumes the manager. It names the approved tickets (so the manager may
/// run them) and the ones still waiting (so it doesn't), but no clock time: every standalone
/// number in a user message counts as a ticket the person named ([`crate::approval`]).
pub fn resume_prompt(a: &Approval, me: Option<&User>) -> String {
    let mut text = format!(
        "[amp-harness] The plan was approved in AMP by {}: {} {} approved now.",
        approver(a.by, me).replace(|c: char| c.is_ascii_digit(), ""),
        ids(&a.approved),
        if a.approved.len() == 1 { "is" } else { "are" },
    );
    if !a.still_waiting.is_empty() {
        text.push_str(&format!(
            " {} still {} for approval: don't start {}.",
            ids(&a.still_waiting),
            if a.still_waiting.len() == 1 {
                "waits"
            } else {
                "wait"
            },
            if a.still_waiting.len() == 1 {
                "it"
            } else {
                "them"
            },
        ));
    }
    text.push_str(" Carry on with the plan: dispatch the approved tickets that are ready.");
    text
}

/// The transcript line when the wait begins: "⏸ Waiting for plan approval in AMP (#4050). The
/// manager carries on by itself once you approve; no need to come back here (Esc or /stop stops
/// waiting). Approve it: <link>".
pub fn waiting_line(tickets: &[i64], link: Option<&str>) -> String {
    let mut line = format!(
        "⏸ Waiting for plan approval in AMP ({}). The manager carries on by itself once you \
         approve; no need to come back here (Esc or /stop stops waiting).",
        ids(tickets)
    );
    if let Some(l) = link {
        line.push_str(&format!(" Approve it: {l}"));
    }
    line
}

/// `[manager] auto_resume_on_approval`: [`ENV_AUTO_RESUME`], then config.toml, else true.
/// Values: `true`/`false`, `on`/`off` (TOML booleans too).
pub fn resolve_auto_resume(
    env: &dyn Fn(&str) -> Option<String>,
    config: Option<&str>,
) -> Result<bool, String> {
    if let Some(v) = env(ENV_AUTO_RESUME).filter(|v| !v.trim().is_empty()) {
        return on_off(&v).map_err(|e| format!("{ENV_AUTO_RESUME}: {e}"));
    }
    let Some(text) = config else {
        return Ok(true);
    };
    let doc: toml::Table = toml::from_str(text).map_err(|e| format!("config.toml: {e}"))?;
    manager_auto_resume(&doc)
}

/// `[manager]` in a parsed config file: only `auto_resume_on_approval`, a boolean.
pub fn manager_auto_resume(doc: &toml::Table) -> Result<bool, String> {
    let Some(manager) = doc.get("manager") else {
        return Ok(true);
    };
    let Some(table) = manager.as_table() else {
        return Err("[manager] must be a table".to_owned());
    };
    for key in table.keys() {
        if key != "auto_resume_on_approval" {
            return Err(format!(
                "[manager] {key}: unknown setting (the only one is auto_resume_on_approval)"
            ));
        }
    }
    match table.get("auto_resume_on_approval") {
        None => Ok(true),
        Some(toml::Value::Boolean(b)) => Ok(*b),
        Some(toml::Value::String(s)) => {
            on_off(s).map_err(|e| format!("[manager] auto_resume_on_approval: {e}"))
        }
        Some(other) => Err(format!(
            "[manager] auto_resume_on_approval: unknown value `{other}`: use true or false"
        )),
    }
}

fn on_off(v: &str) -> Result<bool, String> {
    match v.trim().to_ascii_lowercase().as_str() {
        "true" | "on" | "yes" | "1" => Ok(true),
        "false" | "off" | "no" | "0" => Ok(false),
        other => Err(format!("unknown value `{other}`: use true or false")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::meta::{Fetched, PlanApproval, Project};
    use serde_json::json;
    use std::time::Duration;

    fn meta(awaiting: &[i64], yours: &[i64], approved_at: Option<&str>, at: Instant) -> AmpMeta {
        let tickets: Vec<_> = awaiting
            .iter()
            .map(|id| json!({"task_id": id, "owner_user_id": if yours.contains(id) {7} else {9}, "yours": yours.contains(id)}))
            .collect();
        let plan = PlanApproval::from_check_plan(&json!({
            "require_plan_approval": true,
            "awaiting_approval": awaiting,
            "awaiting_approval_tickets": tickets,
            "awaiting_yours": yours,
            "plan_approved_at": approved_at,
            "plan_approved_by": 7,
        }));
        AmpMeta {
            project_id: Some(2),
            project: Fetched {
                value: Some(Project {
                    id: 2,
                    name: "AMP".into(),
                    code: "amp".into(),
                    state: "active".into(),
                    org_id: None,
                    require_plan_approval: true,
                }),
                updated: Some(at),
                error: None,
            },
            plan: Fetched {
                value: Some(plan),
                updated: Some(at),
                error: None,
            },
            user: Fetched {
                value: Some(User {
                    user_id: 7,
                    email: "ada@example.com".into(),
                    account: "human".into(),
                    via_api_token: false,
                    global_admin: false,
                    orgs: vec![],
                }),
                updated: Some(at),
                error: None,
            },
            ..AmpMeta::default()
        }
    }

    #[test]
    fn a_wait_arms_on_a_fresh_read_and_resumes_on_approval() {
        let t0 = Instant::now();
        let before = meta(&[1, 2, 3], &[1, 2], Some("2026-10-26T10:00:00Z"), t0);
        let mut w = ApprovalWait::new(2, &[], &before, t0 + Duration::from_millis(1));
        // The read from before the turn ended doesn't count.
        assert_eq!(w.check(&before), WaitCheck::Unknown);
        let t = |ms: u64| t0 + Duration::from_millis(ms);
        let fresh = meta(&[1, 2, 3], &[1, 2], Some("2026-10-26T10:00:00Z"), t(10));
        assert_eq!(w.check(&fresh), WaitCheck::Armed(vec![1, 2]));
        // The same read again: nothing new.
        assert_eq!(w.check(&fresh), WaitCheck::Unknown);
        // Polls with no approval: still waiting.
        assert_eq!(
            w.check(&meta(
                &[1, 2, 3],
                &[1, 2],
                Some("2026-10-26T10:00:00Z"),
                t(20)
            )),
            WaitCheck::Waiting
        );
        // A person approves #1 only: resume, #2 still waits; #3 is someone else's.
        let approved = meta(&[2, 3], &[2], Some("2026-10-26T14:02:00Z"), t(30));
        match w.check(&approved) {
            WaitCheck::Approved(a) => {
                assert_eq!(a.approved, vec![1]);
                assert_eq!(a.still_waiting, vec![2]);
                assert_eq!(a.by, Some(7));
                let me = approved.user.get();
                assert!(
                    approved_line(&a, me)
                        .starts_with("✓ Plan approved in AMP by ada@example.com at ")
                );
                let p = resume_prompt(&a, me);
                assert!(p.contains("#1 is approved now"), "{p}");
                assert!(p.contains("#2 still waits for approval"), "{p}");
                // No clock digits that would count as named tickets.
                assert!(!p.contains("14"), "{p}");
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn nothing_to_wait_for_without_awaiting_tickets() {
        let t0 = Instant::now();
        let m0 = meta(&[], &[], None, t0);
        let mut w = ApprovalWait::new(2, &[], &m0, t0);
        assert_eq!(
            w.check(&meta(&[5], &[], None, t0 + Duration::from_millis(5))),
            WaitCheck::Nothing,
            "only others' tickets wait and none was refused"
        );
    }

    #[test]
    fn tickets_gone_without_an_approval_is_a_change_not_a_resume() {
        let t0 = Instant::now();
        let m0 = meta(&[1], &[1], Some("2026-10-26T10:00:00Z"), t0);
        let mut w = ApprovalWait::new(2, &[], &m0, t0);
        assert_eq!(
            w.check(&meta(
                &[1],
                &[1],
                Some("2026-10-26T10:00:00Z"),
                t0 + Duration::from_millis(1)
            )),
            WaitCheck::Armed(vec![1])
        );
        match w.check(&meta(
            &[],
            &[],
            Some("2026-10-26T10:00:00Z"),
            t0 + Duration::from_millis(2),
        )) {
            WaitCheck::Changed(why) => assert!(why.contains("#1 no longer waits"), "{why}"),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn a_refused_ticket_of_someone_else_is_waited_for() {
        let t0 = Instant::now();
        let refusal = crate::approval::refusal(
            "ticket 3 isn't approved: nobody has approved it since it was planned or its scope \
             last changed. Its owner approves it in the AMP web UI: /project/2/plan",
        )
        .expect("refusal");
        let m0 = meta(&[3], &[], None, t0);
        let mut w = ApprovalWait::new(2, &[refusal], &m0, t0);
        assert_eq!(
            w.check(&meta(&[3], &[], None, t0 + Duration::from_millis(1))),
            WaitCheck::Armed(vec![3])
        );
        // A lead approves it on its owner's behalf.
        assert!(matches!(
            w.check(&meta(
                &[],
                &[],
                Some("2026-10-26T11:00:00Z"),
                t0 + Duration::from_millis(2)
            )),
            WaitCheck::Approved(_)
        ));
    }

    #[test]
    fn switching_project_or_turning_approval_off_stops_the_wait() {
        let t0 = Instant::now();
        let m0 = meta(&[1], &[1], None, t0);
        let mut w = ApprovalWait::new(2, &[], &m0, t0);
        let mut other = m0.clone();
        other.project_id = Some(3);
        assert!(matches!(w.check(&other), WaitCheck::Changed(_)));
        let mut w = ApprovalWait::new(2, &[], &m0, t0);
        assert!(matches!(
            w.check(&meta(&[1], &[1], None, t0 + Duration::from_millis(1))),
            WaitCheck::Armed(_)
        ));
        let mut off = meta(&[1], &[1], None, t0 + Duration::from_millis(2));
        if let Some(p) = off.project.value.as_mut() {
            p.require_plan_approval = false;
        }
        if let Some(p) = off.plan.value.as_mut() {
            p.require_plan_approval = false;
        }
        match w.check(&off) {
            WaitCheck::Changed(why) => assert!(why.contains("turned off"), "{why}"),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn the_setting_reads_env_then_config() {
        let none = |_: &str| None;
        assert_eq!(resolve_auto_resume(&none, None), Ok(true));
        assert_eq!(
            resolve_auto_resume(&none, Some("[tui]\nmouse = true\n")),
            Ok(true)
        );
        assert_eq!(
            resolve_auto_resume(&none, Some("[manager]\nauto_resume_on_approval = false\n")),
            Ok(false)
        );
        assert!(resolve_auto_resume(&none, Some("[manager]\nauto_resume = false\n")).is_err());
        assert!(
            resolve_auto_resume(&none, Some("[manager]\nauto_resume_on_approval = 3\n")).is_err()
        );
        let off = |k: &str| (k == ENV_AUTO_RESUME).then(|| "off".to_owned());
        assert_eq!(
            resolve_auto_resume(&off, Some("[manager]\nauto_resume_on_approval = true\n")),
            Ok(false)
        );
    }
}
