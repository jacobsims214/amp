//! Tool policy (D6): each role's allow/deny rules, parsed from its opencode `permission`
//! frontmatter and enforced by the harness in an rpi `before_tool_call` hook. A denied call returns
//! a tool error the model sees. Nothing ever prompts.
//!
//! # Semantics (the same as opencode's, so there is one policy source)
//!
//! The `permission` block is flattened, in file order, into rules `(permission, pattern, action)`:
//! - `edit: allow` becomes `(edit, "*", allow)`;
//! - `bash: { "*": deny, "git status*": allow }` becomes two rules, in that order.
//!
//! A call is checked as one or more `(permission, subject)` pairs (see [`ToolPolicy::checks`]).
//! The **last** rule whose permission and pattern both match wins (opencode's `findLast`), so
//! put the catch-all `"*"` first and the exceptions after it. Patterns are opencode wildcards:
//! - `*` matches any run of characters, `/` included (`**` is the same as `*`);
//! - `?` matches one character;
//! - a pattern ending in `" *"` also matches the bare command, so `ls *` matches `ls`.
//!
//! When no rule matches, the permission's default applies: `external_directory` defaults to
//! `ask`, everything else to `allow` (opencode's shipped defaults).
//!
//! `ask` is treated as `deny`. The harness has no prompt path: a worker that waits for a human
//! hangs, which was the "hung worker on a permission prompt" failure from the wargames.
//!
//! Children (subagent roles) are always denied two things, whatever their frontmatter says:
//! - the `question` tool, because nobody is there to answer;
//! - the dispatch tool [`DISPATCH_TOOL`], so they can't recurse.
//!
//! # Mapping tools to permissions
//!
//! | tool | permission | subject |
//! |---|---|---|
//! | `bash`, `powershell` | `bash` | each command of the script, split on `&&` `\|\|` `;` `\|` `&` and newlines |
//! | `edit`, `write` | `edit` | the path relative to the worktree root (paths outside it also need `external_directory`) |
//! | [`DISPATCH_TOOL`] | `task` | the agent id without AMP's `amp:` prefix |
//! | `webfetch` | `webfetch` | the URL |
//! | `websearch` | `websearch` | the query |
//! | `todowrite`, `todoread` | `todowrite` | `*` |
//! | `read`, `grep`, `find`, `ls` | `read`, `grep`, `glob`, `list` | the path, else `*` |
//! | anything else (`amp_*` MCP tools) | the tool name | `*` |
//!
//! Under a restricted bash policy (any bash rule that isn't `allow`), command substitution
//! (`$(…)`, backticks, `<(…)`, `>(…)`) and output redirection to a file are denied outright. An
//! allowlist like `"git status*": allow` would otherwise pass `git status $(rm -rf x)` or
//! `ls > file`. Redirecting to `/dev/null` and duplicating a descriptor (`2>&1`) are fine.
//!
//! rpi-tools has no `webfetch`, `websearch`, `todowrite` or `question` tool, so those rules only
//! matter if a later ticket adds such a tool.

use std::{
    fmt,
    future::Future,
    path::{Component, Path, PathBuf},
    pin::Pin,
    sync::Arc,
};

use rpi_agent::{AgentTool, BeforeToolCall, BeforeToolCallContext, BeforeToolCallResult};
use serde_json::Value;
use tokio_util::sync::CancellationToken;

/// The manager's dispatch tool (D7). Children never get it.
pub const DISPATCH_TOOL: &str = "amp_run_ticket";
/// The interactive question tool. Children never get it.
pub const QUESTION_TOOL: &str = "question";

/// What a rule does.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Action {
    Allow,
    Deny,
    /// opencode would prompt. The harness never does, so this denies.
    Ask,
}

