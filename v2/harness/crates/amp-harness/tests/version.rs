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
