//! The role registry (D5, D6): one [`Role`] per agent asset, with its system prompt, its preloaded
//! skills, the key its model is resolved by, and its tool [`Policy`].
//!
//! - **Prompt:** the agent file's markdown body.
//! - **Preloaded skills:** a `skills:` list in the frontmatter (an override file can set it), else
//!   the table in [`default_skills`]. Every name must be a loaded skill.
//! - **Model:** [`Role::resolve_model`]: `[roles.<id>]` (the agent id without `amp-`) decides,
//!   as in models.toml and config.toml (#2091). A role with no assignment falls back (#2234, D11)
//!   to the frontmatter `model` if that provider is configured, then its `based_on` role's model,
//!   then `[models] fallback` (default: worker-backend's), so a new agent always runs.
//! - **Dispatch:** a subagent is dispatchable when the manager's `task` rules allow its id
//!   (`amp-*`) or its frontmatter says `dispatch: true`; a manager rule naming it with deny still
//!   wins (#2234, D11).
//! - **Policy:** the frontmatter `permission` block, parsed by [`Policy::parse`].
//!   [`Role::tool_policy`] binds it to a worktree for the before-tool hook.

use std::path::PathBuf;

use serde_json::Value;

use crate::{
    assets::{AgentAsset, Assets},
    models::{ModelHints, ModelsConfig, ModelsError, ResolvedRole, normalize_role},
    policy::{Action, Policy, PolicyError, Rule, ToolPolicy, canonical_agent_id},
};

/// One role, ready to build an agent from.
#[derive(Debug, Clone, PartialEq)]
pub struct Role {
    /// The agent id (`amp-worker-backend`): the file stem, and AMP's `agent_id` without `amp:`.
    pub id: String,
    pub description: String,
    /// `mode: primary` (the manager). Everything else is a child (subagent).
    pub primary: bool,
    pub prompt: String,
    /// Skills whose content is preloaded into the system prompt (M2-4), in order.
    pub skills: Vec<String>,
    /// The key for [`crate::models::ModelsConfig::resolve_role`].
    pub model_role: String,
    /// The frontmatter `model` (opencode's). Used only when the role has no `[roles.<id>]` and
    /// that provider and model are configured ([`Role::resolve_model`]).
    pub frontmatter_model: Option<String>,
    /// The frontmatter `variant` (opencode's thinking variant), with `frontmatter_model`.
    pub frontmatter_variant: Option<String>,
    /// The frontmatter `based_on`: the role whose model this one shares when it has none of its
    /// own (`agents new --from` writes it).
    pub based_on: Option<String>,
    /// The frontmatter `dispatch: true`: the manager may dispatch this subagent even when its id
    /// doesn't match the manager's `task` rules (`amp-*`).
    pub dispatch: bool,
    pub policy: Policy,
}

impl Role {
    pub fn is_child(&self) -> bool {
        !self.primary
    }

    /// The policy bound to `root` (the worktree the role runs in), for the before-tool hook.
    pub fn tool_policy(&self, root: impl Into<PathBuf>) -> ToolPolicy {
        ToolPolicy::new(self.id.clone(), self.is_child(), self.policy.clone(), root)
    }

    /// The hints [`ModelsConfig::resolve_with`] falls back on.
    pub fn model_hints(&self) -> ModelHints {
        ModelHints {
            model: self.frontmatter_model.clone(),
            thinking: self.frontmatter_variant.clone(),
            based_on: self.based_on.clone(),
        }
    }

    /// The provider, model and thinking level this role runs on (D11's fallback chain).
    pub fn resolve_model(&self, models: &ModelsConfig) -> Result<ResolvedRole, ModelsError> {
        models.resolve_with(&self.model_role, &self.model_hints())
    }

