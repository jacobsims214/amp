//! `amp-harness mcp import` (#2244, H10 of decisions/amp-harness-configurability.md, D3): copies MCP
//! servers from Claude Code, opencode, VS Code, Cursor and Claude Desktop into `[mcp.<name>]`
//! tables, explicitly. These files are never loaded on their own: hidden auto-loading would bypass
//! repo trust and leave "where did this server come from" unanswerable.
//!
//! | Source | Files (highest precedence first) | Shape |
//! |---|---|---|
//! | Claude Code | `~/.claude.json` `projects[<project>].mcpServers` (local), `<project>/.mcp.json`, `~/.claude.json` `mcpServers` (user); `$CLAUDE_CONFIG_DIR/.claude.json` first when set | `mcpServers`; `${VAR}` / `${VAR:-d}` |
//! | opencode | `<project>/opencode.json[c]`, `$OPENCODE_CONFIG`, `~/.config/opencode/opencode.json[c]` | `mcp` (local/remote); `{env:VAR}`, `{file:path}` |
//! | VS Code | `<project>/.vscode/mcp.json`, the user profile's `mcp.json` | `servers` + `inputs`; `${input:id}`, `${workspaceFolder}`, `${userHome}`, `${env:X}` |
//! | Cursor | `<project>/.cursor/mcp.json`, `~/.cursor/mcp.json` | `mcpServers`, `auth`; same variables as VS Code |
//! | Claude Desktop | `~/Library/Application Support/Claude/claude_desktop_config.json` (Linux: `~/.config/Claude/`) | `mcpServers` (values are literal) |
//!
//! Each source's variables are rewritten to ours (`${env:VAR}`, `${repo}`, `${home}`), and the
//! server then goes through the same JSON mapping and writer as `mcp add-json`
//! ([`mcp_crud::new_from_json`], [`mcp_crud::add_server`]): checked before anything is written, a
//! backup first, secrets in the keychain before the config names them, repo servers trusted for the
//! person importing them.
//!
//! **Secrets.** A literal header/env value that looks secret (or an OAuth client secret) is moved
//! to the OS keychain; the TOML names it in `secret_headers` / `secret_env`. A VS Code
//! `${input:id}` or an opencode `{file:path}` in a header or env value is asked at import (hidden,
//! or `--secret-stdin`) and stored the same way; the referenced file is never read. Output and
//! `--dry-run` name each secret and where it goes, never its value. Under
//! `AMP_CREDENTIAL_STORE=file` a server that needs the keychain is refused before anything is
//! written.

use std::{
    collections::{BTreeMap, BTreeSet},
    io::Read as _,
    path::{Path, PathBuf},
};

use amp_core::mcp_config::{self, env_account, header_account, oauth_client_secret_account};
use clap::{Args, ValueEnum};
use serde::Serialize;
use serde_json::{Map, Value, json};

use crate::{
    agents_cmd::{Ctx, Scope},
    conn::{CliError, print_json},
    mcp_crud::{self, AddOpts, AddReport, Expect, NewServer, ScopeOpt, SecretItem},
    setup::ui::{self, Ui},
};

/// Source files bigger than this are refused (`~/.claude.json` holds history; a few MB is normal).
const MAX_FILE_BYTES: u64 = 64 << 20;
/// Marks a value the import asks for, inside a string, until it is known (`\u{1}<key>\u{1}`).
const MARK: char = '\u{1}';

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, ValueEnum, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum Source {
    /// Claude Code: .mcp.json and ~/.claude.json (user and local scope).
    #[value(alias = "claude-code")]
    Claude,
    /// opencode: opencode.json[c] (project and global).
    Opencode,
    /// VS Code: .vscode/mcp.json and the user profile's mcp.json.
    #[value(alias = "code", alias = "vs-code")]
    Vscode,
    /// Cursor: .cursor/mcp.json and ~/.cursor/mcp.json.
    Cursor,
    /// Claude Desktop: claude_desktop_config.json.
    #[value(alias = "desktop")]
    ClaudeDesktop,
}

impl Source {
    const ALL: [Source; 5] = [
        Source::Claude,
        Source::Opencode,
        Source::Vscode,
        Source::Cursor,
        Source::ClaudeDesktop,
    ];

    fn word(self) -> &'static str {
        match self {
            Source::Claude => "claude",
            Source::Opencode => "opencode",
            Source::Vscode => "vscode",
            Source::Cursor => "cursor",
            Source::ClaudeDesktop => "claude-desktop",
        }
    }

    fn title(self) -> &'static str {
        match self {
            Source::Claude => "Claude Code",
            Source::Opencode => "opencode",
            Source::Vscode => "VS Code",
            Source::Cursor => "Cursor",
            Source::ClaudeDesktop => "Claude Desktop",
        }
    }
}

#[derive(Args)]
pub struct ImportArgs {
    /// Where from: claude, opencode, vscode, cursor, claude-desktop, or a config file in one of
    /// their formats. Without it: lists what is found here and asks (--dry-run previews them all).
    #[arg(long, value_name = "SOURCE|FILE")]
    pub from: Option<String>,
    /// Import only these servers (their names in the source, or the names they get here).
    #[arg(long, value_delimiter = ',', value_name = "NAME,…")]
    pub only: Vec<String>,
    /// Give a server another name here (repeatable).
    #[arg(long, value_name = "OLD=NEW")]
    pub rename: Vec<String>,
    /// Show the TOML that would be written (secrets only by name) and write nothing.
    #[arg(long)]
    pub dry_run: bool,
    /// Don't ask before writing (needed without a terminal).
    #[arg(long, short = 'y')]
    pub yes: bool,
    /// Replace a server of the same name instead of importing this one as <name>-2.
    #[arg(long)]
    pub force: bool,
    /// Read the values the import asks for (VS Code inputs, opencode {file:…}) from stdin, one per
    /// line, in the order the preview lists them.
    #[arg(long)]
    pub secret_stdin: bool,
    /// Connect to each imported server afterwards (`mcp test`).
    #[arg(long)]
    pub test: bool,
    #[command(flatten)]
    pub scope: ScopeOpt,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

// ------------------------------------------------------------------------------------------------
// Finding and reading source files
// ------------------------------------------------------------------------------------------------

/// A VS Code `inputs` entry.
#[derive(Debug, Clone)]
struct Input {
    kind: String,
    description: String,
    password: bool,
}

/// One config file (or one part of one: `~/.claude.json` holds a user and a local list).
#[derive(Debug)]
struct SourceFile {
    source: Source,
    path: PathBuf,
    /// "project .mcp.json", "user ~/.claude.json", …
    label: String,
    servers: Vec<(String, Value)>,
    inputs: BTreeMap<String, Input>,
    /// Claude Code's `disabledMcpServers` for this project.
    disabled: BTreeSet<String>,
}

fn env_path(k: &str) -> Option<PathBuf> {
    std::env::var_os(k)
        .filter(|v| !v.is_empty())
        .map(PathBuf::from)
}

fn home() -> Option<PathBuf> {
    env_path("HOME")
}

fn xdg_config() -> Option<PathBuf> {
    env_path("XDG_CONFIG_HOME")
        .filter(|p| p.is_absolute())
        .or_else(|| home().map(|h| h.join(".config")))
}

/// Where a desktop app keeps its settings: `~/Library/Application Support/<app>` on macOS, else
/// `$XDG_CONFIG_HOME/<app>`.
fn app_dir(app: &str) -> Option<PathBuf> {
    if cfg!(target_os = "macos") {
        home().map(|h| h.join("Library/Application Support").join(app))
    } else {
        xdg_config().map(|c| c.join(app))
    }
}

fn claude_json() -> Option<PathBuf> {
    if let Some(dir) = env_path("CLAUDE_CONFIG_DIR") {
        let p = dir.join(".claude.json");
        if p.is_file() {
            return Some(p);
        }
    }
    home().map(|h| h.join(".claude.json"))
}

/// `~` instead of the home directory, for labels.
fn tilde(p: &Path) -> String {
    match home().and_then(|h| p.strip_prefix(&h).ok().map(Path::to_path_buf)) {
        Some(rest) => format!("~/{}", rest.display()),
        None => p.display().to_string(),
    }
}

/// Reads JSON or JSONC (comments, trailing commas). Errors never quote the file's content.
fn read_json(path: &Path) -> Result<Value, String> {
    let meta = std::fs::metadata(path).map_err(|e| format!("{}: {e}", path.display()))?;
    if meta.len() > MAX_FILE_BYTES {
        return Err(format!(
            "{} is larger than {} MB",
            path.display(),
            MAX_FILE_BYTES >> 20
        ));
    }
    let text = std::fs::read_to_string(path).map_err(|e| format!("{}: {e}", path.display()))?;
    serde_json::from_str(&strip_jsonc(&text)).map_err(|e| {
        format!(
            "{} does not parse as JSON (line {}, column {})",
            path.display(),
            e.line(),
            e.column()
        )
    })
}

/// JSONC -> JSON: drops `//` and `/* */` comments (newlines kept, so positions still match) and
/// trailing commas, outside strings. A leading BOM is dropped too.
fn strip_jsonc(text: &str) -> String {
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);
    let b: Vec<char> = text.chars().collect();
    let mut no_comments = String::with_capacity(text.len());
    let (mut i, mut in_str) = (0, false);
    while i < b.len() {
        let c = b[i];
        if in_str {
            no_comments.push(c);
            if c == '\\' && i + 1 < b.len() {
                no_comments.push(b[i + 1]);
                i += 2;
                continue;
            }
            if c == '"' {
                in_str = false;
            }
            i += 1;
            continue;
        }
        match (c, b.get(i + 1)) {
            ('"', _) => {
                in_str = true;
                no_comments.push(c);
                i += 1;
            }
            ('/', Some('/')) => {
                while i < b.len() && b[i] != '\n' {
                    i += 1;
                }
            }
            ('/', Some('*')) => {
                i += 2;
                while i < b.len() && !(b[i] == '*' && b.get(i + 1) == Some(&'/')) {
                    if b[i] == '\n' {
                        no_comments.push('\n');
                    }
                    i += 1;
                }
                i += 2;
            }
            _ => {
                no_comments.push(c);
                i += 1;
            }
        }
    }
    // Trailing commas: a `,` followed (after whitespace) by `}` or `]`.
    let b: Vec<char> = no_comments.chars().collect();
    let mut out = String::with_capacity(no_comments.len());
    let mut in_str = false;
    let mut i = 0;
    while i < b.len() {
        let c = b[i];
        if in_str {
            out.push(c);
            if c == '\\' && i + 1 < b.len() {
                out.push(b[i + 1]);
                i += 2;
                continue;
            }
            if c == '"' {
                in_str = false;
            }
        } else if c == '"' {
            in_str = true;
            out.push(c);
        } else if c == ',' {
            let next = b[i + 1..].iter().find(|c| !c.is_whitespace());
            if !matches!(next, Some('}') | Some(']')) {
                out.push(c);
            }
        } else {
            out.push(c);
        }
        i += 1;
    }
    out
}

