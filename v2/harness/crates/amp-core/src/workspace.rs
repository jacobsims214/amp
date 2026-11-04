//! Where workers work (#2364): `[workers] isolation` and what git offers the manager's folder.
//!
//! Git is a harness decision, not only a skill: the supervisor gives every worker that edits
//! files a place of its own, so parallel workers never write over each other. Which place
//! depends on [`Isolation`] and on the folder's [`Repo`]:
//!
//! | isolation | usable git repo (a commit, tracks this folder) | anything else |
//! |---|---|---|
//! | `auto` (default) | git worktree `.worktrees/<ticket>` on branch `amp-<ticket>` | own folder `amp-work/<ticket>/` |
//! | `worktree` | git worktree | refused before dispatch: says how to fix |
//! | `folder` | own folder `amp-work/<ticket>/` | own folder |
//! | `shared` | the manager's folder; editing workers take turns | the same |
//!
//! Read-only roles (researcher, reviewers) always work in the manager's folder. Only a worker in
//! a git worktree is told to commit; every other worker is told not to commit, branch, push or
//! open a pull request, and run reporting sends no git fields for it.

use std::{
    path::{Path, PathBuf},
    process::Command,
    str::FromStr,
};

use serde::Serialize;

/// `AMP_HARNESS_WORKER_ISOLATION`: overrides `[workers] isolation`.
pub const ENV_ISOLATION: &str = "AMP_HARNESS_WORKER_ISOLATION";
/// The folder (inside the manager's folder) that holds each worker's own folder outside git.
pub const WORK_DIR: &str = "amp-work";

/// `[workers] isolation`: how workers that edit files are kept apart.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Isolation {
    /// A git worktree when the folder is a usable git repository, else a folder per ticket.
    #[default]
    Auto,
    /// Git worktrees only: a folder without a usable repository is an error before dispatch.
    Worktree,
    /// A folder per ticket (`amp-work/<ticket>/`), even in a git repository.
    Folder,
    /// Everyone in the manager's folder; workers that edit files run one at a time.
    Shared,
}

impl Isolation {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Auto => "auto",
            Self::Worktree => "worktree",
            Self::Folder => "folder",
            Self::Shared => "shared",
        }
    }
}

impl FromStr for Isolation {
    type Err = String;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s.trim().to_ascii_lowercase().as_str() {
            "auto" => Ok(Self::Auto),
            "worktree" => Ok(Self::Worktree),
            "folder" => Ok(Self::Folder),
            "shared" => Ok(Self::Shared),
            other => Err(format!(
                "isolation must be auto, worktree, folder or shared, not \"{other}\""
            )),
        }
    }
}

/// What git offers a folder.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Repo {
    /// A repository with a commit that tracks this folder: worktrees work.
    Usable { top: PathBuf },
    /// No git repository.
    NotGit,
    /// Inside the repository `top`, which tracks none of this folder's files (a new folder under
    /// a linked one, #2356): a worktree of `top` would not contain it.
    Untracked { top: PathBuf },
    /// The repository `top` has no commit yet: nothing to branch from.
    NoCommit { top: PathBuf },
}

impl Repo {
    /// Asks git about `cwd` (a few quick `git` calls).
    pub fn probe(cwd: &Path) -> Self {
        let Some(top) = git(cwd, &["rev-parse", "--show-toplevel"]).map(PathBuf::from) else {
            return Self::NotGit;
        };
        if !same_path(&top, cwd) && git(cwd, &["ls-files", "--", "."]).is_some_and(|o| o.is_empty())
        {
            return Self::Untracked { top };
        }
        if git(&top, &["rev-parse", "--verify", "--quiet", "HEAD"]).is_none() {
            return Self::NoCommit { top };
        }
        Self::Usable { top }
    }

    /// The folder is in some git repository (usable or not).
    pub fn is_git(&self) -> bool {
        !matches!(self, Self::NotGit)
    }

    /// Why workers can't get worktrees here (`None` when they can).
    pub fn why_not(&self, cwd: &Path) -> Option<String> {
        match self {
            Self::Usable { .. } => None,
            Self::NotGit => Some(format!("{} is not in a git repository", cwd.display())),
            Self::Untracked { top } => Some(format!(
                "{} is inside the git repository {} but none of its files are in it, so a \
                 worktree of that repository would not contain this folder",
                cwd.display(),
                top.display()
            )),
            Self::NoCommit { top } => Some(format!(
                "the git repository {} has no commit yet, so there is nothing to branch from",
                top.display()
            )),
        }
    }