    fn from_asset(asset: &AgentAsset, assets: &Assets) -> Result<Self, RoleError> {
        let fm = &asset.frontmatter;
        let invalid = |message: &str| RoleError::Invalid {
            role: asset.id.clone(),
            message: message.to_owned(),
        };
        let dispatch = match fm.get("dispatch") {
            None | Some(Value::Null) => false,
            Some(Value::Bool(b)) => *b,
            Some(_) => return Err(invalid("dispatch must be true or false")),
        };
        let based_on = match fm.get("based_on") {
            None | Some(Value::Null) => None,
            Some(Value::String(s)) if !s.trim().is_empty() => Some(s.trim().to_owned()),
            Some(_) => return Err(invalid("based_on must be an agent id")),
        };
        let text = |key: &str| match fm.get(key) {
            Some(Value::String(s)) => Some(s.clone()),
            Some(Value::Number(n)) => Some(n.to_string()),
            _ => None,
        };
        let policy = Policy::parse(fm.get("permission")).map_err(|e| RoleError::Policy {
            role: asset.id.clone(),
            source: e,
        })?;
        let skills = match fm.get("skills") {
            None | Some(Value::Null) => default_skills(&asset.id, asset.is_primary())
                .iter()
                .map(|s| s.to_string())
                .collect(),
            Some(Value::Array(items)) => items
                .iter()
                .map(|v| {
                    v.as_str()
                        .map(str::to_string)
                        .ok_or_else(|| RoleError::Invalid {
                            role: asset.id.clone(),
                            message: "skills must be a list of skill names".into(),
                        })
                })
                .collect::<Result<Vec<_>, _>>()?,
            Some(_) => {
                return Err(RoleError::Invalid {
                    role: asset.id.clone(),
                    message: "skills must be a list of skill names".into(),
                });
            }
        };
        for skill in &skills {
            if assets.skill(skill).is_none() {
                return Err(RoleError::MissingSkill {
                    role: asset.id.clone(),
                    skill: skill.clone(),
                });
            }
        }
        Ok(Self {
            id: asset.id.clone(),
            description: asset.description.clone(),
            primary: asset.is_primary(),
            prompt: asset.prompt.clone(),
            skills,
            model_role: normalize_role(&asset.id).to_string(),
            frontmatter_model: text("model"),
            frontmatter_variant: text("variant"),
            based_on,
            dispatch,
            policy,
        })
    }
}

/// The skills a role preloads when its frontmatter has no `skills:` list. These are the skills
/// each prompt says to "load first" (D5), so the model starts with them instead of spending a
/// turn reading them.
///
/// #2367: the manager preloads nothing. Its system prompt goes with every request of every
/// session, and amp-index (a list the `<available_skills>` block and `amp_run_ticket` already
/// give) plus amp-planning (~10k tokens) were a third of it. Its prompt says to read
/// amp-planning before planning, so only planning sessions pay for it.
pub fn default_skills(id: &str, primary: bool) -> &'static [&'static str] {
    match id {
        "amp-manager" => &[],
        "amp-reviewer" => &["amp-review", "code-reviewer"],
        "amp-github-reviewer" => &["amp-review", "github-pr-review"],
        "amp-github-researcher" => &["github-research"],
        "amp-kb-curator" => &["amp-kb-curator"],
        // Read-only and answers a question; its prompt loads nothing up front.
        "amp-researcher" => &[],
        _ if primary => &[],
        // worker-backend/frontend/docs, designer, media, and any added worker.
        _ => &["amp-execution"],
    }
}

/// A role that can't be built. Always names the role.
#[derive(Debug, thiserror::Error)]
pub enum RoleError {
    #[error("agent {role}: {source}")]
    Policy {
        role: String,
        #[source]
        source: PolicyError,
    },
    #[error("agent {role}: preloads skill {skill:?}, which is not loaded")]
    MissingSkill { role: String, skill: String },
    #[error("agent {role}: {message}")]
    Invalid { role: String, message: String },
    #[error("no primary agent (manager) in the roster")]
    NoManager,
}

/// Every role, by id.
#[derive(Debug, Clone)]
pub struct RoleRegistry {
    roles: Vec<Role>,
    manager: String,
}

impl RoleRegistry {
    /// Build every role from the loaded assets (embedded plus overrides). Fails on the first role
    /// with a malformed policy or a missing skill.
    pub fn from_assets(assets: &Assets) -> Result<Self, RoleError> {
        let mut roles = assets
            .agents()
            .map(|a| Role::from_asset(a, assets))
            .collect::<Result<Vec<_>, _>>()?;
        let manager = assets
            .default_agent()
            .filter(|a| a.is_primary())
            .map(|a| a.id.clone())
            .ok_or(RoleError::NoManager)?;
        // D11: `dispatch: true` subagents join every primary's `task` allowlist, as a rule at the
        // end (last match wins), so `dispatchable` and the before-tool hook agree. A primary rule
        // that names the agent (not a catch-all) and denies it still wins.
        let opted_in: Vec<String> = roles
            .iter()
            .filter(|r| r.is_child() && r.dispatch)
            .map(|r| r.id.clone())
            .collect();
        for role in roles.iter_mut().filter(|r| r.primary) {
            for id in &opted_in {
                let explicit_deny = role
                    .policy
                    .matching_rule("task", id)
                    .is_some_and(|r| !r.action.allows() && r.pattern != "*" && r.pattern != "**");
                if !explicit_deny && !role.policy.evaluate("task", id).allows() {
                    role.policy.push(Rule {
                        permission: "task".into(),
                        pattern: id.clone(),
                        action: Action::Allow,
                    });
                }
            }
        }
        Ok(Self { roles, manager })
    }

