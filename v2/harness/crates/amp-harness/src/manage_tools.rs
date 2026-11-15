//! The manager's self-management tools (story 583): `amp_manage_agent` and `amp_manage_skill`
//! (AMP #4252) create or update an agent or a skill in the **user** layer, and
//! `amp_add_mcp_server` (AMP #4253, see [`AddMcpTool`]) adds an MCP server to the user config.
//!
//! Every call goes through the same gates, in this order, and writes nothing unless all pass:
//! 1. a person can confirm here: with no [`Confirmer`] (headless `run -p`) the call is refused
//!    before any other work;
//! 2. the guard (#4246): no tool may touch a primary agent or the manager, make an agent primary,
//!    or widen an agent's permissions (a new agent is compared with
//!    [`roles::new_agent_baseline`]); a skill a primary agent preloads is the manager's prompt, so
//!    it is refused too. The new text must also load;
//! 3. the person's "yes" through [`confirm_or_deny`] (#4247, #4259), with the target path, the
//!    backup location and the dispatch note (#4246) on screen;
//! 4. the guard again (the files may have changed while the prompt was up), then
//!    [`write_agent_in`] / [`write_skill_in`] (#4244, #4245), which validate once more, back the
//!    previous layer file up under `<layer>/.edit/backup/` and write atomically.
//!
//! The reload watcher picks the file up; reloads wait while the manager is replying (#4267), so a
//! write takes effect from the manager's next turn.
//!
//! User scope only (manager decision for v0.4.0): repo trust only covers the CLI's own writes, so
//! a tool's repo write would not load anyway. Paths come from the session's [`ConfigPaths`],
//! never from the process's current directory. The session registers [`tools`] for primary
//! roles only, and `/reload` registers them again with the same confirmer.

use std::{path::PathBuf, sync::Arc};

use amp_core::{
    approval::{
        CONFIRM_TIMEOUT, ConfirmRequest, Confirmed, Confirmer, DenyReason, confirm_or_deny,
    },
    assets::{AGENTS_DIR, Assets, Origin, OverrideDirs, SKILLS_DIR, valid_name},
    mcp_config::{self, McpConfig},
    roles::{self, RoleRegistry},
};
use amp_mcp::ConfigPaths;
use rpi_agent::{AgentError as ToolError, AgentTool, AgentToolResult, ToolResultPartial};
use rpi_ai::types::Tool;
use rpi_harness::types::{HarnessTool, ToolReplay};
use serde::Deserialize;
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use crate::{
    agents_cmd::{Ctx, EDIT_DIR, Scope, write_agent_in},
    mcp_crud::{self, AddOpts, Expect, NewServer},
    skills_cmd::write_skill_in,
};

pub(crate) const MANAGE_AGENT_TOOL: &str = "amp_manage_agent";
pub(crate) const MANAGE_SKILL_TOOL: &str = "amp_manage_skill";
pub(crate) const ADD_MCP_TOOL: &str = "amp_add_mcp_server";

/// The skill file the tools write (the same name `skills_cmd` uses).
const SKILL_FILE: &str = "SKILL.md";

/// The manager's self-management tools over the session's config `paths`. `confirmer` is the
/// chat's y/N prompt; `None` (headless) refuses every call.
pub(crate) fn tools(
    paths: ConfigPaths,
    cwd: PathBuf,
    confirmer: Option<Arc<dyn Confirmer>>,
) -> Vec<HarnessTool> {
    let shared = Arc::new(Shared {
        paths,
        cwd,
        confirmer,
    });
    let mut tools: Vec<HarnessTool> = [Kind::Agent, Kind::Skill]
        .into_iter()
        .map(|kind| {
            HarnessTool::new(Arc::new(ManageTool::new(kind, Arc::clone(&shared))))
                .with_replay(ToolReplay::Never)
        })
        .collect();
    tools.push(
        HarnessTool::new(Arc::new(AddMcpTool::new(Arc::clone(&shared))))
            .with_replay(ToolReplay::Never),
    );
    tools
}

struct Shared {
    paths: ConfigPaths,
    cwd: PathBuf,
    confirmer: Option<Arc<dyn Confirmer>>,
}

