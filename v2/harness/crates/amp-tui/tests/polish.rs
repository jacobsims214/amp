//! #2188 (UX review #2175, finding 13): small polish, and a guard against rpi-tui 0.8.0's
//! truecolor SGR carry bug.
//!
//! - Fenced code is hard-wrapped by `amp_tui::wrap::hard_wrap`, not rpi's `wrap_text_with_ansi`,
//!   whose `line_diff::update_sgr_context` reads `38;2;r;g;b` as `38`, `2` (dim), `r`, `g`, `b`
//!   when re-opening the style on a continuation row: in amp-light, a colour like `103;108;116`
//!   turns on dim and a bright-yellow background (103). Snapshot
//!   `tests/snapshots/codeblock-amp-light-truecolor.txt` (ESC as `␛`; regenerate with
//!   `UPDATE_SNAPSHOTS=1 cargo test -p amp-tui --test polish` and review the diff).
//! - Inline code chips have no space padding; the background marks them.
//! - Every user bubble has a blank row above and below it (`bubbles-60.txt`).
//! - ✅/❌ in system lines become the theme's ✓/✗ in the success/error colours.
//!
//! The theme is process-global, so every test here takes [`LOCK`].

use std::{
    path::PathBuf,
    sync::{Arc, Mutex, MutexGuard},
    time::Duration,
};

use amp_core::{agent::memory_session, events::ChannelEmitter};
use amp_tui::{
    AmpTheme, ColorDepth, Shell, ShellInfo, Submit, Variant, install_theme,
    transcript::{AssistantBlock, MarkdownStyle, WrapMarkdown},
    wrap::plain,
};
use rpi_ai::{
    ThinkingLevel,
    providers::faux::{FauxProvider, FauxScript, FauxStep},
};
use rpi_harness::{agent_harness::AgentHarness, types::AgentHarnessOptions};
use rpi_tui::{Component, Markdown, syntax_highlight::Highlighter, theme, visible_width};

static LOCK: Mutex<()> = Mutex::new(());

fn themed(variant: Variant, depth: ColorDepth) -> MutexGuard<'static, ()> {
    let guard = LOCK.lock().unwrap_or_else(|e| e.into_inner());
    install_theme(&AmpTheme::builtin(variant, depth));
    guard
}

/// Wider than the code surface at 60 columns, so both lines wrap; the comment is in the muted
/// colour, the code in several highlight colours.
const LONG_COMMENT: &str =
    "// a comment that is much longer than the code surface, so it has to wrap onto more rows";
const LONG_CODE: &str =
    "let total = items.iter().map(|item| item.price * item.quantity).sum::<u64>(); // sum";
const W: usize = 60;

fn code_md() -> String {
    format!("Some code:\n\n```rust\n{LONG_COMMENT}\nfn short() {{}}\n{LONG_CODE}\n```\n\nafter")
}

/// The SGR sequences in `row`.
fn sgrs(row: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut rest = row;
    while let Some(at) = rest.find("\x1b[") {
        let tail = &rest[at..];
        let end = tail[2..]
            .find(|c: char| ('@'..='~').contains(&c))
            .map_or(tail.len(), |i| i + 3);
        if tail[..end].ends_with('m') {
            out.push(tail[..end].to_owned());
        }
        rest = &tail[end..];
    }
    out
}

fn visible(lines: &[String]) -> String {
    let mut out = lines
        .iter()
        .map(|l| l.replace('\x1b', "␛").replace('\x07', "␇"))
        .collect::<Vec<_>>()
        .join("\n");
    out.push('\n');
    out
}

fn check_snapshot(name: &str, got: &str) {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/snapshots")
        .join(format!("{name}.txt"));
    if std::env::var_os("UPDATE_SNAPSHOTS").is_some() {
        std::fs::write(&path, got).unwrap();
        return;
    }
    let want = std::fs::read_to_string(&path).unwrap_or_else(|e| {
        panic!(
            "{}: {e} (run with UPDATE_SNAPSHOTS=1 to create it)",
            path.display()
        )
    });
    assert_eq!(
        got,
        want,
        "{name} differs from {} (UPDATE_SNAPSHOTS=1 to accept)",
        path.display()
    );
}

