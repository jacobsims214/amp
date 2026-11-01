//! AMP #2882: the chat's sections in titled boxes (the layout approved in artifact #26).
//!
//! Four states, each at 80x24 and 160x40, compared with `tests/snapshots/boxes-*.txt` (plain
//! text; `|` marks the right edge):
//! - **idle**: the Conversation box (the header in its top edge) and the Message box; no
//!   Workers box and no `workers 0` on the status line;
//! - **replying**: two of this chat's workers running (the Workers box, its count and keys in the
//!   title) while the manager replies (`⠋ manager replying 0s · 2 queued · Esc stops` on the
//!   Message box);
//! - **notice**: a worker finished (`■ worker finished` in the conversation; the Workers box
//!   drops to one);
//! - **workers**: `/workers` open as one box, one row per worker, `elsewhere` as one label, the
//!   key hint in its bottom edge; it has the brighter border, the Message box steps back.
//!
//! The other tests check the themes (dark, light, `NO_COLOR`), ASCII glyphs, widths under 80 and
//! a resize. Regenerate the snapshots with `UPDATE_SNAPSHOTS=1 cargo test -p amp-tui --test boxes`
//! and review the diff.
//!
//! The theme and the glyph mode are process-global, so every test here takes [`LOCK`].

use std::{
    collections::BTreeMap,
    path::PathBuf,
    sync::{Arc, Mutex, MutexGuard},
};

use amp_core::events::UsageJson;
use amp_tui::transcript::{AssistantBlock, Block, ThinkingDisplay};
use amp_tui::{
    AmpTheme, ChatView, ColorDepth, FooterInfo, PanelKind, PanelState, Phase, Variant, WorkerEntry,
    chrome::sample, frame, install_theme, roster::NoticeKind,
};
use rpi_agent::AgentEvent;
use rpi_tui::{strip_ansi, visible_width};
use serde_json::json;

static LOCK: Mutex<()> = Mutex::new(());

fn themed(variant: Variant, depth: ColorDepth) -> MutexGuard<'static, ()> {
    let guard = LOCK.lock().unwrap_or_else(|e| e.into_inner());
    install_theme(&AmpTheme::builtin(variant, depth));
    frame::set_ascii(false);
    guard
}

fn usage(input: i64, output: i64, cost: f64) -> UsageJson {
    UsageJson {
        input_tokens: input,
        output_tokens: output,
        cache_read_tokens: 0,
        cache_write_tokens: 0,
        cost_usd: cost,
    }
}

fn running(id: i64, name: &str, agent: &str, state: &str, tok: i64) -> WorkerEntry {
    WorkerEntry {
        task_id: id,
        name: name.into(),
        epic: "AMP Harness".into(),
        story: "M4".into(),
        agent_id: format!("amp:amp-{agent}"),
        model: "openrouter/deepseek/deepseek-v4.1".into(),
        phase: Phase::Running,
        state: state.into(),
        usage: usage(tok, 0, 0.0),
        local: true,
        viewable: true,
        ..WorkerEntry::default()
    }
}

fn installer() -> WorkerEntry {
    running(
        2142,
        "Fix installer setup hand-off so the person lands in their project",
        "worker-backend",
        "running 3m12s",
        48_000,
    )
}

fn thinking() -> WorkerEntry {
    running(
        2143,
        "Thinking toggle in the transcript",
        "worker-frontend",
        "running 3m10s",
        22_400,
    )
}

fn finished() -> WorkerEntry {
    WorkerEntry {
        phase: Phase::Finished,
        outcome: Some("completed".into()),
        state: "completed after 4m02s".into(),
        usage: usage(61_300, 0, 0.09),
        ..installer()
    }
}

fn elsewhere(id: i64, name: &str, state: &str) -> WorkerEntry {
    WorkerEntry {
        task_id: id,
        name: name.into(),
        epic: "AMP Harness".into(),
        story: "M3".into(),
        agent_id: "amp:amp-worker-backend".into(),
        model: "openrouter/deepseek/deepseek-v4.1".into(),
        phase: Phase::Elsewhere,
        state: state.into(),
        ..WorkerEntry::default()
    }
}

fn set_roster(view: &ChatView, entries: Vec<WorkerEntry>) {
    let map: BTreeMap<i64, WorkerEntry> = entries.into_iter().map(|e| (e.task_id, e)).collect();
    view.roster().set(map);
}

fn reply(view: &ChatView, text: &str) {
    let assistant = Arc::new(AssistantBlock::with_thinking(ThinkingDisplay::default()));
    assistant.update_blocks(&[Block::Text(text.into())]);
    view.add_to_transcript(assistant);
}