impl Shared {
    fn ctx(&self) -> Ctx {
        Ctx {
            paths: self.paths.clone(),
            cwd: self.cwd.clone(),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Kind {
    Agent,
    Skill,
}

impl Kind {
    fn noun(self) -> &'static str {
        match self {
            Kind::Agent => "agent",
            Kind::Skill => "skill",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
enum Action {
    Create,
    Update,
}

impl Action {
    fn verb(self) -> &'static str {
        match self {
            Action::Create => "create",
            Action::Update => "update",
        }
    }
}

/// The tools' arguments. No `scope`: user scope is implied.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Args {
    action: Action,
    name: String,
    content: String,
}

fn parse_args(params: Value) -> Result<Args, String> {
    let args: Args = serde_json::from_value(params).map_err(|e| format!("bad arguments: {e}"))?;
    if args.name.trim().is_empty() {
        return Err("name is required".into());
    }
    if args.content.trim().is_empty() {
        return Err("content is required: the whole file".into());
    }
    Ok(args)
}

/// What passed the guard: the person is asked about this, and it is what gets written.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Plan {
    /// The canonical agent id or the skill name.
    name: String,
    /// The file to write.
    path: PathBuf,
    /// Where the current layer file will be copied first, when there is one.
    backup: Option<String>,
    /// Lines for the confirmation (the dispatch note, a shadowing repo file).
    notes: Vec<String>,
}

/// The guard for an agent write (#4246): the role is not primary and does not become primary,
/// the text loads, and the permissions do not widen.
///
/// The write lands in the user layer, which applies in every repo, so it is judged against the
/// user + built-in roster only (#4316): a repo layer's agent (trusted or not) is never the
/// baseline. An id the repo layer alone defines is judged as a new agent, against
/// [`roles::new_agent_baseline`]; otherwise an untrusted repo's wide agent could be copied into
/// the user layer as an "update" that widens nothing.
fn check_agent(ctx: &Ctx, action: Action, name: &str, content: &str) -> Result<Plan, String> {
    // Every layer: only for the primary/manager refusal and the "a repo file overrides it" note.
    let loaded = ctx.load().map_err(|e| e.message)?;
    // What the user-layer write replaces: built-ins plus the user layer, no repo layer.
    let user_assets = Assets::load(&OverrideDirs {
        user: ctx.paths.user_dir.clone(),
        repo: None,
    })
    .map_err(|e| e.to_string())?;
    let user_roles = RoleRegistry::from_assets(&user_assets).map_err(|e| e.to_string())?;
    let name = name.trim();
    let id = match action {
        Action::Create => {
            if !valid_name(name) {
                return Err(format!(
                    "agent id {name:?} must be lowercase letters, digits and single hyphens"
                ));
            }
            if user_roles.get(name).is_some() {
                return Err(format!("agent {name} already exists; use action update"));
            }
            name.to_owned()
        }
        Action::Update => user_roles
            .get(name)
            .or_else(|| loaded.roles.get(name))
            .map(|r| r.id.clone())
            .ok_or_else(|| format!("no agent {name}; use action create"))?,
    };
    roles::refuse_primary(&user_roles, &id, false).map_err(|e| e.to_string())?;
    roles::refuse_primary(&loaded.roles, &id, false).map_err(|e| e.to_string())?;

    let dir = ctx.scope_dir(Scope::User).map_err(|e| e.message)?;
    let path = dir.join(AGENTS_DIR).join(format!("{id}.md"));
    let mut assets = user_assets.clone();
    assets
        .put_agent(&id, content, Origin::User(path.clone()))
        .map_err(|e| format!("the new text does not load: {e}"))?;
    let registry = RoleRegistry::from_assets(&assets)
        .map_err(|e| format!("the roster does not load with the new text: {e}"))?;
    let new_role = registry
        .get(&id)
        .ok_or_else(|| format!("agent {id} does not load from this text"))?;
    roles::refuse_primary(&user_roles, &id, new_role.primary).map_err(|e| e.to_string())?;
    roles::refuse_primary(&loaded.roles, &id, new_role.primary).map_err(|e| e.to_string())?;

    // Never the repo layer's policy: an id only the repo defines is judged as a create.
    let old = user_roles.get(&id).map(|r| &r.policy);
    let widening = roles::permission_widens(old, &new_role.policy, &roles::new_agent_baseline());
    if widening.widens {
        let against = if old.is_some() {
            "its current permissions"
        } else {
            "a new agent's baseline (bash, edit and webfetch denied)"
        };
        return Err(format!(
            "it widens agent {id}'s permissions compared with {against}: {}. Tools may only \
             narrow permissions; a person can widen them with `amp-harness agents edit {id}`",
            widening.reasons.join("; ")
        ));
    }

    let mut notes = Vec::new();
    if let Some(note) = roles::dispatch_effect(registry.manager(), new_role) {
        notes.push(note);
    }
    if let Some(Origin::Repo(p)) = loaded.assets.agent(&id).map(|a| &a.origin) {
        notes.push(format!(
            "a repo file overrides it in this repo ({}); the user-layer change applies elsewhere",
            p.display()
        ));
    }
    let backup = (action == Action::Update && path.is_file()).then(|| {
        dir.join(EDIT_DIR)
            .join(format!("backup/agents/{id}.<unix ms>/{id}.md"))
            .display()
            .to_string()
    });
    Ok(Plan {
        name: id,
        path,
        backup,
        notes,
    })
}

/// The guard for a skill write: the name is valid, the skill exists (update) or not (create), and
/// no primary agent preloads it (that would change the manager's own prompt).
fn check_skill(ctx: &Ctx, action: Action, name: &str) -> Result<Plan, String> {
    let name = name.trim();
    if !valid_name(name) {
        return Err(format!(
            "skill name {name:?} must be lowercase letters, digits and single hyphens"
        ));
    }
    let loaded = ctx.load().map_err(|e| e.message)?;
    let exists = loaded.assets.skill(name).is_some();
    match action {
        Action::Create if exists => {
            return Err(format!("skill {name} already exists; use action update"));
        }
        Action::Update if !exists => return Err(format!("no skill {name}; use action create")),
        _ => {}
    }
    if let Some(role) = loaded
        .roles
        .roles()
        .find(|r| r.primary && r.skills.iter().any(|s| s == name))
    {
        return Err(format!(
            "skill {name} is preloaded by the primary agent {}; tools can't change it (a person \
             can, with `amp-harness skills edit {name}`)",
            role.id
        ));
    }
    let dir = ctx.scope_dir(Scope::User).map_err(|e| e.message)?;
    let path = dir.join(SKILLS_DIR).join(name).join(SKILL_FILE);
    let backup = (action == Action::Update && path.is_file()).then(|| {
        dir.join(EDIT_DIR)
            .join(format!("backup/skills/{name}.<unix ms>/{SKILL_FILE}"))
            .display()
            .to_string()
    });
    let mut notes = Vec::new();
    if let Some(Origin::Repo(p)) = loaded.assets.skill(name).map(|s| s.origin()) {
        notes.push(format!(
            "a repo skill overrides it in this repo ({}); the user-layer change applies elsewhere",
            p.display()
        ));
    }
    Ok(Plan {
        name: name.to_owned(),
        path,
        backup,
        notes,
    })
}

/// Past this many cells (old lines times new lines, after the common prefix and suffix) the diff
/// stops looking for common lines and shows every old line removed and every new line added.
const DIFF_MAX_CELLS: usize = 4_000_000;

/// The confirmation rows for the text being written (AMP #4302). Without a current file (create,
/// or an update whose user-layer file doesn't exist yet): a header and every new line as `+ `.
/// With one: a header with the totals, then only the changed lines, `- ` old and `+ ` new, under
/// `@@ -a,b +c,d @@` hunk headers. No context lines, so the TUI's row cap keeps the changes.
fn content_rows(current: Option<&str>, new: &str) -> Vec<String> {
    let Some(old) = current else {
        let lines: Vec<&str> = new.lines().collect();
        let mut rows = vec![format!("new text ({} lines), in full:", lines.len())];
        rows.extend(lines.iter().map(|l| format!("+ {l}")));
        return rows;
    };
    let old: Vec<&str> = old.lines().collect();
    let new: Vec<&str> = new.lines().collect();
    let hunks = line_diff(&old, &new);
    if hunks.is_empty() {
        return vec!["diff: no change to the text".to_owned()];
    }
    let removed: usize = hunks.iter().map(|h| h.old_len).sum();
    let added: usize = hunks.iter().map(|h| h.new_len).sum();
    let mut rows = vec![format!(
        "diff against the current file: -{removed} +{added} lines, changed lines only:"
    )];
    for h in &hunks {
        rows.push(format!(
            "@@ -{},{} +{},{} @@",
            h.old_start + 1,
            h.old_len,
            h.new_start + 1,
            h.new_len
        ));
        rows.extend(
            old[h.old_start..h.old_start + h.old_len]
                .iter()
                .map(|l| format!("- {l}")),
        );
        rows.extend(
            new[h.new_start..h.new_start + h.new_len]
                .iter()
                .map(|l| format!("+ {l}")),
        );
    }
    rows
}

/// One run of changed lines: `old_len` old lines from `old_start` replaced by `new_len` new lines
/// from `new_start` (0-based).
#[derive(Debug, PartialEq, Eq)]
struct Hunk {
    old_start: usize,
    old_len: usize,
    new_start: usize,
    new_len: usize,
}

/// A line diff by longest common subsequence, after trimming the common prefix and suffix.
fn line_diff(old: &[&str], new: &[&str]) -> Vec<Hunk> {
    let prefix = old.iter().zip(new).take_while(|(a, b)| a == b).count();
    let suffix = old[prefix..]
        .iter()
        .rev()
        .zip(new[prefix..].iter().rev())
        .take_while(|(a, b)| a == b)
        .count();
    let a = &old[prefix..old.len() - suffix];
    let b = &new[prefix..new.len() - suffix];
    if a.is_empty() && b.is_empty() {
        return Vec::new();
    }
    let whole = || {
        vec![Hunk {
            old_start: prefix,
            old_len: a.len(),
            new_start: prefix,
            new_len: b.len(),
        }]
    };
    if a.is_empty() || b.is_empty() || a.len().saturating_mul(b.len()) > DIFF_MAX_CELLS {
        return whole();
    }
    // lcs[i][j]: the LCS length of a[i..] and b[j..].
    let (n, m) = (a.len(), b.len());
    let mut lcs = vec![0u32; (n + 1) * (m + 1)];
    let at = |i: usize, j: usize| i * (m + 1) + j;
    for i in (0..n).rev() {
        for j in (0..m).rev() {
            lcs[at(i, j)] = if a[i] == b[j] {
                lcs[at(i + 1, j + 1)] + 1
            } else {
                lcs[at(i + 1, j)].max(lcs[at(i, j + 1)])
            };
        }
    }
    let mut hunks: Vec<Hunk> = Vec::new();
    let (mut i, mut j) = (0, 0);
    let mut push = |i: usize, j: usize, del: bool| {
        let (oi, nj) = (prefix + i, prefix + j);
        match hunks.last_mut() {
            Some(h) if h.old_start + h.old_len == oi && h.new_start + h.new_len == nj => {
                if del {
                    h.old_len += 1;
                } else {
                    h.new_len += 1;
                }
            }
            _ => hunks.push(Hunk {
                old_start: oi,
                old_len: usize::from(del),
                new_start: nj,
                new_len: usize::from(!del),
            }),
        }
    };
    while i < n || j < m {
        if i < n && j < m && a[i] == b[j] {
            i += 1;
            j += 1;
        } else if j == m || (i < n && lcs[at(i + 1, j)] >= lcs[at(i, j + 1)]) {
            push(i, j, true);
            i += 1;
        } else {
            push(i, j, false);
            j += 1;
        }
    }
    hunks
}

struct ManageTool {
    kind: Kind,
    shared: Arc<Shared>,
    schema: Tool,
}

impl ManageTool {
    fn new(kind: Kind, shared: Arc<Shared>) -> Self {
        let (name, description, content) = match kind {
            Kind::Agent => (
                MANAGE_AGENT_TOOL,
                "Create or update a subagent definition (agents/<id>.md) in the person's user \
                 config. The person is asked to confirm every write in the chat (y/N); \
                 nothing is written without their yes, and the call is refused where no person \
                 can answer. Refused outright: the manager or any primary agent, `mode: \
                 primary`, and any change that widens permissions (a new agent may not allow \
                 bash, edit or webfetch; an update may only add deny rules or remove allow \
                 rules). The previous file is backed up under .edit/backup/agents/. A write \
                 takes effect from your NEXT turn, once live reload picks it up; workers \
                 dispatched after that reload see it too. If auto-reload is off, ask the person \
                 to run /reload.",
                "REQUIRED. The whole agent file: YAML frontmatter (description, mode: subagent, \
                 permission, skills, ...) and the prompt.",
            ),
            Kind::Skill => (
                MANAGE_SKILL_TOOL,
                "Create or update a skill (skills/<name>/SKILL.md) in the person's user config. \
                 The person is asked to confirm every write in the chat (y/N); nothing is \
                 written without their yes, and the call is refused where no person can answer. \
                 A skill a primary agent preloads can't be changed. Only SKILL.md is written; \
                 the previous one is backed up under .edit/backup/skills/. A write takes effect \
                 from your NEXT turn, once live reload picks it up; workers dispatched after \
                 that reload see it too. If auto-reload is off, ask the person to run /reload.",
                "REQUIRED. The whole SKILL.md: YAML frontmatter (name, description) and the body.",
            ),
        };
        let schema = Tool {
            name: name.to_owned(),
            description: description.to_owned(),
            parameters: json!({
                "type": "object",
                "properties": {
                    "action": {"type": "string", "enum": ["create", "update"], "description": "REQUIRED. create a new one or update an existing one."},
                    "name": {"type": "string", "description": format!("REQUIRED. The {} name: lowercase letters, digits and hyphens.", kind.noun())},
                    "content": {"type": "string", "description": content}
                },
                "required": ["action", "name", "content"],
                "additionalProperties": false
            })
            .into(),
            constrained_sampling: None,
        };
        Self {
            kind,
            shared,
            schema,
        }
    }

    fn check(&self, ctx: &Ctx, args: &Args) -> Result<Plan, String> {
        match self.kind {
            Kind::Agent => check_agent(ctx, args.action, &args.name, &args.content),
            Kind::Skill => check_skill(ctx, args.action, &args.name),
        }
    }

    /// The whole call, as text for the model: the written file and what happens next, or why
    /// nothing was written.
    async fn run(&self, args: Args, cancel: &CancellationToken) -> Result<String, String> {
        let not_written = |why: String| format!("not written: {why}");
        // 1. Nobody to ask: refuse before any work.
        let Some(confirmer) = self.shared.confirmer.as_deref() else {
            return Err(Confirmed::Denied(DenyReason::NoResponder)
                .denial_message()
                .unwrap_or_default());
        };
        let ctx = self.shared.ctx();
        // 2. The guard.
        let plan = self.check(&ctx, &args).map_err(not_written)?;

        // 3. The person.
        let mut request = ConfirmRequest::self_management(
            self.schema.name.clone(),
            plan.path.clone(),
            Scope::User.name(),
            format!("{} {} {}", args.action.verb(), self.kind.noun(), plan.name),
        );
        request = request.detail(match &plan.backup {
            Some(b) => format!("the current file is backed up first to {b}"),
            None => "new file (nothing to back up)".to_owned(),
        });
        for note in &plan.notes {
            request = request.detail(note.clone());
        }
        request = request.detail(format!(
            "{} lines; the {} applies from the manager's next turn",
            args.content.lines().count(),
            self.kind.noun()
        ));
        // The text itself, last (AMP #4302): the person's yes is the only gate on prompt
        // content, so they see what they allow. The TUI caps the rows and says how many more.
        let current = match args.action {
            Action::Update => std::fs::read_to_string(&plan.path).ok(),
            Action::Create => None,
        };
        for row in content_rows(current.as_deref(), &args.content) {
            request = request.detail(row);
        }
        let answer = tokio::select! {
            answer = confirm_or_deny(Some(confirmer), request, CONFIRM_TIMEOUT) => answer,
            () = cancel.cancelled() => return Err(not_written("the call was cancelled".into())),
        };
        if let Some(message) = answer.denial_message() {
            return Err(message);
        }

        // 4. Again, then write.
        self.check(&ctx, &args).map_err(not_written)?;
        let create = args.action == Action::Create;
        let (path, backup) = match self.kind {
            Kind::Agent => {
                let w = write_agent_in(&ctx, Scope::User, &plan.name, &args.content, create)
                    .map_err(|e| not_written(e.message))?;
                (w.path, w.backup)
            }
            Kind::Skill => {
                let w = write_skill_in(&ctx, Scope::User, &plan.name, &args.content, create)
                    .map_err(|e| not_written(e.message))?;
                (w.path, w.backup)
            }
        };
        let mut text = format!(
            "{} {} {}: wrote {} (user scope, confirmed by the person).",
            if create { "Created" } else { "Updated" },
            self.kind.noun(),
            plan.name,
            path.display()
        );
        if let Some(b) = backup {
            text.push_str(&format!(
                " The previous file is backed up at {}.",
                b.display()
            ));
        }
        for note in &plan.notes {
            text.push_str(&format!(" Note: {note}."));
        }
        text.push_str(
            " It takes effect from your next turn, once live reload picks it up (reloads wait \
             while you are replying); workers dispatched after that reload see it too. If \
             auto-reload is off, ask the person to run /reload.",
        );
        Ok(text)
    }
}

impl std::fmt::Debug for ManageTool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ManageTool")
            .field("kind", &self.kind)
            .finish_non_exhaustive()
    }
}

