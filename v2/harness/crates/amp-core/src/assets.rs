//! Agent and skill assets (D5): the roster and skills generated from `v2/.opencode` into
//! `v2/harness/assets` by `v2/scripts/build-harness-assets.sh`, embedded in the binary, with
//! per-file overrides from disk.
//!
//! Layers, lowest to highest precedence (a later layer replaces the same file of an earlier one):
//! 1. embedded: `v2/harness/assets/{agents,skills}` at build time;
//! 2. user: `$XDG_CONFIG_HOME/amp-harness/{agents,skills}/` (else `~/.config/amp-harness/...`);
//! 3. repo: `<repo>/.amp/harness/{agents,skills}/`.
//!
//! An agent is one file, `agents/<id>.md` (YAML frontmatter + system prompt), so an override
//! replaces the whole agent. A skill is a directory, `skills/<name>/`, and overrides are per file:
//! `skills/amp-kb/SKILL.md` replaces only that file and the skill keeps its other embedded files.
//! Overrides may also add agents and skills that aren't embedded. A malformed override is an
//! error, not something silently skipped: a user who wrote one expects it to be used.
//!
//! Frontmatter is kept as parsed YAML ([`AgentAsset::frontmatter`]): roles and tool policy (M2-2)
//! read `mode`, `model`, `variant` and `permission` from it.

use std::collections::BTreeMap;
use std::fmt;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};

use include_dir::{Dir, include_dir};
use serde::Deserialize;
use serde_json::{Map, Value};

/// The generated assets, embedded at build time (`crates/amp-core/build.rs` reruns on changes).
static EMBEDDED: Dir<'static> = include_dir!("$CARGO_MANIFEST_DIR/../../assets");

/// Directory names under each layer.
pub const AGENTS_DIR: &str = "agents";
pub const SKILLS_DIR: &str = "skills";
/// A skill's entry file.
pub const SKILL_FILE: &str = "SKILL.md";

/// Where an asset file came from.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Origin {
    Embedded,
    /// From `~/.config/amp-harness/...`; the path is the file that was used.
    User(PathBuf),
    /// From `<repo>/.amp/harness/...`; the path is the file that was used.
    Repo(PathBuf),
}

impl Origin {
    pub fn is_embedded(&self) -> bool {
        matches!(self, Origin::Embedded)
    }
}

impl fmt::Display for Origin {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Origin::Embedded => f.write_str("embedded"),
            Origin::User(p) => write!(f, "user ({})", p.display()),
            Origin::Repo(p) => write!(f, "repo ({})", p.display()),
        }
    }
}

/// Errors loading assets: always names the file.
#[derive(Debug, thiserror::Error)]
pub enum AssetError {
    #[error("{path}: {source}")]
    Io {
        path: PathBuf,
        #[source]
        source: io::Error,
    },
    #[error("{path}: {message}")]
    Invalid { path: String, message: String },
}

impl AssetError {
    fn invalid(path: impl fmt::Display, message: impl Into<String>) -> Self {
        AssetError::Invalid {
            path: path.to_string(),
            message: message.into(),
        }
    }
}

/// The override directories to layer over the embedded assets.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct OverrideDirs {
    /// `~/.config/amp-harness` (holds `agents/` and `skills/`).
    pub user: Option<PathBuf>,
    /// `<repo>/.amp/harness` (holds `agents/` and `skills/`).
    pub repo: Option<PathBuf>,
}

impl OverrideDirs {
    /// The user dir from the process environment, and `<repo_root>/.amp/harness` when a repo
    /// root is given (the directory holding `.amp.json`).
    pub fn discover(repo_root: Option<&Path>) -> Self {
        Self::discover_with_env(repo_root, &|k| std::env::var(k).ok())
    }

    /// [`OverrideDirs::discover`] with an injectable environment (tests).
    pub fn discover_with_env(
        repo_root: Option<&Path>,
        env: &dyn Fn(&str) -> Option<String>,
    ) -> Self {
        Self {
            user: user_dir(env),
            repo: repo_root.map(|r| r.join(".amp").join("harness")),
        }
    }
}