fn tool(view: &mut ChatView, id: &str, name: &str, result: &str) {
    view.apply(&AgentEvent::ToolExecutionStart {
        tool_call_id: id.into(),
        tool_name: name.into(),
        args: json!({ "project_id": 2 }),
    });
    view.apply(&AgentEvent::ToolExecutionEnd {
        tool_call_id: id.into(),
        tool_name: name.into(),
        result: rpi_agent::AgentToolResult::text(result),
        is_error: false,
    });
}

/// The chat as the person sees it after asking what's ready: the shared base of every state.
fn base() -> ChatView {
    let mut view = ChatView::new();
    view.set_footer(&FooterInfo {
        model: "deepseek-v4.1".into(),
        thinking: "off".into(),
        context_window: 200_000,
        cwd: "~/dev/amp".into(),
        git_branch: Some("main".into()),
        session_name: Some("manager".into()),
    });
    view.chrome().set_snapshot(sample::meta(false));
    view.add_banner(&["AMP harness 0.1.0 · manager · session saved · /help for commands".into()]);
    view.add_user("what's ready in M4?");
    tool(&mut view, "t1", "amp_list_tasks", "[]");
    reply(
        &view,
        "Two tickets in AMP Harness › M4 are ready, and nothing they wait on is open: #2142 Fix \
         installer setup hand-off, and #2143 Thinking toggle in the transcript.",
    );
    view
}

fn idle() -> ChatView {
    base()
}

fn replying() -> ChatView {
    let view = base();
    set_roster(&view, vec![installer(), thinking()]);
    view.add_worker_notice(2142, NoticeKind::Started);
    view.add_worker_notice(2143, NoticeKind::Started);
    view.add_user("while they run, draft the tickets for M5 remote pairing");
    reply(
        &view,
        "M5 · Remote pairing has three acceptance criteria and no tickets yet. I'll draft one \
         ticket per criterion: pairing from the phone, the device list in /remote, and",
    );
    view.set_working(true);
    view.set_working_message("manager replying · 2 queued");
    view
}

fn notice() -> ChatView {
    let view = base();
    set_roster(
        &view,
        vec![
            finished(),
            WorkerEntry {
                state: "running 5m40s".into(),
                ..thinking()
            },
        ],
    );
    view.add_worker_notice(2143, NoticeKind::Started);
    view.add_user("while they run, draft the tickets for M5 remote pairing");
    reply(
        &view,
        "I drafted one ticket per criterion: pairing from the phone, the device list in /remote, \
         and revoking a device. They're in the plan as drafts.",
    );
    view.add_worker_notice(2142, NoticeKind::Finished);
    view
}

fn workers_open() -> ChatView {
    let view = notice();
    set_roster(
        &view,
        vec![
            finished(),
            WorkerEntry {
                state: "running 5m40s".into(),
                usage: usage(31_200, 0, 0.05),
                ..thinking()
            },
            elsewhere(2104, "M3-2 AMP metadata poller", "in progress 1h12m"),
            elsewhere(2106, "M3-3 Header and AMP status line", "in progress 48m"),
        ],
    );
    view.panel().open(PanelState::new(PanelKind::Workers));
    view
}

/// Plain rows, `|` at the right edge; the reply's elapsed seconds masked.
fn plain(view: &ChatView, width: usize, height: usize) -> Vec<String> {
    view.render_frame(width, height)
        .iter()
        .map(|l| {
            let p = strip_ansi(l);
            let w = visible_width(&p);
            assert!(w <= width, "row wider than {width}: {p:?}");
            format!("{}{}|", mask(&p), " ".repeat(width - w))
        })
        .collect()
}

/// `replying 0s` → `replying #s`, and the spinner frame → `⠿`: the render count and the clock
/// don't change the snapshot.
fn mask(row: &str) -> String {
    let mut out = String::with_capacity(row.len());
    let mut rest = row;
    while let Some(at) = rest.find("replying ") {
        out.push_str(&rest[..at + 9]);
        rest = &rest[at + 9..];
        let digits = rest.chars().take_while(|c| c.is_ascii_digit()).count();
        if digits > 0 && rest[digits..].starts_with('s') {
            out.push('#');
            rest = &rest[digits..];
        }
    }
    out.push_str(rest);
    // A tool call's duration (`· 3 ms`).
    let out = match out.find(" ms") {
        Some(end) => {
            let start = out[..end]
                .rfind(|c: char| !c.is_ascii_digit())
                .map_or(0, |i| i + 1);
            if start < end {
                format!("{}#{}", &out[..start], &out[end..])
            } else {
                out
            }
        }
        None => out,
    };
    out.chars()
        .map(|c| {
            if ('⠀'..='⣿').contains(&c) {
                '⠿'
            } else {
                c
            }
        })
        .collect()
}