#[async_trait::async_trait]
impl AgentTool for ManageTool {
    fn schema(&self) -> &Tool {
        &self.schema
    }

    fn label(&self) -> &str {
        match self.kind {
            Kind::Agent => "Manage agent",
            Kind::Skill => "Manage skill",
        }
    }

    fn execution_mode(&self) -> rpi_agent::ToolExecutionMode {
        // One prompt at a time, and no two writes racing on the same layer.
        rpi_agent::ToolExecutionMode::Sequential
    }

    async fn execute(
        &self,
        _tool_call_id: &str,
        params: Value,
        signal: CancellationToken,
        _on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, ToolError> {
        let args = parse_args(params).map_err(ToolError::Validation)?;
        match self.run(args, &signal).await {
            Ok(text) => Ok(AgentToolResult::text(text)),
            Err(why) => Err(ToolError::Tool(why)),
        }
    }
}

// ------------------------------------------------------------------------------------------------
// amp_add_mcp_server (AMP #4253)
// ------------------------------------------------------------------------------------------------

/// The comment written under the table's header (also in the confirmed preview).
const MCP_NOTE: &str = "Added by the manager (amp_add_mcp_server), confirmed by you.";

/// Where secrets, headers and env go instead: a terminal, typed by the person.
fn secrets_hint(name: &str) -> String {
    format!(
        "secrets, headers and environment variables are never taken by this tool: the person sets \
         them in a terminal with `amp-harness mcp secret {name}` or `amp-harness mcp edit {name}`"
    )
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
enum McpTransport {
    Http,
    Sse,
    Stdio,
}

/// `amp_add_mcp_server`'s arguments. No `scope` (user only), no `force` (add-only), and no
/// headers, env, secrets or OAuth fields: an unknown field is refused.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct McpArgs {
    name: String,
    transport: McpTransport,
    #[serde(default)]
    url: Option<String>,
    #[serde(default)]
    command: Option<String>,
    #[serde(default)]
    args: Vec<String>,
    roles: Vec<String>,
    #[serde(default)]
    dry_run: bool,
}

fn parse_mcp_args(params: Value) -> Result<McpArgs, String> {
    let name = params
        .get("name")
        .and_then(Value::as_str)
        .unwrap_or("<name>")
        .to_owned();
    let args: McpArgs = serde_json::from_value(params).map_err(|e| {
        let e = e.to_string();
        if e.contains("unknown field") {
            // Name the field, never its value (it may be a secret).
            format!("bad arguments: {e}; {}", secrets_hint(&name))
        } else {
            format!("bad arguments: {e}")
        }
    })?;
    if args.name.trim().is_empty() {
        return Err("name is required".into());
    }
    if args.roles.is_empty() || args.roles.iter().any(|r| r.trim().is_empty()) {
        return Err(
            "roles is required: the agent ids that get this server's tools (`[\"*\"]` for every \
             role, including every worker)"
                .into(),
        );
    }
    match args.transport {
        McpTransport::Http | McpTransport::Sse => {
            if args.url.as_deref().is_none_or(|u| u.trim().is_empty()) {
                return Err("an http/sse server needs url".into());
            }
            if args.command.is_some() || !args.args.is_empty() {
                return Err("command and args are for stdio servers".into());
            }
        }
        McpTransport::Stdio => {
            if args.command.as_deref().is_none_or(|c| c.trim().is_empty()) {
                return Err("a stdio server needs command (and optionally args)".into());
            }
            if args.url.is_some() {
                return Err("url is for http/sse servers".into());
            }
        }
    }
    Ok(args)
}

/// What passed the checks: shown to the person, and exactly what gets written.
#[derive(Debug, Clone)]
struct McpPlan {
    name: String,
    /// The user config.toml.
    file: PathBuf,
    /// `[mcp.<name>]` as it will be written.
    table: toml::Table,
    /// The same as TOML text, with [`MCP_NOTE`].
    block: String,
    /// The command line (stdio) or the URL.
    target: String,
    stdio: bool,
}

/// Everything short of writing: the name is free in every layer, the spec has no secrets, roles
/// are explicit, a stdio server is disabled, and the user file would load with the new table.
fn check_mcp(ctx: &Ctx, args: &McpArgs) -> Result<McpPlan, String> {
    let name = args.name.trim();
    mcp_config::check_server_name(name).map_err(|(message, fix)| match fix {
        Some(fix) => format!("{message} ({fix})"),
        None => message,
    })?;
    // No `${env:..}` anywhere the loader expands (AMP #4318): the value would come from the
    // person's environment and go to the server (an http URL is sent on the next reload).
    if let Some(url) = &args.url {
        refuse_env_refs(name, "url", url)?;
    }
    if let Some(command) = &args.command {
        refuse_env_refs(name, "command", command)?;
    }
    for (i, a) in args.args.iter().enumerate() {
        refuse_env_refs(name, &format!("args[{i}]"), a)?;
    }
    // Add-only, and never over a repo server: a user table would shadow it for this person.
    let loaded = McpConfig::load(&ctx.paths);
    let mut layers = Vec::new();
    for scope in [Scope::User, Scope::Repo] {
        if mcp_crud::layer_table(ctx, scope, name)
            .map_err(|e| e.message)?
            .is_some()
        {
            layers.push(scope.name());
        }
    }
    if !layers.is_empty() || loaded.servers.contains_key(name) || loaded.broken.contains(name) {
        let where_ = if layers.is_empty() {
            "the config".to_owned()
        } else {
            format!("the {} config", layers.join(" and "))
        };
        return Err(format!(
            "MCP server `{name}` already exists in {where_}; this tool only adds new servers. A \
             person can change it with `amp-harness mcp edit {name}` in a terminal"
        ));
    }

    let spec = match args.transport {
        McpTransport::Http | McpTransport::Sse => mcp_crud::http_spec(
            name,
            args.url.as_deref().unwrap_or_default().trim(),
            args.transport == McpTransport::Sse,
        ),
        McpTransport::Stdio => mcp_crud::stdio_spec(
            name,
            args.command.as_deref().unwrap_or_default().trim(),
            &args.args,
        ),
    }
    .map_err(|e| format!("{}; {}", e.message, secrets_hint(name)))?;
    // The builders above take no secret flags; make sure nothing would prompt or hit the keychain.
    if !spec.secrets.is_empty() {
        return Err(secrets_hint(name));
    }
    let mut table = spec.table;
    // The same over everything the builders produced, in case one ever derives another field.
    refuse_env_refs_in(name, "", &toml::Value::Table(table.clone()))?;
    table.insert(
        "roles".into(),
        toml::Value::Array(
            args.roles
                .iter()
                .map(|r| toml::Value::String(r.trim().to_owned()))
                .collect(),
        ),
    );
    let stdio = args.transport == McpTransport::Stdio;
    if stdio {
        // Registering never runs anything: the person starts it with `/mcp enable`.
        table.insert("enabled".into(), toml::Value::Boolean(false));
    }

    let value = toml::Value::Table(table.clone());
    let origin = mcp_crud::origin(ctx, Scope::User).map_err(|e| e.message)?;
    let (server, _) = mcp_crud::parse_table(name, &value, origin)
        .map_err(|e| format!("{}; {}", e.message, secrets_hint(name)))?;
    let file = mcp_crud::config_file(ctx, Scope::User).map_err(|e| e.message)?;
    let block = mcp_crud::mcp_block(name, &value, Some(MCP_NOTE)).map_err(|e| e.message)?;
    let check = mcp_crud::layer_check(
        Scope::User,
        file.path.clone(),
        name.to_owned(),
        Expect::Present,
    );
    let preview = mcp_crud::preview_replace(&file, &format!("mcp.{name}"), Some(&block))?;
    check(&preview)
        .map_err(|e| format!("{}: the change would not load ({e})", file.path.display()))?;
    Ok(McpPlan {
        name: name.to_owned(),
        file: file.path.clone(),
        table,
        block,
        target: mcp_crud::target(&server),
        stdio,
    })
}

/// Refuses a value that would read the environment once written and loaded: `${env:VAR}` /
/// `${env:VAR:-default}` (the only template the loader fills from the environment;
/// [`mcp_config::parse_template`] is what it expands with), and the Claude-style `${VAR}` /
/// `${VAR:-default}` that the builders rewrite into it ([`mcp_crud::env_refs`]). `$${` is a literal
/// and passes; `${home}`, `${repo}`, `${worktree}` and `${config_dir}` are paths, not secrets, and
/// stay. The error names the field, never the value. A value that does not parse is left to the
/// loader's own checks, which refuse it.
fn refuse_env_refs(name: &str, field: &str, value: &str) -> Result<(), String> {
    let reads_env = mcp_config::parse_template(&mcp_crud::env_refs(value)).is_ok_and(|pieces| {
        pieces
            .iter()
            .any(|p| matches!(p, mcp_config::Piece::Env { .. }))
    });
    if reads_env {
        return Err(format!(
            "{field} references an environment variable (`${{env:..}}`), which this tool never \
             writes: the server would be handed that value from the person's environment; {}",
            secrets_hint(name)
        ));
    }
    Ok(())
}

/// [`refuse_env_refs`] over every string in a TOML value; `field` is its dotted path.
fn refuse_env_refs_in(name: &str, field: &str, value: &toml::Value) -> Result<(), String> {
    let at = |key: &str| {
        if field.is_empty() {
            key.to_owned()
        } else {
            format!("{field}.{key}")
        }
    };
    match value {
        toml::Value::String(s) => refuse_env_refs(name, field, s),
        toml::Value::Array(items) => items
            .iter()
            .enumerate()
            .try_for_each(|(i, v)| refuse_env_refs_in(name, &format!("{field}[{i}]"), v)),
        toml::Value::Table(t) => t
            .iter()
            .try_for_each(|(k, v)| refuse_env_refs_in(name, &at(k), v)),
        _ => Ok(()),
    }
}

/// `amp_add_mcp_server`: adds an http, sse or stdio MCP server to the person's user config.toml
/// after their "yes", with the same gates as [`ManageTool`]: refused with no confirmer, checked
/// ([`check_mcp`]), confirmed with the exact TOML block on screen, checked again, then written by
/// [`mcp_crud::add_server_in`] (add-only, under the config lock, `config.toml.bak` first). stdio
/// servers are written disabled. Secrets are never accepted: the person sets them in a terminal.
struct AddMcpTool {
    shared: Arc<Shared>,
    schema: Tool,
}

impl std::fmt::Debug for AddMcpTool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("AddMcpTool").finish_non_exhaustive()
    }
}