    /// What gives workers worktrees here.
    pub fn fix(&self) -> &'static str {
        match self {
            Self::NoCommit { .. } => "commit once",
            _ => "run `git init` here and commit once",
        }
    }
}

/// Where a worker that edits files works.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Placement {
    /// Its own git worktree and branch.
    Worktree,
    /// Its own folder `amp-work/<ticket>/` inside the manager's folder.
    Folder,
    /// The manager's folder, one editing worker at a time.
    Shared,
}

impl Placement {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Worktree => "worktree",
            Self::Folder => "folder",
            Self::Shared => "shared",
        }
    }
}

/// The placement `isolation` gives an editing worker in a folder whose repository is `repo`.
/// `has_base`: the call names a base to branch from (a repository with no commit can still get
/// a worktree off another ref). `Err` (only for `worktree`) says why and how to fix it.
pub fn placement(
    isolation: Isolation,
    repo: &Repo,
    cwd: &Path,
    has_base: bool,
) -> Result<Placement, String> {
    let usable = match repo {
        Repo::Usable { .. } => true,
        Repo::NoCommit { .. } => has_base,
        _ => false,
    };
    match isolation {
        Isolation::Auto if usable => Ok(Placement::Worktree),
        Isolation::Auto => Ok(Placement::Folder),
        Isolation::Worktree if usable => Ok(Placement::Worktree),
        Isolation::Worktree => Err(format!(
            "[workers] isolation = \"worktree\" needs a git repository with a commit, and {}. \
             To fix it, {}, or set isolation = \"auto\" (each worker then gets its own folder \
             {WORK_DIR}/<ticket>/ here)",
            repo.why_not(cwd).unwrap_or_default(),
            repo.fix()
        )),
        Isolation::Folder => Ok(Placement::Folder),
        Isolation::Shared => Ok(Placement::Shared),
    }
}

/// One line on where workers work in `cwd`: the doctor's `workers` check, the chat banner (when
/// not git worktrees) and the manager's `<amp_context>`.
pub fn summary(isolation: Isolation, cwd: &Path, repo: &Repo) -> String {
    match placement(isolation, repo, cwd, false) {
        Ok(Placement::Worktree) => {
            let top = match repo {
                Repo::Usable { top } | Repo::NoCommit { top } | Repo::Untracked { top } => {
                    top.display().to_string()
                }
                Repo::NotGit => cwd.display().to_string(),
            };
            format!(
                "git: each worker that edits files gets its own worktree {top}/.worktrees/<ticket> \
                 on branch amp-<ticket>"
            )
        }
        Ok(Placement::Folder) => {
            let lead = match (isolation, repo.why_not(cwd)) {
                (Isolation::Folder, _) => "isolation = folder".to_owned(),
                (_, Some(why)) => why,
                (_, None) => String::new(),
            };
            let fix = if isolation == Isolation::Auto {
                format!("; {} to give them git branches", repo.fix())
            } else {
                String::new()
            };
            format!(
                "{lead}: each worker that edits files gets its own folder {WORK_DIR}/<ticket>/ \
                 and nothing is committed{fix}"
            )
        }
        Ok(Placement::Shared) => "isolation = shared: workers work in this folder, those that \
             edit files one at a time, and nothing is committed"
            .to_owned(),
        Err(e) => e,
    }
}

/// The short form for the chat banner, or `None` when workers get git worktrees (as before).
pub fn banner_line(isolation: Isolation, cwd: &Path, repo: &Repo) -> Option<String> {
    match placement(isolation, repo, cwd, false) {
        Ok(Placement::Worktree) => None,
        Ok(Placement::Folder) => Some(match (isolation, repo) {
            (Isolation::Auto, Repo::NotGit) => format!(
                "not a git repository: workers each get a folder {WORK_DIR}/<ticket>/, no branches or commits"
            ),
            (Isolation::Auto, _) => format!(
                "no git branches for workers here ({}): each gets a folder {WORK_DIR}/<ticket>/",
                match repo {
                    Repo::NoCommit { .. } => "no commit yet",
                    _ => "folder not tracked by its repository",
                }
            ),
            _ => format!("workers each get a folder {WORK_DIR}/<ticket>/ (isolation = folder)"),
        }),
        Ok(Placement::Shared) => Some(
            "workers share this folder, editing workers one at a time (isolation = shared)"
                .to_owned(),
        ),
        Err(_) => Some(
            "not usable for git worktrees and isolation = worktree: workers will be refused (amp-harness doctor)"
                .to_owned(),
        ),
    }
}