fn check_snapshot(name: &str, rows: &[String]) {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/snapshots")
        .join(format!("boxes-{name}.txt"));
    let mut got = rows.join("\n");
    got.push('\n');
    if std::env::var_os("UPDATE_SNAPSHOTS").is_some() {
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
            "{name}: render differs from {} at line {}\n got: {}\nwant: {}\n(UPDATE_SNAPSHOTS=1 to accept)\n{got}",
            path.display(),
            first + 1,
            got.lines().nth(first).unwrap_or(""),
            want.lines().nth(first).unwrap_or("")
        );
    }
}

const SIZES: [(usize, usize); 2] = [(80, 24), (160, 40)];

/// Builds one state's screen.
type Build = fn() -> ChatView;

#[test]
fn snapshots_of_the_four_states() {
    let _g = themed(Variant::Dark, ColorDepth::TrueColor);
    let states: [(&str, Build); 4] = [
        ("idle", idle),
        ("replying", replying),
        ("notice", notice),
        ("workers", workers_open),
    ];
    for (name, build) in states {
        for (w, h) in SIZES {
            let view = build();
            check_snapshot(&format!("{name}-{w}x{h}"), &plain(&view, w, h));
        }
    }
}

#[test]
fn idle_has_two_boxes_and_no_workers_count() {
    let _g = themed(Variant::Dark, ColorDepth::TrueColor);
    let rows = plain(&idle(), 80, 24);
    let text = rows.join("\n");
    // The header is the Conversation box's top edge, at the screen's edge at 80 columns.
    assert!(
        rows[1].starts_with("┌─ ▀▀▀▀▀▀▀▀ AMP #2 · simstech · ada@example.com"),
        "{text}"
    );
    assert!(rows[1].contains("● connected ─┐"), "{text}");
    assert!(text.contains("┌─ Message ─"), "{text}");
    assert!(
        !text.contains("Workers"),
        "no workers run: no Workers box\n{text}"
    );
    assert!(!text.contains("workers 0"), "{text}");
    // The status line and the footer stay outside the boxes.
    let status = rows
        .iter()
        .position(|r| r.contains("QUESTIONS"))
        .expect(&text);
    assert!(rows[status - 1].starts_with("└"), "{text}");
    assert!(!rows[status].starts_with('│') && !rows[status + 1].starts_with('│'));
    // The transcript's text keeps column 2 at 80 columns (3 above).
    let user = rows
        .iter()
        .find(|r| r.contains("what's ready in M4?"))
        .expect(&text);
    assert!(user.starts_with("│"), "{user}");
    let wide = plain(&idle(), 160, 40);
    assert!(wide[1].starts_with(" ┌─ ▀▀▀▀▀▀▀▀ AMP #2"), "{}", wide[1]);
    assert!(wide[1].ends_with("─┐ |"), "{}", wide[1]);
    let banner = wide
        .iter()
        .find(|r| r.contains("AMP harness 0.1.0"))
        .unwrap();
    assert!(
        banner.starts_with(" │ AMP harness"),
        "text at column 3: {banner}"
    );
}

#[test]
fn replying_titles_the_message_box_and_counts_the_workers() {
    let _g = themed(Variant::Dark, ColorDepth::TrueColor);
    let rows = plain(&replying(), 80, 24);
    let text = rows.join("\n");
    let workers = rows
        .iter()
        .position(|r| r.contains("Workers · 2 running"))
        .expect(&text);
    assert!(
        rows[workers].contains("Ctrl+W watch · /workers all ─┐"),
        "{text}"
    );
    assert!(
        rows[workers + 1].starts_with("│ ▸ #2142 Fix installer"),
        "{text}"
    );
    assert!(
        rows[workers + 2].starts_with("│ ▸ #2143 Thinking toggle"),
        "{text}"
    );
    assert!(rows[workers + 3].starts_with("└──"), "{text}");
    let message = &rows[workers + 4];
    assert!(message.starts_with("┌─ Message ─"), "{text}");
    assert!(
        message.contains("⠿ manager replying #s · 2 queued · Esc stops ─┐"),
        "{message}"
    );
    assert!(
        !text.contains("Working…") && !text.contains("click one or Ctrl+W"),
        "{text}"
    );
    // The two start notices are not split by a blank row.
    let first = rows
        .iter()
        .position(|r| r.contains("▶ worker started  #2142"))
        .expect(&text);
    assert!(
        rows[first + 1].contains("▶ worker started  #2143"),
        "{text}"
    );
}