    pub fn roles(&self) -> impl Iterator<Item = &Role> {
        self.roles.iter()
    }

    /// Look a role up by any of its names: `amp:amp-worker-backend`, `amp-worker-backend` or
    /// `worker-backend`.
    pub fn get(&self, name: &str) -> Option<&Role> {
        let id = canonical_agent_id(name);
        let prefixed = format!("amp-{id}");
        self.roles
            .iter()
            .find(|r| r.id == id)
            .or_else(|| self.roles.iter().find(|r| r.id == prefixed))
    }

    /// The default primary role (the TUI's and `run -p`'s).
    pub fn manager(&self) -> &Role {
        self.get(&self.manager)
            .expect("the manager id was taken from the roster")
    }

    /// The children `role` may dispatch: subagents its `task` rules allow. M2-5 lists these, with
    /// their descriptions, in `amp_run_ticket`'s tool description.
    pub fn dispatchable(&self, role: &Role) -> Vec<&Role> {
        if role.is_child() {
            return Vec::new();
        }
        self.roles
            .iter()
            .filter(|r| r.is_child() && role.policy.evaluate("task", &r.id).allows())
            .collect()
    }
}

// ------------------------------------------------------------------------------------------------
// Self-management guard (#4246): what a tool-driven agent edit may change without widening.
// ------------------------------------------------------------------------------------------------

/// The outcome of [`permission_widens`]: whether the new `permission:` block lets the agent do
/// anything the old one (or the baseline) did not, and why, in words a person confirms.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Widening {
    pub widens: bool,
    /// One line per widening change (empty when `widens` is false).
    pub reasons: Vec<String>,
}

/// The policy a newly created agent is compared with (there is no old one): bash, edit and
/// webfetch denied. A new agent with no `permission:` block gets the defaults (allow for all
/// but `external_directory`), so it widens on all three.
pub fn new_agent_baseline() -> Policy {
    let mut baseline = Policy::default();
    for permission in ["bash", "edit", "webfetch"] {
        baseline.push(Rule {
            permission: permission.into(),
            pattern: "*".into(),
            action: Action::Deny,
        });
    }
    baseline
}

/// Whether going from `old` (or `baseline` for a new agent, `old == None`) to `new` widens the
/// agent's permissions, with the reasons.
///
/// Glob containment can't be decided in general, so the rule is conservative (#3139, #3144):
/// the only changes that don't widen are **adding deny/ask rules** and **removing allow rules**
/// (`ask` counts as deny, as the harness enforces it). Everything else widens:
/// - a new `allow` rule, including one with a broader glob than the rule it replaces;
/// - a removed `deny`/`ask` rule, or a removed key or block, because a missing key falls back to
///   the default (allow for every permission but `external_directory`, which is ask);
/// - moving a deny before an allow it used to override.
///
/// One exception keeps no-ops quiet: an added `allow` for a literal permission the old policy
/// already allowed everywhere (no rule but allow, default allow) changes nothing.
///
/// Computed as the cheapest alignment of the two rule lists, where matching an equal rule,
/// dropping an old allow and inserting a new deny are free, and dropping an old deny or inserting
/// a new allow cost one each. It widens when every alignment costs something.
pub fn permission_widens(old: Option<&Policy>, new: &Policy, baseline: &Policy) -> Widening {
    let from = old.unwrap_or(baseline);
    let (a, b) = (from.rules(), new.rules());
    let (n, m) = (a.len(), b.len());
    let same = |x: &Rule, y: &Rule| {
        x.permission == y.permission
            && x.pattern == y.pattern
            && x.action.allows() == y.action.allows()
    };
    let drop_cost = |r: &Rule| usize::from(!r.action.allows());
    let add_cost = |r: &Rule| {
        let harmless = !r.action.allows()
            || (!has_wildcard(&r.permission) && from.allows_everything(&r.permission));
        usize::from(!harmless)
    };
    // cost[i][j]: the cheapest way to turn a[i..] into b[j..].
    let mut cost = vec![vec![0usize; m + 1]; n + 1];
    for i in (0..=n).rev() {
        for j in (0..=m).rev() {
            if i == n && j == m {
                continue;
            }
            let mut best = usize::MAX;
            if i < n {
                best = best.min(drop_cost(&a[i]) + cost[i + 1][j]);
            }
            if j < m {
                best = best.min(add_cost(&b[j]) + cost[i][j + 1]);
            }
            if i < n && j < m && same(&a[i], &b[j]) {
                best = best.min(cost[i + 1][j + 1]);
            }
            cost[i][j] = best;
        }
    }
    if cost[0][0] == 0 {
        return Widening::default();
    }
    // Walk one cheapest alignment back and name what it charged for.
    let (mut removed, mut added) = (Vec::new(), Vec::new());
    let (mut i, mut j) = (0, 0);
    while i < n || j < m {
        if i < n && j < m && same(&a[i], &b[j]) && cost[i][j] == cost[i + 1][j + 1] {
            i += 1;
            j += 1;
        } else if i < n && cost[i][j] == drop_cost(&a[i]) + cost[i + 1][j] {
            if drop_cost(&a[i]) > 0 {
                removed.push(&a[i]);
            }
            i += 1;
        } else {
            if add_cost(&b[j]) > 0 {
                added.push(&b[j]);
            }
            j += 1;
        }
    }
    let mut reasons = Vec::new();
    for r in &removed {
        let now = new.evaluate(&r.permission, &r.pattern);
        let changed = added
            .iter()
            .any(|x| x.permission == r.permission && x.pattern == r.pattern);
        if changed {
            reasons.push(format!(
                "{} {:?}: {} -> allow",
                r.permission,
                r.pattern,
                r.action.as_str()
            ));
        } else {
            reasons.push(format!(
                "removes {} {:?}: {} (now {})",
                r.permission,
                r.pattern,
                r.action.as_str(),
                now.as_str()
            ));
        }
    }
    for x in &added {
        let changed = removed
            .iter()
            .any(|r| x.permission == r.permission && x.pattern == r.pattern);
        if !changed {
            reasons.push(format!("adds {} {:?}: allow", x.permission, x.pattern));
        }
    }
    Widening {
        widens: true,
        reasons,
    }
}

