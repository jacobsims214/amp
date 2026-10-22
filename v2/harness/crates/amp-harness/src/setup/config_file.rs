//! Small, comment-preserving edits to `config.toml` (first run and `init`).
//!
//! The file is the user's: it may hold comments, `[models]`, `[roles.*]` and other tables. Setup
//! only ever sets a few plain string keys (`[server] url`, `web_url`, `ca_file`, `[models]
//! profile`), so it edits lines instead of re-serialising the whole document, then checks that the
//! result still parses and reads back the value it wrote. Secrets are never written here (D3).

use std::{
    fs,
    io::Write,
    path::{Path, PathBuf},
};

/// The header of a config.toml that setup creates.
const NEW_FILE_HEADER: &str = "# amp-harness settings, written by the first run, `amp-harness init` and `amp-harness server`. Edit freely.\n\
# Provider keys are never stored here: they live in the OS keychain or the environment.\n";

/// A TOML basic string for `value` (JSON string escapes are valid TOML basic-string escapes).
fn toml_string(value: &str) -> String {
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
    let new_line = format!("{key} = {}", toml_string(value));
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
    DropSection(String),
}

impl Change {
    pub fn set(section: &str, key: &str, value: impl Into<String>) -> Self {
        Self::Set {
            section: section.to_owned(),
            key: key.to_owned(),
            value: value.into(),
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
            Self::Remove { section, key } => remove_key(text, section, key),
            Self::DropSection(section) => remove_section(text, section),
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
            Self::Remove { section, key } => {
                if table_at(doc, section).is_some() {
                    table_mut(doc, section).remove(key);
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
}

impl ConfigFile {
    pub fn new(path: PathBuf) -> Self {
        Self { path }
    }

    pub fn exists(&self) -> bool {
        self.path.is_file()
    }

    fn read(&self) -> Result<String, String> {
        match fs::read_to_string(&self.path) {
            Ok(text) => Ok(text),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(NEW_FILE_HEADER.to_owned()),
            Err(e) => Err(format!("read {}: {e}", self.path.display())),
        }
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
        let existing = self.text()?;
        let before = existing
            .clone()
            .unwrap_or_else(|| NEW_FILE_HEADER.to_owned());
        let mut expected: toml::Table = toml::from_str(&before).map_err(|e| {
            format!(
                "{} does not parse ({e}); fix it first, nothing was changed",
                self.path.display()
            )
        })?;
        let mut text = before.clone();
        for change in changes {
            if let Change::Set { section, .. } | Change::Remove { section, .. } = change
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

    /// Sets the keys and writes the file (0600, in a 0700 directory) atomically. The result must
    /// parse and read back every value, or nothing is written.
    pub fn set(&self, entries: &[(&str, &str, &str)]) -> Result<(), String> {
        if entries.is_empty() {
            return Ok(());
        }
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

/// Writes `text` to `path` through a temporary file in the same directory, mode 0600, creating
/// the directory (0700) when needed.
pub fn write_private(path: &Path, text: &str) -> Result<(), String> {
    let dir = path
        .parent()
        .ok_or_else(|| format!("{}: no parent directory", path.display()))?;
    if !dir.is_dir() {
        fs::create_dir_all(dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = fs::set_permissions(dir, fs::Permissions::from_mode(0o700));
        }
    }
    let tmp = path.with_extension("toml.tmp");
    let write = || -> std::io::Result<()> {
        let mut options = fs::OpenOptions::new();
        options.write(true).create(true).truncate(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut file = options.open(&tmp)?;
        file.write_all(text.as_bytes())?;
        file.sync_all()?;
        fs::rename(&tmp, path)
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
}
