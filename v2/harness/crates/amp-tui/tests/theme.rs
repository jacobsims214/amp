//! M3-1 (#2103): render snapshots of the chat screen in the AMP themes.
//!
//! The same screen (banner, a user message, an assistant message with markdown, a running tool,
//! a notice and an error, plus a palette swatch of the ticket states, an epic and the brand bands)
//! is rendered with ANSI styling in amp-dark and amp-light, truecolor and 256 colours, and
//! compared with `tests/snapshots/theme-*.txt` (ESC shown as `␛`). Regenerate them with
//! `UPDATE_SNAPSHOTS=1 cargo test -p amp-tui --test theme` and review the diff.
//!
//! The theme is process-global, so every test here takes [`LOCK`].

use std::{path::PathBuf, sync::Arc, sync::Mutex};

use amp_core::tickets::Status;
use amp_tui::{AmpTheme, ChatView, ColorDepth, FooterInfo, Variant, install_theme, theme::palette};
use rpi_agent::AgentEvent;
use rpi_tui::{AssistantBlock, AssistantMessageComponent, Text, theme};
use serde_json::json;

static LOCK: Mutex<()> = Mutex::new(());

const W: usize = 80;
const H: usize = 30;

fn screen() -> ChatView {
    let mut view = ChatView::new();
    view.set_footer(&FooterInfo {
        model: "claude-opus-5-5".into(),
        thinking: "high".into(),
        context_window: 200_000,
        cwd: "/work/amp".into(),
        git_branch: Some("harness-m3".into()),
        session_name: Some("#2103".into()),
    });

    let p = palette();
    let states: Vec<String> = Status::ALL.iter().map(|s| p.state_mark(*s)).collect();
    let swatch = format!(
        "{}\n{}\n{} · {}",
        p.brand_bands(2),
        states.join("  "),
        p.epic_label(0, "AMP Harness"),
        p.epic_label(1, "Theme")
    );
    view.header_slot()
        .add_child(Arc::new(Text::new(swatch, 1, 0)));

    view.add_banner(&[
        "AMP harness 0.1.0 · manager".into(),
        "project #2 AMP · http://amp.test/mcp".into(),
    ]);
    view.add_user("Theme check: show me everything.");
    let assistant = Arc::new(AssistantMessageComponent::default());
    assistant.update_blocks(&[AssistantBlock::Text(
        "## Heading\nSome `inline code` and a [link](https://amp.simstech.cloud).\n\n- a bullet\n> a quote"
            .into(),
    )]);
    view.header_slot().add_child(assistant);
    view.apply(&AgentEvent::ToolExecutionStart {
        tool_call_id: "t1".into(),
        tool_name: "amp_get_task".into(),
        args: json!({ "task_id": 2103 }),
    });
    view.add_notice("a notice");
    view.add_error("an error");
    view
}

fn render(variant: Variant, depth: ColorDepth) -> (AmpTheme, Vec<String>) {
    let t = AmpTheme::builtin(variant, depth);
    install_theme(&t);
    let lines = screen().render_frame(W, H);
    (t, lines)
}

/// ESC as `␛`, and the running tool's elapsed time (`0.0s`) as `#.#s`, so a slow run still
/// matches.
fn visible(lines: &[String]) -> String {
    let mut out = lines
        .iter()
        .map(|l| mask_elapsed(&l.replace('\x1b', "␛")))
        .collect::<Vec<_>>()
        .join("\n");
    out.push('\n');
    out
}

/// `m0.0s␛` (a styled duration) → `m#.#s␛`.
fn mask_elapsed(line: &str) -> String {
    let chars: Vec<char> = line.chars().collect();
    let mut out = String::with_capacity(line.len());
    let mut i = 0;
    while i < chars.len() {
        if chars[i].is_ascii_digit() && i > 0 && chars[i - 1] == 'm' {
            let mut j = i;
            while j < chars.len() && (chars[j].is_ascii_digit() || chars[j] == '.') {
                j += 1;
            }
            let run: String = chars[i..j].iter().collect();
            if run.contains('.') && chars.get(j) == Some(&'s') && chars.get(j + 1) == Some(&'␛') {
                out.push_str("#.#");
                i = j;
                continue;
            }
        }
        out.push(chars[i]);
        i += 1;
    }
    out
}

