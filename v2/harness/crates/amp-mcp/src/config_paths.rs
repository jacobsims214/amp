//! Where the harness's configuration lives (#2232, D10 of the configurability design).
//!
//! Two layers of files, each in a directory:
//!
//! | layer | directory ([`ConfigPaths::user_dir`] / [`ConfigPaths::repo_dir`]) | file |
//! |---|---|---|
//! | user | `--config-dir`, else `AMP_HARNESS_CONFIG_DIR`, else `$XDG_CONFIG_HOME/amp-harness`, else `~/.config/amp-harness` | `--config`, else `AMP_HARNESS_CONFIG`, else `<user dir>/config.toml` |
//! | repo | `<repo>/.amp/harness` (the repo is the directory holding `.amp.json`) | `<repo dir>/config.toml` |
//!
//! The user directory also holds `agents/`, `skills/`, `AGENTS.md` and `credentials.json`, so
//! `--config-dir` moves the whole user layer while `--config` moves only the file.
//!
//! The binary resolves the paths once from its flags ([`ConfigPaths::resolve`]) and
//! [`install`]s them; everything that reads or writes configuration asks [`current`] (or
//! [`crate::default_config_path`], which is `current(env).user_file`). Without an installed value
//! (library use, unit tests) [`current`] resolves from the given environment alone, so the env
//! variables work there too. The repo layer depends on the working directory, so it is added per
//! session with [`ConfigPaths::with_repo`].

use std::{
    path::{Path, PathBuf},
    sync::OnceLock,
};

use crate::settings::Source;

/// The user config file, instead of `<user dir>/config.toml` (same as `--config`).
pub const ENV_CONFIG: &str = "AMP_HARNESS_CONFIG";
/// The user config directory, instead of `~/.config/amp-harness` (same as `--config-dir`).
pub const ENV_CONFIG_DIR: &str = "AMP_HARNESS_CONFIG_DIR";
/// The config file's name in either directory.
pub const CONFIG_FILE_NAME: &str = "config.toml";
/// The repo layer's directory, relative to the repo root.
pub const REPO_CONFIG_DIR: &str = ".amp/harness";

/// The resolved configuration paths. `None` paths mean there is no HOME/XDG_CONFIG_HOME to put
/// them under (readers then use the built-in defaults; writers fail with a message).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConfigPaths {
    /// The user's config.toml.
    pub user_file: Option<PathBuf>,
    /// What chose `user_file`: [`Source::Flag`], [`Source::Env`] (`AMP_HARNESS_CONFIG`, or
    /// `AMP_HARNESS_CONFIG_DIR` when the directory chose it) or [`Source::Default`].
    pub user_file_source: Source,
    /// The file itself was named (`--config` / `AMP_HARNESS_CONFIG`), not just its directory.
    pub user_file_named: bool,
    /// The user layer's directory (`agents/`, `skills/`, `AGENTS.md`, `credentials.json`).
    pub user_dir: Option<PathBuf>,
    pub user_dir_source: Source,
    /// `<repo>/.amp/harness`, when the session is inside a repo with `.amp.json`.
    pub repo_dir: Option<PathBuf>,
}

impl Default for ConfigPaths {
    fn default() -> Self {
        Self {
            user_file: None,
            user_file_source: Source::Default,
            user_file_named: false,
            user_dir: None,
            user_dir_source: Source::Default,
            repo_dir: None,
        }
    }
}