fn servers_of(v: Option<&Value>) -> Vec<(String, Value)> {
    match v {
        Some(Value::Object(m)) => m.iter().map(|(k, v)| (k.clone(), v.clone())).collect(),
        _ => Vec::new(),
    }
}

fn inputs_of(v: &Value) -> BTreeMap<String, Input> {
    let mut out = BTreeMap::new();
    if let Some(Value::Array(a)) = v.get("inputs") {
        for i in a {
            if let Some(id) = i.get("id").and_then(Value::as_str) {
                out.insert(
                    id.to_owned(),
                    Input {
                        kind: i
                            .get("type")
                            .and_then(Value::as_str)
                            .unwrap_or("promptString")
                            .to_owned(),
                        description: i
                            .get("description")
                            .and_then(Value::as_str)
                            .unwrap_or(id)
                            .to_owned(),
                        password: i.get("password").and_then(Value::as_bool).unwrap_or(false),
                    },
                );
            }
        }
    }
    out
}

fn file(source: Source, path: &Path, label: String, root: &Value, key: &str) -> SourceFile {
    SourceFile {
        source,
        path: path.to_path_buf(),
        label,
        servers: servers_of(root.get(key)),
        inputs: if source == Source::Vscode {
            inputs_of(root)
        } else {
            BTreeMap::new()
        },
        disabled: BTreeSet::new(),
    }
}

/// `~/.claude.json`: the local list for `projects` (first match), then the user list.
fn claude_json_files(path: &Path, root: &Value, projects: &[PathBuf]) -> Vec<SourceFile> {
    let mut out = Vec::new();
    if let Some(Value::Object(all)) = root.get("projects") {
        let keys: Vec<String> = projects
            .iter()
            .flat_map(|p| {
                let canon = std::fs::canonicalize(p).unwrap_or_else(|_| p.clone());
                [p.display().to_string(), canon.display().to_string()]
            })
            .collect();
        if let Some((key, entry)) = keys.iter().find_map(|k| all.get(k).map(|e| (k.clone(), e))) {
            let mut f = file(
                Source::Claude,
                path,
                format!("local scope in {} (projects[\"{key}\"])", tilde(path)),
                entry,
                "mcpServers",
            );
            if let Some(Value::Array(d)) = entry.get("disabledMcpServers") {
                f.disabled = d
                    .iter()
                    .filter_map(|x| x.as_str().map(str::to_owned))
                    .collect();
            }
            out.push(f);
        }
    }
    out.push(file(
        Source::Claude,
        path,
        format!("user scope in {}", tilde(path)),
        root,
        "mcpServers",
    ));
    out
}

/// The files `source` keeps MCP servers in that exist here, highest precedence first.
fn find(source: Source, projects: &[PathBuf]) -> Result<Vec<SourceFile>, String> {
    let mut out = Vec::new();
    let read = |out: &mut Vec<SourceFile>, path: PathBuf, label: &str, key: &str| {
        read_into(out, source, path, label, key)
    };
    match source {
        Source::Claude => {
            let user = claude_json().filter(|p| p.is_file());
            let root = user.as_deref().map(read_json).transpose()?;
            let mut files = match (&user, &root) {
                (Some(p), Some(r)) => claude_json_files(p, r, projects),
                _ => Vec::new(),
            };
            // local, then project .mcp.json, then user.
            let user_list = files.pop();
            out.append(&mut files);
            for p in projects {
                read(&mut out, p.join(".mcp.json"), "project", "mcpServers")?;
            }
            out.extend(user_list);
        }
        Source::Opencode => {
            for p in projects {
                read(&mut out, p.join("opencode.jsonc"), "project", "mcp")?;
                read(&mut out, p.join("opencode.json"), "project", "mcp")?;
            }
            if let Some(p) = env_path("OPENCODE_CONFIG") {
                read(&mut out, p, "$OPENCODE_CONFIG", "mcp")?;
            }
            if let Some(c) = xdg_config() {
                read(&mut out, c.join("opencode/opencode.jsonc"), "global", "mcp")?;
                read(&mut out, c.join("opencode/opencode.json"), "global", "mcp")?;
            }
        }
        Source::Vscode => {
            for p in projects {
                read(&mut out, p.join(".vscode/mcp.json"), "workspace", "servers")?;
            }
            if let Some(d) = app_dir("Code") {
                read(&mut out, d.join("User/mcp.json"), "user", "servers")?;
            }
        }
        Source::Cursor => {
            for p in projects {
                read(
                    &mut out,
                    p.join(".cursor/mcp.json"),
                    "project",
                    "mcpServers",
                )?;
            }
            if let Some(h) = home() {
                read(&mut out, h.join(".cursor/mcp.json"), "global", "mcpServers")?;
            }
        }
        Source::ClaudeDesktop => {
            if let Some(d) = app_dir("Claude") {
                read(
                    &mut out,
                    d.join("claude_desktop_config.json"),
                    "",
                    "mcpServers",
                )?;
            }
        }
    }
    // VS Code also accepts the portable `mcpServers` key.
    for f in &mut out {
        if f.source == Source::Vscode && f.servers.is_empty() {
            let root = read_json(&f.path)?;
            f.servers = servers_of(root.get("mcpServers"));
        }
    }
    Ok(out)
}

