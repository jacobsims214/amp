//! The repo config layer, `<repo>/.amp/harness/config.toml` (#2232, D2/D10 of the configurability
//! design).
//!
//! A repo shares settings that belong to the project: which MCP servers it uses (`[mcp.*]`, read
//! by the MCP layer), the model of a role (`[roles.*]`) and the assignment mode
//! (`[models] profile`). Nothing else is allowed, because a cloned repo must not redirect the
//! person's AMP server, provider keys, bash environment or workers: `[server]`, `[providers]`,
//! `[secrets]`, `[workers]`, `[tools]`, `[tui]`, `[profiles]`, `[models] inherit` and any other key
//! are an error that names the key and the file.
//!
//! Precedence, lowest first: built-in defaults < the user's config.toml < this file (merged per
//! role, see [`crate::models::ModelsConfig::from_layers`]). `AMP_HARNESS_PROFILE` still beats the
//! repo's `[models] profile`.

use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
};

use crate::models::{ModelsError, ModelsFile, ModelsSection, RoleSpec};

/// The top-level tables a repo config may hold.
pub const REPO_TABLES: [&str; 3] = ["mcp", "roles", "models"];
/// The keys of `[models]` a repo config may set.
pub const REPO_MODELS_KEYS: [&str; 1] = ["profile"];

/// A parsed, validated repo config.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct RepoConfig {
    /// The file it was read from.
    pub path: PathBuf,
    /// `[roles.<id>]`, the same schema as the user file's.
    pub roles: BTreeMap<String, RoleSpec>,
    /// `[models] profile`.
    pub profile: Option<String>,
    /// `[mcp]` as written (each `[mcp.<name>]` is a table); the MCP layer parses it.
    pub mcp: toml::Table,
    /// `[roles]` and `[models]` as written, for trust fingerprints (#2310).
    pub raw_roles: toml::Table,
    pub raw_models: toml::Table,
}

impl RepoConfig {
    /// Reads `path`; `Ok(None)` when it does not exist.
    pub fn load(path: &Path) -> Result<Option<Self>, ModelsError> {
        match std::fs::read_to_string(path) {
            Ok(text) => Self::parse(&text, path).map(Some),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(e) => Err(ModelsError(format!("read {}: {e}", path.display()))),
        }
    }

    /// Parses and validates `text` (the contents of `path`).
    pub fn parse(text: &str, path: &Path) -> Result<Self, ModelsError> {
        let where_ = path.display();
        let table: toml::Table =
            toml::from_str(text).map_err(|e| ModelsError(format!("parse {where_}: {e}")))?;
        let not_allowed = |key: &str| {
            ModelsError(format!(
                "{where_}: `{key}` is not allowed in a repo config. A repo's .amp/harness/config.toml \
                 may set only [mcp.*], [roles.*] and [models] profile; put `{key}` in your user \
                 config.toml instead"
            ))
        };
        for key in table.keys() {
            if !REPO_TABLES.contains(&key.as_str()) {
                return Err(not_allowed(key));
            }
        }
        let mut config = RepoConfig {
            path: path.to_path_buf(),
            ..RepoConfig::default()
        };
        if let Some(models) = table.get("models") {
            let models = models
                .as_table()
                .ok_or_else(|| ModelsError(format!("{where_}: `models` must be a table")))?;
            for key in models.keys() {
                if !REPO_MODELS_KEYS.contains(&key.as_str()) {
                    return Err(not_allowed(&format!("models.{key}")));
                }
            }
            let section: ModelsSection = models
                .clone()
                .try_into()
                .map_err(|e| ModelsError(format!("{where_} [models]: {e}")))?;
            config.profile = section.profile;
            config.raw_models = models.clone();
        }
        if let Some(roles) = table.get("roles") {
            config.raw_roles = roles.as_table().cloned().unwrap_or_default();
            config.roles = roles
                .clone()
                .try_into()
                .map_err(|e| ModelsError(format!("{where_} [roles]: {e}")))?;
        }
        if let Some(mcp) = table.get("mcp") {
            config.mcp = mcp
                .as_table()
                .cloned()
                .ok_or_else(|| ModelsError(format!("{where_}: `mcp` must be a table")))?;
        }
        Ok(config)
    }

