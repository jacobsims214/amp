//! Stamps the resolved rpi and rmcp versions from the workspace Cargo.lock into the binary, so
//! `amp-harness --version` reports what was actually linked (D11), not a hand-copied string.

use std::{env, fs, path::PathBuf};

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