fn read_into(
    out: &mut Vec<SourceFile>,
    source: Source,
    path: PathBuf,
    label: &str,
    key: &str,
) -> Result<(), String> {
    if path.is_file() && !out.iter().any(|f| f.path == path) {
        let root = read_json(&path)?;
        let label = if label.is_empty() {
            tilde(&path)
        } else {
            format!("{label} {}", tilde(&path))
        };
        out.push(file(source, &path, label, &root, key));
    }
    Ok(())
}

/// `--from FILE`: which tool's format a file is in.
fn read_named_file(path: &Path, projects: &[PathBuf]) -> Result<Vec<SourceFile>, String> {
    if !path.is_file() {
        return Err(format!("{} is not a file", path.display()));
    }
    let root = read_json(path)?;
    let name = path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    let in_dir = |d: &str| {
        path.parent()
            .and_then(Path::file_name)
            .is_some_and(|p| p == d)
    };
    let source = if name == ".claude.json" {
        return Ok(claude_json_files(path, &root, projects));
    } else if name == "claude_desktop_config.json" {
        Source::ClaudeDesktop
    } else if name.starts_with("opencode.json")
        || (root.get("mcp").is_some_and(Value::is_object) && root.get("mcpServers").is_none())
    {
        Source::Opencode
    } else if in_dir(".vscode") || root.get("servers").is_some() || root.get("inputs").is_some() {
        Source::Vscode
    } else if in_dir(".cursor") {
        Source::Cursor
    } else if root.get("mcpServers").is_some() {
        Source::Claude
    } else {
        return Err(format!(
            "{} has no MCP servers in a format amp-harness knows (`mcpServers`, `servers` or \
             opencode's `mcp`)",
            path.display()
        ));
    };
    let key = match source {
        Source::Opencode => "mcp",
        Source::Vscode if root.get("servers").is_some() => "servers",
        _ => "mcpServers",
    };
    Ok(vec![file(
        source,
        path,
        format!("file {}", tilde(path)),
        &root,
        key,
    )])
}

// ------------------------------------------------------------------------------------------------
// Converting one server
// ------------------------------------------------------------------------------------------------

/// A value the import asks the person for.
#[derive(Debug, Clone, Serialize)]
struct Ask {
    /// Unique within one import: `<file>#input:<id>` / `<file>#file:<path>`.
    #[serde(skip)]
    key: String,
    /// What the person is asked.
    prompt: String,
    /// Typed hidden and kept only in the keychain.
    hidden: bool,
}

/// A keychain secret a server gets: a value from the source, or one made of asked values.
#[derive(Debug, Clone)]
struct PlannedSecret {
    item: SecretItem,
    /// The value, with asked parts as markers.
    value: String,
    /// `literal` (moved out of the source file) or `asked`.
    from: &'static str,
}

/// What VS Code / Cursor substitute that amp-harness has no equivalent for.
const EDITOR_ONLY: [&str; 17] = [
    "workspaceFolderBasename",
    "file",
    "fileWorkspaceFolder",
    "relativeFile",
    "relativeFileDirname",
    "fileBasename",
    "fileBasenameNoExtension",
    "fileExtname",
    "fileDirname",
    "fileDirnameBasename",
    "cwd",
    "lineNumber",
    "selectedText",
    "execPath",
    "defaultBuildTask",
    "workspaceRoot",
    "extensionInstallFolder",
];

fn marker(key: &str) -> String {
    format!("{MARK}{key}{MARK}")
}

/// One string from `src` in our syntax: references rewritten, asked values as markers (pushed to
/// `asks`). Claude Code's `${VAR}` is left for [`mcp_crud::env_refs`].
fn norm(src: &SourceFile, s: &str, asks: &mut Vec<Ask>) -> Result<String, String> {
    match src.source {
        Source::Claude => Ok(s.to_owned()),
        // Taken literally: `${` must stay text.
        Source::ClaudeDesktop => Ok(s.replace("${", "$${")),
        Source::Opencode => norm_opencode(src, s, asks),
        Source::Vscode | Source::Cursor => norm_editor(src, s, asks),
    }
}

fn norm_opencode(src: &SourceFile, s: &str, asks: &mut Vec<Ask>) -> Result<String, String> {
    let mut out = String::with_capacity(s.len());
    let mut rest = s;
    while let Some(c) = rest.chars().next() {
        let reference = ["{env:", "{file:"]
            .into_iter()
            .find(|p| rest.starts_with(p))
            .and_then(|p| rest.find('}').map(|end| (p, end)));
        if let Some((prefix, end)) = reference {
            if out.ends_with('$') {
                return Err(format!(
                    "`${}…}}` (a `$` right before an opencode reference) can't be written in \
                     amp-harness's syntax",
                    &prefix[..prefix.len() - 1]
                ));
            }
            let body = &rest[prefix.len()..end];
            if prefix == "{env:" {
                // opencode puts an empty string for an unset variable.
                out.push_str(&format!("${{env:{body}:-}}"));
            } else {
                let key = format!("{}#file:{body}", src.path.display());
                if !asks.iter().any(|a| a.key == key) {
                    asks.push(Ask {
                        key: key.clone(),
                        prompt: format!(
                            "the contents of {{file:{body}}} (opencode reads it from that file; \
                             amp-harness keeps it in the OS keychain)"
                        ),
                        hidden: true,
                    });
                }
                out.push_str(&marker(&key));
            }
            rest = &rest[end + 1..];
            continue;
        }
        if rest.starts_with("${") {
            out.push_str("$${");
            rest = &rest[2..];
            continue;
        }
        out.push(c);
        rest = &rest[c.len_utf8()..];
    }
    Ok(out)
}

fn norm_editor(src: &SourceFile, s: &str, asks: &mut Vec<Ask>) -> Result<String, String> {
    let mut out = String::with_capacity(s.len());
    let mut rest = s;
    while let Some(i) = rest.find("${") {
        out.push_str(&rest[..i]);
        let after = &rest[i + 2..];
        let Some(end) = after.find('}') else {
            out.push_str("$${");
            rest = after;
            continue;
        };
        let body = &after[..end];
        rest = &after[end + 1..];
        match body {
            "workspaceFolder" => out.push_str("${repo}"),
            "userHome" => out.push_str("${home}"),
            "pathSeparator" | "/" => out.push(std::path::MAIN_SEPARATOR),
            _ if body.starts_with("env:") => {
                let var = &body[4..];
                if var.contains(":-") {
                    out.push_str(&format!("${{env:{var}}}"));
                } else {
                    // The editors put an empty string for an unset variable.
                    out.push_str(&format!("${{env:{var}:-}}"));
                }
            }
            _ if body.starts_with("input:") && src.source == Source::Vscode => {
                let id = &body[6..];
                let input = src.inputs.get(id).ok_or_else(|| {
                    format!("`${{input:{id}}}` has no entry in the file's `inputs`")
                })?;
                if input.kind != "promptString" {
                    return Err(format!(
                        "`${{input:{id}}}` is a {} input; only promptString inputs can be asked \
                         at import",
                        input.kind
                    ));
                }
                let key = format!("{}#input:{id}", src.path.display());
                if !asks.iter().any(|a| a.key == key) {
                    asks.push(Ask {
                        key: key.clone(),
                        prompt: format!("{} (VS Code input `{id}`)", input.description),
                        hidden: input.password,
                    });
                }
                out.push_str(&marker(&key));
            }
            _ if EDITOR_ONLY.contains(&body)
                || body.starts_with("input:")
                || body.starts_with("config:")
                || body.starts_with("command:")
                || body.starts_with("workspaceFolder:") =>
            {
                return Err(format!(
                    "`${{{body}}}` has no amp-harness equivalent (supported: ${{workspaceFolder}}, \
                     ${{userHome}}, ${{env:VAR}}, ${{pathSeparator}}{})",
                    if src.source == Source::Vscode {
                        ", ${input:id}"
                    } else {
                        ""
                    }
                ));
            }
            // `${NAME}` / `${NAME:-default}` (the portable form): env_refs makes it ${env:NAME}.
            _ => {
                out.push_str("${");
                out.push_str(body);
                out.push('}');
            }
        }
    }
    out.push_str(rest);
    Ok(out)
}

