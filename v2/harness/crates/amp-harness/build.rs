//! Stamps the resolved rpi and rmcp versions from the workspace Cargo.lock into the binary, so
//! `amp-harness --version` reports what was actually linked (D11), not a hand-copied string. It also
//! stamps the commit that last changed the embedded agent/skill assets (`AMP_ASSETS_COMMIT`), with
//! `-dirty` when the working tree has uncommitted asset changes, or `unknown` without git. And it
//! stamps the commit the binary was built from (`AMP_BUILD_COMMIT`, M5-1 #2122): the short HEAD,
//! `-dirty` when tracked files under v2/harness have uncommitted changes, `unknown` without git.
//! `AMP_HARNESS_BUILD_COMMIT` overrides it, for builds from a source tree that isn't a git checkout.

use std::{env, fs, path::Path, path::PathBuf, process::Command};

/// Crates whose resolved versions `--version` reports, and the env var each is exported as.
const PINNED: &[(&str, &str)] = &[
    ("rpi-ai", "AMP_PIN_RPI_AI"),
    ("rpi-agent", "AMP_PIN_RPI_AGENT"),
    ("rpi-tools", "AMP_PIN_RPI_TOOLS"),
    ("rpi-harness", "AMP_PIN_RPI_HARNESS"),
    ("rpi-tui", "AMP_PIN_RPI_TUI"),
    ("rmcp", "AMP_PIN_RMCP"),
];

fn main() {
    let manifest_dir = PathBuf::from(env::var("CARGO_MANIFEST_DIR").expect("CARGO_MANIFEST_DIR"));
    let lock = manifest_dir.join("../../Cargo.lock");
    println!("cargo::rerun-if-changed={}", lock.display());

    let contents = fs::read_to_string(&lock).unwrap_or_default();
    for (name, var) in PINNED {
        let version = locked_version(&contents, name).unwrap_or_else(|| "unknown".to_string());
        println!("cargo::rustc-env={var}={version}");
    }

    let assets = manifest_dir.join("../../assets");
    println!("cargo::rerun-if-changed={}", assets.display());
    // A new commit (or checkout) changes the worktree's HEAD log; rebuild the stamp then too.
    if let Some(log) = git(
        &manifest_dir,
        &[
            "rev-parse",
            "--path-format=absolute",
            "--git-path",
            "logs/HEAD",
        ],
    ) && Path::new(&log).exists()
    {
        println!("cargo::rerun-if-changed={log}");
    }
    println!(
        "cargo::rustc-env=AMP_ASSETS_COMMIT={}",
        assets_commit(&manifest_dir)
    );

    println!("cargo::rerun-if-env-changed=AMP_HARNESS_BUILD_COMMIT");
    let build_commit = env::var("AMP_HARNESS_BUILD_COMMIT")
        .ok()
        .filter(|c| !c.trim().is_empty())
        .map(|c| c.trim().to_owned())
        .unwrap_or_else(|| build_commit(&manifest_dir));
    println!("cargo::rustc-env=AMP_BUILD_COMMIT={build_commit}");
}

/// The short HEAD commit, `-dirty` when tracked files under v2/harness have local changes. Only a
/// new commit reruns this (see the HEAD log above), so a dev build can keep a stale `-dirty`; the
/// release workflow builds from a clean checkout and checks the stamp against the tag's commit.
fn build_commit(manifest_dir: &Path) -> String {
    let Some(commit) =
        git(manifest_dir, &["rev-parse", "--short", "HEAD"]).filter(|c| !c.is_empty())
    else {
        return "unknown".to_owned();
    };
    let dirty = git(
        manifest_dir,
        &[
            "status",
            "--porcelain",
            "--untracked-files=no",
            "--",
            "../..",
        ],
    )
    .is_some_and(|s| !s.is_empty());
    if dirty {
        format!("{commit}-dirty")
    } else {
        commit
    }
}

/// The short commit that last touched `v2/harness/assets`, `-dirty` when it has local changes.
fn assets_commit(manifest_dir: &Path) -> String {
    let Some(commit) = git(
        manifest_dir,
        &["log", "-1", "--format=%h", "--", "../../assets"],
    )
    .filter(|c| !c.is_empty()) else {
        return "unknown".to_owned();
    };
    let dirty = git(
        manifest_dir,
        &["status", "--porcelain", "--", "../../assets"],
    )
    .is_some_and(|s| !s.is_empty());
    if dirty {
        format!("{commit}-dirty")
    } else {
        commit
    }
}

fn git(dir: &Path, args: &[&str]) -> Option<String> {
    let out = Command::new("git")
        .arg("-C")
        .arg(dir)
        .args(args)
        .output()
        .ok()?;
    out.status
        .success()
        .then(|| String::from_utf8_lossy(&out.stdout).trim().to_owned())
}

/// Finds `version` for the `[[package]]` named `name` in a Cargo.lock. Assumes one version of each
/// pinned crate, which exact pins guarantee.
fn locked_version(lock: &str, name: &str) -> Option<String> {
    let want = format!("name = \"{name}\"");
    let mut lines = lock.lines();
    while let Some(line) = lines.next() {
        if line.trim() == want {
            let next = lines.next()?.trim();
            let v = next.strip_prefix("version = \"")?.strip_suffix('"')?;
            return Some(v.to_string());
        }
    }
    None
}