impl Action {
    fn parse(text: &str) -> Option<Self> {
        match text {
            "allow" => Some(Self::Allow),
            "deny" => Some(Self::Deny),
            "ask" => Some(Self::Ask),
            _ => None,
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Self::Allow => "allow",
            Self::Deny => "deny",
            Self::Ask => "ask",
        }
    }

    pub fn allows(self) -> bool {
        self == Self::Allow
    }
}

/// One flattened rule.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Rule {
    /// The permission key (`edit`, `bash`, `task`, a tool name, ...). May itself be a wildcard.
    pub permission: String,
    pub pattern: String,
    pub action: Action,
}

/// A role's rules in file order. [`Policy::evaluate`] picks the last match.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Policy {
    rules: Vec<Rule>,
}

/// A malformed `permission` block.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("permission: {0}")]
pub struct PolicyError(pub String);

impl Policy {
    /// Parse an opencode `permission` value. `None`/null means no rules (all defaults).
    pub fn parse(permission: Option<&Value>) -> Result<Self, PolicyError> {
        let mut rules = Vec::new();
        match permission {
            None | Some(Value::Null) => {}
            // `permission: deny` sets every permission at once.
            Some(Value::String(action)) => rules.push(Rule {
                permission: "*".into(),
                pattern: "*".into(),
                action: parse_action(action, "permission")?,
            }),
            Some(Value::Object(map)) => {
                for (permission, value) in map {
                    match value {
                        Value::String(action) => rules.push(Rule {
                            permission: permission.clone(),
                            pattern: "*".into(),
                            action: parse_action(action, permission)?,
                        }),
                        Value::Object(patterns) => {
                            for (pattern, action) in patterns {
                                let where_ = format!("{permission}.{pattern:?}");
                                let action = action.as_str().ok_or_else(|| {
                                    PolicyError(format!("{where_} must be allow, deny or ask"))
                                })?;
                                rules.push(Rule {
                                    permission: permission.clone(),
                                    pattern: pattern.clone(),
                                    action: parse_action(action, &where_)?,
                                });
                            }
                        }
                        _ => {
                            return Err(PolicyError(format!(
                                "{permission} must be allow/deny/ask or a map of pattern to action"
                            )));
                        }
                    }
                }
            }
            Some(_) => {
                return Err(PolicyError(
                    "must be an action or a map of permission to rules".into(),
                ));
            }
        }
        Ok(Self { rules })
    }

    pub fn rules(&self) -> &[Rule] {
        &self.rules
    }

    /// The last rule matching `(permission, subject)`, or `None` when the default applies.
    pub fn matching_rule(&self, permission: &str, subject: &str) -> Option<&Rule> {
        self.rules.iter().rev().find(|r| {
            wildcard_match(permission, &r.permission) && wildcard_match(subject, &r.pattern)
        })
    }

    /// The action for `(permission, subject)`: the last matching rule, else the default.
    pub fn evaluate(&self, permission: &str, subject: &str) -> Action {
        self.matching_rule(permission, subject)
            .map(|r| r.action)
            .unwrap_or_else(|| default_action(permission))
    }

    /// True when no subject can be allowed: a catch-all `"*"` rule that isn't `allow` with no
    /// `allow` rule after it (or no rules and a non-allow default). Such tools are left out of the
    /// tool list altogether, as well as denied by the hook.
    pub fn denies_everything(&self, permission: &str) -> bool {
        for rule in self.rules.iter().rev() {
            if !wildcard_match(permission, &rule.permission) {
                continue;
            }
            if rule.action.allows() {
                return false;
            }
            if rule.pattern == "*" || rule.pattern == "**" {
                return true;
            }
        }
        !default_action(permission).allows()
    }

    /// True when every rule for `permission` allows and so does the default.
    pub fn allows_everything(&self, permission: &str) -> bool {
        default_action(permission).allows()
            && self
                .rules
                .iter()
                .filter(|r| wildcard_match(permission, &r.permission))
                .all(|r| r.action.allows())
    }
}

