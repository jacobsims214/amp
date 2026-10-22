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
//! | `bash`, `powershell` | `bash` | each command of the script, split on `&&` `\|\|` `;` `\|` `&` and newlines, comments dropped |
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
//! `ls > file`. Redirecting to `/dev/null` and duplicating a descriptor (`2>&1`) are fine. An
//! option that writes output to a file counts as a redirect: `--output[=…]` (and its
//! abbreviations and `--output-…` variants) for any command, and `-o…` after `git`
//! ([`writes_via_option`]), since `"git log*": allow` would otherwise pass
//! `git log -1 --format=… --output=~/.zshrc`.
//!
//! A script the lexer can't split with certainty is denied too ([`ShellScan::unparseable`]: an
//! unbalanced quote, `$'…'`, a newline left inside a command, or `${X:-…}` / brace expansion, which
//! could turn into an option the harness can't see). A quote read differently from
//! bash would hide whole commands inside one that matches an allow rule, so the harness fails
//! closed.
//!
//! Paths are resolved the way rpi-tools resolves them before it touches the disk
//! ([`resolve_tool_path`], [`resolve_read_tool_path`]): a leading `@` is stripped, unicode spaces
//! become ASCII spaces, `~` and `~/` expand to the home directory, `file://` URLs become paths,
//! and `.`/`..` collapse. Then symlinks are followed ([`resolve_symlinks`]) and the result is
//! compared with the worktree root with its own symlinks resolved. So `~/.ssh/x`, `@../x` and a
//! link inside the worktree that points outside it all count as outside, and the globs see the
//! path of the file that would really be written.
//!
//! rpi-tools has no `webfetch`, `websearch`, `todowrite` or `question` tool, so those rules only
//! matter if a later ticket adds such a tool.

use std::{
    ffi::OsString,
    fmt, fs,
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
    /// Comments are dropped.
    pub commands: Vec<String>,
    /// `$(…)`, a backtick, `<(…)` or `>(…)` outside single quotes.
    pub substitution: bool,
    /// Output redirected to a file (not `/dev/null`, not a descriptor duplication), or a file
    /// opened read-write with `<>` (which creates it).
    pub writes_file: bool,
    /// The lexer can't be sure it split the script the way bash will: a quote is still open at
    /// the end, the script uses ANSI-C quoting (`$'…'`), or a command still contains a newline
    /// after splitting (a quoted newline, or a here-document body), or a word uses `${…}` with an
    /// operator or brace expansion (`{a,b}`, `{a..b}`), which can expand to an option such as
    /// `--output` that the harness never sees ([`shell_words`]). A restricted policy denies
    /// these rather than guess, because a quote the lexer reads differently from bash can hide
    /// whole commands inside what looks like one allowed command.
    pub unparseable: bool,
    /// A command passes an option that writes its output to a file: `--output[=…]` (git's diff
    /// option, and any abbreviation or `--output-…` variant of it, for any command), or `-o…`
    /// after a `git` word (`git format-patch -o <dir>`). See [`writes_via_option`].
    pub output_option: bool,
}