/// The light truecolor theme is where the bug showed (#2186's banner, dim #676C74 → 103 =
/// bright-yellow background). Every SGR on a code row must be one the highlighter wrote for that
/// line, the code surface, or a reset: no `2`, `103`... split out of a truecolor colour.
#[test]
fn code_blocks_carry_truecolor_whole_in_the_light_theme() {
    let _g = themed(Variant::Light, ColorDepth::TrueColor);
    let colors = theme().colors;
    let md = WrapMarkdown::new(code_md(), 1, MarkdownStyle::Plain);
    let rows = md.render(W);
    check_snapshot("codeblock-amp-light-truecolor", &visible(&rows));

    let surface = colors.md_code_block_bg.to_bg();
    let mut h = Highlighter::new("rust");
    let mut allowed: Vec<String> = vec!["\x1b[0m".into(), surface.clone()];
    for line in [LONG_COMMENT, "fn short() {}", LONG_CODE] {
        allowed.extend(sgrs(&h.highlight(line, &colors)));
    }
    let code_rows: Vec<&String> = rows.iter().filter(|r| r.contains(&surface)).collect();
    assert!(
        code_rows.len() >= 5,
        "both long lines wrap: {}",
        visible(&rows)
    );
    for row in &code_rows {
        assert!(visible_width(row) <= W, "{}", row.replace('\x1b', "␛"));
        for sgr in sgrs(row) {
            assert!(
                allowed.contains(&sgr),
                "stray {:?} in {}",
                sgr.replace('\x1b', "␛"),
                row.replace('\x1b', "␛")
            );
        }
    }
    // The wrapped rows keep every character, so the code copies whole.
    let joined: String = code_rows.iter().map(|r| plain(r)).collect::<String>();
    for word in ["quantity).sum::<u64>();", "onto more rows", "let total"] {
        assert!(joined.contains(word), "{word} in {joined}");
    }

    // rpi's own render of the same block still carries the colour as separate codes (the bug
    // this guards against); if a future rpi fixes it, this check can go.
    let raw = Markdown::new(code_md(), 1, 0).render(W);
    let stray = raw
        .iter()
        .flat_map(|r| sgrs(r))
        .any(|s| !allowed.contains(&s) && !s.contains("]8;"));
    assert!(stray, "rpi-tui no longer splits truecolor SGR on wrap");
}

/// Without wrapping, the code rows read exactly like rpi's (indent, inset, surface width).
#[test]
fn short_code_blocks_match_rpi() {
    let _g = themed(Variant::Dark, ColorDepth::TrueColor);
    let src = "```rust\nfn main() {\n\tprintln!(\"hi\");\n}\n```";
    for width in [40, 80] {
        let ours: Vec<String> = WrapMarkdown::new(src, 1, MarkdownStyle::Plain)
            .render(width)
            .iter()
            .map(|r| plain(r))
            .collect();
        let rpi: Vec<String> = Markdown::new(src, 1, 0)
            .render(width)
            .iter()
            .map(|r| plain(r))
            .collect();
        assert_eq!(ours, rpi, "width {width}");
    }
}

#[test]
fn inline_code_chips_have_no_space_padding() {
    let _g = themed(Variant::Light, ColorDepth::TrueColor);
    let colors = theme().colors;
    let md = WrapMarkdown::new(
        "Then run `amp_check_plan`: it checks the plan.",
        1,
        MarkdownStyle::Plain,
    );
    let rows = md.render(80);
    let text = plain(&rows.join("\n"));
    assert!(text.contains("run amp_check_plan: it"), "{text}");
    // The chip is still marked by its background, around the code only.
    let bg = colors.md_code_bg.to_bg();
    let fg = colors.md_code.to_fg();
    let row = rows.iter().find(|r| r.contains(&bg)).expect("a chip");
    assert!(
        row.contains(&format!("{bg}{fg}amp_check_plan\x1b[0m")),
        "{}",
        row.replace('\x1b', "␛")
    );
}