fn parse_action(text: &str, where_: &str) -> Result<Action, PolicyError> {
    Action::parse(text)
        .ok_or_else(|| PolicyError(format!("{where_}: {text:?} is not allow, deny or ask")))
}

/// opencode's defaults when no rule matches.
fn default_action(permission: &str) -> Action {
    match permission {
        "external_directory" => Action::Ask,
        _ => Action::Allow,
    }
}

/// opencode's `Wildcard.match`: `*` is any run (including `/` and newlines), `?` one character,
/// everything else literal, anchored at both ends; a trailing `" *"` is optional. Backslashes
/// are read as `/` on both sides.
pub fn wildcard_match(input: &str, pattern: &str) -> bool {
    let input: Vec<char> = input
        .chars()
        .map(|c| if c == '\\' { '/' } else { c })
        .collect();
    let mut pattern: Vec<char> = pattern
        .chars()
        .map(|c| if c == '\\' { '/' } else { c })
        .collect();
    if pattern.ends_with(&[' ', '*']) {
        let bare = &pattern[..pattern.len() - 2];
        if glob(&input, bare) {
            return true;
        }
    }
    // `**` is the same as `*`; collapse runs so the matcher stays linear-ish.
    pattern.dedup_by(|a, b| *a == '*' && *b == '*');
    glob(&input, &pattern)
}

/// Iterative glob with single-star backtracking (O(n*m) worst case).
fn glob(input: &[char], pattern: &[char]) -> bool {
    let (mut i, mut p) = (0, 0);
    let mut star: Option<(usize, usize)> = None;
    while i < input.len() {
        if p < pattern.len() && (pattern[p] == '?' || (pattern[p] != '*' && pattern[p] == input[i]))
        {
            i += 1;
            p += 1;
        } else if p < pattern.len() && pattern[p] == '*' {
            star = Some((p, i));
            p += 1;
        } else if let Some((sp, si)) = star {
            p = sp + 1;
            i = si + 1;
            star = Some((sp, si + 1));
        } else {
            return false;
        }
    }
    pattern[p..].iter().all(|&c| c == '*')
}

// ------------------------------------------------------------------------------------------------
// Bash command splitting.
// ------------------------------------------------------------------------------------------------

/// A shell script split into the commands a bash policy checks.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ShellScan {
    /// Each simple command, trimmed, in order (pipes, lists and background jobs split apart).
    pub commands: Vec<String>,
    /// `$(…)`, a backtick, `<(…)` or `>(…)` outside single quotes.
    pub substitution: bool,
    /// Output redirected to a file (not `/dev/null`, not a descriptor duplication).
    pub writes_file: bool,
}

/// Split a script on `&&`, `||`, `;`, `|`, `&` and newlines outside quotes. This is a lexer, not a
/// shell parser: it errs towards more, smaller commands and flags anything it can't see through.
pub fn scan_shell(script: &str) -> ShellScan {
    let chars: Vec<char> = script.chars().collect();
    let mut scan = ShellScan::default();
    let mut current = String::new();
    let (mut single, mut double) = (false, false);
    let mut i = 0;
    let flush = |current: &mut String, scan: &mut ShellScan| {
        let cmd = current.trim();
        if !cmd.is_empty() {
            scan.commands.push(cmd.to_string());
        }
        current.clear();
    };
    while i < chars.len() {
        let c = chars[i];
        let next = chars.get(i + 1).copied();
        if single {
            if c == '\'' {
                single = false;
            }
            current.push(c);
            i += 1;
            continue;
        }
        match c {
            '\\' => {
                current.push(c);
                if let Some(n) = next {
                    current.push(n);
                }
                i += 2;
                continue;
            }
            '\'' if !double => single = true,
            '"' => double = !double,
            '`' => scan.substitution = true,
            '$' if next == Some('(') => scan.substitution = true,
            '<' | '>' if next == Some('(') && !double => scan.substitution = true,
            '>' if !double && redirect_writes_file(&chars, i) => scan.writes_file = true,
            _ => {}
        }
        if !double {
            let prev = if i > 0 { Some(chars[i - 1]) } else { None };
            let split = match c {
                ';' | '\n' => Some(1),
                '|' | '&' if next == Some(c) => Some(2),
                '|' => Some(1),
                // `&` that is part of a redirection (`2>&1`, `&>file`, `>&2`) is not a separator.
                '&' if prev != Some('>') && next != Some('>') => Some(1),
                _ => None,
            };
            if let Some(len) = split {
                flush(&mut current, &mut scan);
                i += len;
                continue;
            }
        }
        current.push(c);
        i += 1;
    }
    flush(&mut current, &mut scan);
    scan
}