impl AddMcpTool {
    fn new(shared: Arc<Shared>) -> Self {
        let schema = Tool {
            name: ADD_MCP_TOOL.to_owned(),
            description: "Add a NEW MCP server to the person's user config (config.toml). Call it \
                only when the person explicitly asked for that server. The person is asked to \
                confirm in the chat (y/N) with the exact TOML block on screen; nothing is \
                written without their yes, and the call is refused where no person can answer. \
                Add-only: a name that exists in any config is refused (changes are made by the \
                person with `amp-harness mcp edit <name>`). Never ask for, and never pass, \
                tokens, API keys, headers or environment values: this tool has no field for \
                them, and an environment reference (${env:VAR}) in url, command or args is refused; \
                the person sets them in a terminal with `amp-harness mcp secret <name>`. \
                stdio servers are added DISABLED (the person starts one with `/mcp enable \
                <name>`). The server appears after your current reply ends, once live reload \
                picks it up (or after /reload if auto-reload is off); http servers that need a \
                login need `/mcp login <name>`. dry_run returns the block without asking or \
                writing."
                .to_owned(),
            parameters: json!({
                "type": "object",
                "properties": {
                    "name": {"type": "string", "description": "REQUIRED. Server name: lowercase letters, digits and hyphens (up to 32); amp_* is reserved."},
                    "transport": {"type": "string", "enum": ["http", "sse", "stdio"], "description": "REQUIRED. http (streamable HTTP), sse (legacy HTTP+SSE) or stdio (a local command)."},
                    "url": {"type": "string", "description": "http/sse only, REQUIRED there. The server URL, with no credentials in it. Environment references (${env:VAR}) are refused."},
                    "command": {"type": "string", "description": "stdio only, REQUIRED there. The program to run, e.g. npx. Environment references (${env:VAR}) are refused."},
                    "args": {"type": "array", "items": {"type": "string"}, "description": "stdio only. The command's arguments, with no secrets in them. Environment references (${env:VAR}) are refused."},
                    "roles": {"type": "array", "items": {"type": "string"}, "minItems": 1, "description": "REQUIRED. The agent ids that get this server's tools; [\"*\"] means every role, workers included."},
                    "dry_run": {"type": "boolean", "description": "Optional. Return the TOML block and file without asking or writing."}
                },
                "required": ["name", "transport", "roles"],
                "additionalProperties": false
            })
            .into(),
            constrained_sampling: None,
        };
        Self { shared, schema }
    }

    async fn run(&self, args: McpArgs, cancel: &CancellationToken) -> Result<String, String> {
        let not_written = |why: String| format!("not written: {why}");
        // 1. Nobody to ask: refuse before any work (dry runs too).
        let Some(confirmer) = self.shared.confirmer.as_deref() else {
            return Err(Confirmed::Denied(DenyReason::NoResponder)
                .denial_message()
                .unwrap_or_default());
        };
        let ctx = self.shared.ctx();
        // 2. The checks.
        let plan = check_mcp(&ctx, &args).map_err(not_written)?;
        if args.dry_run {
            return Ok(format!(
                "Dry run, nothing written and nothing asked. This block would be added to {} \
                 (user scope):\n{}",
                plan.file.display(),
                plan.block
            ));
        }

        // 3. The person.
        let kind = if plan.stdio {
            "stdio"
        } else if args.transport == McpTransport::Sse {
            "sse"
        } else {
            "http"
        };
        let mut request = ConfirmRequest::self_management(
            ADD_MCP_TOOL,
            plan.file.clone(),
            Scope::User.name(),
            format!("add {kind} MCP server {}", plan.name),
        );
        if plan.stdio {
            request = request.detail(format!("command: {}", plan.target)).detail(
                "added DISABLED: it runs on your machine, with your environment, once you \
                     `/mcp enable` it"
                    .to_owned(),
            );
        } else {
            request = request.detail(format!(
                "url: {}; it connects on the next reload (a login: `/mcp login {}`)",
                plan.target, plan.name
            ));
        }
        let roles = args.roles.join(", ");
        request = request.detail(if args.roles.iter().any(|r| r.trim() == "*") {
            format!("roles: {roles} (every agent, workers included, gets its tools)")
        } else {
            format!("roles: {roles}")
        });
        request = request.detail("the block to add:".to_owned());
        for line in plan.block.lines() {
            request = request.detail(format!("  {line}"));
        }
        let answer = tokio::select! {
            answer = confirm_or_deny(Some(confirmer), request, CONFIRM_TIMEOUT) => answer,
            () = cancel.cancelled() => return Err(not_written("the call was cancelled".into())),
        };
        if let Some(message) = answer.denial_message() {
            return Err(message);
        }

        // 4. Again (the config may have changed while the prompt was up), then write exactly
        // what was shown. add_server_in re-checks under the config lock and never replaces.
        let again = check_mcp(&ctx, &args).map_err(not_written)?;
        if again.block != plan.block {
            return Err(not_written(
                "the config changed while the person was asked; ask again".into(),
            ));
        }
        let added = mcp_crud::add_server_in(
            &ctx,
            &plan.name,
            Scope::User,
            NewServer {
                table: plan.table.clone(),
                secrets: Vec::new(),
            },
            &AddOpts {
                force: false,
                no_test: true,
                secret_stdin: false,
                json: false,
                note: MCP_NOTE.into(),
                quiet: true,
            },
        )
        .map_err(|e| not_written(e.message))?;

        let mut text = format!(
            "Added {kind} MCP server `{}` to {} (user scope, confirmed by the person), roles: {roles}.",
            plan.name,
            added.file.display()
        );
        if let Some(b) = &added.backup {
            text.push_str(&format!(
                " The previous file is backed up at {}.",
                b.display()
            ));
        }
        for w in &added.warnings {
            text.push_str(&format!(" Warning: {w}."));
        }
        if plan.stdio {
            text.push_str(&format!(
                " It is DISABLED: tell the person to run `/mcp enable {}` to start it.",
                plan.name
            ));
        } else {
            text.push_str(&format!(
                " If it needs a login, the person runs `/mcp login {}`.",
                plan.name
            ));
        }
        text.push_str(&format!(
            " Any token, key or header is set by the person in a terminal: `amp-harness mcp \
             secret {name}` or `amp-harness mcp edit {name}`. The server appears after your \
             current reply ends, once live reload picks it up; if auto-reload is off, ask the \
             person to run /reload.",
            name = plan.name
        ));
        Ok(text)
    }
}

