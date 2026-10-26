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