impl ConfigPaths {
    /// From the flags (`--config`, `--config-dir`) and `env`. Flags beat env variables; an
    /// explicit file beats the file inside an explicit directory. Relative paths are made absolute
    /// against the current directory now, so a later change of directory (a worker's worktree)
    /// does not move them.
    pub fn resolve(
        config: Option<PathBuf>,
        config_dir: Option<PathBuf>,
        env: &dyn Fn(&str) -> Option<String>,
    ) -> Self {
        let non_empty = |k: &str| env(k).filter(|v| !v.trim().is_empty()).map(PathBuf::from);
        let flag = |p: Option<PathBuf>| p.filter(|p| !p.as_os_str().is_empty());
        let (user_dir, user_dir_source) = if let Some(dir) = flag(config_dir) {
            (Some(absolute(dir)), Source::Flag)
        } else if let Some(dir) = non_empty(ENV_CONFIG_DIR) {
            (Some(absolute(dir)), Source::Env(ENV_CONFIG_DIR.to_owned()))
        } else {
            (default_user_dir(env), Source::Default)
        };
        let (user_file, user_file_source, user_file_named) = if let Some(file) = flag(config) {
            (Some(absolute(file)), Source::Flag, true)
        } else if let Some(file) = non_empty(ENV_CONFIG) {
            (
                Some(absolute(file)),
                Source::Env(ENV_CONFIG.to_owned()),
                true,
            )
        } else {
            (
                user_dir.as_ref().map(|d| d.join(CONFIG_FILE_NAME)),
                user_dir_source.clone(),
                false,
            )
        };
        Self {
            user_file,
            user_file_source,
            user_file_named,
            user_dir,
            user_dir_source,
            repo_dir: None,
        }
    }

    /// From `env` alone (no flags).
    pub fn from_env(env: &dyn Fn(&str) -> Option<String>) -> Self {
        Self::resolve(None, None, env)
    }

    /// The same paths with the repo layer of `repo_root` (the directory holding `.amp.json`);
    /// `None` drops it.
    pub fn with_repo(mut self, repo_root: Option<&Path>) -> Self {
        self.repo_dir = repo_root.map(|r| r.join(REPO_CONFIG_DIR));
        self
    }

    /// The same paths with another user file (a writer's target, a test's temp file).
    pub fn with_user_file(mut self, file: PathBuf, source: Source) -> Self {
        self.user_file_named = !matches!(source, Source::Default);
        self.user_file = Some(file);
        self.user_file_source = source;
        self
    }

    /// `<repo>/.amp/harness/config.toml`.
    pub fn repo_file(&self) -> Option<PathBuf> {
        self.repo_dir.as_ref().map(|d| d.join(CONFIG_FILE_NAME))
    }

    /// The user file when the person named the file itself (`--config` / `AMP_HARNESS_CONFIG`),
    /// so readers can treat a missing file as an error rather than "no config yet". A file inside
    /// `--config-dir` may be missing (a fresh layer).
    pub fn explicit_user_file(&self) -> Option<PathBuf> {
        self.user_file.clone().filter(|_| self.user_file_named)
    }

    /// The user file for a writer: an error naming what to set when there is none.
    pub fn user_file_for_writing(&self) -> Result<PathBuf, String> {
        self.user_file.clone().ok_or_else(|| {
            format!(
                "no HOME or XDG_CONFIG_HOME: nowhere to keep config.toml (set --config, {ENV_CONFIG} \
                 or {ENV_CONFIG_DIR})"
            )
        })
    }
}

/// `$XDG_CONFIG_HOME/amp-harness`, else `$HOME/.config/amp-harness` (the default user dir).
pub fn default_user_dir(env: &dyn Fn(&str) -> Option<String>) -> Option<PathBuf> {
    let non_empty = |k: &str| env(k).filter(|v| !v.is_empty());
    let base = non_empty("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .or_else(|| non_empty("HOME").map(|h| PathBuf::from(h).join(".config")))?;
    Some(base.join("amp-harness"))
}

fn absolute(path: PathBuf) -> PathBuf {
    std::path::absolute(&path).unwrap_or(path)
}

static INSTALLED: OnceLock<ConfigPaths> = OnceLock::new();

/// Makes `paths` the process's configuration paths (the binary does this once, from its flags,
/// before anything reads configuration). A second call is ignored and returns `false`.
pub fn install(paths: ConfigPaths) -> bool {
    INSTALLED.set(paths).is_ok()
}

/// The installed paths, if the binary installed any.
pub fn installed() -> Option<&'static ConfigPaths> {
    INSTALLED.get()
}