/// `chars[at]` is `>`. True unless the target is a descriptor (`>&2`, `2>&1`, `>&-`) or
/// `/dev/null`.
fn redirect_writes_file(chars: &[char], at: usize) -> bool {
    let mut j = at + 1;
    if chars.get(j) == Some(&'>') || chars.get(j) == Some(&'|') {
        j += 1;
    }
    if chars.get(j) == Some(&'&') {
        let after = chars.get(j + 1).copied();
        if after.is_some_and(|c| c.is_ascii_digit() || c == '-') {
            return false;
        }
        j += 1;
    }
    while chars.get(j).is_some_and(|c| *c == ' ' || *c == '\t') {
        j += 1;
    }
    let target: String = chars[j.min(chars.len())..]
        .iter()
        .take_while(|c| !c.is_whitespace() && !matches!(c, ';' | '|' | '&' | ')'))
        .collect();
    target.trim_matches(|c| c == '"' || c == '\'') != "/dev/null"
}

// ------------------------------------------------------------------------------------------------
// Enforcement.
// ------------------------------------------------------------------------------------------------

/// Why a call was denied. `Display` is the text the model sees as the tool error.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Denial {
    pub role: String,
    pub tool: String,
    pub permission: String,
    pub subject: String,
    /// The rule that decided it, if one did (else a default or a fixed harness rule).
    pub rule: Option<Rule>,
    pub reason: DenialReason,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DenialReason {
    /// A `deny` rule (or default).
    Deny,
    /// An `ask` rule (or default): the harness never prompts.
    Ask,
    /// Children never get the question tool.
    ChildQuestion,
    /// Children never get the dispatch tool.
    ChildDispatch,
    /// Command substitution under a restricted bash policy.
    Substitution,
    /// Output redirection to a file under a restricted bash policy.
    Redirect,
    /// The call is missing the argument the policy checks (e.g. no `command`).
    BadArguments,
}

impl fmt::Display for Denial {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let who = format!("Denied by the {} tool policy", self.role);
        let tail = "The harness never asks for approval, so this call will not run. Use a tool \
                    or command your role allows, or say in your final message what you could \
                    not do.";
        match self.reason {
            DenialReason::ChildQuestion => write!(
                f,
                "{who}: `{}` is not available to workers; nobody is there to answer. If you need \
                 a person, comment the question on your ticket and call amp_block_task.",
                self.tool
            ),
            DenialReason::ChildDispatch => write!(
                f,
                "{who}: workers cannot dispatch other agents (`{}`). Only the manager does.",
                self.tool
            ),
            DenialReason::Substitution => write!(
                f,
                "{who}: command substitution is not allowed under this role's bash rules: \
                 `{}`. {tail}",
                self.subject
            ),
            DenialReason::Redirect => write!(
                f,
                "{who}: redirecting output to a file is not allowed under this role's bash \
                 rules: `{}`. {tail}",
                self.subject
            ),
            DenialReason::BadArguments => write!(
                f,
                "{who}: `{}` was called without the argument its `{}` rule checks. {tail}",
                self.tool, self.permission
            ),
            DenialReason::Deny | DenialReason::Ask => {
                write!(f, "{who}: {} `{}` ", self.permission, self.subject)?;
                match &self.rule {
                    Some(rule) => write!(
                        f,
                        "matches the rule {:?}: {}",
                        rule.pattern,
                        rule.action.as_str()
                    )?,
                    None => write!(
                        f,
                        "has no rule, and the default is {}",
                        default_action(&self.permission).as_str()
                    )?,
                }
                if self.reason == DenialReason::Ask {
                    write!(f, " (ask is treated as deny)")?;
                }
                write!(f, ". {tail}")
            }
        }
    }
}