#[test]
fn the_box_with_the_keys_is_brighter_and_bold() {
    let _g = themed(Variant::Dark, ColorDepth::TrueColor);
    // amp-dark: muted #9AA4B2 (the box with the keys), border #5E6876 (the others).
    let bright = "\x1b[38;2;154;164;178m┌";
    let quiet = "\x1b[38;2;94;104;118m┌";
    let find = |lines: &[String], title: &str| -> String {
        lines
            .iter()
            .find(|l| strip_ansi(l).contains(title))
            .cloned()
            .unwrap_or_else(|| panic!("no {title} row"))
    };
    let lines = idle().render_frame(80, 24);
    let message = find(&lines, "┌─ Message");
    assert!(message.starts_with(bright), "{message:?}");
    assert!(
        message.contains("\x1b[1m"),
        "the focused title is bold: {message:?}"
    );
    assert!(find(&lines, "┌─ ▀▀").starts_with(quiet));

    let lines = workers_open().render_frame(80, 24);
    let workers = find(&lines, "┌─ Workers · 1 running · 1 done · 2 elsewhere");
    assert!(workers.starts_with(bright), "{workers:?}");
    let message = find(&lines, "┌─ Message");
    assert!(
        message.starts_with(quiet),
        "the Message box steps back: {message:?}"
    );
    assert!(!message.contains("\x1b[1m"), "{message:?}");
}

#[test]
fn workers_panel_is_one_box_with_one_row_per_worker() {
    let _g = themed(Variant::Dark, ColorDepth::TrueColor);
    let rows = plain(&workers_open(), 80, 24);
    let text = rows.join("\n");
    let top = rows
        .iter()
        .position(|r| r.starts_with("┌─ Workers"))
        .expect(&text);
    assert_eq!(
        rows.iter().filter(|r| r.contains("┌─ Workers")).count(),
        1,
        "the strip's box is hidden while /workers shows them\n{text}"
    );
    assert!(
        rows[top + 1].starts_with("│ › ▸ #2143 Thinking toggle"),
        "{text}"
    );
    assert!(
        rows[top + 2].starts_with("│   ■ #2142 Fix installer"),
        "{text}"
    );
    assert!(rows[top + 2].contains("completed after 4m02s"), "{text}");
    assert!(rows[top + 3].starts_with("│   elsewhere "), "{text}");
    assert!(
        rows[top + 4].starts_with("│   ■ #2104 M3-2 AMP metadata poller"),
        "{text}"
    );
    assert!(
        rows[top + 5].starts_with("│   ■ #2106 M3-3 Header"),
        "{text}"
    );
    assert!(
        rows[top + 6].starts_with(
            "└─ ↑↓ select · Enter watch · Ctrl+X stop · Ctrl+G open in AMP · Esc close ─"
        ),
        "{text}"
    );
    assert!(
        !text.contains("amp:amp-worker") && !text.contains("· elsewhere"),
        "{text}"
    );
}

#[test]
fn light_theme_uses_its_own_border_colours() {
    let _g = themed(Variant::Light, ColorDepth::TrueColor);
    let lines = workers_open().render_frame(160, 40);
    let frame = lines.join("\n");
    // amp-light: muted #50555C (keys), border #83888F.
    assert!(
        frame.contains("\x1b[38;2;80;85;92m┌"),
        "light focused border missing"
    );
    assert!(
        frame.contains("\x1b[38;2;131;136;143m┌"),
        "light border missing"
    );
    assert!(
        !frame.contains("38;2;154;164;178"),
        "dark colour leaked into light"
    );
    let dark_rows = {
        install_theme(&AmpTheme::builtin(Variant::Dark, ColorDepth::TrueColor));
        plain(&workers_open(), 160, 40)
    };
    install_theme(&AmpTheme::builtin(Variant::Light, ColorDepth::TrueColor));
    assert_eq!(
        plain(&workers_open(), 160, 40),
        dark_rows,
        "only the colours change"
    );
}

