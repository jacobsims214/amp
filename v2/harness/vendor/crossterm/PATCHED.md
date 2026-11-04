# crossterm 0.27.0, patched for amp-harness

This is crossterm 0.27.0 exactly as published on crates.io (MIT, see LICENSE), used through
`[patch.crates-io]` in `v2/harness/Cargo.toml`, with one change (#2349):

- `src/event/sys/unix/parse.rs`, `parse_csi_special_key_code`: xterm's modifyOtherKeys encoding
  `CSI 27 ; modifiers ; codepoint ~` is decoded like `CSI codepoint ; modifiers u`. Upstream
  fails to parse it and drops the key silently, so Shift+Enter (`CSI 27;2;13~`) in xterm with
  modifyOtherKeys, or tmux with `extended-keys`, did nothing in the chat.
  Test: `test_parse_csi_27_modify_other_keys`.

Trimmed from the published crate: examples, docs, CHANGELOG and the dev-dependencies (they are
not built as a path dependency outside the workspace).

When rpi-tui moves to another crossterm, drop this directory and the `[patch.crates-io]` entry
(cargo warns that the patch is unused), and check whether that crossterm parses `CSI 27 … ~`.
- Cargo.toml: `[lints.rust] warnings = "allow"` so upstream's own warnings (cfg names, unused) stay quiet in our builds.
