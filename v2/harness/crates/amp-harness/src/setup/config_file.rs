//! Small, comment-preserving edits to `config.toml` (first run and `init`).
//!
//! The file is the user's: it may hold comments, `[models]`, `[roles.*]` and other tables. Setup
//! only ever sets a few plain string keys (`[server] url`, `web_url`, `ca_file`, `[models]
//! profile`), so it edits lines instead of re-serialising the whole document, then checks that the
//! result still parses and reads back the value it wrote. Secrets are never written here (D3).
//!
//! `amp-harness connect` (#2163) also adds and removes whole `[providers.<id>]` tables
//! ([`ConfigFile::replace_tables`]): the tables of that one provider are cut out line by line, the
//! new block is appended, the result must pass the caller's check, and the previous file is kept as
//! `config.toml.bak` first.
//!
//! Writes are safe against other writers (#2309): the chat's panels and any number of CLI
//! commands may change the same file at once. Every read-modify-write holds an advisory lock on
//! `<file>.lock` next to the real file, re-reads the file under it, and writes through a uniquely
//! named temporary file in the same directory. A symlinked config.toml (stow, chezmoi,
//! home-manager) stays a symlink: the file it points to is written instead.

use std::{
    fs,
    io::Write,
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

/// How long a writer waits for another one to finish before giving up.
const LOCK_WAIT: Duration = Duration::from_secs(10);

/// The header of a config.toml that setup creates.
const NEW_FILE_HEADER: &str = "# amp-harness settings, written by the first run, `amp-harness init` and `amp-harness server`. Edit freely.\n\
# Provider keys are never stored here: they live in the OS keychain or the environment.\n";

/// The header of a repo `.amp/harness/config.toml` that `amp-harness models --repo` creates.
pub const REPO_FILE_HEADER: &str = "# amp-harness settings for everyone working in this repo (`amp-harness models --repo`).\n\
# Only [mcp.*], [roles.*] and [models] profile are allowed here; keys and servers stay in your user config.\n";

/// A TOML key, bare when it can be (`worker-backend`), else quoted (`"openai/gpt-oss-120b"`).
pub fn toml_key(key: &str) -> String {
    if !key.is_empty()
        && key
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
    {
        key.to_owned()
    } else {
        toml_string(key)
    }
}

/// A TOML basic string for `value` (JSON string escapes are valid TOML basic-string escapes).
pub fn toml_string(value: &str) -> String {
    serde_json::to_string(value).expect("a string always encodes")
}

fn is_header(line: &str) -> bool {
    line.trim_start().starts_with('[')
}

fn header_name(line: &str) -> Option<&str> {
    let t = line.trim();
    let t = t.split('#').next().unwrap_or(t).trim();
    t.strip_prefix('[')
        .and_then(|t| t.strip_suffix(']'))
        .filter(|t| !t.starts_with('['))
        .map(str::trim)
}

fn is_key_line(line: &str, key: &str) -> bool {
    let t = line.trim_start();
    t.strip_prefix(key)
        .is_some_and(|rest| rest.trim_start().starts_with('='))
}

/// `text` with `[section] key = "value"` set: an existing key line is replaced, otherwise the key
/// is added at the end of the section, which is appended when missing.
pub fn set_string(text: &str, section: &str, key: &str, value: &str) -> String {
    set_raw(text, section, key, &toml_string(value))
}

/// [`set_string`] for any value already written as TOML (`false`, `["a", "b"]`, `30`).
pub fn set_raw(text: &str, section: &str, key: &str, raw: &str) -> String {
    let new_line = format!("{key} = {raw}");
    let mut lines: Vec<String> = text.lines().map(str::to_owned).collect();
    let start = lines.iter().position(|l| header_name(l) == Some(section));
    let Some(start) = start else {
        let mut out = text.trim_end().to_owned();
        if !out.is_empty() {
            out.push_str("\n\n");
        }
        out.push_str(&format!("[{section}]\n{new_line}\n"));
        return out;
    };
    let end = lines[start + 1..]
        .iter()
        .position(|l| is_header(l))
        .map(|i| start + 1 + i)
        .unwrap_or(lines.len());
    if let Some(i) = (start + 1..end).find(|&i| is_key_line(&lines[i], key)) {
        lines[i] = new_line;
    } else {
        // After the section's last non-blank line, so a blank separator stays below it.
        let last = (start..end)
            .rev()
            .find(|&i| !lines[i].trim().is_empty())
            .unwrap_or(start);
        lines.insert(last + 1, new_line);
    }
    let mut out = lines.join("\n");
    out.push('\n');
    out
}

/// A table header's dotted name with spaces and quotes dropped (`[providers."x" . models."m"]` ->
/// `providers.x.models.m`); `None` for other lines and `[[arrays]]`.
fn table_name(line: &str) -> Option<String> {
    header_name(line).map(|n| {
        n.chars()
            .filter(|c| *c != '"' && !c.is_whitespace())
            .collect()
    })
}

/// `text` without the table `prefix` and its sub-tables (`[prefix]`, `[prefix.*]`). Comment and
/// blank lines just above the next remaining table stay with it. Returns whether anything was
/// cut.
pub fn remove_tables(text: &str, prefix: &str) -> (String, bool) {
    let lines: Vec<&str> = text.lines().collect();
    let inside = |line: &str| {
        table_name(line).is_some_and(|n| n == prefix || n.starts_with(&format!("{prefix}.")))
    };
    let mut keep = vec![true; lines.len()];
    let mut i = 0;
    let mut cut = false;
    while i < lines.len() {
        if !inside(lines[i]) {
            i += 1;
            continue;
        }
        cut = true;
        let start = i;
        let mut end = i + 1;
        while end < lines.len() && !is_header(lines[end]) {
            end += 1;
        }
        // Comments right above the next table belong to it.
        let mut stop = end;
        if end < lines.len() && !inside(lines[end]) {
            while stop > start + 1 {
                let t = lines[stop - 1].trim();
                if t.starts_with('#') {
                    stop -= 1;
                } else {
                    break;
                }
            }
        }
        for k in keep.iter_mut().take(stop).skip(start) {
            *k = false;
        }
        i = end;
    }
    let mut out: Vec<&str> = Vec::with_capacity(lines.len());
    for (line, k) in lines.iter().zip(&keep) {
        // No runs of blank lines where a table was cut out.
        if *k && !(line.trim().is_empty() && out.last().is_some_and(|l| l.trim().is_empty())) {
            out.push(line);
        }
    }
    while out.last().is_some_and(|l| l.trim().is_empty()) {
        out.pop();
    }
    let mut text = out.join("\n");
    if !text.is_empty() {
        text.push('\n');
    }
    (text, cut)
}

/// The text of the table `prefix` and its sub-tables (`[prefix]`, `[prefix.*]`), as written
/// (comments inside it included), when it is written with its own headers; `None` when there is
/// no such header (absent, or only defined inline under a parent table).
pub fn table_text(text: &str, prefix: &str) -> Option<String> {
    // The same lines `remove_tables` cuts, comments and blank lines inside the table included.
    let lines: Vec<&str> = text.lines().collect();
    let inside = |line: &str| {
        table_name(line).is_some_and(|n| n == prefix || n.starts_with(&format!("{prefix}.")))
    };
    let mut out: Vec<&str> = Vec::new();
    let mut i = 0;
    while i < lines.len() {
        if !inside(lines[i]) {
            i += 1;
            continue;
        }
        let mut end = i + 1;
        while end < lines.len() && !is_header(lines[end]) {
            end += 1;
        }
        // Comments right above the next table belong to it.
        let mut stop = end;
        if end < lines.len() && !inside(lines[end]) {
            while stop > i + 1 && lines[stop - 1].trim().starts_with('#') {
                stop -= 1;
            }
        }
        out.extend_from_slice(&lines[i..stop]);
        i = end;
    }
    if out.is_empty() {
        return None;
    }
    let mut block = out.join("\n").trim_end().to_owned();
    block.push('\n');
    Some(block)
}

/// Reads `[section] key` as a string, if the text parses and has it. A dotted section
/// (`servers.local`) is a nested table.
pub fn get_string(text: &str, section: &str, key: &str) -> Option<String> {
    let doc: toml::Table = toml::from_str(text).ok()?;
    table_at(&doc, section)?
        .get(key)?
        .as_str()
        .map(str::to_owned)
}

/// The table at a dotted `section` path.
pub fn table_at<'a>(doc: &'a toml::Table, section: &str) -> Option<&'a toml::Table> {
    let mut table = doc;
    for part in section.split('.') {
        table = table.get(part)?.as_table()?;
    }
    Some(table)
}