/// One `(permission, subject)` pair a call is checked against.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Check {
    pub permission: String,
    pub subject: String,
}

impl Check {
    fn new(permission: impl Into<String>, subject: impl Into<String>) -> Self {
        Self {
            permission: permission.into(),
            subject: subject.into(),
        }
    }
}

/// A role's policy bound to a worktree root: what the before-tool hook enforces.
#[derive(Debug, Clone)]
pub struct ToolPolicy {
    role: String,
    child: bool,
    policy: Policy,
    root: PathBuf,
}

type HookFuture = Pin<Box<dyn Future<Output = Option<BeforeToolCallResult>> + Send>>;

impl ToolPolicy {
    /// `child` is true for subagent roles. `root` is the worktree (cwd) that edit paths are
    /// relative to.
    pub fn new(
        role: impl Into<String>,
        child: bool,
        policy: Policy,
        root: impl Into<PathBuf>,
    ) -> Self {
        Self {
            role: role.into(),
            child,
            policy,
            root: lexical_normalize(&root.into()),
        }
    }

    pub fn role(&self) -> &str {
        &self.role
    }

    pub fn is_child(&self) -> bool {
        self.child
    }

    pub fn policy(&self) -> &Policy {
        &self.policy
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    /// Whether `tool` should be in this role's tool list at all. False for the children's fixed
    /// denials and for tools whose permission denies every subject (e.g. the manager's `edit`).
    /// The hook still checks every call; this only keeps dead tools out of the prompt.
    pub fn exposes(&self, tool: &str) -> bool {
        if self.child && (tool == QUESTION_TOOL || tool == DISPATCH_TOOL) {
            return false;
        }
        !self.policy.denies_everything(permission_for(tool))
    }

    /// Keep the tools this role may see ([`Self::exposes`]).
    pub fn filter_tools(&self, tools: Vec<Arc<dyn AgentTool>>) -> Vec<Arc<dyn AgentTool>> {
        tools
            .into_iter()
            .filter(|t| self.exposes(&t.schema().name))
            .collect()
    }

    /// The `(permission, subject)` pairs a call is checked against. `Err` when the call is denied
    /// before any rule is consulted (children's fixed rules, shell constructs, bad arguments).
    pub fn checks(&self, tool: &str, args: &Value) -> Result<Vec<Check>, Box<Denial>> {
        let permission = permission_for(tool);
        let deny = |subject: String, reason| {
            Box::new(Denial {
                role: self.role.clone(),
                tool: tool.to_string(),
                permission: permission.to_string(),
                subject,
                rule: None,
                reason,
            })
        };
        if self.child && tool == QUESTION_TOOL {
            return Err(deny("*".into(), DenialReason::ChildQuestion));
        }
        if self.child && tool == DISPATCH_TOOL {
            let agent = str_arg(args, &["agent_id"]).unwrap_or("*").to_string();
            return Err(deny(agent, DenialReason::ChildDispatch));
        }
        let missing = || deny(String::new(), DenialReason::BadArguments);
        Ok(match tool {
            "bash" | "powershell" => {
                let script = str_arg(args, &["command"]).ok_or_else(missing)?;
                let scan = scan_shell(script);
                if !self.policy.allows_everything("bash") {
                    if scan.substitution {
                        return Err(deny(script.to_string(), DenialReason::Substitution));
                    }
                    if scan.writes_file {
                        return Err(deny(script.to_string(), DenialReason::Redirect));
                    }
                }
                if scan.commands.is_empty() {
                    vec![Check::new("bash", script.trim())]
                } else {
                    scan.commands
                        .into_iter()
                        .map(|c| Check::new("bash", c))
                        .collect()
                }
            }
            "edit" | "write" => {
                let raw = str_arg(args, &["path", "file_path", "filePath"]).ok_or_else(missing)?;
                let (inside, rel) = self.relative_path(raw);
                let mut checks = Vec::new();
                if !inside {
                    checks.push(Check::new("external_directory", rel.clone()));
                }
                checks.push(Check::new("edit", rel));
                checks
            }
            DISPATCH_TOOL => {
                let agent =
                    str_arg(args, &["agent_id", "agent", "subagent_type"]).ok_or_else(missing)?;
                vec![Check::new("task", canonical_agent_id(agent))]
            }
            "webfetch" => vec![Check::new(
                "webfetch",
                str_arg(args, &["url"]).unwrap_or("*"),
            )],
            "websearch" => vec![Check::new(
                "websearch",
                str_arg(args, &["query"]).unwrap_or("*"),
            )],
            "read" | "grep" | "find" | "ls" => {
                let subject = str_arg(args, &["path", "file_path"])
                    .map(|p| self.relative_path(p).1)
                    .unwrap_or_else(|| "*".into());
                vec![Check::new(permission, subject)]
            }
            _ => vec![Check::new(permission, "*")],
        })
    }

    /// `Ok(())` to let the call run, `Err` with the reason the model will see.
    pub fn check(&self, tool: &str, args: &Value) -> Result<(), Box<Denial>> {
        for check in self.checks(tool, args)? {
            let rule = self.policy.matching_rule(&check.permission, &check.subject);
            let action = rule
                .map(|r| r.action)
                .unwrap_or_else(|| default_action(&check.permission));
            if !action.allows() {
                return Err(Box::new(Denial {
                    role: self.role.clone(),
                    tool: tool.to_string(),
                    permission: check.permission,
                    subject: check.subject,
                    rule: rule.cloned(),
                    reason: if action == Action::Ask {
                        DenialReason::Ask
                    } else {
                        DenialReason::Deny
                    },
                }));
            }
        }
        Ok(())
    }

    /// The rpi `before_tool_call` hook: blocks denied calls with the denial as the tool error.
    /// It never terminates the run, so the model can choose another way.
    pub fn before_tool_call(self: &Arc<Self>) -> BeforeToolCall {
        self.before_tool_call_then(None)
    }

    /// Like [`Self::before_tool_call`], then runs `next` for allowed calls (M2-6 chains its
    /// run-reporting hook here).
    pub fn before_tool_call_then(self: &Arc<Self>, next: Option<BeforeToolCall>) -> BeforeToolCall {
        let this = Arc::clone(self);
        Arc::new(
            move |ctx: BeforeToolCallContext<'_>, signal: CancellationToken| -> HookFuture {
                match this.check(&ctx.tool_call.name, ctx.args) {
                    Err(denial) => {
                        let result = BeforeToolCallResult {
                            block: true,
                            reason: Some(denial.to_string()),
                            terminate: false,
                            args: None,
                        };
                        Box::pin(async move { Some(result) })
                    }
                    Ok(()) => match &next {
                        Some(next) => next(ctx, signal),
                        None => Box::pin(async { None }),
                    },
                }
            },
        )
    }