/// `$XDG_CONFIG_HOME/amp-harness`, else `$HOME/.config/amp-harness` (same base as config.toml).
pub fn user_dir(env: &dyn Fn(&str) -> Option<String>) -> Option<PathBuf> {
    let non_empty = |k: &str| env(k).filter(|v| !v.is_empty());
    let base = non_empty("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .or_else(|| non_empty("HOME").map(|h| PathBuf::from(h).join(".config")))?;
    Some(base.join("amp-harness"))
}

/// One agent role: frontmatter plus its system prompt.
#[derive(Debug, Clone, PartialEq)]
pub struct AgentAsset {
    /// The file stem, e.g. `amp-worker-backend`. This is the `agent_id` AMP tickets are assigned to.
    pub id: String,
    pub description: String,
    /// The parsed YAML frontmatter (an object; empty when the file has none).
    pub frontmatter: Value,
    /// The system prompt (the markdown after the frontmatter, trimmed).
    pub prompt: String,
    pub origin: Origin,
}

impl AgentAsset {
    /// opencode's `mode`: `primary` (the manager) or `subagent` (the default).
    pub fn mode(&self) -> &str {
        self.frontmatter
            .get("mode")
            .and_then(Value::as_str)
            .unwrap_or("subagent")
    }

    pub fn is_primary(&self) -> bool {
        self.mode() == "primary"
    }

    fn parse(id: &str, text: &str, origin: Origin, path: &str) -> Result<Self, AssetError> {
        if !valid_name(id) {
            return Err(AssetError::invalid(
                path,
                "agent file name must be lowercase letters, digits and hyphens",
            ));
        }
        let (frontmatter, prompt) =
            parse_frontmatter(text).map_err(|m| AssetError::invalid(path, m))?;
        let description = required_str(&frontmatter, "description", path)?;
        if prompt.is_empty() {
            return Err(AssetError::invalid(path, "the agent has no prompt"));
        }
        Ok(Self {
            id: id.to_string(),
            description,
            frontmatter,
            prompt,
            origin,
        })
    }
}

/// One file of a skill directory.
#[derive(Debug, Clone, PartialEq)]
pub struct SkillFile {
    pub bytes: Vec<u8>,
    pub origin: Origin,
}

/// One skill: `SKILL.md` plus any supporting files (examples, scripts, data).
#[derive(Debug, Clone, PartialEq)]
pub struct SkillAsset {
    pub name: String,
    pub description: String,
    pub frontmatter: Value,
    /// The instructions: `SKILL.md` after its frontmatter, trimmed.
    pub content: String,
    /// Every file, keyed by its `/`-separated path relative to the skill dir (includes `SKILL.md`).
    pub files: BTreeMap<String, SkillFile>,
}

impl SkillAsset {
    /// Where `SKILL.md` came from.
    pub fn origin(&self) -> &Origin {
        &self.files[SKILL_FILE].origin
    }

    fn from_files(name: &str, files: BTreeMap<String, SkillFile>) -> Result<Self, AssetError> {
        let label = |f: &SkillFile| match &f.origin {
            Origin::Embedded => format!("embedded {SKILLS_DIR}/{name}/{SKILL_FILE}"),
            Origin::User(p) | Origin::Repo(p) => p.display().to_string(),
        };
        let Some(entry) = files.get(SKILL_FILE) else {
            let any = files.values().next().map(label).unwrap_or_default();
            return Err(AssetError::invalid(
                any,
                format!("skill {name} has no {SKILL_FILE}"),
            ));
        };
        let path = label(entry);
        let text = std::str::from_utf8(&entry.bytes)
            .map_err(|_| AssetError::invalid(&path, "not UTF-8"))?;
        let (frontmatter, content) =
            parse_frontmatter(text).map_err(|m| AssetError::invalid(&path, m))?;
        let description = required_str(&frontmatter, "description", &path)?;
        if let Some(fm_name) = frontmatter.get("name").and_then(Value::as_str)
            && fm_name != name
        {
            return Err(AssetError::invalid(
                &path,
                format!("frontmatter name {fm_name:?} must match its directory {name:?}"),
            ));
        }
        Ok(Self {
            name: name.to_string(),
            description,
            frontmatter,
            content,
            files,
        })
    }
}

/// What the generator recorded about the embedded assets (`assets/manifest.json`).
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct Manifest {
    /// SHA-256 over the `.opencode` sources and the overrides file the assets were built from.
    pub source_sha256: String,
    /// opencode.json's `default_agent` (the TUI's default role).
    pub default_agent: Option<String>,
    #[serde(default)]
    pub excluded_skills: Vec<String>,
}

/// The loaded roster and skills.
#[derive(Debug, Clone)]
pub struct Assets {
    agents: BTreeMap<String, AgentAsset>,
    skills: BTreeMap<String, SkillAsset>,
    manifest: Manifest,
}

impl Assets {
    /// The embedded assets only, no overrides.
    pub fn embedded() -> Result<Self, AssetError> {
        Self::load(&OverrideDirs::default())
    }

    /// The embedded assets with the user and repo overrides layered on top.
    pub fn load(dirs: &OverrideDirs) -> Result<Self, AssetError> {
        let manifest_file = EMBEDDED
            .get_file("manifest.json")
            .ok_or_else(|| AssetError::invalid("embedded manifest.json", "missing"))?;
        let manifest: Manifest = serde_json::from_slice(manifest_file.contents())
            .map_err(|e| AssetError::invalid("embedded manifest.json", e.to_string()))?;

        // agents: id -> (text, origin, label)
        let mut agent_files: BTreeMap<String, (String, Origin, String)> = BTreeMap::new();
        // skills: name -> rel path -> file
        let mut skill_files: BTreeMap<String, BTreeMap<String, SkillFile>> = BTreeMap::new();

        if let Some(dir) = EMBEDDED.get_dir(AGENTS_DIR) {
            for f in dir.files() {
                let Some(id) = md_stem(f.path()) else {
                    continue;
                };
                let label = format!("embedded {}", f.path().display());
                let text = f
                    .contents_utf8()
                    .ok_or_else(|| AssetError::invalid(&label, "not UTF-8"))?;
                agent_files.insert(id, (text.to_string(), Origin::Embedded, label));
            }
        }
        if let Some(dir) = EMBEDDED.get_dir(SKILLS_DIR) {
            for skill in dir.dirs() {
                let name = file_name(skill.path());
                let files = skill_files.entry(name).or_default();
                collect_embedded(skill, skill.path(), files);
            }
        }

        for (root, origin) in [
            (dirs.user.as_deref(), Origin::User as fn(PathBuf) -> Origin),
            (dirs.repo.as_deref(), Origin::Repo as fn(PathBuf) -> Origin),
        ] {
            let Some(root) = root else { continue };
            for path in list_dir(&root.join(AGENTS_DIR))? {
                let Some(id) = md_stem(&path) else { continue };
                if !path.is_file() {
                    continue;
                }
                let text = read_utf8(&path)?;
                let label = path.display().to_string();
                agent_files.insert(id, (text, origin(path), label));
            }
            for skill_dir in list_dir(&root.join(SKILLS_DIR))? {
                if !skill_dir.is_dir() {
                    continue;
                }
                let name = file_name(&skill_dir);
                let files = skill_files.entry(name).or_default();
                collect_disk(&skill_dir, &skill_dir, origin, files)?;
            }
        }

        let mut agents = BTreeMap::new();
        for (id, (text, origin, label)) in agent_files {
            agents.insert(id.clone(), AgentAsset::parse(&id, &text, origin, &label)?);
        }
        let mut skills = BTreeMap::new();
        for (name, files) in skill_files {
            if !valid_name(&name) {
                return Err(AssetError::invalid(
                    format!("{SKILLS_DIR}/{name}"),
                    "skill directory name must be lowercase letters, digits and hyphens",
                ));
            }
            skills.insert(name.clone(), SkillAsset::from_files(&name, files)?);
        }
        Ok(Self {
            agents,
            skills,
            manifest,
        })
    }

    pub fn agents(&self) -> impl Iterator<Item = &AgentAsset> {
        self.agents.values()
    }

    pub fn agent(&self, id: &str) -> Option<&AgentAsset> {
        self.agents.get(id)
    }

    pub fn skills(&self) -> impl Iterator<Item = &SkillAsset> {
        self.skills.values()
    }

    pub fn skill(&self, name: &str) -> Option<&SkillAsset> {
        self.skills.get(name)
    }

    pub fn manifest(&self) -> &Manifest {
        &self.manifest
    }

    /// The TUI's default role: the manifest's `default_agent`, else the first primary agent.
    pub fn default_agent(&self) -> Option<&AgentAsset> {
        self.manifest
            .default_agent
            .as_deref()
            .and_then(|id| self.agent(id))
            .or_else(|| self.agents().find(|a| a.is_primary()))
    }

    /// Write every skill's files under `dest/<name>/` and return them as rpi skills whose
    /// `file_path` points at the written `SKILL.md`, so the model can read the skill and the files
    /// it references (scripts, examples) by path. `dest` should be a harness-owned directory
    /// (e.g. under `~/.local/state/amp-harness/`); existing files there are overwritten.
    pub fn materialize_skills(
        &self,
        dest: &Path,
    ) -> Result<Vec<rpi_harness::types::Skill>, AssetError> {
        let io_err = |path: &Path| {
            let path = path.to_path_buf();
            move |source| AssetError::Io { path, source }
        };
        let mut out = Vec::with_capacity(self.skills.len());
        for skill in self.skills.values() {
            let dir = dest.join(&skill.name);
            for (rel, file) in &skill.files {
                let path = dir.join(rel);
                if let Some(parent) = path.parent() {
                    fs::create_dir_all(parent).map_err(io_err(parent))?;
                }
                fs::write(&path, &file.bytes).map_err(io_err(&path))?;
            }
            out.push(rpi_harness::types::Skill {
                name: skill.name.clone(),
                description: skill.description.clone(),
                content: skill.content.clone(),
                file_path: dir.join(SKILL_FILE).to_string_lossy().into_owned(),
                disable_model_invocation: skill
                    .frontmatter
                    .get("disable-model-invocation")
                    .and_then(Value::as_bool),
            });
        }
        Ok(out)
    }
}

fn collect_embedded(dir: &Dir<'static>, root: &Path, out: &mut BTreeMap<String, SkillFile>) {
    for f in dir.files() {
        if let Ok(rel) = f.path().strip_prefix(root) {
            out.insert(
                rel_key(rel),
                SkillFile {
                    bytes: f.contents().to_vec(),
                    origin: Origin::Embedded,
                },
            );
        }
    }
    for d in dir.dirs() {
        collect_embedded(d, root, out);
    }
}

fn collect_disk(
    dir: &Path,
    root: &Path,
    origin: fn(PathBuf) -> Origin,
    out: &mut BTreeMap<String, SkillFile>,
) -> Result<(), AssetError> {
    for path in list_dir(dir)? {
        if file_name(&path).starts_with('.') {
            continue;
        }
        if path.is_dir() {
            collect_disk(&path, root, origin, out)?;
        } else if path.is_file() {
            let bytes = fs::read(&path).map_err(|source| AssetError::Io {
                path: path.clone(),
                source,
            })?;
            let rel = path.strip_prefix(root).unwrap_or(&path);
            out.insert(
                rel_key(rel),
                SkillFile {
                    bytes,
                    origin: origin(path.clone()),
                },
            );
        }
    }
    Ok(())
}

/// Sorted entries of a directory; a missing directory is empty.
fn list_dir(dir: &Path) -> Result<Vec<PathBuf>, AssetError> {
    let entries = match fs::read_dir(dir) {
        Ok(e) => e,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(source) => {
            return Err(AssetError::Io {
                path: dir.to_path_buf(),
                source,
            });
        }
    };
    let mut out = Vec::new();
    for entry in entries {
        let entry = entry.map_err(|source| AssetError::Io {
            path: dir.to_path_buf(),
            source,
        })?;
        out.push(entry.path());
    }
    out.sort();
    Ok(out)
}

fn read_utf8(path: &Path) -> Result<String, AssetError> {
    fs::read_to_string(path).map_err(|source| AssetError::Io {
        path: path.to_path_buf(),
        source,
    })
}

fn rel_key(rel: &Path) -> String {
    rel.components()
        .map(|c| c.as_os_str().to_string_lossy())
        .collect::<Vec<_>>()
        .join("/")
}

fn file_name(path: &Path) -> String {
    path.file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default()
}

/// `amp-worker-backend` for `.../amp-worker-backend.md`; `None` for anything else.
fn md_stem(path: &Path) -> Option<String> {
    if path.extension()? != "md" {
        return None;
    }
    Some(path.file_stem()?.to_string_lossy().into_owned())
}

/// Lowercase letters, digits and single hyphens, as rpi's skill names (and AMP agent ids).
fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')
        && !name.starts_with('-')
        && !name.ends_with('-')
        && !name.contains("--")
}