fn section_range(lines: &[String], section: &str) -> Option<(usize, usize)> {
    let start = lines.iter().position(|l| header_name(l) == Some(section))?;
    let end = lines[start + 1..]
        .iter()
        .position(|l| is_header(l))
        .map(|i| start + 1 + i)
        .unwrap_or(lines.len());
    Some((start, end))
}

fn join_lines(lines: &[String]) -> String {
    let mut out = lines.join("\n");
    while out.ends_with("\n\n") {
        out.pop();
    }
    if !out.is_empty() && !out.ends_with('\n') {
        out.push('\n');
    }
    out
}

/// `text` without `[section] key` (comments and other keys stay).
pub fn remove_key(text: &str, section: &str, key: &str) -> String {
    let mut lines: Vec<String> = text.lines().map(str::to_owned).collect();
    let Some((start, end)) = section_range(&lines, section) else {
        return text.to_owned();
    };
    if let Some(i) = (start + 1..end).find(|&i| is_key_line(&lines[i], key)) {
        lines.remove(i);
    }
    join_lines(&lines)
}

/// `text` without the `[section]` table: its header and every line up to the next header.
pub fn remove_section(text: &str, section: &str) -> String {
    let mut lines: Vec<String> = text.lines().map(str::to_owned).collect();
    let Some((start, end)) = section_range(&lines, section) else {
        return text.to_owned();
    };
    lines.drain(start..end);
    join_lines(&lines)
}