/// The paths in effect: the installed ones, else resolved from `env` (no repo layer either way).
pub fn current(env: &dyn Fn(&str) -> Option<String>) -> ConfigPaths {
    match installed() {
        Some(paths) => paths.clone(),
        None => ConfigPaths::from_env(env),
    }
}

/// [`current`] with the process environment.
pub fn current_from_process() -> ConfigPaths {
    current(&|k| std::env::var(k).ok())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn env_of(pairs: &'static [(&'static str, &'static str)]) -> impl Fn(&str) -> Option<String> {
        move |k| {
            pairs
                .iter()
                .find(|(n, _)| *n == k)
                .map(|(_, v)| (*v).to_owned())
        }
    }

    #[test]
    fn default_is_xdg_then_home() {
        let p = ConfigPaths::from_env(&env_of(&[("XDG_CONFIG_HOME", "/x"), ("HOME", "/h")]));
        assert_eq!(p.user_dir, Some(PathBuf::from("/x/amp-harness")));
        assert_eq!(
            p.user_file,
            Some(PathBuf::from("/x/amp-harness/config.toml"))
        );
        assert_eq!(p.user_file_source, Source::Default);
        let p = ConfigPaths::from_env(&env_of(&[("HOME", "/h")]));
        assert_eq!(p.user_dir, Some(PathBuf::from("/h/.config/amp-harness")));
        assert_eq!(ConfigPaths::from_env(&env_of(&[])).user_file, None);
        assert!(
            ConfigPaths::from_env(&env_of(&[]))
                .user_file_for_writing()
                .is_err()
        );
    }

    #[test]
    fn env_dir_moves_file_and_dir_env_file_moves_only_file() {
        let p = ConfigPaths::from_env(&env_of(&[(ENV_CONFIG_DIR, "/d"), ("HOME", "/h")]));
        assert_eq!(p.user_dir, Some(PathBuf::from("/d")));
        assert_eq!(p.user_file, Some(PathBuf::from("/d/config.toml")));
        assert_eq!(p.user_file_source, Source::Env(ENV_CONFIG_DIR.to_owned()));
        assert_eq!(p.explicit_user_file(), None);

        let p = ConfigPaths::from_env(&env_of(&[(ENV_CONFIG, "/f.toml"), ("HOME", "/h")]));
        assert_eq!(p.user_dir, Some(PathBuf::from("/h/.config/amp-harness")));
        assert_eq!(p.user_file, Some(PathBuf::from("/f.toml")));
        assert_eq!(p.user_file_source, Source::Env(ENV_CONFIG.to_owned()));
        assert_eq!(p.explicit_user_file(), Some(PathBuf::from("/f.toml")));
    }

    #[test]
    fn flags_beat_env_and_relative_paths_become_absolute() {
        let env = env_of(&[(ENV_CONFIG, "/env.toml"), (ENV_CONFIG_DIR, "/envdir")]);
        let p = ConfigPaths::resolve(
            Some(PathBuf::from("rel.toml")),
            Some(PathBuf::from("/flagdir")),
            &env,
        );
        assert!(p.user_file.as_ref().unwrap().is_absolute());
        assert!(p.user_file.as_ref().unwrap().ends_with("rel.toml"));
        assert_eq!(p.user_file_source, Source::Flag);
        assert_eq!(p.user_dir, Some(PathBuf::from("/flagdir")));
        // An explicit file (env) beats the file inside an explicit directory (flag).
        let p = ConfigPaths::resolve(None, Some(PathBuf::from("/flagdir")), &env);
        assert_eq!(p.user_file, Some(PathBuf::from("/env.toml")));
        assert_eq!(p.user_dir, Some(PathBuf::from("/flagdir")));
    }

    #[test]
    fn repo_layer_is_under_dot_amp_harness() {
        let p = ConfigPaths::from_env(&env_of(&[("HOME", "/h")])).with_repo(Some(Path::new("/r")));
        assert_eq!(p.repo_dir, Some(PathBuf::from("/r/.amp/harness")));
        assert_eq!(
            p.repo_file(),
            Some(PathBuf::from("/r/.amp/harness/config.toml"))
        );
        assert_eq!(p.with_repo(None).repo_file(), None);
    }
}