fn required_str(fm: &Value, key: &str, path: &str) -> Result<String, AssetError> {
    match fm.get(key).and_then(Value::as_str) {
        Some(s) if !s.trim().is_empty() => Ok(s.to_string()),
        _ => Err(AssetError::invalid(
            path,
            format!("frontmatter needs a non-empty `{key}`"),
        )),
    }
}

/// Split `---\n<yaml>\n---\n<body>` (the same shape rpi-harness and opencode read). No leading
/// fence means no frontmatter. The body is trimmed.
pub fn parse_frontmatter(text: &str) -> Result<(Value, String), String> {
    let text = text.replace("\r\n", "\n");
    let Some(rest) = text.strip_prefix("---\n") else {
        return Ok((Value::Object(Map::new()), text.trim().to_string()));
    };
    let (yaml, body) = if let Some(body) = rest.strip_prefix("---") {
        ("", body)
    } else {
        match rest.find("\n---") {
            Some(end) => (&rest[..end], &rest[end + 4..]),
            None => return Err("frontmatter has no closing `---`".to_string()),
        }
    };
    let value: Value = if yaml.trim().is_empty() {
        Value::Object(Map::new())
    } else {
        yaml_serde::from_str(yaml).map_err(|e| format!("frontmatter is not valid YAML: {e}"))?
    };
    if !value.is_object() {
        return Err("frontmatter must be a YAML mapping".to_string());
    }
    Ok((value, body.trim().to_string()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn frontmatter_shapes() {
        let (fm, body) = parse_frontmatter("---\nname: x\nvariant: on\n---\n\n# Body\n").unwrap();
        assert_eq!(fm["name"], "x");
        // YAML 1.2: `on` stays a string (opencode's thinking variant), not a boolean.
        assert_eq!(fm["variant"], "on");
        assert_eq!(body, "# Body");

        let (fm, body) = parse_frontmatter("no fence\n").unwrap();
        assert_eq!(fm, Value::Object(Map::new()));
        assert_eq!(body, "no fence");

        assert!(parse_frontmatter("---\nname: x\n").is_err());
        assert!(parse_frontmatter("---\n- a\n---\nbody").is_err());
        assert!(parse_frontmatter("---\nkey: [unclosed\n---\nbody").is_err());
    }

    #[test]
    fn names() {
        for ok in ["amp-manager", "go-engineer", "a1"] {
            assert!(valid_name(ok), "{ok}");
        }
        for bad in ["", "Amp", "a_b", "-a", "a-", "a--b", "a b"] {
            assert!(!valid_name(bad), "{bad}");
        }
    }

    #[test]
    fn user_dir_prefers_xdg() {
        let env = |pairs: &'static [(&'static str, &'static str)]| {
            move |k: &str| {
                pairs
                    .iter()
                    .find(|(n, _)| *n == k)
                    .map(|(_, v)| v.to_string())
            }
        };
        assert_eq!(
            user_dir(&env(&[("XDG_CONFIG_HOME", "/x"), ("HOME", "/h")])),
            Some(PathBuf::from("/x/amp-harness"))
        );
        assert_eq!(
            user_dir(&env(&[("XDG_CONFIG_HOME", ""), ("HOME", "/h")])),
            Some(PathBuf::from("/h/.config/amp-harness"))
        );
        assert_eq!(user_dir(&env(&[])), None);
        let dirs = OverrideDirs::discover_with_env(Some(Path::new("/repo")), &env(&[]));
        assert_eq!(dirs.repo, Some(PathBuf::from("/repo/.amp/harness")));
    }
}
