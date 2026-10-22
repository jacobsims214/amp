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
const NEW_FILE_HEADER: &str = "# amp-harness settings, written by the first run / `amp-harness init`. Edit freely.\n\
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

/// Reads `[section] key` as a string, if the text parses and has it.
pub fn get_string(text: &str, section: &str, key: &str) -> Option<String> {
    let doc: toml::Table = toml::from_str(text).ok()?;
    doc.get(section)?.get(key)?.as_str().map(str::to_owned)
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