/// One change to config.toml, for [`ConfigFile::edit`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Change {
    Set {
        section: String,
        key: String,
        value: String,
    },
    Remove {
        section: String,
        key: String,
    },
    /// `[section] key = <raw>`, where `raw` is a TOML value as written (`false`, `["a"]`), made
    /// by [`Change::set_value`].
    SetRaw {
        section: String,
        key: String,
        raw: String,
    },
    DropSection(String),
    /// Drops the table `prefix` and all its sub-tables (`[prefix]`, `[prefix.*]`, quoted or not).
    DropTables(String),
    /// Adds an empty table at this key path (`["providers", "openrouter", "models", "a/b"]`)
    /// unless its header is already there.
    AddTable(Vec<String>),
}

impl Change {
    pub fn set(section: &str, key: &str, value: impl Into<String>) -> Self {
        Self::Set {
            section: section.to_owned(),
            key: key.to_owned(),
            value: value.into(),
        }
    }

    /// Sets a non-string value (a bool, a number, an array).
    pub fn set_value(section: &str, key: &str, value: &toml::Value) -> Self {
        Self::SetRaw {
            section: section.to_owned(),
            key: key.to_owned(),
            raw: value.to_string(),
        }
    }

    pub fn remove(section: &str, key: &str) -> Self {
        Self::Remove {
            section: section.to_owned(),
            key: key.to_owned(),
        }
    }

    fn apply_text(&self, text: &str) -> String {
        match self {
            Self::Set {
                section,
                key,
                value,
            } => set_string(text, section, key, value),
            Self::SetRaw { section, key, raw } => set_raw(text, section, key, raw),
            Self::Remove { section, key } => remove_key(text, section, key),
            Self::DropSection(section) => remove_section(text, section),
            Self::DropTables(prefix) => remove_tables(text, prefix).0,
            Self::AddTable(parts) => {
                let name = parts.join(".");
                if text
                    .lines()
                    .any(|l| table_name(l).as_deref() == Some(&name))
                {
                    return text.to_owned();
                }
                let header: Vec<String> = parts.iter().map(|p| toml_key(p)).collect();
                let mut out = text.trim_end().to_owned();
                if !out.is_empty() {
                    out.push_str("\n\n");
                }
                out.push_str(&format!("[{}]\n", header.join(".")));
                out
            }
        }
    }

    /// The same change on the parsed document: what the edited text must parse to.
    fn apply_doc(&self, doc: &mut toml::Table) {
        fn table_mut<'a>(doc: &'a mut toml::Table, section: &str) -> &'a mut toml::Table {
            let mut table = doc;
            for part in section.split('.') {
                table = table
                    .entry(part.to_owned())
                    .or_insert_with(|| toml::Value::Table(toml::Table::new()))
                    .as_table_mut()
                    .expect("checked: a table");
            }
            table
        }
        match self {
            Self::Set {
                section,
                key,
                value,
            } => {
                table_mut(doc, section).insert(key.clone(), toml::Value::String(value.clone()));
            }
            Self::SetRaw { section, key, raw } => {
                // Made from a Value by `set_value`, so it parses back.
                if let Ok(mut t) = toml::from_str::<toml::Table>(&format!("v = {raw}"))
                    && let Some(v) = t.remove("v")
                {
                    table_mut(doc, section).insert(key.clone(), v);
                }
            }
            Self::Remove { section, key } => {
                if table_at(doc, section).is_some() {
                    table_mut(doc, section).remove(key);
                }
            }
            Self::DropTables(prefix) => {
                let parts: Vec<&str> = prefix.split('.').collect();
                let (last, parents) = parts.split_last().expect("split yields one part");
                let mut table = Some(doc);
                for part in parents {
                    table = table
                        .and_then(|t| t.get_mut(*part))
                        .and_then(|v| v.as_table_mut());
                }
                if let Some(t) = table {
                    t.remove(*last);
                }
            }
            Self::AddTable(parts) => {
                let mut table = doc;
                for part in parts {
                    table = table
                        .entry(part.clone())
                        .or_insert_with(|| toml::Value::Table(toml::Table::new()))
                        .as_table_mut()
                        .expect("checked: a table");
                }
            }
            Self::DropSection(section) => match section.rsplit_once('.') {
                Some((parent, name)) => {
                    if table_at(doc, parent).is_some() {
                        table_mut(doc, parent).remove(name);
                    }
                }
                None => {
                    doc.remove(section);
                }
            },
        }
    }
}

/// Tables that only hold other tables (`[servers]` once its last server is dropped) carry no
/// settings; leave them out when comparing.
fn prune_empty(doc: &mut toml::Table) {
    for (_, value) in doc.iter_mut() {
        if let toml::Value::Table(t) = value {
            prune_empty(t);
        }
    }
    doc.retain(|_, v| !matches!(v, toml::Value::Table(t) if t.is_empty()));
}

/// The config file setup edits.
#[derive(Debug, Clone)]
pub struct ConfigFile {
    pub path: PathBuf,
    /// The comment a new file starts with.
    header: &'static str,
}

impl ConfigFile {
    pub fn new(path: PathBuf) -> Self {
        Self {
            path,
            header: NEW_FILE_HEADER,
        }
    }

    /// A repo's `.amp/harness/config.toml` (#2235): a new file gets [`REPO_FILE_HEADER`].
    pub fn repo(path: PathBuf) -> Self {
        Self {
            path,
            header: REPO_FILE_HEADER,
        }
    }

    pub fn exists(&self) -> bool {
        self.path.is_file()
    }

