//! `amp-harness --version` must report the harness version and the exact rpi/rmcp pins from the
//! workspace Cargo.toml. This also catches a Cargo.lock that drifted from the pins.

use std::{fs, path::Path, process::Command};

const PINNED: &[&str] = &[
    "rpi-ai",
    "rpi-agent",
    "rpi-tools",
    "rpi-harness",
    "rpi-tui",
    "rmcp",
];

/// Reads the `=x.y.z` pin for `name` from `[workspace.dependencies]`.
fn workspace_pin(manifest: &str, name: &str) -> String {
    let line = manifest
        .lines()
        .find(|l| l.trim_start().starts_with(&format!("{name} =")))
        .unwrap_or_else(|| panic!("{name} missing from [workspace.dependencies]"));
    let start = line
        .find("\"=")
        .unwrap_or_else(|| panic!("{name} is not an exact pin: {line}"))
        + 2;
    let rest = &line[start..];
    rest[..rest.find('"').expect("closing quote")].to_string()
}

fn run(args: &[&str]) -> String {
    let out = Command::new(env!("CARGO_BIN_EXE_amp-harness"))
        .args(args)
        .output()
        .expect("run amp-harness");
    assert!(out.status.success(), "amp-harness {args:?} failed: {out:?}");
    String::from_utf8(out.stdout).expect("utf-8 stdout")
}

#[test]
fn version_prints_harness_version_and_exact_pins() {
    let manifest =
        fs::read_to_string(Path::new(env!("CARGO_MANIFEST_DIR")).join("../../Cargo.toml"))
            .expect("read workspace Cargo.toml");

    for args in [&["--version"][..], &["version"][..]] {
        let out = run(args);
        assert!(
            out.starts_with(&format!("amp-harness {}\n", env!("CARGO_PKG_VERSION"))),
            "{args:?}: {out}"
        );
        for name in PINNED {
            let pin = workspace_pin(&manifest, name);
            assert!(
                out.contains(&format!("\n{name} {pin}\n")),
                "{args:?}: expected `{name} {pin}` in:\n{out}"
            );
        }
    }
}

/// #2122: `--version` ends with the commit the binary was built from, which the release workflow
/// checks against the tagged commit. Here it must be a prefix of this checkout's HEAD (`-dirty`
/// allowed), unless AMP_HARNESS_BUILD_COMMIT overrode it or there is no git.
#[test]
fn version_stamps_the_build_commit() {
    let out = run(&["--version"]);
    let stamp = out
        .lines()
        .last()
        .and_then(|l| l.strip_prefix("commit "))
        .unwrap_or_else(|| panic!("no `commit` line last in:\n{out}"));
    assert!(!stamp.is_empty(), "{out}");
    if std::env::var_os("AMP_HARNESS_BUILD_COMMIT").is_some() || stamp == "unknown" {
        return;
    }
    let head = Command::new("git")
        .args(["-C", env!("CARGO_MANIFEST_DIR"), "rev-parse", "HEAD"])
        .output()
        .expect("git rev-parse HEAD");
    let head = String::from_utf8(head.stdout)
        .expect("utf-8")
        .trim()
        .to_owned();
    let sha = stamp.strip_suffix("-dirty").unwrap_or(stamp);
    assert!(
        sha.len() >= 7 && head.starts_with(sha),
        "stamp {stamp} is not HEAD {head}"
    );
}