    /// `(inside root, path relative to root or absolute if outside)`.
    fn relative_path(&self, raw: &str) -> (bool, String) {
        let path = Path::new(raw);
        let abs = if path.is_absolute() {
            lexical_normalize(path)
        } else {
            lexical_normalize(&self.root.join(path))
        };
        match abs.strip_prefix(&self.root) {
            Ok(rel) if rel.as_os_str().is_empty() => (true, ".".into()),
            Ok(rel) => (true, rel.to_string_lossy().replace('\\', "/")),
            Err(_) => (false, abs.to_string_lossy().replace('\\', "/")),
        }
    }
}

/// The permission key a tool is checked under.
pub fn permission_for(tool: &str) -> &str {
    match tool {
        "bash" | "powershell" => "bash",
        "edit" | "write" => "edit",
        DISPATCH_TOOL => "task",
        "todowrite" | "todoread" => "todowrite",
        "find" => "glob",
        "ls" => "list",
        other => other,
    }
}

/// AMP's agent ids are `amp:amp-worker-backend`; the roster files are `amp-worker-backend.md`.
/// The `task` rules are matched against the id without the `amp:` prefix, the way opencode matched
/// its agent names (so `amp-*` allows `amp:amp-worker-backend` and denies `general`).
pub fn canonical_agent_id(agent: &str) -> String {
    let agent = agent.trim();
    agent.strip_prefix("amp:").unwrap_or(agent).to_string()
}