/// The manager's `<amp_context>` line: where its workers work, and (outside git worktrees) not
/// to plan git steps.
pub fn manager_context(isolation: Isolation, cwd: &Path, repo: &Repo) -> String {
    let summary = summary(isolation, cwd, repo);
    match placement(isolation, repo, cwd, false) {
        Ok(Placement::Worktree) => format!("workers_workspace: {summary}"),
        _ => format!(
            "workers_workspace: {summary}. Workers here don't commit, branch, push or open pull \
             requests, so don't plan those steps or review-and-merge tickets for this work; a \
             worker's deliverables are the files it lists in its completion comment."
        ),
    }
}

fn git(dir: &Path, args: &[&str]) -> Option<String> {
    let out = Command::new("git")
        .arg("-C")
        .arg(dir)
        .args(args)
        .env("GIT_TERMINAL_PROMPT", "0")
        .stdin(std::process::Stdio::null())
        .output()
        .ok()?;
    out.status
        .success()
        .then(|| String::from_utf8_lossy(&out.stdout).trim().to_owned())
}

fn same_path(a: &Path, b: &Path) -> bool {
    match (std::fs::canonicalize(a), std::fs::canonicalize(b)) {
        (Ok(a), Ok(b)) => a == b,
        _ => a == b,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn isolation_parses_its_four_words_and_names_them_on_error() {
        for (s, want) in [
            ("auto", Isolation::Auto),
            (" Worktree ", Isolation::Worktree),
            ("folder", Isolation::Folder),
            ("SHARED", Isolation::Shared),
        ] {
            assert_eq!(s.parse::<Isolation>().unwrap(), want);
            assert_eq!(want.as_str().parse::<Isolation>().unwrap(), want);
        }
        let e = "none".parse::<Isolation>().unwrap_err();
        assert!(e.contains("auto, worktree, folder or shared"), "{e}");
    }

    #[test]
    fn placement_follows_the_table() {
        let cwd = Path::new("/w");
        let usable = Repo::Usable { top: "/w".into() };
        let none = Repo::NotGit;
        let fresh = Repo::NoCommit { top: "/w".into() };
        let untracked = Repo::Untracked { top: "/".into() };
        use Isolation::*;
        assert_eq!(
            placement(Auto, &usable, cwd, false),
            Ok(Placement::Worktree)
        );
        assert_eq!(placement(Auto, &none, cwd, false), Ok(Placement::Folder));
        assert_eq!(
            placement(Auto, &untracked, cwd, false),
            Ok(Placement::Folder)
        );
        assert_eq!(placement(Auto, &fresh, cwd, false), Ok(Placement::Folder));
        assert_eq!(placement(Auto, &fresh, cwd, true), Ok(Placement::Worktree));
        assert_eq!(
            placement(Folder, &usable, cwd, false),
            Ok(Placement::Folder)
        );
        assert_eq!(
            placement(Shared, &usable, cwd, false),
            Ok(Placement::Shared)
        );
        assert_eq!(
            placement(Worktree, &usable, cwd, false),
            Ok(Placement::Worktree)
        );
        let e = placement(Worktree, &none, cwd, false).unwrap_err();
        assert!(e.contains("/w is not in a git repository"), "{e}");
        assert!(e.contains("git init"), "{e}");
        assert!(e.contains("isolation = \"auto\""), "{e}");
    }

    #[test]
    fn outside_git_the_lines_say_folders_and_no_commits() {
        let cwd = Path::new("/biz");
        let s = summary(Isolation::Auto, cwd, &Repo::NotGit);
        assert!(s.starts_with("/biz is not in a git repository: "), "{s}");
        assert!(s.contains("amp-work/<ticket>/"), "{s}");
        assert!(s.contains("nothing is committed"), "{s}");
        assert!(s.contains("git init"), "{s}");
        let b = banner_line(Isolation::Auto, cwd, &Repo::NotGit).unwrap();
        assert!(b.starts_with("not a git repository"), "{b}");
        let m = manager_context(Isolation::Auto, cwd, &Repo::NotGit);
        assert!(m.starts_with("workers_workspace: "), "{m}");
        assert!(m.contains("don't plan those steps"), "{m}");

        let usable = Repo::Usable {
            top: "/repo".into(),
        };
        assert_eq!(
            banner_line(Isolation::Auto, Path::new("/repo"), &usable),
            None
        );
        let m = manager_context(Isolation::Auto, Path::new("/repo"), &usable);
        assert!(m.contains("/repo/.worktrees/<ticket>"), "{m}");
        assert!(!m.contains("don't plan"), "{m}");
        let s = summary(Isolation::Shared, Path::new("/repo"), &usable);
        assert!(s.contains("one at a time"), "{s}");
    }
}