/// Whether a string has asked parts, and whether any is hidden.
fn asked_in(s: &str, asks: &[Ask]) -> (bool, bool) {
    let found: Vec<&Ask> = asks
        .iter()
        .filter(|a| s.contains(&marker(&a.key)))
        .collect();
    (!found.is_empty(), found.iter().any(|a| a.hidden))
}

/// A converted server: the table (asked parts as markers), the keychain secrets, the values to ask.
#[derive(Debug)]
struct Converted {
    table: toml::Table,
    secrets: Vec<PlannedSecret>,
    asks: Vec<Ask>,
    warnings: Vec<String>,
}

fn convert(
    src: &SourceFile,
    source_name: &str,
    name: &str,
    v: &Value,
) -> Result<Converted, String> {
    let obj = v
        .as_object()
        .ok_or_else(|| "the server entry is not an object".to_owned())?;
    let mut asks = Vec::new();
    let mut warnings = Vec::new();
    let mut asked_secrets: Vec<(&'static str, String, String)> = Vec::new();
    let mut ask_client_secret: Option<String> = None;
    let mut out = Map::new();
    let mut opencode_timeout = None;
    let strings = |x: &Value, what: &str| -> Result<Vec<String>, String> {
        match x {
            Value::String(s) => Ok(vec![s.clone()]),
            Value::Array(a) => a
                .iter()
                .map(|i| {
                    i.as_str()
                        .map(str::to_owned)
                        .ok_or_else(|| format!("`{what}` must be strings"))
                })
                .collect(),
            _ => Err(format!("`{what}` must be a string or a list of strings")),
        }
    };
    for (k, x) in obj {
        match k.as_str() {
            "type" => {
                let t = x.as_str().unwrap_or_default().to_ascii_lowercase();
                let ours = match t.as_str() {
                    "stdio" | "local" => "stdio",
                    "http" | "streamable-http" | "streamablehttp" | "remote" => "http",
                    "sse" => "sse",
                    other => {
                        return Err(format!(
                            "type `{other}` is not supported (stdio, http or sse)"
                        ));
                    }
                };
                out.insert("type".into(), json!(ours));
            }
            "command" | "args" => {
                let mut parts = Vec::new();
                for s in strings(x, k)? {
                    let n = norm(src, &s, &mut asks)?;
                    if asked_in(&n, &asks).1 {
                        return Err(format!(
                            "`{k}` uses a value that is asked hidden (a password input or a \
                             file); amp-harness keeps secrets only in env and headers, so move \
                             it into `env` there, or add the server with `mcp add --secret-env`"
                        ));
                    }
                    parts.push(n);
                }
                out.insert(
                    k.clone(),
                    if k == "command" && x.is_string() {
                        json!(parts[0])
                    } else {
                        json!(parts)
                    },
                );
            }
            "url" | "cwd" | "envFile" => {
                let s = x
                    .as_str()
                    .ok_or_else(|| format!("`{k}` must be a string"))?;
                let n = norm(src, s, &mut asks)?;
                if asked_in(&n, &asks).1 {
                    return Err(format!("`{k}` uses a value that is asked hidden"));
                }
                out.insert(k.clone(), json!(n));
            }
            "env" | "environment" | "headers" => {
                let kind = if k == "headers" { "header" } else { "env" };
                let m = x
                    .as_object()
                    .ok_or_else(|| format!("`{k}` must be an object"))?;
                let mut keep = Map::new();
                for (mk, mv) in m {
                    let s = mv
                        .as_str()
                        .ok_or_else(|| format!("`{k}.{mk}` must be a string"))?;
                    let n = norm(src, s, &mut asks)?;
                    if asked_in(&n, &asks).0 {
                        asked_secrets.push((kind, mk.clone(), n));
                    } else {
                        keep.insert(mk.clone(), json!(n));
                    }
                }
                let key = if kind == "header" { "headers" } else { "env" };
                let entry = out.entry(key).or_insert_with(|| json!({}));
                if let Value::Object(e) = entry {
                    e.extend(keep);
                }
            }
            "oauth" => {
                if let Value::Object(o) = x {
                    let mut o = o.clone();
                    if let Some(secret) = o.get("clientSecret").and_then(Value::as_str) {
                        let n = norm(src, secret, &mut asks)?;
                        if n.contains("${") || n.contains(MARK) {
                            o.remove("clientSecret");
                            ask_client_secret = Some(n);
                        }
                    }
                    out.insert("oauth".into(), Value::Object(o));
                } else {
                    out.insert("oauth".into(), x.clone());
                }
            }
            // Cursor's `auth: {CLIENT_ID, CLIENT_SECRET, scopes}`.
            "auth" if src.source == Source::Cursor => {
                let mut o = Map::new();
                if let Some(id) = x.get("CLIENT_ID").and_then(Value::as_str) {
                    o.insert("clientId".into(), json!(norm(src, id, &mut asks)?));
                }
                if let Some(secret) = x.get("CLIENT_SECRET").and_then(Value::as_str) {
                    let n = norm(src, secret, &mut asks)?;
                    if n.contains("${") {
                        ask_client_secret = Some(n);
                    } else {
                        o.insert("clientSecret".into(), json!(n));
                    }
                }
                if let Some(s) = x.get("scopes") {
                    o.insert("scopes".into(), s.clone());
                }
                out.insert("oauth".into(), Value::Object(o));
            }
            "timeout" if src.source == Source::Opencode => {
                opencode_timeout = x.as_u64();
            }
            "timeout" | "enabled" | "disabled" => {
                out.insert(k.clone(), x.clone());
            }
            "headersHelper" => warnings.push(
                "dropped `headersHelper` (a command that prints headers is not supported); \
                 give the headers it prints with `amp-harness mcp edit` or `mcp secret`"
                    .to_owned(),
            ),
            "description" | "alwaysLoad" => {}
            other => warnings.push(format!("dropped `{other}` (amp-harness has no equivalent)")),
        }
    }
    if out
        .get("env")
        .is_some_and(|e| e.as_object().is_some_and(Map::is_empty))
    {
        out.remove("env");
    }
    if out
        .get("headers")
        .is_some_and(|e| e.as_object().is_some_and(Map::is_empty))
    {
        out.remove("headers");
    }
    // A server whose only env/headers are asked still needs its type from command/url.
    let text = Value::Object(out).to_string();
    let spec = mcp_crud::new_from_json(name, &text).map_err(|e| e.message)?;
    let mut table = spec.table;
    let mut secrets: Vec<PlannedSecret> = spec
        .secrets
        .into_iter()
        .map(|(item, v)| PlannedSecret {
            item,
            value: v.unwrap_or_default(),
            from: "literal",
        })
        .collect();
    let stdio = table.get("type").and_then(toml::Value::as_str) == Some("stdio");
    for (kind, key, value) in asked_secrets {
        let (list, item) = match kind {
            "header" if !stdio => (
                "secret_headers",
                SecretItem {
                    label: format!("header {key}"),
                    which: key.clone(),
                    account: header_account(name, &key),
                },
            ),
            "env" if stdio => (
                "secret_env",
                SecretItem {
                    label: format!("env {key}"),
                    which: key.clone(),
                    account: env_account(name, &key),
                },
            ),
            _ => {
                warnings.push(format!(
                    "dropped {kind} `{key}` (a {} server has no {kind}s)",
                    if stdio { "stdio" } else { "remote" }
                ));
                continue;
            }
        };
        let entry = table
            .entry(list)
            .or_insert_with(|| toml::Value::Array(Vec::new()));
        if let toml::Value::Array(a) = entry {
            a.push(toml::Value::String(key.clone()));
        }
        secrets.push(PlannedSecret {
            item,
            value: mcp_crud::env_refs(&value),
            from: "asked",
        });
    }
    if let Some(value) = ask_client_secret {
        if value.contains("${") && !value.contains(MARK) {
            return Err(
                "the OAuth client secret references an environment variable, which amp-harness \
                 can't do; import it without one and store it with `amp-harness mcp secret <name> \
                 oauth`"
                    .to_owned(),
            );
        }
        if let Some(toml::Value::Table(o)) = table.get_mut("oauth") {
            o.insert("client_secret".into(), toml::Value::Boolean(true));
        }
        secrets.push(PlannedSecret {
            item: SecretItem {
                label: "OAuth client secret".to_owned(),
                which: "oauth".to_owned(),
                account: oauth_client_secret_account(name),
            },
            value,
            from: "asked",
        });
    }
    if let Some(ms) = opencode_timeout {
        // opencode's timeout is how long to wait for the tool list, so a startup timeout here.
        table.remove("call_timeout_secs");
        table.insert(
            "startup_timeout_secs".into(),
            toml::Value::Integer(ms.div_ceil(1000).max(1) as i64),
        );
    }
    if src.disabled.contains(source_name) {
        table.insert("enabled".into(), toml::Value::Boolean(false));
    }
    // Asked values that ended up in no field (an env var on an http server, …) are not asked.
    let used = |a: &Ask| {
        let m = marker(&a.key);
        secrets.iter().any(|s| s.value.contains(&m)) || table_has(&table, &m)
    };
    let asks: Vec<Ask> = asks.iter().filter(|a| used(a)).cloned().collect();
    Ok(Converted {
        table,
        secrets,
        asks,
        warnings,
    })
}

fn table_has(t: &toml::Table, needle: &str) -> bool {
    fn walk(v: &toml::Value, needle: &str) -> bool {
        match v {
            toml::Value::String(s) => s.contains(needle),
            toml::Value::Array(a) => a.iter().any(|x| walk(x, needle)),
            toml::Value::Table(t) => t.values().any(|x| walk(x, needle)),
            _ => false,
        }
    }
    t.values().any(|v| walk(v, needle))
}

/// Replaces every marker in the table's strings with `answer(key)`.
fn fill(t: &toml::Table, answer: &dyn Fn(&str) -> String) -> toml::Table {
    fn walk(v: &toml::Value, answer: &dyn Fn(&str) -> String) -> toml::Value {
        match v {
            toml::Value::String(s) => toml::Value::String(fill_str(s, answer)),
            toml::Value::Array(a) => {
                toml::Value::Array(a.iter().map(|x| walk(x, answer)).collect())
            }
            toml::Value::Table(t) => toml::Value::Table(
                t.iter()
                    .map(|(k, x)| (k.clone(), walk(x, answer)))
                    .collect(),
            ),
            other => other.clone(),
        }
    }
    t.iter()
        .map(|(k, v)| (k.clone(), walk(v, answer)))
        .collect()
}

fn fill_str(s: &str, answer: &dyn Fn(&str) -> String) -> String {
    if !s.contains(MARK) {
        return s.to_owned();
    }
    let mut out = String::new();
    for (i, part) in s.split(MARK).enumerate() {
        if i % 2 == 1 {
            out.push_str(&answer(part));
        } else {
            out.push_str(part);
        }
    }
    out
}

/// What dry-run and the preview show for an asked value.
fn placeholder(key: &str) -> String {
    let what = key.rsplit_once('#').map_or(key, |(_, w)| w);
    format!("<asked: {what}>")
}

/// A valid server name for `name`: lowercase, `[a-z0-9-]`, at most 32, not reserved.
fn sanitize(name: &str) -> String {
    let mut s = String::new();
    for c in name.to_ascii_lowercase().chars() {
        let c = if c.is_ascii_lowercase() || c.is_ascii_digit() {
            c
        } else {
            '-'
        };
        if !(c == '-' && (s.is_empty() || s.ends_with('-'))) {
            s.push(c);
        }
    }
    let mut s = s.trim_end_matches('-').to_owned();
    s.truncate(30);
    let s = s.trim_end_matches('-').to_owned();
    if s.is_empty() {
        return "server".to_owned();
    }
    if mcp_config::check_server_name(&s).is_err() {
        return format!("{s}-mcp");
    }
    s
}

// ------------------------------------------------------------------------------------------------
// The plan
// ------------------------------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "kebab-case")]
enum Action {
    /// New here.
    Add,
    /// Replaces a different server of the same name (`--force`).
    Replace,
    /// The same table is already there.
    Unchanged,
    /// A higher-precedence file of the same tool defines the name.
    Shadowed,
    /// Left out by `--only` or the person.
    NotSelected,
    /// Can't be imported (`reason`).
    Error,
}

#[derive(Debug, Serialize)]
struct SecretRow {
    label: String,
    account: String,
    /// `literal` (moved out of the source) or `asked`.
    from: &'static str,
}

#[derive(Serialize)]
struct PlanRow {
    name: String,
    source_name: String,
    source: Source,
    from: String,
    file: PathBuf,
    action: Action,
    #[serde(skip_serializing_if = "Option::is_none")]
    reason: Option<String>,
    #[serde(rename = "type", skip_serializing_if = "Option::is_none")]
    kind: Option<String>,
    /// The TOML to be written; asked values as `<asked: …>`, secrets not at all.
    #[serde(skip_serializing_if = "Option::is_none")]
    toml: Option<String>,
    secrets: Vec<SecretRow>,
    asks: Vec<Ask>,
    warnings: Vec<String>,
    #[serde(skip)]
    converted: Option<Converted>,
    #[serde(skip_serializing_if = "Option::is_none")]
    result: Option<AddReport>,
}

fn note_for(row: &PlanRow) -> String {
    format!(
        "Imported from {} ({}) by `amp-harness mcp import`.",
        row.source.title(),
        row.from
    )
}

/// Plans every server of `files` for `scope`: names, conversion, clashes with the target file and
/// within the batch. Pure apart from reading the target layer.
fn plan(
    ctx: &Ctx,
    scope: Scope,
    files: &[SourceFile],
    renames: &BTreeMap<String, String>,
    force: bool,
) -> Result<Vec<PlanRow>, CliError> {
    let mut rows: Vec<PlanRow> = Vec::new();
    let mut taken: BTreeSet<String> = BTreeSet::new();
    let mut seen: BTreeMap<(Source, String), String> = BTreeMap::new();
    let origin = mcp_crud::origin(ctx, scope)?;
    let file_cfg = mcp_crud::config_file(ctx, scope)?;
    for f in files {
        for (source_name, v) in &f.servers {
            let mut row = PlanRow {
                name: String::new(),
                source_name: source_name.clone(),
                source: f.source,
                from: f.label.clone(),
                file: f.path.clone(),
                action: Action::Add,
                reason: None,
                kind: None,
                toml: None,
                secrets: Vec::new(),
                asks: Vec::new(),
                warnings: Vec::new(),
                converted: None,
                result: None,
            };
            let base = match renames.get(source_name) {
                Some(n) => n.clone(),
                None => sanitize(source_name),
            };
            row.name = base.clone();
            if let Some(by) = seen.get(&(f.source, source_name.clone())) {
                row.action = Action::Shadowed;
                row.reason = Some(format!("{by} defines `{source_name}` too and wins"));
                rows.push(row);
                continue;
            }
            seen.insert((f.source, source_name.clone()), f.label.clone());
            // The first free name: base, base-2, … (an identical table already there is reused).
            let mut n = 1;
            let outcome = loop {
                let name = if n == 1 {
                    base.clone()
                } else {
                    format!("{base}-{n}")
                };
                n += 1;
                if taken.contains(&name) {
                    continue;
                }
                let converted = match convert(f, source_name, &name, v) {
                    Ok(c) => c,
                    Err(e) => break Err((name, e)),
                };
                match mcp_crud::layer_table(ctx, scope, &name)? {
                    None => break Ok((name, converted, Action::Add)),
                    Some(existing) => {
                        if existing == toml::Value::Table(converted.table.clone()) {
                            break Ok((name, converted, Action::Unchanged));
                        }
                        if force {
                            break Ok((name, converted, Action::Replace));
                        }
                    }
                }
                if n > 99 {
                    break Err((base.clone(), "no free name".to_owned()));
                }
            };
            match outcome {
                Err((name, e)) => {
                    row.name = name;
                    row.action = Action::Error;
                    row.reason = Some(e);
                }
                Ok((name, c, action)) => {
                    row.name = name.clone();
                    taken.insert(name.clone());
                    row.action = action;
                    let shown = fill(&c.table, &|k| placeholder(k));
                    let value = toml::Value::Table(shown);
                    // Checked as the target layer would load it.
                    let checked = mcp_crud::parse_table(&name, &value, origin.clone())
                        .map_err(|e| e.message)
                        .and_then(|(server, _)| {
                            let block = mcp_crud::mcp_block(&name, &value, Some(&note_for(&row)))
                                .map_err(|e| e.message)?;
                            let preview = mcp_crud::preview_replace(
                                &file_cfg,
                                &format!("mcp.{name}"),
                                Some(&block),
                            )?;
                            mcp_crud::layer_check(
                                scope,
                                file_cfg.path.clone(),
                                name.clone(),
                                Expect::Present,
                            )(&preview)?;
                            Ok((server, block))
                        });
                    match checked {
                        Ok((server, block)) => {
                            row.kind = Some(server.transport.kind().as_str().to_owned());
                            row.toml = Some(block);
                        }
                        Err(e) => {
                            row.action = Action::Error;
                            row.reason = Some(e);
                        }
                    }
                    row.secrets = c
                        .secrets
                        .iter()
                        .map(|s| SecretRow {
                            label: s.item.label.clone(),
                            account: s.item.account.clone(),
                            from: s.from,
                        })
                        .collect();
                    row.asks = c.asks.clone();
                    row.warnings = c.warnings.clone();
                    row.converted = Some(c);
                }
            }
            rows.push(row);
        }
    }
    Ok(rows)
}

// ------------------------------------------------------------------------------------------------
// The command
// ------------------------------------------------------------------------------------------------

#[derive(Serialize)]
struct FileRow {
    source: Source,
    label: String,
    file: PathBuf,
    servers: usize,
}

#[derive(Serialize)]
struct ImportReport {
    dry_run: bool,
    scope: &'static str,
    target: PathBuf,
    sources: Vec<FileRow>,
    servers: Vec<PlanRow>,
    imported: usize,
}

fn projects(ctx: &Ctx) -> Vec<PathBuf> {
    let mut out = vec![ctx.cwd.clone()];
    if let Some(r) = mcp_crud::repo_root(ctx)
        && !out.contains(&r)
    {
        out.push(r);
    }
    out
}

fn file_rows(files: &[SourceFile]) -> Vec<FileRow> {
    files
        .iter()
        .map(|f| FileRow {
            source: f.source,
            label: f.label.clone(),
            file: f.path.clone(),
            servers: f.servers.len(),
        })
        .collect()
}

fn found_list(files: &[SourceFile]) -> String {
    files
        .iter()
        .map(|f| {
            format!(
                "  {:<15} {} ({} server{})",
                f.source.word(),
                f.label,
                f.servers.len(),
                if f.servers.len() == 1 { "" } else { "s" }
            )
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// Every source's files here that hold at least one server.
fn detect_all(projects: &[PathBuf]) -> Result<Vec<SourceFile>, CliError> {
    let mut out = Vec::new();
    for s in Source::ALL {
        out.extend(
            find(s, projects)
                .map_err(CliError::new)?
                .into_iter()
                .filter(|f| !f.servers.is_empty()),
        );
    }
    Ok(out)
}

/// The files `--from` names (or the person picks).
fn source_files(
    args: &ImportArgs,
    projects: &[PathBuf],
    ui: &mut dyn Ui,
) -> Result<Vec<SourceFile>, CliError> {
    if let Some(from) = &args.from {
        if let Ok(source) = Source::from_str(from, true) {
            let files = find(source, projects).map_err(CliError::new)?;
            if files.iter().all(|f| f.servers.is_empty()) {
                return Err(CliError::new(format!(
                    "no {} MCP servers found here{}",
                    source.title(),
                    if files.is_empty() {
                        String::new()
                    } else {
                        format!(
                            " (read {})",
                            files
                                .iter()
                                .map(|f| f.label.as_str())
                                .collect::<Vec<_>>()
                                .join(", ")
                        )
                    }
                )));
            }
            return Ok(files);
        }
        return read_named_file(Path::new(from), projects).map_err(CliError::usage);
    }
    let found = detect_all(projects)?;
    if found.is_empty() {
        return Err(CliError::new(
            "found no MCP servers from Claude Code, opencode, VS Code, Cursor or Claude Desktop \
             here; name a file with --from FILE",
        ));
    }
    if args.dry_run {
        return Ok(found);
    }
    let sources: Vec<Source> = found
        .iter()
        .map(|f| f.source)
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect();
    if !ui.interactive() || args.json {
        return Err(CliError::usage(format!(
            "found MCP servers in:\n{}\nchoose one with --from {} (or --dry-run to preview them \
             all)",
            found_list(&found),
            sources
                .iter()
                .map(|s| s.word())
                .collect::<Vec<_>>()
                .join("|")
        )));
    }
    ui.say(&format!("Found MCP servers in:\n{}", found_list(&found)));
    let answer = ui.ask(
        &format!(
            "Import from which? ({})",
            sources
                .iter()
                .map(|s| s.word())
                .collect::<Vec<_>>()
                .join(", ")
        ),
        sources.first().map(|s| s.word()),
    )?;
    let chosen = Source::from_str(&answer, true)
        .map_err(|_| CliError::usage(format!("`{answer}` is not one of the sources found")))?;
    Ok(found.into_iter().filter(|f| f.source == chosen).collect())
}

fn print_plan(rows: &[PlanRow], scope: Scope, target: &Path, dry_run: bool) {
    println!(
        "{} the {} config ({}):",
        if dry_run {
            "Would import into"
        } else {
            "Importing into"
        },
        scope.name(),
        target.display()
    );
    let mut last = String::new();
    for r in rows {
        let header = format!("{} {}", r.source.title(), r.from);
        if header != last {
            println!("\nFrom {header}:");
            last = header;
        }
        let rename = if r.name == r.source_name {
            String::new()
        } else {
            format!(" as `{}`", r.name)
        };
        match r.action {
            Action::Add | Action::Replace => {
                println!(
                    "  `{}`{rename}: {}",
                    r.source_name,
                    if r.action == Action::Add {
                        "new"
                    } else {
                        "replaces the server of that name (--force)"
                    }
                );
                if let Some(t) = &r.toml {
                    for l in t.lines() {
                        println!("    {l}");
                    }
                }
                for s in &r.secrets {
                    println!(
                        "    keychain: {} -> {} ({})",
                        s.label,
                        s.account,
                        if s.from == "literal" {
                            "moved out of the source file"
                        } else {
                            "asked at import"
                        }
                    );
                }
                for a in &r.asks {
                    println!(
                        "    asks: {}{}",
                        a.prompt,
                        if a.hidden { " (hidden)" } else { "" }
                    );
                }
            }
            Action::Unchanged => println!(
                "  `{}`{rename}: already there, unchanged (skipped)",
                r.source_name
            ),
            Action::Shadowed | Action::NotSelected | Action::Error => println!(
                "  `{}`{rename}: {}{}",
                r.source_name,
                match r.action {
                    Action::Error => "can't import: ",
                    Action::Shadowed => "skipped: ",
                    _ => "not selected",
                },
                r.reason.as_deref().unwrap_or_default()
            ),
        }
        for w in &r.warnings {
            println!("    warning: {w}");
        }
    }
    println!();
}

fn importable(r: &PlanRow) -> bool {
    matches!(r.action, Action::Add | Action::Replace)
}

/// Asks (or reads from stdin) every value the chosen servers need, once each.
fn answers(
    asks: &[Ask],
    from_stdin: bool,
    ui: &mut dyn Ui,
) -> Result<BTreeMap<String, String>, CliError> {
    let mut out = BTreeMap::new();
    if asks.is_empty() {
        return Ok(out);
    }
    if from_stdin {
        let mut text = String::new();
        std::io::stdin()
            .read_to_string(&mut text)
            .map_err(|e| CliError::new(format!("read values from stdin: {e}")))?;
        let lines: Vec<&str> = text
            .lines()
            .map(str::trim)
            .filter(|l| !l.is_empty())
            .collect();
        if lines.len() < asks.len() {
            return Err(CliError::usage(format!(
                "--secret-stdin: expected {} line(s) on stdin ({}), got {}; nothing was written",
                asks.len(),
                asks.iter()
                    .map(|a| a.prompt.as_str())
                    .collect::<Vec<_>>()
                    .join("; "),
                lines.len()
            )));
        }
        for (a, l) in asks.iter().zip(lines) {
            out.insert(a.key.clone(), l.to_owned());
        }
        return Ok(out);
    }
    if !ui.interactive() {
        return Err(CliError::usage(format!(
            "the import needs {} value(s) typed in ({}); run it in a terminal, or pass \
             --secret-stdin and pipe them in, one per line; nothing was written",
            asks.len(),
            asks.iter()
                .map(|a| a.prompt.as_str())
                .collect::<Vec<_>>()
                .join("; ")
        )));
    }
    for a in asks {
        let v = if a.hidden {
            ui.secret(&format!("{} (hidden; kept in the OS keychain)", a.prompt))?
        } else {
            ui.ask(&a.prompt, None)?
        };
        let v = v.trim().to_owned();
        if v.is_empty() {
            return Err(CliError::usage(format!(
                "no value for {}; nothing was written",
                a.prompt
            )));
        }
        out.insert(a.key.clone(), v);
    }
    Ok(out)
}

pub async fn import(args: ImportArgs) -> Result<(), CliError> {
    let ctx = mcp_crud::ctx()?;
    let scope = args.scope.get().unwrap_or(Scope::User);
    let target = mcp_crud::config_file(&ctx, scope)?.path;
    let mut renames = BTreeMap::new();
    for r in &args.rename {
        let (old, new) = r
            .split_once('=')
            .ok_or_else(|| CliError::usage("--rename must look like OLD=NEW"))?;
        mcp_config::check_server_name(new.trim()).map_err(|(m, _)| CliError::usage(m))?;
        renames.insert(old.trim().to_owned(), new.trim().to_owned());
    }
    let mut ui = ui::detect(!args.secret_stdin);
    let projects = projects(&ctx);
    let files = source_files(&args, &projects, ui.as_mut())?;
    let mut rows = plan(&ctx, scope, &files, &renames, args.force)?;

    if !args.only.is_empty() {
        for o in &args.only {
            if !rows.iter().any(|r| &r.source_name == o || &r.name == o) {
                return Err(CliError::usage(format!(
                    "--only: no server `{o}` in {}",
                    files
                        .iter()
                        .map(|f| f.label.as_str())
                        .collect::<Vec<_>>()
                        .join(", ")
                )));
            }
        }
        for r in &mut rows {
            if !args.only.contains(&r.source_name) && !args.only.contains(&r.name) {
                r.action = Action::NotSelected;
                r.reason = None;
            }
        }
    }
    let report = |rows: Vec<PlanRow>, imported: usize, dry_run: bool| ImportReport {
        dry_run,
        scope: scope.name(),
        target: target.clone(),
        sources: file_rows(&files),
        servers: rows,
        imported,
    };

    if args.dry_run {
        if args.json {
            return print_json(&report(rows, 0, true));
        }
        print_plan(&rows, scope, &target, true);
        println!("Dry run: nothing was written.");
        return Ok(());
    }

    let errors: Vec<&PlanRow> = rows.iter().filter(|r| r.action == Action::Error).collect();
    if !errors.is_empty() {
        let ok: Vec<&str> = rows
            .iter()
            .filter(|r| importable(r))
            .map(|r| r.source_name.as_str())
            .collect();
        return Err(CliError::usage(format!(
            "nothing was written; these servers can't be imported:\n{}\n{}",
            errors
                .iter()
                .map(|r| format!(
                    "  `{}` ({}): {}",
                    r.source_name,
                    r.from,
                    r.reason.as_deref().unwrap_or_default()
                ))
                .collect::<Vec<_>>()
                .join("\n"),
            if ok.is_empty() {
                "Fix them in the source file, or add them with `amp-harness mcp add`.".to_owned()
            } else {
                format!("Leave them out with --only {}", ok.join(","))
            }
        )));
    }
    if !rows.iter().any(importable) {
        if args.json {
            return print_json(&report(rows, 0, false));
        }
        print_plan(&rows, scope, &target, false);
        println!("Nothing to import.");
        return Ok(());
    }
    if !args.json {
        print_plan(&rows, scope, &target, false);
    }
    if !args.yes {
        if !ui.interactive() {
            return Err(CliError::usage(
                "nothing was written: pass --yes to import without a question (the plan is \
                 above; --dry-run only shows it)",
            ));
        }
        let n = rows.iter().filter(|r| importable(r)).count();
        if !ui.confirm(
            &format!(
                "Import {n} server{} into {}?",
                if n == 1 { "" } else { "s" },
                target.display()
            ),
            false,
        )? {
            let picked = ui.ask(
                "Which ones, then? (names separated by commas; Enter for none)",
                Some(""),
            )?;
            let picked: BTreeSet<String> = picked
                .split(',')
                .map(|s| s.trim().to_owned())
                .filter(|s| !s.is_empty())
                .collect();
            for r in &mut rows {
                if importable(r) && !picked.contains(&r.source_name) && !picked.contains(&r.name) {
                    r.action = Action::NotSelected;
                }
            }
            if !rows.iter().any(importable) {
                println!("Nothing imported.");
                return Ok(());
            }
        }
    }

    // The keychain must be there before anything is asked or written.
    let needs_keychain: Vec<&str> = rows
        .iter()
        .filter(|r| importable(r) && !r.secrets.is_empty())
        .map(|r| r.name.as_str())
        .collect();
    if let Some(first) = needs_keychain.first() {
        mcp_crud::keychain_service(first).map_err(|e| {
            CliError::new(format!(
                "nothing was written: {} (servers with secrets: {})",
                e.message,
                needs_keychain.join(", ")
            ))
        })?;
    }
    let mut asks: Vec<Ask> = Vec::new();
    for r in rows.iter().filter(|r| importable(r)) {
        for a in &r.asks {
            if !asks.iter().any(|x| x.key == a.key) {
                asks.push(a.clone());
            }
        }
    }
    let answers = answers(&asks, args.secret_stdin, ui.as_mut())?;
    let answer = |k: &str| answers.get(k).cloned().unwrap_or_default();

    let mut imported = 0;
    for r in rows.iter_mut().filter(|r| importable(r)) {
        let c = r.converted.as_ref().expect("planned");
        let spec = NewServer {
            table: fill(&c.table, &answer),
            secrets: c
                .secrets
                .iter()
                .map(|s| (s.item.clone(), Some(fill_str(&s.value, &answer))))
                .collect(),
        };
        let note: &'static str = note_for(r).leak();
        let done = mcp_crud::add_server(
            &r.name,
            scope,
            spec,
            AddOpts {
                force: r.action == Action::Replace,
                no_test: !args.test,
                secret_stdin: false,
                json: args.json,
                note,
                quiet: true,
            },
        )
        .await
        .map_err(|e| {
            CliError::new(format!(
                "`{}`: {}{}",
                r.name,
                e.message,
                if imported > 0 {
                    format!(" ({imported} server(s) before it were imported)")
                } else {
                    String::new()
                }
            ))
        })?;
        imported += 1;
        r.result = Some(done);
    }
    if args.json {
        return print_json(&report(rows, imported, false));
    }
    println!(
        "Imported {imported} MCP server{} into the {} config ({}).",
        if imported == 1 { "" } else { "s" },
        scope.name(),
        target.display()
    );
    for r in rows.iter().filter(|r| r.result.is_some()) {
        let res = r.result.as_ref().expect("filtered");
        println!(
            "  {} {} `{}`{}",
            if res.replaced { "Replaced" } else { "Added" },
            res.kind,
            r.name,
            if res.trusted {
                " (trusted for you as configured now)"
            } else {
                ""
            }
        );
        for (label, s) in res.secrets_stored.iter().zip(&r.secrets) {
            println!(
                "    Stored {label} in the OS keychain (account {}).",
                s.account
            );
        }
        if let Some(t) = &res.test {
            mcp_crud::print_test(t, "    Test: ");
        }
    }
    if let Some(b) = rows
        .iter()
        .filter_map(|r| r.result.as_ref().and_then(|x| x.backup.clone()))
        .next()
    {
        println!("  Backup of the file before the import: {}", b.display());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn src(source: Source, inputs: &[(&str, &str, bool)]) -> SourceFile {
        SourceFile {
            source,
            path: PathBuf::from("/x/mcp.json"),
            label: "test".into(),
            servers: Vec::new(),
            inputs: inputs
                .iter()
                .map(|(id, kind, pw)| {
                    (
                        (*id).to_owned(),
                        Input {
                            kind: (*kind).to_owned(),
                            description: format!("the {id}"),
                            password: *pw,
                        },
                    )
                })
                .collect(),
            disabled: BTreeSet::new(),
        }
    }

    #[test]
    fn jsonc_comments_and_trailing_commas() {
        let text = "\u{feff}{\n // c\n \"a\": \"//not\", /* b\n c */ \"u\": \"x/*y*/\",\n \"l\": [1,2,],\n}";
        let v: Value = serde_json::from_str(&strip_jsonc(text)).unwrap();
        assert_eq!(v, json!({"a": "//not", "u": "x/*y*/", "l": [1, 2]}));
        assert_eq!(strip_jsonc("{\"s\": \"a\\\",}\"}"), "{\"s\": \"a\\\",}\"}");
    }

    #[test]
    fn names() {
        assert_eq!(sanitize("GitHub"), "github");
        assert_eq!(sanitize("my_server v2"), "my-server-v2");
        assert_eq!(sanitize("__x__"), "x");
        assert_eq!(sanitize("amp"), "amp-mcp");
        assert_eq!(sanitize("!!!"), "server");
        assert!(sanitize(&"a".repeat(60)).len() <= 32);
    }

    #[test]
    fn variables_per_source() {
        let mut asks = Vec::new();
        let oc = src(Source::Opencode, &[]);
        assert_eq!(
            norm(&oc, "Bearer {env:TOKEN} ${HOME}", &mut asks).unwrap(),
            "Bearer ${env:TOKEN:-} $${HOME}"
        );
        let n = norm(&oc, "{file:~/.secrets/k}", &mut asks).unwrap();
        assert!(n.contains(MARK) && asks.len() == 1 && asks[0].hidden);
        assert!(norm(&oc, "${env:X}", &mut asks).is_ok());
        assert!(norm(&oc, "${file:x}", &mut asks).is_ok());

        let vs = src(
            Source::Vscode,
            &[("tok", "promptString", true), ("pick", "pickString", false)],
        );
        let mut asks = Vec::new();
        assert_eq!(
            norm(
                &vs,
                "${workspaceFolder}/a:${userHome}${/}b ${env:X} ${Y:-d}",
                &mut asks
            )
            .unwrap(),
            format!(
                "${{repo}}/a:${{home}}{0}b ${{env:X:-}} ${{Y:-d}}",
                std::path::MAIN_SEPARATOR
            )
        );
        let n = norm(&vs, "Bearer ${input:tok}", &mut asks).unwrap();
        assert_eq!(fill_str(&n, &|_| "v".into()), "Bearer v");
        assert!(asks[0].hidden);
        assert!(
            norm(&vs, "${input:pick}", &mut asks)
                .unwrap_err()
                .contains("pickString")
        );
        assert!(norm(&vs, "${input:nope}", &mut asks).is_err());
        assert!(norm(&vs, "${workspaceFolderBasename}", &mut asks).is_err());

        let cu = src(Source::Cursor, &[]);
        assert!(norm(&cu, "${input:x}", &mut asks).is_err());
        assert_eq!(norm(&cu, "${userHome}/x", &mut asks).unwrap(), "${home}/x");

        let cd = src(Source::ClaudeDesktop, &[]);
        assert_eq!(norm(&cd, "a${B}", &mut asks).unwrap(), "a$${B}");
    }

    fn conv(source: Source, inputs: &[(&str, &str, bool)], v: Value) -> Result<Converted, String> {
        convert(&src(source, inputs), "s", "s", &v)
    }

    #[test]
    fn secrets_are_planned_for_the_keychain() {
        // Claude Code: a literal bearer and a literal key move; references stay.
        let c = conn_ok(conv(
            Source::Claude,
            &[],
            json!({"type": "http", "url": "${BASE:-https://x.test}/mcp",
                   "headers": {"Authorization": "Bearer sk-live-123456", "X-Ref": "${REF}"}}),
        ));
        assert_eq!(
            c.table["url"].as_str(),
            Some("${env:BASE:-https://x.test}/mcp")
        );
        assert_eq!(
            c.table["secret_headers"],
            toml::Value::Array(vec!["Authorization".into()])
        );
        assert_eq!(c.table["headers"]["X-Ref"].as_str(), Some("${env:REF}"));
        assert_eq!(c.secrets[0].item.account, "mcp:s:header:authorization");
        assert_eq!(c.secrets[0].value, "Bearer sk-live-123456");
        assert_eq!(c.secrets[0].from, "literal");

        // VS Code: an input in an env value is asked and stored whole.
        let c = conn_ok(conv(
            Source::Vscode,
            &[("key", "promptString", true)],
            json!({"type": "stdio", "command": "npx", "args": ["-y", "pkg"],
                   "env": {"API_KEY": "${input:key}", "MODE": "x"}, "dev": {"watch": "a"}}),
        ));
        assert_eq!(
            c.table["secret_env"],
            toml::Value::Array(vec!["API_KEY".into()])
        );
        assert_eq!(c.secrets[0].item.account, "mcp:s:env:API_KEY");
        assert_eq!(c.secrets[0].from, "asked");
        assert_eq!(c.asks.len(), 1);
        assert!(c.warnings.iter().any(|w| w.contains("`dev`")));
        assert!(!format!("{:?}", c.table).contains("input"));

        // A password input in args can't be kept.
        assert!(
            conv(
                Source::Vscode,
                &[("key", "promptString", true)],
                json!({"command": "x", "args": ["--key", "${input:key}"]}),
            )
            .unwrap_err()
            .contains("asked hidden")
        );

        // Cursor auth: the client secret goes to the keychain.
        let c = conn_ok(conv(
            Source::Cursor,
            &[],
            json!({"url": "https://c.test/mcp",
                   "auth": {"CLIENT_ID": "id1", "CLIENT_SECRET": "shh-1", "scopes": ["a", "b"]}}),
        ));
        assert_eq!(c.table["oauth"]["client_id"].as_str(), Some("id1"));
        assert_eq!(c.table["oauth"]["client_secret"].as_bool(), Some(true));
        assert_eq!(c.secrets[0].item.account, "mcp:s:oauth-client-secret");
        assert_eq!(c.secrets[0].value, "shh-1");

        // opencode: local command list, {env:}, timeout is a startup timeout, oauth false.
        let c = conn_ok(conv(
            Source::Opencode,
            &[],
            json!({"type": "local", "command": ["bunx", "pkg", "{env:DIR}"],
                   "environment": {"TOKEN": "tok-literal-value"}, "timeout": 2500, "enabled": false}),
        ));
        assert_eq!(c.table["command"].as_str(), Some("bunx"));
        assert_eq!(
            c.table["args"],
            toml::Value::Array(vec!["pkg".into(), "${env:DIR:-}".into()])
        );
        assert_eq!(c.table["startup_timeout_secs"].as_integer(), Some(3));
        assert!(!c.table.contains_key("call_timeout_secs"));
        assert_eq!(c.table["enabled"].as_bool(), Some(false));
        assert_eq!(
            c.table["secret_env"],
            toml::Value::Array(vec!["TOKEN".into()])
        );
        let c = conn_ok(conv(
            Source::Opencode,
            &[],
            json!({"type": "remote", "url": "https://o.test", "oauth": false}),
        ));
        assert_eq!(c.table["auth"].as_str(), Some("none"));

        assert!(
            conv(Source::Claude, &[], json!({"type": "ws", "url": "ws://x"}))
                .unwrap_err()
                .contains("`ws`")
        );
    }

    fn conn_ok(c: Result<Converted, String>) -> Converted {
        c.unwrap_or_else(|e| panic!("{e}"))
    }

    #[test]
    fn claude_json_local_then_user() {
        let root = json!({
            "mcpServers": {"u": {"command": "a"}},
            "projects": {"/p": {"mcpServers": {"l": {"command": "b"}}, "disabledMcpServers": ["l"]}}
        });
        let files = claude_json_files(Path::new("/h/.claude.json"), &root, &[PathBuf::from("/p")]);
        assert_eq!(files.len(), 2);
        assert_eq!(files[0].servers[0].0, "l");
        assert!(files[0].disabled.contains("l"));
        assert_eq!(files[1].servers[0].0, "u");
    }
}