fn has_wildcard(text: &str) -> bool {
    text.contains(['*', '?'])
}

/// A tool-driven edit of an agent that no confirmation can allow.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("agent {id}: {reason}")]
pub struct AgentEditRefused {
    pub id: String,
    pub reason: String,
}

/// Refuse a tool-driven create/edit/remove of a primary agent (#4246): the role `id` names in
/// `registry` is primary or is [`RoleRegistry::manager`], or the edit makes it primary
/// (`new_primary`, the new file's `mode: primary`). Judged on the role, never on the name
/// `amp-manager`. A person edits primaries with `amp-harness agents edit`.
pub fn refuse_primary(
    registry: &RoleRegistry,
    id: &str,
    new_primary: bool,
) -> Result<(), AgentEditRefused> {
    let refused = |reason: &str| AgentEditRefused {
        id: id.to_owned(),
        reason: reason.to_owned(),
    };
    if let Some(role) = registry.get(id) {
        if role.id == registry.manager().id {
            return Err(refused(
                "is the manager; tools can't change it (a person can, with `amp-harness agents edit`)",
            ));
        }
        if role.primary {
            return Err(refused(
                "is a primary agent; tools can't change it (a person can, with `amp-harness agents edit`)",
            ));
        }
    }
    if new_primary {
        return Err(refused(
            "would become a primary agent (mode: primary); tools may only write subagents",
        ));
    }
    Ok(())
}

/// What saving `role` means for dispatch (#4246): `Some(note)` when the `manager` may dispatch it
/// as soon as it is loaded (live reload picks the file up within seconds), because the manager's
/// `task` rules allow its id (the shipped `"amp-*": allow`) or it says `dispatch: true` and no
/// manager rule names it denied. `None` for primaries and agents the manager can't dispatch.
pub fn dispatch_effect(manager: &Role, role: &Role) -> Option<String> {
    if role.primary {
        return None;
    }
    let explicit_deny = manager
        .policy
        .matching_rule("task", &role.id)
        .is_some_and(|r| !r.action.allows() && r.pattern != "*" && r.pattern != "**");
    if role.dispatch && !explicit_deny {
        return Some(format!(
            "{} becomes dispatchable by {} immediately: it says dispatch: true",
            role.id, manager.id
        ));
    }
    match manager.policy.matching_rule("task", &role.id) {
        Some(rule) if rule.action.allows() => Some(format!(
            "{} becomes dispatchable by {} immediately: its task rule {:?} allows it",
            role.id, manager.id, rule.pattern
        )),
        None if default_task_allows() => Some(format!(
            "{} becomes dispatchable by {} immediately: no task rule names it and task defaults to allow",
            role.id, manager.id
        )),
        _ => None,
    }
}

fn default_task_allows() -> bool {
    Policy::default().evaluate("task", "x").allows()
}