    fn read(&self) -> Result<String, String> {
        match fs::read_to_string(&self.path) {
            Ok(text) => Ok(text),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(self.header.to_owned()),
            Err(e) => Err(format!("read {}: {e}", self.path.display())),
        }
    }

    /// Replaces the tables `prefix` / `prefix.*` with `block` (or just removes them when `block` is
    /// `None`). The result must pass `check` (it gets the new text), or nothing is written. An
    /// existing file is first copied to `config.toml.bak`, whose path is returned.
    pub fn replace_tables(
        &self,
        prefix: &str,
        block: Option<&str>,
        check: &dyn Fn(&str) -> Result<(), String>,
    ) -> Result<Option<PathBuf>, String> {
        let _lock = lock_config(&self.path)?;
        let before = self.read()?;
        let (mut text, _) = remove_tables(&before, prefix);
        if let Some(block) = block {
            let trimmed = text.trim_end().to_owned();
            text = if trimmed.is_empty() {
                String::new()
            } else {
                format!("{trimmed}\n\n")
            };
            text.push_str(block.trim_end());
            text.push('\n');
        }
        check(&text).map_err(|e| {
            format!(
                "{}: the change would not load ({e}); nothing was written, edit the file by hand",
                self.path.display()
            )
        })?;
        let backup = if self.exists() {
            let bak = self.path.with_extension("toml.bak");
            write_private(&bak, &before)?;
            Some(bak)
        } else {
            None
        };
        write_private(&self.path, &text)?;
        Ok(backup)
    }

    pub fn get(&self, section: &str, key: &str) -> Option<String> {
        get_string(&self.read().ok()?, section, key)
    }

    /// Reads the file's text (`Ok(None)` when it does not exist yet).
    pub fn text(&self) -> Result<Option<String>, String> {
        match fs::read_to_string(&self.path) {
            Ok(text) => Ok(Some(text)),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(e) => Err(format!("read {}: {e}", self.path.display())),
        }
    }

    /// Applies `changes` line by line and writes the file atomically (0600), after copying the
    /// old file to `config.toml.bak` (0600). Nothing is written unless the result parses to
    /// exactly the old document with `changes` applied, so the person's other settings can never
    /// be lost. Returns the backup's path, when there was a file to back up.
    pub fn edit(&self, changes: &[Change]) -> Result<Option<PathBuf>, String> {
        if changes.is_empty() {
            return Ok(None);
        }
        // Read, check and write under the lock, so a concurrent writer's change is either already
        // in the text the changes apply to, or waits for this one.
        let _lock = lock_config(&self.path)?;
        let existing = self.text()?;
        let text = self.preview(changes)?;
        if existing.as_deref() == Some(text.as_str()) {
            return Ok(None);
        }
        let backup = match &existing {
            Some(old) => {
                let backup = self.path.with_extension("toml.bak");
                write_private(&backup, old)?;
                Some(backup)
            }
            None => None,
        };
        write_private(&self.path, &text)?;
        Ok(backup)
    }

    /// The text [`ConfigFile::edit`] would write, without writing it (so a caller can check that
    /// the result loads first). Errors exactly when `edit` would refuse.
    pub fn preview(&self, changes: &[Change]) -> Result<String, String> {
        let existing = self.text()?;
        let before = existing.unwrap_or_else(|| self.header.to_owned());
        let mut expected: toml::Table = toml::from_str(&before).map_err(|e| {
            format!(
                "{} does not parse ({e}); fix it first, nothing was changed",
                self.path.display()
            )
        })?;
        let mut text = before.clone();
        for change in changes {
            if let Change::Set { section, .. }
            | Change::SetRaw { section, .. }
            | Change::Remove { section, .. } = change
                && let Some(path) = non_table_on(&expected, section)
            {
                return Err(format!(
                    "{}: `{path}` is not a table; fix it first, nothing was changed",
                    self.path.display()
                ));
            }
            text = change.apply_text(&text);
            change.apply_doc(&mut expected);
        }
        let mut got: toml::Table = toml::from_str(&text).map_err(|e| {
            format!(
                "{}: the change would break the file ({e}); nothing was changed, edit it by hand",
                self.path.display()
            )
        })?;
        prune_empty(&mut expected);
        prune_empty(&mut got);
        if got != expected {
            return Err(format!(
                "{}: could not make the change without touching other settings; nothing was \
                 changed, edit it by hand",
                self.path.display()
            ));
        }
        Ok(text)
    }

    /// Sets the keys and writes the file (0600, in a 0700 directory) atomically. The result must
    /// parse and read back every value, or nothing is written.
    pub fn set(&self, entries: &[(&str, &str, &str)]) -> Result<(), String> {
        if entries.is_empty() {
            return Ok(());
        }
        let _lock = lock_config(&self.path)?;
        let mut text = self.read()?;
        for (section, key, value) in entries {
            text = set_string(&text, section, key, value);
        }
        for (section, key, value) in entries {
            if get_string(&text, section, key).as_deref() != Some(*value) {
                return Err(format!(
                    "{}: could not set [{section}] {key} without breaking the file; edit it by hand",
                    self.path.display()
                ));
            }
        }
        write_private(&self.path, &text)
    }
}