#[async_trait::async_trait]
impl AgentTool for AddMcpTool {
    fn schema(&self) -> &Tool {
        &self.schema
    }

    fn label(&self) -> &str {
        "Add MCP server"
    }

    fn execution_mode(&self) -> rpi_agent::ToolExecutionMode {
        rpi_agent::ToolExecutionMode::Sequential
    }

    async fn execute(
        &self,
        _tool_call_id: &str,
        params: Value,
        signal: CancellationToken,
        _on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, ToolError> {
        let args = parse_mcp_args(params).map_err(ToolError::Validation)?;
        match self.run(args, &signal).await {
            Ok(text) => Ok(AgentToolResult::text(text)),
            Err(why) => Err(ToolError::Tool(why)),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        path::Path,
        sync::atomic::{AtomicUsize, Ordering},
    };

    /// A user layer only, under the crate's target dir (never the person's config), whose config
    /// gives every role a faux model.
    fn ctx(name: &str) -> Ctx {
        let d = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/unit-manage-tools")
            .join(name);
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        std::fs::write(
            d.join("config.toml"),
            "[providers.f]\napi = \"faux\"\n[providers.f.models.\"f-1\"]\n\n[roles.manager]\nmodel = \"f/f-1\"\n[roles.worker-backend]\nmodel = \"f/f-1\"\n[roles.sql-expert]\nmodel = \"f/f-1\"\n",
        )
        .unwrap();
        Ctx {
            paths: ConfigPaths {
                user_file: Some(d.join("config.toml")),
                user_dir: Some(d.clone()),
                ..ConfigPaths::default()
            },
            cwd: d,
        }
    }

    /// Answers every request with `yes` and counts them.
    struct Fake {
        yes: bool,
        asked: AtomicUsize,
        last: std::sync::Mutex<Option<ConfirmRequest>>,
    }

    impl Fake {
        fn new(yes: bool) -> Arc<Self> {
            Arc::new(Self {
                yes,
                asked: AtomicUsize::new(0),
                last: std::sync::Mutex::new(None),
            })
        }
    }

    #[async_trait::async_trait]
    impl Confirmer for Fake {
        async fn confirm(&self, request: ConfirmRequest) -> Confirmed {
            self.asked.fetch_add(1, Ordering::SeqCst);
            *self.last.lock().unwrap() = Some(request);
            if self.yes {
                Confirmed::Approved
            } else {
                Confirmed::Denied(DenyReason::Declined)
            }
        }
    }

    fn tool(ctx: &Ctx, kind: Kind, confirmer: Option<Arc<dyn Confirmer>>) -> ManageTool {
        ManageTool::new(
            kind,
            Arc::new(Shared {
                paths: ctx.paths.clone(),
                cwd: ctx.cwd.clone(),
                confirmer,
            }),
        )
    }

    async fn call(t: &ManageTool, args: Value) -> Result<String, String> {
        let result = t
            .execute(
                "id",
                args,
                CancellationToken::new(),
                Arc::new(|_: ToolResultPartial| {}),
            )
            .await;
        match result {
            Ok(r) => Ok(serde_json::to_string(&r.into_content()).unwrap()),
            Err(e) => Err(e.to_string()),
        }
    }

    /// A subagent that denies bash, edit and webfetch (the new-agent baseline), plus `extra`.
    fn narrow_agent(description: &str, extra: &str) -> String {
        format!(
            "---\ndescription: {description}\nmode: subagent\npermission:\n  bash:\n    \"*\": deny\n  edit:\n    \"*\": deny\n  webfetch:\n    \"*\": deny\n{extra}---\n\nYou answer SQL questions.\n"
        )
    }

    fn args(action: &str, name: &str, content: &str) -> Value {
        json!({"action": action, "name": name, "content": content})
    }

    #[tokio::test]
    async fn confirmed_create_and_update_write_and_the_next_load_sees_them() {
        let ctx = ctx("agent-confirmed");
        let yes = Fake::new(true);
        let t = tool(&ctx, Kind::Agent, Some(yes.clone()));
        let file = ctx
            .paths
            .user_dir
            .clone()
            .unwrap()
            .join("agents/sql-expert.md");

        let out = call(&t, args("create", "sql-expert", &narrow_agent("SQL", "")))
            .await
            .unwrap();
        assert!(out.contains("Created agent sql-expert"), "{out}");
        assert!(
            out.contains("next turn") && out.contains("/reload"),
            "{out}"
        );
        assert_eq!(yes.asked.load(Ordering::SeqCst), 1);
        let req = yes.last.lock().unwrap().clone().unwrap();
        assert_eq!(req.tool, MANAGE_AGENT_TOOL);
        assert_eq!(req.path, file);
        assert_eq!(req.scope, "user");
        assert!(
            req.details.iter().any(|d| d.contains("new file")),
            "{req:?}"
        );
        // The person sees the text they approve (#4302), after the notes.
        assert!(
            req.details
                .iter()
                .any(|d| d == "+ You answer SQL questions."),
            "{req:?}"
        );
        assert!(
            req.details.iter().any(|d| d == "+ description: SQL"),
            "{req:?}"
        );
        // The next run (a fresh load of the layers) sees it.
        assert_eq!(
            ctx.load()
                .unwrap()
                .roles
                .get("sql-expert")
                .unwrap()
                .description,
            "SQL"
        );

        // update: narrowing more (an added deny) is allowed, and backs the old file up.
        let narrower = narrow_agent("SQL v2", "  task:\n    \"*\": deny\n");
        let out = call(&t, args("update", "sql-expert", &narrower))
            .await
            .unwrap();
        assert!(out.contains("Updated agent sql-expert"), "{out}");
        assert!(out.contains(".edit/backup/agents/sql-expert."), "{out}");
        let req = yes.last.lock().unwrap().clone().unwrap();
        assert!(
            req.details.iter().any(|d| d.contains("backed up first")),
            "{req:?}"
        );
        // update: only the changed lines, old and new (#4302).
        assert!(
            req.details.iter().any(|d| d == "- description: SQL"),
            "{req:?}"
        );
        assert!(
            req.details.iter().any(|d| d == "+ description: SQL v2"),
            "{req:?}"
        );
        assert!(req.details.iter().any(|d| d == "+   task:"), "{req:?}");
        assert!(
            !req.details
                .iter()
                .any(|d| d.contains("You answer SQL questions")),
            "unchanged lines are left out: {req:?}"
        );
        assert_eq!(std::fs::read_to_string(&file).unwrap(), narrower);
        assert_eq!(
            ctx.load()
                .unwrap()
                .roles
                .get("sql-expert")
                .unwrap()
                .description,
            "SQL v2"
        );
    }

    #[tokio::test]
    async fn no_or_no_confirmer_writes_nothing() {
        let ctx = ctx("agent-unconfirmed");
        let file = ctx
            .paths
            .user_dir
            .clone()
            .unwrap()
            .join("agents/sql-expert.md");
        let text = narrow_agent("SQL", "");

        // The person says no.
        let no = Fake::new(false);
        let e = call(
            &tool(&ctx, Kind::Agent, Some(no.clone())),
            args("create", "sql-expert", &text),
        )
        .await
        .unwrap_err();
        assert!(e.contains("not written") && e.contains("declined"), "{e}");
        assert_eq!(no.asked.load(Ordering::SeqCst), 1);
        assert!(!file.exists());

        // Headless: refused before anything else (even a bad name is not looked at).
        for a in [
            args("create", "sql-expert", &text),
            args("create", "Bad Name", &text),
        ] {
            let e = call(&tool(&ctx, Kind::Agent, None), a).await.unwrap_err();
            assert!(e.contains("no person can confirm it here"), "{e}");
        }
        assert!(!file.exists());

        // Skills too.
        let skill = ctx.paths.user_dir.clone().unwrap().join("skills/my-skill");
        let md = "---\nname: my-skill\ndescription: Does things\n---\n\nDo it.\n";
        let e = call(
            &tool(&ctx, Kind::Skill, None),
            args("create", "my-skill", md),
        )
        .await
        .unwrap_err();
        assert!(e.contains("no person can confirm"), "{e}");
        let e = call(
            &tool(&ctx, Kind::Skill, Some(no.clone())),
            args("create", "my-skill", md),
        )
        .await
        .unwrap_err();
        assert!(e.contains("declined"), "{e}");
        assert!(!skill.exists());
    }

    #[tokio::test]
    async fn primary_and_manager_edits_are_refused_without_asking() {
        let ctx = ctx("agent-primary");
        let yes = Fake::new(true);
        let t = tool(&ctx, Kind::Agent, Some(yes.clone()));
        let manager = ctx.load().unwrap().roles.manager().id.clone();
        for name in [manager.as_str(), "manager"] {
            let e = call(&t, args("update", name, &narrow_agent("x", "")))
                .await
                .unwrap_err();
            assert!(e.contains("is the manager"), "{e}");
        }
        // Becoming primary.
        let primary = narrow_agent("x", "").replace("mode: subagent", "mode: primary");
        let e = call(&t, args("create", "sql-expert", &primary))
            .await
            .unwrap_err();
        assert!(e.contains("primary"), "{e}");
        assert_eq!(yes.asked.load(Ordering::SeqCst), 0);
        assert!(!ctx.paths.user_dir.clone().unwrap().join("agents").exists());
    }

    #[tokio::test]
    async fn widening_writes_are_refused_without_asking() {
        let ctx = ctx("agent-widening");
        let yes = Fake::new(true);
        let t = tool(&ctx, Kind::Agent, Some(yes.clone()));
        let file = ctx
            .paths
            .user_dir
            .clone()
            .unwrap()
            .join("agents/sql-expert.md");

        // A new agent with no permission block gets bash/edit/webfetch: wider than the baseline.
        let open = "---\ndescription: SQL\nmode: subagent\n---\n\nHi.\n";
        let e = call(&t, args("create", "sql-expert", open))
            .await
            .unwrap_err();
        assert!(e.contains("widens") && e.contains("bash"), "{e}");
        assert!(!file.exists());

        // An existing agent: allowing bash it used to deny.
        call(&t, args("create", "sql-expert", &narrow_agent("SQL", "")))
            .await
            .unwrap();
        let before = std::fs::read_to_string(&file).unwrap();
        let asked = yes.asked.load(Ordering::SeqCst);
        let wider = narrow_agent("SQL", "").replacen("\"*\": deny", "\"*\": allow", 1);
        let e = call(&t, args("update", "sql-expert", &wider))
            .await
            .unwrap_err();
        assert!(e.contains("widens"), "{e}");
        assert_eq!(yes.asked.load(Ordering::SeqCst), asked);
        assert_eq!(std::fs::read_to_string(&file).unwrap(), before);

        // A built-in worker: dropping its permission block widens too.
        let e = call(&t, args("update", "amp-worker-backend", open))
            .await
            .unwrap_err();
        assert!(e.contains("widens"), "{e}");
        assert!(
            !ctx.paths
                .user_dir
                .clone()
                .unwrap()
                .join("agents/amp-worker-backend.md")
                .exists()
        );
    }

    /// [`ctx`] plus a repo layer (`<d>/repo/.amp/harness`, never trusted) holding `agents`.
    fn ctx_with_repo(name: &str, agents: &[(&str, &str)]) -> Ctx {
        let mut ctx = ctx(name);
        let repo = ctx.cwd.join("repo/.amp/harness");
        std::fs::create_dir_all(repo.join("agents")).unwrap();
        for (id, text) in agents {
            std::fs::write(repo.join(format!("agents/{id}.md")), text).unwrap();
        }
        ctx.paths.repo_dir = Some(repo);
        ctx
    }

    /// A subagent that allows bash, edit and webfetch and can be dispatched.
    const WIDE_AGENT: &str = "---\ndescription: Helps\nmode: subagent\ndispatch: true\npermission:\n  bash:\n    \"*\": allow\n  edit:\n    \"*\": allow\n  webfetch:\n    \"*\": allow\n---\n\nYou help.\n";

    // #4316: a repo layer's wide agent must not become the baseline for a user-layer write.
    #[tokio::test]
    async fn a_repo_only_wide_agent_is_not_laundered_into_the_user_layer() {
        let ctx = ctx_with_repo("agent-repo-launder", &[("helper", WIDE_AGENT)]);
        // The repo layer is loaded (every file, trusted or not) with its wide policy.
        assert!(ctx.load().unwrap().roles.get("helper").is_some());
        let yes = Fake::new(true);
        let t = tool(&ctx, Kind::Agent, Some(yes.clone()));
        let user_agents = ctx.paths.user_dir.clone().unwrap().join("agents");

        for action in ["update", "create"] {
            let e = call(&t, args(action, "helper", WIDE_AGENT))
                .await
                .unwrap_err();
            assert!(
                e.contains("widens") && e.contains("baseline") && e.contains("bash"),
                "{action}: {e}"
            );
        }
        assert_eq!(yes.asked.load(Ordering::SeqCst), 0);
        assert!(!user_agents.join("helper.md").exists());

        // A narrow user-layer copy is still allowed (judged against the baseline).
        call(&t, args("update", "helper", &narrow_agent("Helps", "")))
            .await
            .unwrap();
        assert!(user_agents.join("helper.md").is_file());
    }

    // #4316: a built-in the repo layer widens is judged against the built-in's own policy.
    #[tokio::test]
    async fn a_built_in_widened_in_the_repo_is_judged_against_the_built_in() {
        let ctx = ctx_with_repo("agent-repo-builtin", &[("amp-worker-backend", WIDE_AGENT)]);
        let yes = Fake::new(true);
        let t = tool(&ctx, Kind::Agent, Some(yes.clone()));
        let e = call(&t, args("update", "amp-worker-backend", WIDE_AGENT))
            .await
            .unwrap_err();
        assert!(
            e.contains("widens") && e.contains("its current permissions"),
            "{e}"
        );
        assert_eq!(yes.asked.load(Ordering::SeqCst), 0);
        assert!(
            !ctx.paths
                .user_dir
                .clone()
                .unwrap()
                .join("agents/amp-worker-backend.md")
                .exists()
        );
    }

    #[tokio::test]
    async fn skills_create_and_update_with_confirmation() {
        let ctx = ctx("skill-confirmed");
        let yes = Fake::new(true);
        let t = tool(&ctx, Kind::Skill, Some(yes.clone()));
        let md = "---\nname: my-skill\ndescription: Does things\n---\n\nDo it.\n";
        let out = call(&t, args("create", "my-skill", md)).await.unwrap();
        assert!(out.contains("Created skill my-skill"), "{out}");
        let req = yes.last.lock().unwrap().clone().unwrap();
        assert!(req.details.iter().any(|d| d == "+ Do it."), "{req:?}");
        assert_eq!(
            ctx.load_assets()
                .unwrap()
                .skill("my-skill")
                .unwrap()
                .description,
            "Does things"
        );
        let md2 = md.replace("Does things", "Does more");
        let out = call(&t, args("update", "my-skill", &md2)).await.unwrap();
        assert!(out.contains(".edit/backup/skills/my-skill."), "{out}");
        let req = yes.last.lock().unwrap().clone().unwrap();
        assert!(
            req.details
                .iter()
                .any(|d| d == "- description: Does things"),
            "{req:?}"
        );
        assert!(
            req.details.iter().any(|d| d == "+ description: Does more"),
            "{req:?}"
        );
        assert!(!req.details.iter().any(|d| d == "+ Do it."), "{req:?}");
        assert_eq!(
            ctx.load_assets()
                .unwrap()
                .skill("my-skill")
                .unwrap()
                .description,
            "Does more"
        );
        // create on an existing one, update on a missing one: refused before asking.
        let asked = yes.asked.load(Ordering::SeqCst);
        assert!(
            call(&t, args("create", "my-skill", md))
                .await
                .unwrap_err()
                .contains("already exists")
        );
        assert!(
            call(&t, args("update", "nope", md))
                .await
                .unwrap_err()
                .contains("no skill nope")
        );
        assert_eq!(yes.asked.load(Ordering::SeqCst), asked);
    }

    #[test]
    fn content_rows_show_new_text_or_only_changed_lines() {
        let rows = content_rows(None, "a\nb\n");
        assert_eq!(rows[1..], ["+ a", "+ b"]);
        assert!(rows[0].contains("2 lines"), "{rows:?}");

        let rows = content_rows(Some("a\nb\nc\nd\ne\n"), "a\nB\nc\nd\ne\nf\n");
        assert_eq!(
            rows[1..],
            ["@@ -2,1 +2,1 @@", "- b", "+ B", "@@ -6,0 +6,1 @@", "+ f"]
        );
        assert!(rows[0].contains("-1 +2"), "{rows:?}");
        assert_eq!(
            content_rows(Some("x\n"), "x\n"),
            ["diff: no change to the text"]
        );
        // A pure deletion in the middle.
        assert_eq!(
            content_rows(Some("a\nb\nc\n"), "a\nc\n")[1..],
            ["@@ -2,1 +2,0 @@", "- b"]
        );
    }

    #[tokio::test]
    async fn skills_a_primary_preloads_are_refused() {
        let ctx = ctx("skill-primary");
        let user = ctx.paths.user_dir.clone().unwrap();
        // Test setup, written directly: a skill and a primary agent that preloads it.
        let md = "---\nname: boss-notes\ndescription: x\n---\n\nx\n";
        std::fs::create_dir_all(user.join("skills/boss-notes")).unwrap();
        std::fs::write(user.join("skills/boss-notes/SKILL.md"), md).unwrap();
        std::fs::create_dir_all(user.join("agents")).unwrap();
        std::fs::write(
            user.join("agents/boss.md"),
            "---\ndescription: Boss\nmode: primary\nskills:\n  - boss-notes\n---\n\nBoss.\n",
        )
        .unwrap();
        assert!(ctx.load().unwrap().roles.get("boss").unwrap().primary);

        let yes = Fake::new(true);
        let t = tool(&ctx, Kind::Skill, Some(yes.clone()));
        let e = call(
            &t,
            args("update", "boss-notes", &md.replace("x\n\nx", "y\n\ny")),
        )
        .await
        .unwrap_err();
        assert!(e.contains("preloaded by the primary agent boss"), "{e}");
        assert_eq!(yes.asked.load(Ordering::SeqCst), 0);
        assert_eq!(
            std::fs::read_to_string(user.join("skills/boss-notes/SKILL.md")).unwrap(),
            md
        );
        // And the primary itself can't be edited by the agent tool.
        let a = tool(&ctx, Kind::Agent, Some(yes.clone()));
        let e = call(&a, args("update", "boss", &narrow_agent("x", "")))
            .await
            .unwrap_err();
        assert!(e.contains("primary agent"), "{e}");
    }

    #[test]
    fn arguments_deny_unknown_fields_and_have_no_scope() {
        assert!(parse_args(args("create", "a", "x")).is_ok());
        let mut with_scope = args("create", "a", "x");
        with_scope["scope"] = json!("repo");
        assert!(
            parse_args(with_scope)
                .unwrap_err()
                .contains("unknown field")
        );
        assert!(parse_args(args("delete", "a", "x")).is_err());
        assert!(parse_args(args("create", " ", "x")).is_err());
        assert!(parse_args(json!({"action": "create", "name": "a"})).is_err());
    }

    #[test]
    fn tools_are_never_replayed() {
        let ctx = ctx("replay");
        let ts = tools(ctx.paths.clone(), ctx.cwd.clone(), None);
        let names: Vec<_> = ts.iter().map(|t| t.tool.schema().name.clone()).collect();
        assert_eq!(names, [MANAGE_AGENT_TOOL, MANAGE_SKILL_TOOL, ADD_MCP_TOOL]);
        assert!(ts.iter().all(|t| t.replay == ToolReplay::Never));
    }

    // -------------------------------------------------------------------------------------------
    // amp_add_mcp_server (#4253)

    /// [`ctx`] plus a repo layer (`<d>/repo/.amp/harness`), both disposable.
    fn mcp_ctx(name: &str) -> Ctx {
        let mut c = ctx(name);
        let repo = c.cwd.join("repo/.amp/harness");
        std::fs::create_dir_all(&repo).unwrap();
        c.paths.repo_dir = Some(repo);
        c
    }

    fn mcp_tool(ctx: &Ctx, confirmer: Option<Arc<dyn Confirmer>>) -> AddMcpTool {
        AddMcpTool::new(Arc::new(Shared {
            paths: ctx.paths.clone(),
            cwd: ctx.cwd.clone(),
            confirmer,
        }))
    }

    async fn mcp_call(t: &AddMcpTool, args: Value) -> Result<String, String> {
        match t
            .execute(
                "id",
                args,
                CancellationToken::new(),
                Arc::new(|_: ToolResultPartial| {}),
            )
            .await
        {
            Ok(r) => Ok(serde_json::to_string(&r.into_content()).unwrap()),
            Err(e) => Err(e.to_string()),
        }
    }

    fn user_text(ctx: &Ctx) -> String {
        std::fs::read_to_string(ctx.paths.user_file.as_ref().unwrap()).unwrap()
    }

    #[tokio::test]
    async fn confirmed_http_add_lands_in_the_user_config_and_the_next_load() {
        let ctx = mcp_ctx("mcp-http");
        let yes = Fake::new(true);
        let t = mcp_tool(&ctx, Some(yes.clone()));
        let watched = crate::reload::fingerprint(&ctx.paths);
        let out = mcp_call(
            &t,
            json!({"name": "docs", "transport": "http", "url": "https://docs.example/mcp", "roles": ["amp-manager"]}),
        )
        .await
        .unwrap();
        assert!(out.contains("Added http MCP server `docs`"), "{out}");
        assert!(out.contains("after your current reply ends"), "{out}");
        assert!(out.contains("amp-harness mcp secret docs"), "{out}");

        // What the person saw: the file, user scope, the url, the roles and the exact block.
        assert_eq!(yes.asked.load(Ordering::SeqCst), 1);
        let req = yes.last.lock().unwrap().clone().unwrap();
        assert_eq!(req.tool, ADD_MCP_TOOL);
        assert_eq!(&req.path, ctx.paths.user_file.as_ref().unwrap());
        assert_eq!(req.scope, "user");
        assert!(
            req.details
                .iter()
                .any(|d| d.contains("https://docs.example/mcp")),
            "{req:?}"
        );
        assert!(
            req.details.iter().any(|d| d == "roles: amp-manager"),
            "{req:?}"
        );
        assert!(
            req.details.iter().any(|d| d.trim() == "[mcp.docs]"),
            "{req:?}"
        );

        // Written to the user file (the rest of it kept): the reload watcher sees the change, and
        // what a reload reads has the server.
        assert_ne!(crate::reload::fingerprint(&ctx.paths), watched);
        let text = user_text(&ctx);
        assert!(
            text.contains("[roles.manager]") && text.contains("[mcp.docs]"),
            "{text}"
        );
        assert!(text.contains(MCP_NOTE), "{text}");
        let loaded = McpConfig::load(&ctx.paths);
        let s = &loaded.servers["docs"];
        assert!(s.enabled);
        assert_eq!(s.roles, ["amp-manager"]);
        assert_eq!(mcp_crud::target(s), "https://docs.example/mcp");
    }

    #[tokio::test]
    async fn confirmed_stdio_add_is_written_disabled_and_shows_the_command() {
        let ctx = mcp_ctx("mcp-stdio");
        let yes = Fake::new(true);
        let t = mcp_tool(&ctx, Some(yes.clone()));
        let out = mcp_call(
            &t,
            json!({"name": "files", "transport": "stdio", "command": "npx", "args": ["-y", "@acme/files-mcp", "${home}/files"], "roles": ["*"]}),
        )
        .await
        .unwrap();
        assert!(out.contains("Added stdio MCP server `files`"), "{out}");
        assert!(out.contains("/mcp enable files"), "{out}");
        let req = yes.last.lock().unwrap().clone().unwrap();
        assert!(
            req.details
                .iter()
                .any(|d| d == "command: npx -y @acme/files-mcp ${home}/files"),
            "{req:?}"
        );
        assert!(
            req.details.iter().any(|d| d.contains("DISABLED")),
            "{req:?}"
        );
        assert!(
            req.details.iter().any(|d| d.contains("workers included")),
            "{req:?}"
        );
        assert!(
            req.details.iter().any(|d| d.trim() == "enabled = false"),
            "{req:?}"
        );

        let loaded = McpConfig::load(&ctx.paths);
        let s = &loaded.servers["files"];
        assert!(!s.enabled, "a stdio server is registered, never started");
        assert_eq!(s.roles, ["*"]);
        assert!(user_text(&ctx).contains("enabled = false"));
    }

    #[tokio::test]
    async fn declined_headless_and_dry_runs_write_nothing() {
        let ctx = mcp_ctx("mcp-unconfirmed");
        let before = user_text(&ctx);
        let a = json!({"name": "docs", "transport": "http", "url": "https://docs.example/mcp", "roles": ["amp-manager"]});

        let no = Fake::new(false);
        let e = mcp_call(&mcp_tool(&ctx, Some(no.clone())), a.clone())
            .await
            .unwrap_err();
        assert!(e.contains("not written") && e.contains("declined"), "{e}");
        assert_eq!(no.asked.load(Ordering::SeqCst), 1);

        // Headless: refused before any check (an invalid name is not even looked at).
        for a in [
            a.clone(),
            json!({"name": "Bad Name", "transport": "http", "url": "https://x", "roles": ["*"]}),
        ] {
            let e = mcp_call(&mcp_tool(&ctx, None), a).await.unwrap_err();
            assert!(e.contains("no person can confirm it here"), "{e}");
        }

        // Dry run: the block, without asking or writing.
        let yes = Fake::new(true);
        let mut dry = a.clone();
        dry["dry_run"] = json!(true);
        let out = mcp_call(&mcp_tool(&ctx, Some(yes.clone())), dry)
            .await
            .unwrap();
        assert!(
            out.contains("Dry run") && out.contains("[mcp.docs]"),
            "{out}"
        );
        assert_eq!(yes.asked.load(Ordering::SeqCst), 0);

        assert_eq!(user_text(&ctx), before);
        assert!(!McpConfig::load(&ctx.paths).servers.contains_key("docs"));
    }

    #[tokio::test]
    async fn existing_names_in_any_layer_are_refused_without_asking() {
        let ctx = mcp_ctx("mcp-existing");
        // Test setup, written directly: a repo server and a user server.
        std::fs::write(
            ctx.paths.repo_file().unwrap(),
            "[mcp.shared]\ntype = \"http\"\nurl = \"https://repo.example/mcp\"\n",
        )
        .unwrap();
        let user = format!(
            "{}\n[mcp.mine]\ntype = \"http\"\nurl = \"https://mine.example/mcp\"\n",
            user_text(&ctx)
        );
        std::fs::write(ctx.paths.user_file.as_ref().unwrap(), &user).unwrap();

        let yes = Fake::new(true);
        let t = mcp_tool(&ctx, Some(yes.clone()));
        for (name, layer) in [("shared", "repo"), ("mine", "user")] {
            let e = mcp_call(
                &t,
                json!({"name": name, "transport": "http", "url": "https://other.example/mcp", "roles": ["*"]}),
            )
            .await
            .unwrap_err();
            assert!(e.contains("already exists") && e.contains(layer), "{e}");
            assert!(e.contains(&format!("amp-harness mcp edit {name}")), "{e}");
        }
        // Reserved names too.
        let e = mcp_call(
            &t,
            json!({"name": "amp_tools", "transport": "http", "url": "https://x.example", "roles": ["*"]}),
        )
        .await
        .unwrap_err();
        assert!(e.contains("not written"), "{e}");
        assert_eq!(yes.asked.load(Ordering::SeqCst), 0);
        assert_eq!(user_text(&ctx), user);
    }

    #[tokio::test]
    async fn secrets_are_refused_and_never_echoed() {
        let ctx = mcp_ctx("mcp-secrets");
        let before = user_text(&ctx);
        let yes = Fake::new(true);
        let t = mcp_tool(&ctx, Some(yes.clone()));
        let planted = "ghp_abcdefghijklmnopqrstuvwxyz0123456789";
        for a in [
            // Fields the tool does not have: headers, env, tokens, scope, force.
            json!({"name": "gh", "transport": "http", "url": "https://gh.example/mcp", "roles": ["*"], "headers": {"Authorization": format!("Bearer {planted}")}}),
            json!({"name": "gh", "transport": "stdio", "command": "gh-mcp", "roles": ["*"], "env": {"GITHUB_TOKEN": planted}}),
            json!({"name": "gh", "transport": "http", "url": "https://gh.example/mcp", "roles": ["*"], "token": planted}),
            json!({"name": "gh", "transport": "http", "url": "https://gh.example/mcp", "roles": ["*"], "secret_env": ["GITHUB_TOKEN"]}),
            json!({"name": "gh", "transport": "http", "url": "https://gh.example/mcp", "roles": ["*"], "scope": "repo"}),
            json!({"name": "gh", "transport": "http", "url": "https://gh.example/mcp", "roles": ["*"], "force": true}),
            // Secrets in the values it does have.
            json!({"name": "gh", "transport": "stdio", "command": "gh-mcp", "args": ["--token", planted], "roles": ["*"]}),
            json!({"name": "gh", "transport": "stdio", "command": planted, "roles": ["*"]}),
            json!({"name": "gh", "transport": "http", "url": format!("https://gh.example/mcp?api_key={planted}"), "roles": ["*"]}),
        ] {
            let e = mcp_call(&t, a.clone()).await.unwrap_err();
            assert!(!e.contains(planted), "echoed a secret: {e}");
            assert!(e.contains("amp-harness mcp secret gh"), "{a}: {e}");
        }
        // roles is required (no silent "*"), and transport fields must match.
        for a in [
            json!({"name": "gh", "transport": "http", "url": "https://gh.example/mcp"}),
            json!({"name": "gh", "transport": "http", "url": "https://gh.example/mcp", "roles": []}),
            json!({"name": "gh", "transport": "stdio", "url": "https://gh.example/mcp", "roles": ["*"]}),
            json!({"name": "gh", "transport": "http", "command": "x", "roles": ["*"]}),
        ] {
            assert!(mcp_call(&t, a.clone()).await.is_err(), "{a}");
        }
        assert_eq!(yes.asked.load(Ordering::SeqCst), 0);
        assert_eq!(user_text(&ctx), before);
    }

    #[tokio::test]
    async fn env_references_are_refused_before_asking_and_never_written() {
        // AMP #4318: `${env:..}` would be filled from the person's environment on load, and an
        // http server sends its URL on the next reload.
        let ctx = mcp_ctx("mcp-env-refs");
        let before = user_text(&ctx);
        let yes = Fake::new(true);
        let t = mcp_tool(&ctx, Some(yes.clone()));
        let cases = [
            // In the query, the path and the host of an http URL, a whole sse URL, with a default.
            (
                "url",
                json!({"name": "gh", "transport": "http", "url": "https://x.example/mcp?t=${env:GITHUB_TOKEN}", "roles": ["*"]}),
            ),
            (
                "url",
                json!({"name": "gh", "transport": "http", "url": "https://x.example/${env:AMP_TOKEN}/mcp", "roles": ["*"]}),
            ),
            (
                "url",
                json!({"name": "gh", "transport": "http", "url": "https://${env:AWS_SECRET_ACCESS_KEY}.x.example/mcp", "roles": ["*"]}),
            ),
            (
                "url",
                json!({"name": "gh", "transport": "sse", "url": "${env:MCP_URL}", "roles": ["*"]}),
            ),
            (
                "url",
                json!({"name": "gh", "transport": "http", "url": "https://x.example/mcp?t=${env:UNSET_ONE:-fallback}", "roles": ["*"]}),
            ),
            // stdio: any argument, and the command.
            (
                "args[1]",
                json!({"name": "gh", "transport": "stdio", "command": "gh-mcp", "args": ["--token", "${env:GITHUB_TOKEN}"], "roles": ["*"]}),
            ),
            (
                "args[0]",
                json!({"name": "gh", "transport": "stdio", "command": "gh-mcp", "args": ["--url=https://x.example/?k=${env:ANTHROPIC_API_KEY}"], "roles": ["*"]}),
            ),
            (
                "command",
                json!({"name": "gh", "transport": "stdio", "command": "${env:TOOL_DIR}/bin/mcp", "roles": ["*"]}),
            ),
            // The Claude-style shorthand, which the builders turn into `${env:..}`.
            (
                "url",
                json!({"name": "gh", "transport": "http", "url": "https://x.example/mcp?t=${GITHUB_TOKEN}", "roles": ["*"]}),
            ),
            (
                "args[0]",
                json!({"name": "gh", "transport": "stdio", "command": "gh-mcp", "args": ["${AMP_TOKEN:-fallback}"], "roles": ["*"]}),
            ),
        ];
        for (field, a) in cases {
            // Dry runs too: the check comes before anything is shown.
            for dry_run in [false, true] {
                let mut a = a.clone();
                a["dry_run"] = json!(dry_run);
                let e = mcp_call(&t, a.clone()).await.unwrap_err();
                assert!(
                    e.contains(&format!("{field} references an environment variable")),
                    "{a}: {e}"
                );
                assert!(e.contains("amp-harness mcp secret gh"), "{a}: {e}");
                for value in [
                    "GITHUB_TOKEN",
                    "AMP_TOKEN",
                    "AWS_SECRET_ACCESS_KEY",
                    "ANTHROPIC_API_KEY",
                    "TOOL_DIR",
                    "fallback",
                ] {
                    assert!(!e.contains(value), "echoed the value: {e}");
                }
            }
        }
        assert_eq!(yes.asked.load(Ordering::SeqCst), 0);
        assert_eq!(user_text(&ctx), before);

        // Path references are not secrets and stay allowed, and `$${` is a literal.
        mcp_call(
            &t,
            json!({"name": "fs", "transport": "stdio", "command": "${home}/bin/fs-mcp", "args": ["--root", "${home}/notes", "$${env:NOT_A_REF}"], "roles": ["*"]}),
        )
        .await
        .unwrap();
        assert_eq!(yes.asked.load(Ordering::SeqCst), 1);
        assert!(user_text(&ctx).contains("[mcp.fs]"));
    }

    #[test]
    fn the_env_check_covers_every_string_the_builders_produce() {
        let table: toml::Value = toml::from_str(
            "url = \"https://x.example/mcp\"\n[nested]\nlist = [\"ok\", \"${env:SECRET}\"]\n",
        )
        .unwrap();
        let e = refuse_env_refs_in("gh", "", &table).unwrap_err();
        assert!(
            e.starts_with("nested.list[1] references an environment variable"),
            "{e}"
        );
        assert!(!e.contains("SECRET"), "{e}");
        let ok: toml::Value =
            toml::from_str("command = \"${home}/x\"\nargs = [\"${repo}\"]\n").unwrap();
        refuse_env_refs_in("gh", "", &ok).unwrap();
    }
}