    /// The models part as a [`ModelsFile`] layer (`[roles]` and `[models] profile` only).
    pub fn models_file(&self) -> ModelsFile {
        ModelsFile {
            models: ModelsSection {
                profile: self.profile.clone(),
                ..ModelsSection::default()
            },
            roles: self.roles.clone(),
            ..ModelsFile::default()
        }
    }

    /// Leaves out every `[roles.<id>]` table and `[models]` setting `gate` does not trust as
    /// written now (#2310: a cloned repo can't pick your models or add request fields until you
    /// trusted that). Returns what was left out, as `role:<id>` / `setting:<key>`.
    pub fn retain_trusted(&mut self, gate: &crate::repo_trust::Gate) -> Vec<String> {
        use crate::repo_trust::{ItemKind, role_fingerprint, setting_fingerprint};
        let mut dropped = Vec::new();
        let raw_roles = self.raw_roles.clone();
        self.roles.retain(|role, _| {
            let ok = raw_roles
                .get(role)
                .is_some_and(|v| gate.allows(ItemKind::Role, role, &role_fingerprint(v)));
            if !ok {
                dropped.push(format!("role:{role}"));
            }
            ok
        });
        if self.profile.is_some() {
            let ok = self.raw_models.get("profile").is_some_and(|v| {
                gate.allows(
                    ItemKind::Setting,
                    "profile",
                    &setting_fingerprint("profile", v),
                )
            });
            if !ok {
                self.profile = None;
                dropped.push("setting:profile".to_owned());
            }
        }
        dropped
    }

    /// How sources name this layer: `repo <path>`.
    pub fn label(&self) -> String {
        format!("repo {}", self.path.display())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(text: &str) -> Result<RepoConfig, ModelsError> {
        RepoConfig::parse(text, Path::new("/r/.amp/harness/config.toml"))
    }

    #[test]
    fn allowed_keys_load() {
        let c = parse(
            r#"
[models]
profile = "anthropic"

[roles.worker-backend]
model = "anthropic/claude-sonnet-4-6"
thinking = "high"

[mcp.github]
type = "http"
url = "https://example.test/mcp"
"#,
        )
        .unwrap();
        assert_eq!(c.profile.as_deref(), Some("anthropic"));
        assert_eq!(
            c.roles["worker-backend"].model.as_deref(),
            Some("anthropic/claude-sonnet-4-6")
        );
        assert_eq!(c.mcp["github"]["type"].as_str(), Some("http"));
        assert_eq!(c.models_file().roles.len(), 1);
        assert_eq!(c.label(), "repo /r/.amp/harness/config.toml");
        assert_eq!(parse("").unwrap().roles.len(), 0);
    }

    #[test]
    fn other_keys_are_errors_naming_the_key_and_file() {
        for (text, key) in [
            ("[server]\nurl = \"https://evil.test/mcp\"\n", "`server`"),
            ("[providers.x]\nbase_url = \"http://x\"\n", "`providers`"),
            ("[secrets]\npass_to_bash = [\"AMP_TOKEN\"]\n", "`secrets`"),
            ("[workers]\nmax_parallel = 9\n", "`workers`"),
            ("[tools]\nbash_timeout_secs = 1\n", "`tools`"),
            ("[tui]\nmouse = false\n", "`tui`"),
            (
                "[profiles.p.roles.manager]\nmodel = \"a/b\"\n",
                "`profiles`",
            ),
            ("[models]\ninherit = \"a/b\"\n", "`models.inherit`"),
            ("stray = 1\n", "`stray`"),
        ] {
            let e = parse(text).unwrap_err().0;
            assert!(e.contains(key), "{text}: {e}");
            assert!(e.contains("/r/.amp/harness/config.toml"), "{e}");
            assert!(e.contains("[mcp.*], [roles.*] and [models] profile"), "{e}");
        }
        // Inside [roles.*] unknown keys are errors too (same schema as the user file).
        let e = parse("[roles.manager]\nmodle = \"a/b\"\n").unwrap_err().0;
        assert!(e.contains("[roles]") && e.contains("modle"), "{e}");
    }

    #[test]
    fn missing_file_is_none() {
        assert_eq!(
            RepoConfig::load(Path::new("/nonexistent/amp-harness/repo.toml")).unwrap(),
            None
        );
    }
}