/// The first part of a dotted `section` path that exists in `doc` but is not a table.
fn non_table_on(doc: &toml::Table, section: &str) -> Option<String> {
    let mut table = doc;
    let mut path = Vec::new();
    for part in section.split('.') {
        path.push(part);
        match table.get(part) {
            None => return None,
            Some(toml::Value::Table(t)) => table = t,
            Some(_) => return Some(path.join(".")),
        }
    }
    None
}

/// The file a write to `path` lands in: `path` itself, or, when it is a symlink, the file the
/// link (and any links after it) points to, which need not exist yet. Relative link targets are
/// resolved against the link's directory.
pub fn write_target(path: &Path) -> Result<PathBuf, String> {
    let mut current = path.to_path_buf();
    for _ in 0..40 {
        match fs::symlink_metadata(&current) {
            Ok(meta) if meta.file_type().is_symlink() => {
                let link = fs::read_link(&current)
                    .map_err(|e| format!("read link {}: {e}", current.display()))?;
                current = match current.parent() {
                    Some(dir) if link.is_relative() => dir.join(link),
                    _ => link,
                };
            }
            _ => return Ok(current),
        }
    }
    Err(format!("{}: too many levels of symlinks", path.display()))
}

/// An advisory lock on a config file, held until dropped (the OS also releases it when the
/// process exits, so a crashed writer never leaves it held).
#[derive(Debug)]
pub struct ConfigLock {
    _file: fs::File,
}

/// Takes the lock on `<real file>.lock` that every writer of `path` takes, waiting up to
/// [`LOCK_WAIT`] for another writer to finish. The lock file sits next to the real file (behind
/// any symlink), so writers that reach it through different links share one lock. It is left in
/// place: removing it would let two writers lock two different files.
pub fn lock_config(path: &Path) -> Result<ConfigLock, String> {
    let target = write_target(path)?;
    let dir = parent_of(&target)?;
    ensure_private_dir(dir)?;
    let mut name = target.file_name().unwrap_or_default().to_os_string();
    name.push(".lock");
    let lock_path = dir.join(name);
    let mut options = fs::OpenOptions::new();
    options.read(true).write(true).create(true).truncate(false);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let file = options
        .open(&lock_path)
        .map_err(|e| format!("lock {}: {e}", lock_path.display()))?;
    let started = Instant::now();
    loop {
        match file.try_lock() {
            Ok(()) => return Ok(ConfigLock { _file: file }),
            Err(fs::TryLockError::WouldBlock) if started.elapsed() < LOCK_WAIT => {
                std::thread::sleep(Duration::from_millis(20));
            }
            Err(fs::TryLockError::WouldBlock) => {
                return Err(format!(
                    "{}: another amp-harness has been writing it for {} s; nothing was \
                     changed, try again",
                    target.display(),
                    LOCK_WAIT.as_secs()
                ));
            }
            Err(fs::TryLockError::Error(e)) => {
                return Err(format!("lock {}: {e}", lock_path.display()));
            }
        }
    }
}

fn parent_of(path: &Path) -> Result<&Path, String> {
    path.parent()
        .filter(|d| !d.as_os_str().is_empty())
        .ok_or_else(|| format!("{}: no parent directory", path.display()))
}

/// Creates `dir` (0700) when it does not exist yet; an existing directory is left as it is.
fn ensure_private_dir(dir: &Path) -> Result<(), String> {
    if !dir.is_dir() {
        fs::create_dir_all(dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = fs::set_permissions(dir, fs::Permissions::from_mode(0o700));
        }
    }
    Ok(())
}