#[test]
fn no_color_draws_no_colour_but_keeps_bold_and_the_chip() {
    let _g = themed(Variant::Dark, ColorDepth::NoColor);
    for build in [idle as fn() -> ChatView, replying, workers_open] {
        let frame = build().render_frame(80, 24).join("\n");
        for colour in ["38;2;", "48;2;", "38;5;", "48;5;"] {
            assert!(
                !frame.contains(colour),
                "a colour ({colour}) under NO_COLOR"
            );
        }
        assert!(frame.contains("\x1b[1m"), "bold marks the focused box");
        assert!(
            frame.contains("\x1b[7m 2 QUESTIONS \x1b[27m"),
            "the chip is reverse video"
        );
    }
    let depth =
        amp_tui::theme::select::detect_depth(&|k: &str| (k == "NO_COLOR").then(|| "1".to_owned()));
    assert_eq!(depth, ColorDepth::NoColor);
    let depth =
        amp_tui::theme::select::detect_depth(&|k: &str| (k == "NO_COLOR").then(String::new));
    assert_ne!(depth, ColorDepth::NoColor, "an empty NO_COLOR is not set");
}

#[test]
fn ascii_mode_draws_plus_minus_bar() {
    let _g = themed(Variant::Dark, ColorDepth::TrueColor);
    frame::set_ascii(true);
    let rows = plain(&replying(), 80, 24);
    frame::set_ascii(false);
    let text = rows.join("\n");
    assert!(rows[1].starts_with("+- ▀▀▀▀▀▀▀▀ AMP #2"), "{text}");
    assert!(text.contains("+- Workers · 2 running"), "{text}");
    assert!(text.contains("| ▸ #2142"), "{text}");
    assert!(text.contains("+- Message -"), "{text}");
    // (The status line's own `│` separators are not box edges.)
    for g in ['┌', '┐', '└', '┘'] {
        assert!(!text.contains(g), "{g} in ASCII mode\n{text}");
    }
    assert!(!rows.iter().any(|r| r.starts_with('│')), "{text}");
    let env = |pairs: &'static [(&'static str, &'static str)]| {
        move |k: &str| {
            pairs
                .iter()
                .find(|(n, _)| *n == k)
                .map(|(_, v)| v.to_string())
        }
    };
    let ascii = amp_tui::theme::select::resolve_ascii;
    assert!(ascii(&env(&[("TERM", "dumb")]), None).unwrap());
    assert!(ascii(&env(&[("LANG", "C")]), None).unwrap());
    assert!(!ascii(&env(&[("LANG", "en_US.UTF-8")]), None).unwrap());
    assert!(
        !ascii(&env(&[]), None).unwrap(),
        "no locale: the box glyphs"
    );
    assert!(ascii(&env(&[("AMP_HARNESS_ASCII", "1")]), None).unwrap());
    assert!(!ascii(&env(&[("LANG", "C")]), Some("[tui]\nascii = false\n")).unwrap());
}

#[test]
fn narrow_terminals_keep_the_boxes_and_never_overflow() {
    let _g = themed(Variant::Dark, ColorDepth::TrueColor);
    for build in [idle as fn() -> ChatView, replying, notice, workers_open] {
        let view = build();
        for (w, h) in [(79, 24), (60, 20), (44, 16), (30, 12), (14, 10)] {
            let rows = plain(&view, w, h);
            assert_eq!(rows.len(), h);
            let text = rows.join("\n");
            assert!(
                rows.iter().any(|r| r.contains("Message")),
                "{w}x{h}\n{text}"
            );
        }
        // Resizing back gives the same screen as a fresh one at that size.
        let again = plain(&view, 80, 24);
        let fresh = plain(&build(), 80, 24);
        assert_eq!(again.len(), fresh.len());
        assert_eq!(again[1], fresh[1], "the header edge after a resize");
    }
}

#[test]
fn clicks_in_the_message_box_land_in_the_text() {
    let _g = themed(Variant::Dark, ColorDepth::TrueColor);
    let view = idle();
    view.editor().set_text("hello world");
    let rows = plain(&view, 80, 24);
    let row = rows
        .iter()
        .position(|r| r.starts_with("│ hello world"))
        .unwrap();
    // Column 2 is the text's first cell (border, padding), so column 8 is "w".
    assert_eq!(view.editor_hit(80, 24, row, 8), Some((0, 6)));
    assert_eq!(view.editor_hit(80, 24, row, 0), Some((0, 0)));
    let wide = plain(&view, 160, 40);
    let row = wide
        .iter()
        .position(|r| r.starts_with(" │ hello world"))
        .unwrap();
    assert_eq!(view.editor_hit(160, 40, row, 9), Some((0, 6)));
}