/// Split a script on `&&`, `||`, `;`, `|`, `&` and newlines outside quotes, dropping comments. This
/// is a lexer, not a shell parser: it errs towards more, smaller commands and flags anything it
/// can't see through (see [`ShellScan::unparseable`]).
///
/// What it follows, as bash does:
/// - `'…'` is literal; `"…"` is literal except that `\` escapes the next character;
/// - `$'…'` (ANSI-C quoting) is literal except that `\` escapes the next character, `\'`
///   included;
/// - outside single quotes a `\` escapes the next character, and `\` + newline is a line
///   continuation (both are removed);
/// - an unquoted `#` at the start of a word (start of input, or after a blank or one of
///   `; & | ( ) < >` or a newline) starts a comment that runs to the end of the line.
pub fn scan_shell(script: &str) -> ShellScan {
    #[derive(Clone, Copy, PartialEq, Eq)]
    enum Quote {
        None,
        Single,
        Double,
        AnsiC,
    }
    let chars: Vec<char> = script.chars().collect();
    let mut scan = ShellScan::default();
    let mut current = String::new();
    let mut quote = Quote::None;
    // True when the next unquoted character begins a new word (so `#` would start a comment).
    let mut word_start = true;
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
        match quote {
            Quote::Single => {
                if c == '\'' {
                    quote = Quote::None;
                }
                current.push(c);
                i += 1;
                continue;
            }
            Quote::AnsiC => {
                match c {
                    '\\' => {
                        current.push(c);
                        if let Some(n) = next {
                            current.push(n);
                        }
                        i += 2;
                        continue;
                    }
                    '\'' => quote = Quote::None,
                    _ => {}
                }
                current.push(c);
                i += 1;
                continue;
            }
            Quote::Double | Quote::None => {}
        }
        if c == '\\' {
            // A line continuation disappears; any other escaped character is kept as is.
            if next != Some('\n') {
                current.push(c);
                if let Some(n) = next {
                    current.push(n);
                }
                word_start = false;
            }
            i += 2;
            continue;
        }
        if quote == Quote::None && c == '#' && word_start {
            // A comment: skip to the newline, which still splits.
            while i < chars.len() && chars[i] != '\n' {
                i += 1;
            }
            continue;
        }
        match c {
            '\'' if quote == Quote::None => quote = Quote::Single,
            '$' if quote == Quote::None && next == Some('\'') => {
                quote = Quote::AnsiC;
                scan.unparseable = true;
                current.push_str("$'");
                word_start = false;
                i += 2;
                continue;
            }
            '"' => {
                quote = if quote == Quote::Double {
                    Quote::None
                } else {
                    Quote::Double
                }
            }
            '`' => scan.substitution = true,
            '$' if next == Some('(') => scan.substitution = true,
            '<' | '>' if next == Some('(') && quote == Quote::None => scan.substitution = true,
            '<' if quote == Quote::None && next == Some('>') => scan.writes_file = true,
            '>' if quote == Quote::None
                && chars.get(i.wrapping_sub(1)) != Some(&'<')
                && redirect_writes_file(&chars, i) =>
            {
                scan.writes_file = true
            }
            _ => {}
        }
        if quote == Quote::None {
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
                word_start = true;
                i += len;
                continue;
            }
            word_start = c.is_whitespace() || matches!(c, '&' | '|' | '(' | ')' | '<' | '>');
        } else {
            word_start = false;
        }
        current.push(c);
        i += 1;
    }
    if quote != Quote::None {
        scan.unparseable = true;
    }
    flush(&mut current, &mut scan);
    if scan.commands.iter().any(|c| c.contains('\n')) {
        scan.unparseable = true;
    }
    for cmd in &scan.commands {
        let (words, uncertain) = shell_words(cmd);
        scan.unparseable |= uncertain;
        scan.output_option |= writes_via_option(&words);
    }
    scan
}