fn check_snapshot(name: &str, lines: &[String]) {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/snapshots")
        .join(format!("theme-{name}.txt"));
    let got = visible(lines);
    if std::env::var_os("UPDATE_SNAPSHOTS").is_some() {
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(&path, &got).unwrap();
        return;
    }
    let want = std::fs::read_to_string(&path).unwrap_or_else(|e| {
        panic!(
            "{}: {e} (run with UPDATE_SNAPSHOTS=1 to create it)",
            path.display()
        )
    });
    if got != want {
        let first = got
            .lines()
            .zip(want.lines())
            .position(|(a, b)| a != b)
            .unwrap_or(0);
        panic!(
            "{name}: render differs from {} at line {}\n got: {}\nwant: {}\n(UPDATE_SNAPSHOTS=1 to accept)",
            path.display(),
            first + 1,
            got.lines().nth(first).unwrap_or(""),
            want.lines().nth(first).unwrap_or("")
        );
    }
}

#[test]
fn snapshots() {
    let _g = LOCK.lock().unwrap_or_else(|e| e.into_inner());
    for (variant, depth, name) in [
        (Variant::Dark, ColorDepth::TrueColor, "amp-dark-truecolor"),
        (Variant::Dark, ColorDepth::Ansi256, "amp-dark-256"),
        (Variant::Light, ColorDepth::TrueColor, "amp-light-truecolor"),
        (Variant::Light, ColorDepth::Ansi256, "amp-light-256"),
    ] {
        let (_, lines) = render(variant, depth);
        check_snapshot(name, &lines);
    }
}

#[test]
fn themes_apply_to_rpi_components() {
    let _g = LOCK.lock().unwrap_or_else(|e| e.into_inner());

    let (dark, lines) = render(Variant::Dark, ColorDepth::TrueColor);
    assert_eq!(theme().colors.text, dark.theme.colors.text);
    let frame = lines.join("\n");
    // amp-dark error (danger-ink #F0876D), dim/label (#7C8796), user-message surface (#161D26).
    assert!(frame.contains("38;2;240;135;109"), "error colour missing");
    assert!(frame.contains("38;2;124;135;150"), "dim colour missing");
    assert!(frame.contains("48;2;22;29;38"), "surface colour missing");
    // The question chip: white on AMP red.
    assert!(frame.contains("48;2;207;68;38"), "flag chip missing");
    assert!(frame.contains("QUESTION"));
    let plain_dark: Vec<String> = lines.iter().map(|l| rpi_tui::strip_ansi(l)).collect();

    let (_, lines) = render(Variant::Light, ColorDepth::TrueColor);
    let frame = lines.join("\n");
    assert!(
        frame.contains("38;2;178;58;32"),
        "light error colour missing"
    );
    assert!(frame.contains("48;2;239;239;236"), "light surface missing");
    assert!(
        !frame.contains("38;2;240;135;109"),
        "dark colour leaked into light"
    );
    // Only the colours change, not the text.
    let plain_light: Vec<String> = lines.iter().map(|l| rpi_tui::strip_ansi(l)).collect();
    assert_eq!(plain_dark, plain_light);
}

#[test]
fn fallback_256_renders_without_truecolor() {
    let _g = LOCK.lock().unwrap_or_else(|e| e.into_inner());
    for variant in [Variant::Dark, Variant::Light] {
        let (t, lines) = render(variant, ColorDepth::Ansi256);
        let frame = lines.join("\n");
        assert!(
            !frame.contains("38;2;") && !frame.contains("48;2;"),
            "{}: a 24-bit colour in 256 mode",
            t.name
        );
        assert!(frame.contains("38;5;"), "{}: no 256 colours at all", t.name);
    }
}