/// Every bubble sits one blank row from what comes before and after it: after the banner, after
/// a reply, after a tool call (the review saw padding on the first message only).
// The theme lock is a std Mutex held for the whole test: no other task on this runtime takes it.
#[allow(clippy::await_holding_lock)]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn user_bubbles_have_the_same_padding_every_time() {
    let guard = themed(Variant::Dark, ColorDepth::TrueColor);
    let script = FauxScript::new();
    script.set_responses(vec![
        FauxStep::text("first answer"),
        FauxStep::tool_call("ls", serde_json::json!({})),
        FauxStep::text("second answer"),
        FauxStep::text("third answer"),
    ]);
    let provider = FauxProvider::new(script);
    let model = provider.default_model().clone();
    let (emitter, mut events) = ChannelEmitter::new();
    let harness = AgentHarness::create(AgentHarnessOptions {
        model: model.clone(),
        session: memory_session(),
        models: vec![provider],
        agent_emitter: Some(emitter.into_emitter()),
        ..Default::default()
    })
    .await
    .expect("harness");
    let info = ShellInfo {
        role: "amp-manager".into(),
        provider: model.provider.to_string(),
        model_id: model.id.clone(),
        thinking: ThinkingLevel::Off,
        banner: vec!["AMP harness · manager".into()],
        ..Default::default()
    };
    let (mut shell, mut done) = Shell::new(Arc::new(harness), info, vec![]);
    for text in ["one", "two", "three"] {
        assert_eq!(shell.submit(text).await, Submit::Continue);
        shell
            .drive_until_idle(&mut events, &mut done, Duration::from_secs(20))
            .await
            .expect("idle");
    }
    let frame = shell.view().render_frame(W, 30);
    let surface = theme().colors.surface.to_bg();
    drop(guard);
    let rows: Vec<String> = frame.iter().map(|r| plain(r)).collect();
    // `▌` marks the rows on the bubble's surface.
    let marked: Vec<String> = frame
        .iter()
        .zip(&rows)
        .map(|(f, r)| {
            let mark = if f.contains(&surface) { "▌" } else { " " };
            format!("{mark}{}", r.trim_end())
        })
        .collect();
    check_snapshot("bubbles-60", &(marked.join("\n") + "\n"));

    for text in ["one", "two", "three"] {
        let at = rows
            .iter()
            .position(|r| r.trim() == text)
            .unwrap_or_else(|| panic!("{text} in {rows:#?}"));
        // The bubble: a surface row above and below the text, then a blank row outside it.
        assert!(frame[at - 1].contains("\x1b[48;"), "{text}: top padding");
        assert!(frame[at + 1].contains("\x1b[48;"), "{text}: bottom padding");
        assert!(
            rows[at - 2].trim().is_empty(),
            "{text}: gap above: {rows:#?}"
        );
        assert!(
            !frame[at - 2].contains("\x1b[48;"),
            "{text}: gap above is not surface"
        );
        assert!(
            rows[at + 2].trim().is_empty(),
            "{text}: gap below: {rows:#?}"
        );
        assert!(
            !frame[at + 2].contains("\x1b[48;"),
            "{text}: gap below is not surface"
        );
        // Never two blank rows in a row around a bubble.
        assert!(
            !rows[at - 3].trim().is_empty(),
            "{text}: one gap above: {rows:#?}"
        );
    }
}

#[test]
fn emoji_in_system_lines_become_theme_glyphs() {
    let _g = themed(Variant::Dark, ColorDepth::TrueColor);
    let colors = theme().colors;
    let view = amp_tui::ChatView::new();
    view.add_notice("✅ saved");
    view.add_error("❌ not saved");
    let block = Arc::new(AssistantBlock::default());
    block.update_error("boom", "error");
    view.add_to_transcript(block);
    let frame = view.render_frame(W, 20).join("\n");
    assert!(!frame.contains('✅') && !frame.contains('❌'), "{frame}");
    assert!(
        frame.contains(&colors.success.fg("✓")),
        "{}",
        frame.replace('\x1b', "␛")
    );
    assert!(
        frame.contains(&colors.error.fg("✗")),
        "{}",
        frame.replace('\x1b', "␛")
    );
    let text = plain(&frame);
    assert!(text.contains("✓ saved"), "{text}");
    assert!(text.contains("✗ not saved"), "{text}");
    assert!(text.contains("✗ Error: boom"), "{text}");
}

/// #2334 (QA pass 3 finding 3): notices, warnings and errors in a row have one blank row between
/// them, not two, so a few of them don't fill the screen.
#[test]
fn notices_in_a_row_have_one_blank_row_between() {
    let _g = themed(Variant::Dark, ColorDepth::TrueColor);
    let view = amp_tui::ChatView::new();
    view.add_notice("first");
    view.add_warning("second");
    view.add_error("third");
    view.add_notice("fourth");
    let rows: Vec<String> = view
        .render_frame(W, 30)
        .iter()
        .map(|r| rpi_tui::strip_ansi(r).trim().to_owned())
        .collect();
    let at = |t: &str| rows.iter().position(|r| r.contains(t)).expect(t);
    for pair in [
        ["first", "second"],
        ["second", "third"],
        ["third", "fourth"],
    ] {
        assert_eq!(at(pair[1]) - at(pair[0]), 2, "{pair:?}: {rows:#?}");
    }
}