/// Split one command into words the way bash would before expansion, with quotes and escapes
/// removed. The flag is true when bash would expand a word into text the lexer can't see:
/// - `${…}` with an operator (`${X:---output=f}`), quoted or not;
/// - an unquoted parameter (`$X`, `${X}`, `$@`, …): bash splits its value on `$IFS` and globs it,
///   so `$IFS--output=f` becomes the word `--output=f` (#2145). Inside double quotes a plain
///   `"$X"` / `"${X}"` is one word and stays allowed;
/// - brace expansion (`{--output=f,}`, `{a..b}`);
/// - after a `git` word, a word with an unquoted glob character (`*`, `?`, `[`): it can expand
///   to a file named like an option, such as `--output=.git/hooks/pre-commit` (#2145). Quote
///   git pathspecs (`-- '*.rs'`); git expands them itself.
///
/// `$(…)` and backticks are [`ShellScan::substitution`]; `$'…'` is already unparseable, and is
/// treated here like `'…'`.
fn shell_words(cmd: &str) -> (Vec<String>, bool) {
    let chars: Vec<char> = cmd.chars().collect();
    let mut words = Vec::new();
    let mut word = String::new();
    let mut in_word = false;
    let mut uncertain = false;
    // Unquoted `{` not yet closed, for spotting brace expansion.
    let mut braces = 0usize;
    // A git word has been seen, and the current word has an unquoted glob character.
    let mut after_git = false;
    let mut glob_word = false;
    let mut i = 0;
    while i < chars.len() {
        let c = chars[i];
        let next = chars.get(i + 1).copied();
        match c {
            '\\' => {
                if let Some(n) = next {
                    word.push(n);
                }
                in_word = true;
                i += 2;
                continue;
            }
            '\'' => {
                // `'…'`, or the body of `$'…'` (where `\` escapes the next character).
                let ansi = word.ends_with('$') && i > 0 && chars[i - 1] == '$';
                if ansi {
                    word.pop();
                }
                i += 1;
                while i < chars.len() && chars[i] != '\'' {
                    if ansi && chars[i] == '\\' && i + 1 < chars.len() {
                        i += 1;
                    }
                    word.push(chars[i]);
                    i += 1;
                }
                in_word = true;
                i += 1;
                continue;
            }
            '"' => {
                i += 1;
                while i < chars.len() && chars[i] != '"' {
                    if chars[i] == '\\' && i + 1 < chars.len() {
                        i += 1;
                    } else if chars[i] == '$' && chars.get(i + 1) == Some(&'{') {
                        uncertain |= plain_param_end(&chars, i).is_none();
                    }
                    word.push(chars[i]);
                    i += 1;
                }
                in_word = true;
                i += 1;
                continue;
            }
            // Unquoted, the value is split on `$IFS` and globbed.
            '$' if next.is_some_and(|n| {
                n == '{' || n == '_' || n.is_ascii_alphanumeric() || "@*#?$!-".contains(n)
            }) =>
            {
                uncertain = true
            }
            '{' => braces += 1,
            '}' => braces = braces.saturating_sub(1),
            ',' if braces > 0 => uncertain = true,
            '.' if braces > 0 && next == Some('.') => uncertain = true,
            _ => {}
        }
        if c.is_whitespace() {
            if in_word {
                after_git |= is_git(&word);
                uncertain |= after_git && glob_word;
                words.push(std::mem::take(&mut word));
                in_word = false;
            }
            braces = 0;
            glob_word = false;
        } else {
            if matches!(c, '*' | '?' | '[') {
                glob_word = true;
            }
            word.push(c);
            in_word = true;
        }
        i += 1;
    }
    if in_word {
        after_git |= is_git(&word);
        uncertain |= after_git && glob_word;
        words.push(word);
    }
    (words, uncertain)
}

/// `git` or a path to it (`/usr/bin/git`).
fn is_git(word: &str) -> bool {
    word == "git" || word.ends_with("/git")
}

/// `chars[at..]` starts with `${`. The index of the closing `}` when the expansion is a plain
/// `${NAME}` (or `${1}`), which expands to a variable's value and nothing else; `None` for any
/// operator (`${X:-…}`, `${X/a/b}`, `${!X}`, …), whose result the lexer can't know.
fn plain_param_end(chars: &[char], at: usize) -> Option<usize> {
    let start = at + 2;
    let len = chars[start.min(chars.len())..]
        .iter()
        .take_while(|c| c.is_ascii_alphanumeric() || **c == '_')
        .count();
    let end = start + len;
    (len > 0 && chars.get(end) == Some(&'}')).then_some(end)
}