/// Writes `text` to `path` atomically, mode 0600, creating the directory (0700) when needed.
///
/// The text goes to a uniquely named temporary file in the same directory (so concurrent writers
/// never truncate each other's), which is then renamed over the file. A symlink at `path` is kept:
/// the file it points to is replaced instead. Callers that read the file first hold
/// [`lock_config`] around the read and this write.
pub fn write_private(path: &Path, text: &str) -> Result<(), String> {
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let target = write_target(path)?;
    let dir = parent_of(&target)?;
    ensure_private_dir(dir)?;
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or_default();
    let mut name = std::ffi::OsString::from(".");
    name.push(target.file_name().unwrap_or_default());
    name.push(format!(
        ".{}-{nanos}-{}.tmp",
        std::process::id(),
        COUNTER.fetch_add(1, Ordering::Relaxed)
    ));
    let tmp = dir.join(name);
    let write = || -> std::io::Result<()> {
        let mut options = fs::OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut file = options.open(&tmp)?;
        file.write_all(text.as_bytes())?;
        file.sync_all()?;
        fs::rename(&tmp, &target)
    };
    write().map_err(|e| {
        let _ = fs::remove_file(&tmp);
        format!("write {}: {e}", path.display())
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn adds_a_section_to_an_empty_file() {
        let t = set_string("", "server", "url", "http://localhost:18000/mcp");
        assert_eq!(t, "[server]\nurl = \"http://localhost:18000/mcp\"\n");
        assert_eq!(
            get_string(&t, "server", "url").as_deref(),
            Some("http://localhost:18000/mcp")
        );
    }

    #[test]
    fn replaces_a_key_and_keeps_comments_and_other_tables() {
        let before = "# mine\n[server]\nurl = \"https://old/mcp\" # old\nprotocol = \"auto\"\n\n[tui]\ntheme = \"dark\"\n\n[roles.manager]\nmodel = \"anthropic/x\"\n";
        let t = set_string(before, "server", "url", "https://new/mcp");
        let t = set_string(&t, "server", "web_url", "https://new");
        let t = set_string(&t, "models", "profile", "anthropic");
        assert!(t.starts_with("# mine\n[server]\nurl = \"https://new/mcp\"\nprotocol = \"auto\"\nweb_url = \"https://new\"\n\n[tui]"), "{t}");
        assert_eq!(get_string(&t, "tui", "theme").as_deref(), Some("dark"));
        assert_eq!(
            get_string(&t, "models", "profile").as_deref(),
            Some("anthropic")
        );
        let doc: toml::Table = toml::from_str(&t).unwrap();
        assert_eq!(
            doc["roles"]["manager"]["model"].as_str(),
            Some("anthropic/x")
        );
    }

    #[test]
    fn does_not_touch_a_commented_key_or_a_key_with_a_longer_name() {
        let before = "[server]\n# url = \"https://commented/mcp\"\nurl_extra = 1\n";
        let t = set_string(before, "server", "url", "https://a/mcp");
        assert!(t.contains("# url = \"https://commented/mcp\""));
        assert!(t.contains("url_extra = 1"));
        assert_eq!(
            get_string(&t, "server", "url").as_deref(),
            Some("https://a/mcp")
        );
    }

    #[test]
    fn escapes_values() {
        let t = set_string("", "server", "ca_file", "C:\\certs\\a \"b\".pem");
        assert_eq!(
            get_string(&t, "server", "ca_file").as_deref(),
            Some("C:\\certs\\a \"b\".pem")
        );
    }

    #[test]
    fn removes_one_providers_tables_and_keeps_everything_else() {
        let before = "# mine\n[server]\nurl = \"u\"\n\n[providers.mine]\napi = \"openai-completions\"\n\
                      # a note inside\nbase_url = \"http://x\"\n\n[providers.mine.models.\"a/b\"]\n\n\
                      # about the next table\n[providers.mine2]\napi = \"faux\"\n\n[providers . \"mine\" . models.\"c\"]\n\
                      [roles.manager]\nmodel = \"mine2/m\"\n";
        let (t, cut) = remove_tables(before, "providers.mine");
        assert!(cut);
        assert!(
            !t.contains("http://x") && !t.contains("a note inside"),
            "{t}"
        );
        assert!(!t.contains("\"c\""), "spaced/quoted header is cut too: {t}");
        assert!(
            t.contains("# about the next table\n[providers.mine2]"),
            "{t}"
        );
        assert!(
            t.contains("[roles.manager]") && t.starts_with("# mine\n[server]"),
            "{t}"
        );
        let doc: toml::Table = toml::from_str(&t).unwrap();
        assert!(doc["providers"].get("mine").is_none());
        assert_eq!(doc["providers"]["mine2"]["api"].as_str(), Some("faux"));
        let (same, cut) = remove_tables(&t, "providers.other");
        assert!(!cut);
        assert_eq!(same, t);
    }

    #[test]
    fn replace_tables_backs_up_and_refuses_a_bad_result() {
        let dir =
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../../target/test-scratch/config-tables");
        let _ = fs::remove_dir_all(&dir);
        let file = ConfigFile::new(dir.join("config.toml"));
        let ok = |_: &str| Ok(());
        // A new file: no backup.
        let bak = file
            .replace_tables("providers.p", Some("[providers.p]\napi = \"faux\"\n"), &ok)
            .unwrap();
        assert!(bak.is_none());
        let first = fs::read_to_string(&file.path).unwrap();
        assert!(first.starts_with("# amp-harness settings") && first.contains("[providers.p]"));
        // Replaced: one copy of the table, the old file in config.toml.bak.
        let bak = file
            .replace_tables(
                "providers.p",
                Some("[providers.p]\napi = \"openai\"\n"),
                &ok,
            )
            .unwrap()
            .unwrap();
        assert_eq!(fs::read_to_string(&bak).unwrap(), first);
        let second = fs::read_to_string(&file.path).unwrap();
        assert_eq!(second.matches("[providers.p]").count(), 1, "{second}");
        // A failing check writes nothing.
        let err = file
            .replace_tables("providers.p", None, &|_| Err("nope".into()))
            .unwrap_err();
        assert!(err.contains("nope"), "{err}");
        assert_eq!(fs::read_to_string(&file.path).unwrap(), second);
        // Removed.
        file.replace_tables("providers.p", None, &ok).unwrap();
        assert!(
            !fs::read_to_string(&file.path)
                .unwrap()
                .contains("providers.p")
        );
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn named_servers_round_trip_and_keep_everything_else() {
        let before = "# mine\n[server]\nurl = \"https://amp.simstech.cloud/mcp\"\n\n[tui]\ntheme = \"dark\" # keep\n\n[roles.manager]\nmodel = \"anthropic/x\"\n";
        let t = set_string(before, "servers.local", "url", "http://localhost:18000/mcp");
        let t = set_string(&t, "servers.local", "web_url", "http://localhost:5173");
        assert_eq!(
            get_string(&t, "servers.local", "web_url").as_deref(),
            Some("http://localhost:5173")
        );
        let t = remove_key(&t, "servers.local", "web_url");
        assert_eq!(get_string(&t, "servers.local", "web_url"), None);
        assert!(t.contains("theme = \"dark\" # keep"), "{t}");
        let t = remove_section(&t, "servers.local");
        assert_eq!(t, before);
    }

    #[test]
    fn edit_backs_up_and_refuses_to_clobber() {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-scratch/config-file-edit");
        let _ = fs::remove_dir_all(&dir);
        let file = ConfigFile::new(dir.join("amp-harness/config.toml"));
        // A new file: no backup.
        assert_eq!(
            file.edit(&[Change::set("server", "url", "http://a/mcp")])
                .unwrap(),
            None
        );
        let mut text = file.text().unwrap().unwrap();
        text.push_str("\n[tui]\ntheme = \"light\"\n");
        fs::write(&file.path, &text).unwrap();
        let backup = file
            .edit(&[
                Change::set("servers.hosted", "url", "https://h/mcp"),
                Change::set("server", "name", "hosted"),
                Change::set("server", "url", "https://h/mcp"),
            ])
            .unwrap()
            .expect("a backup");
        assert_eq!(fs::read_to_string(&backup).unwrap(), text);
        assert_eq!(file.get("tui", "theme").as_deref(), Some("light"));
        assert_eq!(
            file.get("servers.hosted", "url").as_deref(),
            Some("https://h/mcp")
        );
        file.edit(&[Change::DropSection("servers.hosted".into())])
            .unwrap();
        assert_eq!(file.get("servers.hosted", "url"), None);
        assert_eq!(file.get("server", "url").as_deref(), Some("https://h/mcp"));

        // A quoted header the line editor can't see: the result would differ, so nothing is written.
        fs::write(&file.path, "[servers.\"x\"]\nurl = \"http://x/mcp\"\n").unwrap();
        let err = file
            .edit(&[Change::set("servers.x", "web_url", "http://x")])
            .unwrap_err();
        assert!(err.contains("nothing was changed"), "{err}");
        assert_eq!(
            fs::read_to_string(&file.path).unwrap(),
            "[servers.\"x\"]\nurl = \"http://x/mcp\"\n"
        );
        // A broken file is never rewritten.
        fs::write(&file.path, "[server\nurl=").unwrap();
        assert!(
            file.edit(&[Change::set("server", "url", "http://y/mcp")])
                .is_err()
        );
        assert_eq!(fs::read_to_string(&file.path).unwrap(), "[server\nurl=");
        let _ = fs::remove_dir_all(&dir);
    }

    /// #2235: whole role tables go (quoted headers and sub-tables too), a model table is added
    /// once, and `preview` writes nothing.
    #[test]
    fn drop_and_add_tables() {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-scratch/config-file-tables2");
        let _ = fs::remove_dir_all(&dir);
        let file = ConfigFile::repo(dir.join(".amp/harness/config.toml"));
        let changes = [
            Change::AddTable(vec![
                "providers".into(),
                "openrouter".into(),
                "models".into(),
                "openai/gpt-oss-120b".into(),
            ]),
            Change::set("roles.reviewer", "model", "openrouter/openai/gpt-oss-120b"),
        ];
        let text = file.preview(&changes).unwrap();
        assert!(!file.path.exists());
        assert!(text.starts_with(REPO_FILE_HEADER), "{text}");
        assert!(
            text.contains("[providers.openrouter.models.\"openai/gpt-oss-120b\"]\n"),
            "{text}"
        );
        file.edit(&changes).unwrap();
        // Adding it again changes nothing.
        assert_eq!(file.edit(&changes).unwrap(), None);
        let mut text = fs::read_to_string(&file.path).unwrap();
        text.push_str(
            "# kept\n[roles.\"manager\"]\nthinking = \"low\"\n[roles.manager.extra_body]\na = 1\n\n[roles.managerial]\nthinking = \"high\"\n",
        );
        fs::write(&file.path, &text).unwrap();
        let bak = file
            .edit(&[Change::DropTables("roles.manager".into())])
            .unwrap()
            .unwrap();
        assert_eq!(fs::read_to_string(bak).unwrap(), text);
        let doc: toml::Table = toml::from_str(&fs::read_to_string(&file.path).unwrap()).unwrap();
        assert!(doc["roles"].get("manager").is_none(), "{doc}");
        assert_eq!(
            doc["roles"]["managerial"]["thinking"].as_str(),
            Some("high")
        );
        assert!(doc["roles"]["reviewer"]["model"].is_str());
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn writes_a_private_file() {
        let dir =
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../../target/test-scratch/config-file");
        let _ = fs::remove_dir_all(&dir);
        let file = ConfigFile::new(dir.join("amp-harness/config.toml"));
        file.set(&[
            ("server", "url", "http://x/mcp"),
            ("models", "profile", "anthropic"),
        ])
        .unwrap();
        let text = fs::read_to_string(&file.path).unwrap();
        assert!(text.starts_with("# amp-harness settings"), "{text}");
        assert_eq!(file.get("server", "url").as_deref(), Some("http://x/mcp"));
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = fs::metadata(&file.path).unwrap().permissions().mode() & 0o777;
            assert_eq!(mode, 0o600);
        }
        let _ = fs::remove_dir_all(&dir);
    }

    fn scratch(name: &str) -> PathBuf {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../target/test-scratch/config-file-2309")
            .join(name);
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        dir
    }

    /// #2309 (H18 finding 9): writers racing on one file each keep their change, and leave no
    /// temporary file behind.
    #[test]
    fn concurrent_writers_never_lose_a_change() {
        let dir = scratch("race");
        let path = dir.join("config.toml");
        fs::write(&path, "# mine\n[tui]\ntheme = \"dark\"\n").unwrap();
        let threads: Vec<_> = (0..12)
            .map(|i| {
                let path = path.clone();
                std::thread::spawn(move || {
                    let file = ConfigFile::new(path);
                    for round in 0..5 {
                        let section = format!("roles.r{i}");
                        if i % 3 == 0 {
                            file.set(&[(section.as_str(), "model", &format!("p/m{round}"))])
                                .unwrap();
                        } else if i % 3 == 1 {
                            file.edit(&[Change::set(&section, "model", format!("p/m{round}"))])
                                .unwrap();
                        } else {
                            let block = format!("[{section}]\nmodel = \"p/m{round}\"\n");
                            file.replace_tables(&section, Some(&block), &|_| Ok(()))
                                .unwrap();
                        }
                    }
                })
            })
            .collect();
        for t in threads {
            t.join().unwrap();
        }
        let text = fs::read_to_string(&path).unwrap();
        let doc: toml::Table = toml::from_str(&text).unwrap();
        for i in 0..12 {
            assert_eq!(
                doc["roles"][&format!("r{i}")]["model"].as_str(),
                Some("p/m4"),
                "r{i} lost: {text}"
            );
        }
        assert_eq!(doc["tui"]["theme"].as_str(), Some("dark"));
        assert!(text.starts_with("# mine"));
        let left: Vec<_> = fs::read_dir(&dir)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .filter(|n| n.ends_with(".tmp"))
            .collect();
        assert!(left.is_empty(), "{left:?}");
        let _ = fs::remove_dir_all(&dir);
    }

    /// #2309: a symlinked config.toml (dotfile managers) stays a link; its target is written.
    #[cfg(unix)]
    #[test]
    fn a_symlinked_config_stays_a_symlink() {
        use std::os::unix::fs::{PermissionsExt, symlink};
        let dir = scratch("symlink");
        fs::create_dir_all(dir.join("dotfiles")).unwrap();
        fs::create_dir_all(dir.join("home")).unwrap();
        let real = dir.join("dotfiles/config.toml");
        fs::write(&real, "# tracked\n[tui]\ntheme = \"dark\"\n").unwrap();
        let link = dir.join("home/config.toml");
        symlink("../dotfiles/config.toml", &link).unwrap();
        let file = ConfigFile::new(link.clone());

        let backup = file
            .edit(&[Change::set("server", "url", "http://x/mcp")])
            .unwrap()
            .unwrap();
        file.set(&[("models", "profile", "anthropic")]).unwrap();
        file.replace_tables(
            "providers.gw",
            Some("[providers.gw]\nbase_url = \"http://gw/v1\"\n"),
            &|_| Ok(()),
        )
        .unwrap();
        assert!(
            fs::symlink_metadata(&link)
                .unwrap()
                .file_type()
                .is_symlink()
        );
        let text = fs::read_to_string(&real).unwrap();
        assert!(text.starts_with("# tracked"), "{text}");
        for want in ["http://x/mcp", "anthropic", "http://gw/v1", "theme"] {
            assert!(text.contains(want), "{want}: {text}");
        }
        assert_eq!(
            fs::metadata(&real).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert!(fs::read_to_string(backup).unwrap().contains("# tracked"));
        // The lock sits next to the real file, not the link.
        assert!(dir.join("dotfiles/config.toml.lock").is_file());

        // A link to a file that does not exist yet: the file is created where it points.
        let fresh = dir.join("home/fresh.toml");
        symlink("../dotfiles/fresh.toml", &fresh).unwrap();
        ConfigFile::new(fresh.clone())
            .set(&[("server", "url", "http://y/mcp")])
            .unwrap();
        assert!(
            fs::symlink_metadata(&fresh)
                .unwrap()
                .file_type()
                .is_symlink()
        );
        assert!(
            fs::read_to_string(dir.join("dotfiles/fresh.toml"))
                .unwrap()
                .contains("http://y/mcp")
        );
        let _ = fs::remove_dir_all(&dir);
    }

    /// #2309: a writer that cannot get the lock gives up with a message instead of waiting forever.
    #[test]
    fn a_held_lock_is_waited_for() {
        let dir = scratch("held");
        let path = dir.join("config.toml");
        let held = lock_config(&path).unwrap();
        let writer = {
            let path = path.clone();
            std::thread::spawn(move || ConfigFile::new(path).set(&[("server", "url", "http://z")]))
        };
        std::thread::sleep(Duration::from_millis(300));
        assert!(!writer.is_finished(), "the writer waits for the lock");
        drop(held);
        writer.join().unwrap().unwrap();
        assert!(fs::read_to_string(&path).unwrap().contains("http://z"));
        let _ = fs::remove_dir_all(&dir);
    }
}