fn str_arg<'a>(args: &'a Value, keys: &[&str]) -> Option<&'a str> {
    keys.iter()
        .find_map(|k| args.get(*k).and_then(Value::as_str))
}

/// Resolve `.` and `..` without touching the disk (the file may not exist yet).
fn lexical_normalize(path: &Path) -> PathBuf {
    let mut out = PathBuf::new();
    for component in path.components() {
        match component {
            Component::CurDir => {}
            Component::ParentDir => {
                if !out.pop() {
                    out.push("..");
                }
            }
            other => out.push(other.as_os_str()),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn wildcard_follows_opencode() {
        assert!(wildcard_match("git status", "git status*"));
        assert!(wildcard_match("git status --short", "git status*"));
        assert!(wildcard_match("ls", "ls *"), "trailing ' *' is optional");
        assert!(wildcard_match("ls -la src", "ls *"));
        assert!(!wildcard_match("lsof", "ls *"));
        assert!(wildcard_match("src/a/b.rs", "src/*"), "* crosses /");
        assert!(wildcard_match("src/a/b.rs", "src/**"));
        assert!(wildcard_match("amp-worker-backend", "amp-*"));
        assert!(!wildcard_match("general", "amp-*"));
        assert!(wildcard_match("a.b", "a?b"));
        assert!(!wildcard_match("ab", "a?b"));
        assert!(wildcard_match("x", "*"));
        assert!(wildcard_match("", "*"));
        assert!(wildcard_match("a\\b", "a/b"));
    }

    #[test]
    fn last_matching_rule_wins() {
        let p = Policy::parse(Some(&json!({
            "bash": {"*": "deny", "git *": "allow", "git push*": "deny"}
        })))
        .unwrap();
        assert_eq!(p.evaluate("bash", "git status"), Action::Allow);
        assert_eq!(p.evaluate("bash", "git push origin"), Action::Deny);
        assert_eq!(p.evaluate("bash", "rm -rf /"), Action::Deny);
        // Catch-all last overrides everything before it (the old manager bug).
        let p = Policy::parse(Some(&json!({"bash": {"git *": "allow", "*": "deny"}}))).unwrap();
        assert_eq!(p.evaluate("bash", "git status"), Action::Deny);
    }

    #[test]
    fn defaults_and_shapes() {
        let p = Policy::parse(None).unwrap();
        assert_eq!(p.evaluate("edit", "x"), Action::Allow);
        assert_eq!(p.evaluate("external_directory", "/tmp/x"), Action::Ask);
        let p = Policy::parse(Some(&json!("deny"))).unwrap();
        assert_eq!(p.evaluate("anything", "x"), Action::Deny);
        assert!(Policy::parse(Some(&json!({"edit": "maybe"}))).is_err());
        assert!(Policy::parse(Some(&json!({"edit": 3}))).is_err());
        assert!(Policy::parse(Some(&json!({"bash": {"*": true}}))).is_err());
        assert!(Policy::parse(Some(&json!([1]))).is_err());
    }

    #[test]
    fn denies_everything_and_allows_everything() {
        let p = Policy::parse(Some(&json!({
            "edit": {"*": "deny"},
            "bash": {"*": "deny", "ls *": "allow"},
            "webfetch": "deny",
            "websearch": "ask",
        })))
        .unwrap();
        assert!(p.denies_everything("edit"));
        assert!(!p.denies_everything("bash"));
        assert!(p.denies_everything("webfetch"));
        assert!(p.denies_everything("websearch"));
        assert!(!p.denies_everything("read"));
        assert!(p.denies_everything("external_directory"));
        assert!(!p.allows_everything("bash"));
        assert!(p.allows_everything("read"));
    }

    #[test]
    fn shell_scan_splits_and_flags() {
        let s = scan_shell(
            "git status && rm -rf x; ls | wc -l || echo 'a;b' & sleep 1\necho \"x && y\"",
        );
        assert_eq!(
            s.commands,
            vec![
                "git status",
                "rm -rf x",
                "ls",
                "wc -l",
                "echo 'a;b'",
                "sleep 1",
                "echo \"x && y\""
            ]
        );
        assert!(!s.substitution && !s.writes_file);
        assert!(scan_shell("echo $(id)").substitution);
        assert!(scan_shell("echo `id`").substitution);
        assert!(scan_shell("diff <(ls a) <(ls b)").substitution);
        assert!(
            !scan_shell("echo '$(id)'").substitution,
            "single quotes are literal"
        );
        assert!(
            scan_shell("echo \"$(id)\"").substitution,
            "double quotes still substitute"
        );
        assert!(scan_shell("ls > out.txt").writes_file);
        assert!(scan_shell("ls >> out.txt").writes_file);
        assert!(scan_shell("ls &> out.txt").writes_file);
        assert!(!scan_shell("ls 2>&1").writes_file);
        assert_eq!(
            scan_shell("ls 2>&1 | head").commands,
            vec!["ls 2>&1", "head"]
        );
        assert!(!scan_shell("ls > /dev/null 2>&1").writes_file);
        assert!(!scan_shell("echo 'a > b'").writes_file);
    }

    #[test]
    fn canonical_ids() {
        assert_eq!(
            canonical_agent_id("amp:amp-worker-backend"),
            "amp-worker-backend"
        );
        assert_eq!(canonical_agent_id("general"), "general");
        assert_eq!(canonical_agent_id("amp-reviewer"), "amp-reviewer");
    }

    #[test]
    fn edit_paths_are_relative_to_root_and_external_needs_its_own_rule() {
        let p = Policy::parse(Some(&json!({"edit": {"*": "deny", "docs/**": "allow"}}))).unwrap();
        let tp = ToolPolicy::new("amp-x", true, p, "/repo/wt");
        assert!(tp.check("edit", &json!({"path": "docs/a.md"})).is_ok());
        assert!(
            tp.check("write", &json!({"path": "/repo/wt/docs/a.md"}))
                .is_ok()
        );
        assert!(tp.check("write", &json!({"path": "src/a.rs"})).is_err());
        // `..` can't climb out of the allowed directory.
        assert!(
            tp.check("edit", &json!({"path": "docs/../src/a.rs"}))
                .is_err()
        );
        let d = tp
            .check("write", &json!({"path": "../../etc/passwd"}))
            .unwrap_err();
        assert_eq!(d.permission, "external_directory");
        assert_eq!(d.subject, "/etc/passwd");
        assert_eq!(d.reason, DenialReason::Ask);
        assert_eq!(
            tp.check("write", &json!({})).unwrap_err().reason,
            DenialReason::BadArguments
        );
    }
}