/// True when a command's words (from [`shell_words`]) include an option that makes the command
/// write its output to a file, which a restricted bash policy denies like a `>` redirect:
/// - for any command, `--output` / `--output=…` (git log/diff/show/format-patch), an
///   abbreviation of it (`--outp=…`, which some git versions accept), or a longer option that
///   starts with it (`--output-directory`; this also catches the harmless
///   `--output-indicator-*`);
/// - after a `git` word, `-o` / `-o<dir>` (`git format-patch -o <dir>`). Elsewhere `-o` means
///   other things (`ls -o`), so it is left to the allow rules.
///
/// git's revision parser (log, show, diff) does not bundle short options, so `-po` is not
/// checked. `--ext-diff` / `--textconv` only run a diff driver set in git config, which a
/// restricted role cannot change, so they are not denied.
pub fn writes_via_option(words: &[String]) -> bool {
    let mut after_git = false;
    for w in words {
        if w == "--" {
            // The end of options: what follows is paths (`git log -- --output` is a file).
            break;
        }
        if let Some(long) = w.strip_prefix("--") {
            let name = long.split('=').next().unwrap_or_default();
            if !name.is_empty() && ("output".starts_with(name) || name.starts_with("output")) {
                return true;
            }
        } else if after_git && w.starts_with("-o") {
            return true;
        }
        if w == "git" || w.ends_with("/git") {
            after_git = true;
        }
    }
    false
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
    /// Output written to a file under a restricted bash policy: a redirect
    /// ([`ShellScan::writes_file`]) or an option such as `--output`
    /// ([`ShellScan::output_option`]).
    Redirect,
    /// A script the lexer can't split with certainty, under a restricted bash policy
    /// ([`ShellScan::unparseable`]).
    Unparseable,
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
                "{who}: writing output to a file (a `>` redirect, or an option such as \
                 `--output` or `git ... -o`) is not allowed under this role's bash rules: \
                 `{}`. {tail}",
                self.subject
            ),
            DenialReason::Unparseable => write!(
                f,
                "{who}: the harness can't split this script into commands with certainty, so \
                 it is not allowed under this role's bash rules: `{}`. It denies unbalanced \
                 quotes, ANSI-C quoting ($'...'), newlines inside quotes or \
                 here-documents, and ${{X:-...}} or {{a,b}} expansion. Write each command \
                 plainly, one per line or joined with && or ;. {tail}",
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
                    // First: when the split itself is in doubt, the other flags are too.
                    if scan.unparseable {
                        return Err(deny(script.to_string(), DenialReason::Unparseable));
                    }
                    if scan.substitution {
                        return Err(deny(script.to_string(), DenialReason::Substitution));
                    }
                    if scan.writes_file || scan.output_option {
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
                let (inside, rel) = self.classify(&resolve_tool_path(&self.root, raw));
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
                    .map(|p| self.classify(&resolve_read_tool_path(&self.root, p)).1)
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

    /// `(inside root, path relative to root or absolute if outside)` for a path the tool has
    /// already resolved ([`resolve_tool_path`]). Symlinks are followed on both sides, so a link
    /// that leaves the root is outside and the subject names the file that would be touched.
    fn classify(&self, abs: &Path) -> (bool, String) {
        let real = resolve_symlinks(abs);
        // Resolved per call: the worktree may be created after the policy.
        let root = resolve_symlinks(&self.root);
        match real.strip_prefix(&root) {
            Ok(rel) if rel.as_os_str().is_empty() => (true, ".".into()),
            Ok(rel) => (true, rel.to_string_lossy().replace('\\', "/")),
            Err(_) => (false, real.to_string_lossy().replace('\\', "/")),
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

/// Where rpi-tools 0.8.0's `write` and `edit` tools put `raw` when their cwd is `cwd`, before any
/// symlink is followed: `rpi_tools::resolve_tool_path` on an `OsExecutionEnv`. That is
/// `normalize_tool_path` (unicode spaces to ASCII, one leading `@` stripped), then
/// `OsExecutionEnv::resolve_path`: `~` and `~/` expand to `dirs::home_dir()` (empty if unknown,
/// and no `~user`), a `file://` URL becomes its path, a relative path is joined to `cwd`, and `.`
/// and `..` collapse (`..` at `/` stays at `/`). Kept in step with rpi by
/// `tests/roles_policy.rs::policy_resolves_paths_exactly_like_rpi_tools`.
pub fn resolve_tool_path(cwd: &Path, raw: &str) -> PathBuf {
    let mut p = rpi_tools::normalize_tool_path(raw);
    if p == "~" {
        p = home_dir_string();
    } else if let Some(rest) = p.strip_prefix("~/") {
        p = format!("{}/{}", home_dir_string(), rest);
    } else if cfg!(windows)
        && let Some(rest) = p.strip_prefix("~\\")
    {
        p = format!("{}\\{}", home_dir_string(), rest);
    }
    if p.starts_with("file://")
        && let Some(local) = file_url_to_path(&p)
    {
        p = local;
    }
    let pb = PathBuf::from(&p);
    if pb.is_absolute() {
        rpi_normalize(&pb)
    } else {
        rpi_normalize(&cwd.join(pb))
    }
}

/// Where rpi-tools 0.8.0's `read`, `grep`, `find` and `ls` tools look for `raw`
/// (`rpi_tools::resolve_read_tool_path`): [`resolve_tool_path`], then the first of its typographic
/// variants that exists (`lstat`): itself, a narrow no-break space before `AM.`/`PM.`, NFD, `'` as
/// `’`, and NFD with `’`. None exists: the resolved path.
pub fn resolve_read_tool_path(cwd: &Path, raw: &str) -> PathBuf {
    use unicode_normalization::UnicodeNormalization;
    let resolved = resolve_tool_path(cwd, raw).to_string_lossy().into_owned();
    let nfd: String = resolved.nfd().collect();
    let variants = [
        resolved.clone(),
        replace_am_pm_space(&resolved),
        nfd.clone(),
        resolved.replace('\'', "\u{2019}"),
        nfd.replace('\'', "\u{2019}"),
    ];
    variants
        .iter()
        .find(|v| fs::symlink_metadata(v).is_ok())
        .map_or_else(|| PathBuf::from(&resolved), PathBuf::from)
}

/// The path the OS would really open for the absolute, normalized `path`: the longest ancestor
/// that exists is canonicalized (every symlink in it followed), and the missing rest is put back.
/// A dangling symlink just past that ancestor is followed too, because writing through it creates
/// its target. `..` in a dangling link's target is collapsed lexically.
pub fn resolve_symlinks(path: &Path) -> PathBuf {
    // The kernel's own limit on links followed in one lookup (Linux and macOS: 40 and 32).
    const MAX_LINKS: usize = 40;
    let mut path = path.to_path_buf();
    for _ in 0..MAX_LINKS {
        let mut base = path.clone();
        let mut tail: Vec<OsString> = Vec::new();
        let mut real = loop {
            if let Ok(real) = fs::canonicalize(&base) {
                break real;
            }
            match (base.file_name().map(|n| n.to_os_string()), base.parent()) {
                (Some(name), Some(parent)) => {
                    tail.push(name);
                    base = parent.to_path_buf();
                }
                _ => return path,
            }
        };
        let Some(first) = tail.pop() else {
            return real;
        };
        let next = real.join(&first);
        if let Ok(target) = fs::read_link(&next) {
            let mut followed = real.join(target);
            followed.extend(tail.iter().rev());
            path = lexical_normalize(&followed);
            continue;
        }
        real.push(first);
        real.extend(tail.iter().rev());
        return real;
    }
    // A link loop: the tool fails with ELOOP, so any subject will do.
    path
}

fn home_dir_string() -> String {
    dirs::home_dir()
        .map(|p| p.to_string_lossy().into_owned())
        .unwrap_or_default()
}

/// rpi-tools' `normalize_absolute`: drops `.`, and `..` pops (a no-op at the root).
fn rpi_normalize(p: &Path) -> PathBuf {
    let mut out = PathBuf::new();
    for comp in p.components() {
        match comp {
            Component::ParentDir => {
                out.pop();
            }
            Component::CurDir => {}
            other => out.push(other),
        }
    }
    out
}

/// rpi-tools' `file_url_to_path`: `file:///abs` → `/abs`, `file:///C:/x` → `C:/x`,
/// `file://rest` → `rest`. No percent-decoding.
fn file_url_to_path(url: &str) -> Option<String> {
    let rest = url.strip_prefix("file://")?;
    if let Some(drive_rest) = rest.strip_prefix('/') {
        if drive_rest.len() >= 2 && drive_rest.as_bytes()[1] == b':' {
            return Some(drive_rest.to_string());
        }
        return Some(format!("/{drive_rest}"));
    }
    Some(rest.to_string())
}

/// rpi-tools' `replace_am_pm_space`: ` AM.`/` PM.` (ASCII, any case) get a narrow no-break space.
fn replace_am_pm_space(s: &str) -> String {
    let bytes = s.as_bytes();
    let mut out = String::with_capacity(s.len());
    let mut i = 0;
    while i < bytes.len() {
        if i + 3 < bytes.len() && bytes[i] == b' ' {
            let a = bytes[i + 1].to_ascii_lowercase();
            let m = bytes[i + 2].to_ascii_lowercase();
            if (a == b'a' || a == b'p') && m == b'm' && bytes[i + 3] == b'.' {
                out.push('\u{202F}');
                out.push(bytes[i + 1] as char);
                out.push(bytes[i + 2] as char);
                out.push('.');
                i += 4;
                continue;
            }
        }
        let first = bytes[i];
        let len = if first < 0x80 {
            1
        } else if first >> 5 == 0b110 {
            2
        } else if first >> 4 == 0b1110 {
            3
        } else if first >> 3 == 0b11110 {
            4
        } else {
            1
        };
        let end = (i + len).min(bytes.len());
        if let Ok(slice) = std::str::from_utf8(&bytes[i..end]) {
            out.push_str(slice);
        }
        i = end;
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

    fn words(cmd: &str) -> Vec<String> {
        shell_words(cmd).0
    }

    #[test]
    fn shell_words_unquote_like_bash() {
        assert_eq!(
            words("git log  -1 --oneline"),
            ["git", "log", "-1", "--oneline"]
        );
        assert_eq!(words("git log '--out'put=x"), ["git", "log", "--output=x"]);
        assert_eq!(
            words("git log \"--output\"=x"),
            ["git", "log", "--output=x"]
        );
        assert_eq!(words("git log --out\\put=x"), ["git", "log", "--output=x"]);
        assert_eq!(words("git log $'--output'=x"), ["git", "log", "--output=x"]);
        assert_eq!(words("echo 'a b' \"c d\""), ["echo", "a b", "c d"]);
        assert!(!shell_words("git log HEAD@{1} --format='%h {a,b}'").1);
        assert!(!shell_words("ls \"$HOME\" \"/${HOME}\" \"/${A_1}x\"").1);
        assert!(!shell_words("ls $ x$").1);
        assert!(!shell_words("ls *.md src/*").1);
        assert!(!shell_words("git log -1 -- 'src/*' \\*.rs \"[ab]\"").1);
        for expands in [
            "ls $HOME",
            "ls /${A_1}x",
            "git log -1 $IFS--output=f",
            "git log -1 x${IFS}--output=f",
            "git log $@",
            "git diff *",
            "git log -1 src/*",
            "git diff '-'-output*",
            "git diff HEAD ?-output=f",
            "/usr/bin/git log [-]-output=f",
            "git log ${X:---output=f}",
            "git log \"${X:---output=f}\"",
            "git log {--output=f,}",
            "git log --o{u,}tput=f",
            "ls {a..c}",
        ] {
            assert!(shell_words(expands).1, "{expands}");
        }
    }

    #[test]
    fn output_options_count_as_writing_a_file() {
        for writes in [
            "git log --output=x",
            "git diff --output x",
            "git log -1 --format='%n' --output=~/.zshrc",
            "git show --outp=x",
            "git log --o=x",
            "git format-patch --output-directory d",
            "git format-patch -o d",
            "git format-patch -od",
            "/usr/bin/git log -o x",
            "git log '--output'=x",
            "git log --out\\put=x",
            "anything --output=x",
            "ls && git diff --output=x",
        ] {
            assert!(scan_shell(writes).output_option, "{writes}");
        }
        for reads in [
            "git log --oneline -5",
            "git diff HEAD~1 --stat",
            "git show HEAD",
            "git log --format='--output=x'",
            "git log -- --output",
            "ls -o",
            "git status --porcelain",
            "git log --ours",
        ] {
            assert!(!scan_shell(reads).output_option, "{reads}");
        }
    }

    /// The review #2101 bypasses: `$'…'` and `#` comments used to desync the lexer from bash, so
    /// a whole script read as one `ls …` command while bash ran `rm -rf x` too.
    #[test]
    fn shell_scan_follows_ansi_c_quotes_and_comments() {
        let s = scan_shell("ls $'\\'';rm -rf x #'");
        assert_eq!(s.commands, vec!["ls $'\\''", "rm -rf x"]);
        assert!(s.unparseable, "$'...' is always flagged");
        let s = scan_shell("ls #'\nrm -rf x\n#'");
        assert_eq!(s.commands, vec!["ls", "rm -rf x"]);
        assert!(!s.unparseable, "a comment is understood, not guessed at");

        // Comments start only at a word start, and only outside quotes.
        assert_eq!(scan_shell("ls -la # list it").commands, vec!["ls -la"]);
        assert_eq!(scan_shell("# just a note").commands, Vec::<String>::new());
        assert_eq!(scan_shell("ls a#b").commands, vec!["ls a#b"]);
        assert_eq!(scan_shell("echo $#").commands, vec!["echo $#"]);
        assert_eq!(scan_shell("echo '#' x").commands, vec!["echo '#' x"]);
        assert_eq!(scan_shell("echo \"#\" x").commands, vec!["echo \"#\" x"]);
        assert_eq!(scan_shell("echo \\# x").commands, vec!["echo \\# x"]);
        assert_eq!(scan_shell("ls;#x\nls -a").commands, vec!["ls", "ls -a"]);
        assert_eq!(scan_shell("ls|#x\nwc").commands, vec!["ls", "wc"]);
        // A line continuation is removed, as bash does.
        assert_eq!(scan_shell("ls \\\n-la").commands, vec!["ls -la"]);

        // Anything the lexer can't be sure of is flagged.
        assert!(scan_shell("ls 'abc").unparseable, "unclosed single quote");
        assert!(scan_shell("ls \"abc").unparseable, "unclosed double quote");
        assert!(scan_shell("ls $'abc").unparseable, "unclosed ANSI-C quote");
        assert!(
            scan_shell("ls \"a\nb\"").unparseable,
            "newline inside quotes"
        );
        assert!(
            scan_shell("cat <<EOF\n'\nEOF\nrm -rf x\n'").unparseable,
            "a here-document quote that swallows real commands"
        );
        assert!(!scan_shell("git status && git log --oneline").unparseable);
        assert!(
            !scan_shell("echo $\"x\"").unparseable,
            "$\"...\" is a plain string"
        );

        // `<>` opens a file read-write and creates it.
        assert!(scan_shell("ls <> x").writes_file);
        assert!(scan_shell("ls 3<>x").writes_file);
        assert!(!scan_shell("ls < x").writes_file);
    }

    /// Cross-check the lexer against real bash: `bash -xc` traces every simple command it runs,
    /// one `+ ` line each, so for scripts made only of `echo`/`true` the counts must agree. This
    /// includes both #2101 bypasses, rewritten so the hidden command is a harmless `echo`.
    #[test]
    fn shell_scan_agrees_with_real_bash() {
        let Ok(probe) = std::process::Command::new("bash")
            .arg("-c")
            .arg("true")
            .output()
        else {
            eprintln!("bash not found; skipping");
            return;
        };
        assert!(probe.status.success());
        for script in [
            "echo $'\\'';echo PWNED #'",
            "echo $'\\'' && echo PWNED #'",
            "echo $'\\'' | echo PWNED #'",
            "echo #'\necho PWNED\n#'",
            "echo #'\necho a && echo PWNED\n#'",
            "echo #'\necho a | echo PWNED\n#'",
            "echo a;#x\necho b",
            "echo a|#x\ncat",
            "echo a&#x\necho b",
            "(echo c)#x\necho d",
            "echo a#b; echo c",
            "echo '#' \"#\" \\# x # y",
            "echo \\\n#x; echo b",
            "echo 'a;b' \"c && d\" | cat && true",
            "echo $'a\\tb'; echo c",
        ] {
            let out = std::process::Command::new("bash")
                .arg("-xc")
                .arg(script)
                .output()
                .unwrap();
            let traced = String::from_utf8_lossy(&out.stderr)
                .lines()
                .filter(|l| l.starts_with('+'))
                .count();
            let scan = scan_shell(script);
            assert_eq!(
                scan.commands.len(),
                traced,
                "{script:?}: lexer {:?} vs bash trace {:?}",
                scan.commands,
                String::from_utf8_lossy(&out.stderr)
            );
            if String::from_utf8_lossy(&out.stdout).contains("PWNED") {
                assert!(
                    scan.commands.iter().any(|c| c == "echo PWNED"),
                    "{script:?}: bash ran `echo PWNED` but the lexer saw {:?}",
                    scan.commands
                );
            }
        }
    }

    #[test]
    fn restricted_bash_denies_what_the_lexer_cannot_split() {
        let p = Policy::parse(Some(&json!({"bash": {"*": "deny", "ls *": "allow"}}))).unwrap();
        let tp = ToolPolicy::new("amp-manager", false, p, "/repo/wt");
        let bash = |cmd: &str| tp.check("bash", &json!({ "command": cmd }));
        assert_eq!(
            bash("ls $'\\'';rm -rf x #'").unwrap_err().reason,
            DenialReason::Unparseable
        );
        assert_eq!(
            bash("ls $'a'").unwrap_err().reason,
            DenialReason::Unparseable
        );
        assert_eq!(
            bash("ls #'\nrm -rf x\n#'").unwrap_err().reason,
            DenialReason::Deny,
            "the comment is dropped, so `rm -rf x` is checked on its own"
        );
        assert_eq!(bash("ls 'x").unwrap_err().reason, DenialReason::Unparseable);
        assert_eq!(bash("ls <> x").unwrap_err().reason, DenialReason::Redirect);
        assert!(bash("ls -la # list").is_ok());
        let text = bash("ls $'x'").unwrap_err().to_string();
        assert!(text.contains("can't split this script"), "{text}");
        // An unrestricted policy runs whatever bash would.
        let tp = ToolPolicy::new("amp-x", true, Policy::default(), "/repo/wt");
        assert!(tp.check("bash", &json!({"command": "ls $'x'"})).is_ok());
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
        // (A directory that doesn't exist: on macOS `/etc` is a link to `/private/etc`.)
        let d = tp
            .check("write", &json!({"path": "../../amp-no-such-dir/passwd"}))
            .unwrap_err();
        assert_eq!(d.permission, "external_directory");
        assert_eq!(d.subject, "/amp-no-such-dir/passwd");
        assert_eq!(d.reason, DenialReason::Ask);
        assert_eq!(
            tp.check("write", &json!({})).unwrap_err().reason,
            DenialReason::BadArguments
        );
    }
}
