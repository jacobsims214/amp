//! M3-3 (#2106): the AMP header and status line.
//!
//! Render snapshots of the sample chrome at 80 and 160 columns, in amp-dark and amp-light,
//! truecolor and 256 colours, are compared with `tests/snapshots/chrome-*.txt` (ESC shown as `␛`,
//! BEL as `␇`). Regenerate them with `UPDATE_SNAPSHOTS=1 cargo test -p amp-tui --test chrome` and
//! review the diff. The other tests check widths, links, untrusted text, stale items, run
//! reports and the chat screen's placement.
//!
//! The theme is process-global, so every test here takes [`LOCK`].

use std::{
    path::PathBuf,
    sync::{Mutex, MutexGuard},
};

use amp_core::{
    meta::{AmpMeta, Fetched},
    supervisor::WorkerPhase,
    tickets::Status,
};
use amp_tui::{
    AmpTheme, ChatView, Chrome, ChromeData, ColorDepth, ReportState, Variant,
    chrome::{
        MIN_GAP, MIN_NAME_CELLS, clean, link, plain, render_header, render_status, sample, shorten,
    },
    install_theme,
    theme::palette,
};
use rpi_agent::AgentEvent;
use rpi_tui::{theme, visible_width};
use serde_json::json;

static LOCK: Mutex<()> = Mutex::new(());

fn themed(variant: Variant, depth: ColorDepth) -> MutexGuard<'static, ()> {
    let guard = LOCK.lock().unwrap_or_else(|e| e.into_inner());
    install_theme(&AmpTheme::builtin(variant, depth));
    guard
}

fn lines(data: &ChromeData, width: usize) -> Vec<String> {
    let p = palette();
    let mut out = render_header(data, &p, width);
    out.extend(render_status(data, &p, width));
    out
}

fn visible(s: &str) -> String {
    s.replace('\x1b', "␛").replace('\x07', "␇")
}

#[test]
fn snapshots() {
    let variants = [("dark", Variant::Dark), ("light", Variant::Light)];
    let depths = [
        ("truecolor", ColorDepth::TrueColor),
        ("256", ColorDepth::Ansi256),
    ];
    let update = std::env::var_os("UPDATE_SNAPSHOTS").is_some();
    let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/snapshots");
    let mut failures = Vec::new();
    for (vname, variant) in variants {
        for (dname, depth) in depths {
            let _guard = themed(variant, depth);
            for width in [80, 160] {
                let out = lines(&sample::data(true), width);
                for l in &out {
                    assert!(visible_width(l) <= width, "{vname} {dname} {width}: {l}");
                }
                if depth == ColorDepth::Ansi256 {
                    let all = out.join("\n");
                    assert!(!all.contains("38;2;") && !all.contains("48;2;"));
                }
                let text = out
                    .iter()
                    .map(|l| visible(l))
                    .collect::<Vec<_>>()
                    .join("\n")
                    + "\n";
                let path = dir.join(format!("chrome-amp-{vname}-{dname}-{width}.txt"));
                if update {
                    std::fs::write(&path, &text).unwrap();
                } else {
                    let want = std::fs::read_to_string(&path).unwrap_or_default();
                    if want != text {
                        failures.push(format!("{}:\n{text}", path.display()));
                    }
                }
            }
        }
    }
    assert!(
        failures.is_empty(),
        "snapshots differ (UPDATE_SNAPSHOTS=1 to regenerate):\n{}",
        failures.join("\n")
    );
}

#[test]
fn plain_layout_at_80_and_160() {
    let _guard = themed(Variant::Dark, ColorDepth::TrueColor);
    let at = |w| {
        lines(&sample::data(true), w)
            .iter()
            .map(|l| plain(l))
            .collect::<Vec<_>>()
    };
    let wide = at(160);
    assert_eq!(wide.len(), 3);
    assert!(
        wide[0].starts_with("▀▀▀▀▀▀▀▀ amp.simstech.cloud · ada@example.com · simstech › AMP #2")
    );
    assert!(wide[0].ends_with("● connected 2026-07-28"));
    assert!(wide[1].ends_with("▌ AMP Harness › M3 theme + metadata"));
    assert!(wide[2].contains("│ ■ 3 In progress · □ 5 Ready · ■ 1 Scheduled"));
    assert!(wide[2].ends_with("unreported #2106"));
    // Narrow: the protocol, the story and the board counts go first; the name stays whole. The
    // plan chip (#2171: the person's next step) outranks the workers count.
    let narrow = at(80);
    assert!(narrow[0].ends_with("● connected"), "{}", narrow[0]);
    assert_eq!(
        narrow[1],
        "■ In progress #2106 M3-3 Header and AMP status line                ▌ AMP Harness"
    );
    assert_eq!(
        narrow[2],
        "■  2 QUESTIONS  │ plan: 2 waiting for your approval"
    );
    assert!(
        wide[2].contains("│ workers 3 ■ running · 1 queued │ plan: 2 waiting for your approval │")
    );
}

#[test]
fn every_width_fits() {
    let _guard = themed(Variant::Light, ColorDepth::Ansi256);
    for (data, ticket) in [
        (sample::data(true), true),
        (sample::data(false), false),
        (long_name(), true),
    ] {
        for width in 10..=220 {
            let out = lines(&data, width);
            assert_eq!(out.len(), if ticket { 3 } else { 2 });
            for l in &out {
                assert!(visible_width(l) <= width, "{width}: {}", visible(l));
            }
        }
    }
}

/// `data` shaped like the live board the #2109 review saw at 80 columns: two questions, two
/// workers running elsewhere, eight tickets awaiting plan approval, and the current ticket #67
/// unreported.
fn live_like() -> ChromeData {
    let mut meta = sample::meta(true);
    let ticket = meta.ticket.value.as_mut().unwrap();
    ticket.id = 67;
    meta.ticket_id = Some(67);
    let board = meta.board.value.as_mut().unwrap();
    board.in_progress = 2;
    board.in_progress_tickets.truncate(2);
    board.in_progress_tickets[0].id = 67;
    board.in_progress_tickets[1].id = 68;
    meta.plan.value.as_mut().unwrap().awaiting_approval = (100..108).collect();
    ChromeData::new(meta)
}

/// The blank cells before the right-aligned item that starts with `marker`, if it is drawn after
/// a left group.
fn gap_before(line: &str, marker: &str) -> Option<usize> {
    let at = line.find(marker)?;
    let left = &line[..at];
    let trimmed = left.trim_end_matches(' ');
    (!trimmed.is_empty()).then(|| left.len() - trimmed.len())
}

#[test]
fn right_items_keep_a_three_cell_gap() {
    let _guard = themed(Variant::Dark, ColorDepth::TrueColor);
    // The review's case: at 80 columns `unreported #67` sat one cell after the plan chip. With
    // a 3-cell gap it no longer fits there, so it is dropped by rank and the plan chip stays.
    let status = |w| plain(&render_status(&live_like(), &palette(), w)[0]);
    assert_eq!(
        status(80),
        "■  2 QUESTIONS  │ workers 2 ■ running │ plan: 8 waiting for your approval"
    );
    // At 90 (73 cells + the 3-cell gap + 14), it fits with exactly the minimum gap.
    let wider = status(90);
    assert!(wider.ends_with("approval   unreported #67"), "{wider}");
    assert_eq!(gap_before(&wider, "unreported #67"), Some(MIN_GAP));
    assert!(gap_before(&status(89), "unreported").is_none());

    let markers = ["●", "▌", "unreported #", "reported ✓ #", "report failed #"];
    for data in [sample::data(true), sample::data(false), live_like()] {
        for width in 10..=220 {
            for (i, l) in lines(&data, width).iter().enumerate() {
                let l = plain(l);
                for marker in markers {
                    if let Some(g) = gap_before(&l, marker) {
                        assert!(g >= MIN_GAP, "{width} line {i}: {l:?}");
                    }
                }
            }
        }
    }
}

/// The #2109 review's case: a 63-character ticket name.
fn long_name() -> ChromeData {
    let mut meta = sample::meta(true);
    meta.ticket.value.as_mut().unwrap().name =
        "Card form: inline validation for expiry date, CVC and postcode.".to_owned();
    ChromeData::new(meta)
}

/// Header line 2 as plain text, with the name's cells and the label.
fn line_two(data: &ChromeData, width: usize) -> (String, usize, Option<String>) {
    let l = plain(&render_header(data, &palette(), width)[1]);
    let (left, label) = match l.find('▌') {
        Some(at) => (l[..at].trim_end().to_owned(), Some(l[at..].to_owned())),
        None => (l.trim_end().to_owned(), None),
    };
    let name = left.split_once("#2106 ").map_or("", |(_, n)| n);
    let name = visible_width(name);
    (l, name, label)
}

#[test]
fn line_two_keeps_the_name_before_the_story() {
    let _guard = themed(Variant::Dark, ColorDepth::TrueColor);
    let data = long_name();
    // 100 columns: the epic stays and the name keeps at least 40 cells.
    let (l, name, label) = line_two(&data, 100);
    assert_eq!(label.as_deref(), Some("▌ AMP Harness"), "{l}");
    assert!(name >= 40, "{l}");
    // 80 columns: the name keeps at least 40 cells; never Epic › Story beside a cut name.
    let (l, name, label) = line_two(&data, 80);
    assert!(name >= 40, "{l}");
    assert!(label.as_deref().is_none_or(|x| x == "▌ AMP Harness"), "{l}");
    // Every width: the story only beside the whole name, the epic only beside MIN_NAME_CELLS.
    for width in 10..=220 {
        let (l, name, label) = line_two(&data, width);
        assert!(visible_width(&l) <= width, "{width}: {l}");
        match label.as_deref() {
            Some("▌ AMP Harness › M3 theme + metadata") => assert_eq!(name, 63, "{width}: {l}"),
            Some("▌ AMP Harness") => assert!(name >= MIN_NAME_CELLS, "{width}: {l}"),
            Some(other) => panic!("{width}: unexpected label {other:?}"),
            None => {}
        }
    }
    // Wide enough for everything: the whole name and Epic › Story.
    let (l, name, label) = line_two(&data, 140);
    assert_eq!(name, 63, "{l}");
    assert_eq!(
        label.as_deref(),
        Some("▌ AMP Harness › M3 theme + metadata")
    );
}

#[test]
fn links_come_from_the_url_helpers() {
    let _guard = themed(Variant::Dark, ColorDepth::TrueColor);
    let out = lines(&sample::data(true), 160);
    assert!(out[0].contains("\x1b]8;;https://amp.simstech.cloud/project/2\x07"));
    assert!(out[1].contains("\x1b]8;;https://amp.simstech.cloud/project/2/board?task=2106\x07"));
    assert!(out[2].contains("\x1b]8;;https://amp.simstech.cloud/project/2/board?task=2106\x07"));
    // Every link is closed.
    for l in &out {
        assert_eq!(
            l.matches("\x1b]8;;h").count(),
            l.matches("\x1b]8;;\x07").count()
        );
    }
    // Anything but a plain http(s) URL is not linked.
    assert_eq!(link("javascript:alert(1)", "x"), "x");
    assert_eq!(link("https://a.test/\x1b]8;;evil", "x"), "x");
    assert_eq!(link("https://a.test/a b", "x"), "x");
    assert_eq!(
        link("https://a.test/p", "x"),
        "\x1b]8;;https://a.test/p\x07x\x1b]8;;\x07"
    );
}

#[test]
fn amp_text_cannot_inject_escapes() {
    let _guard = themed(Variant::Dark, ColorDepth::TrueColor);
    let evil = "Fix \x1b]8;;https://evil.test\x07click\x1b]8;;\x07\x1b[2J\nnow\u{202E}\u{9b}31m";
    assert_eq!(clean(evil), "Fix ]8;;https://evil.testclick]8;;[2J now31m");
    let mut meta = sample::meta(true);
    if let Some(t) = meta.ticket.value.as_mut() {
        t.name = evil.to_owned();
        t.epic.as_mut().unwrap().name = "Epic\x1b[31m".to_owned();
    }
    if let Some(u) = meta.user.value.as_mut() {
        u.email = "a@b.test\x07\x1b[?1049l".to_owned();
    }
    if let Some(p) = meta.project.value.as_mut() {
        p.name = "P\r\x1b[2K".to_owned();
    }
    let out = lines(&ChromeData::new(meta), 200).join("\n");
    assert!(!out.contains("\x1b[2J") && !out.contains("\x1b[?1049l") && !out.contains("\x1b[2K"));
    assert!(!out.contains('\u{202E}') && !out.contains('\r') && !out.contains('\u{9b}'));
    // The only OSC 8 targets are the helpers' URLs.
    for target in out.split("\x1b]8;;").skip(1) {
        let url = target.split('\x07').next().unwrap();
        assert!(
            url.is_empty() || url.starts_with("https://amp.simstech.cloud/project/2"),
            "unexpected link target {url:?}"
        );
    }
    assert!(plain(&out).contains("Fix ]8;;https://evil.testclick]8;;[2J now31m"));
}

#[test]
fn shorten_counts_cells() {
    assert_eq!(shorten("abcdef", 6), "abcdef");
    assert_eq!(shorten("abcdef", 4), "abc…");
    assert_eq!(shorten("日本語テキスト", 7), "日本語…");
    assert_eq!(shorten("abc", 0), "");
}

#[test]
fn no_ticket_hides_line_two_and_no_source_hides_everything() {
    let _guard = themed(Variant::Dark, ColorDepth::TrueColor);
    let p = palette();
    assert_eq!(render_header(&sample::data(false), &p, 120).len(), 1);

    // A chat screen without a metadata source has no AMP chrome at all (M2 layout).
    let view = ChatView::new();
    assert!(view.chrome().data().is_none());
    let screen = view.render_plain(100, 12);
    assert!(
        !screen
            .iter()
            .any(|l| l.contains('▀') || l.contains("workers"))
    );

    // With one, the header is on top and the status line sits between the editor and the
    // footer (#2186: with no cwd, model or usage set, the footer has no rows).
    view.chrome().set_snapshot(sample::meta(true));
    let screen = view.render_plain(160, 20);
    assert!(screen[0].starts_with("▀▀▀▀▀▀▀▀ amp.simstech.cloud"));
    assert!(screen[1].starts_with("■ In progress #2106"));
    assert!(screen[18].starts_with("────"), "{screen:#?}");
    assert!(
        screen[19].starts_with("■  2 QUESTIONS  │ workers 3 ■ running │"),
        "{screen:#?}"
    );
}

#[test]
fn stale_items_are_dimmed_and_loading_is_shown() {
    let _guard = themed(Variant::Dark, ColorDepth::TrueColor);
    let dim = theme().colors.dim;
    let mut meta = sample::meta(true);
    meta.board.error = Some("timed out".into());
    meta.ticket.error = Some("timed out".into());
    let out = lines(&ChromeData::new(meta), 200);
    assert!(
        out[2].contains(&dim.fg("□ 5 Ready")),
        "{}",
        visible(&out[2])
    );
    assert!(
        out[1].contains(&dim.fg("■ In progress")),
        "{}",
        visible(&out[1])
    );
    assert!(plain(&out[2]).contains("■ 3 In progress"));

    let mut meta = sample::meta(true);
    meta.ticket = Fetched::default();
    meta.board = Fetched::default();
    let out: Vec<String> = lines(&ChromeData::new(meta), 120)
        .iter()
        .map(|l| plain(l))
        .collect();
    assert_eq!(out[1], "#2106 loading…");
    assert!(out[2].ends_with("│ board…"), "{}", out[2]);

    let mut meta = sample::meta(false);
    meta.board.value = Some(Default::default());
    let out = plain(&render_status(&ChromeData::new(meta), &palette(), 120)[0]);
    assert_eq!(
        out,
        "workers 0 │ plan: 2 waiting for your approval │ no open tickets"
    );

    let meta = AmpMeta::default();
    let out: Vec<String> = lines(&ChromeData::new(meta), 80)
        .iter()
        .map(|l| plain(l))
        .collect();
    assert!(
        out[0].starts_with("▀▀▀▀▀▀▀▀ connecting… · no project "),
        "{}",
        out[0]
    );
    assert!(out[0].ends_with(" ● connecting"), "{}", out[0]);
    assert_eq!(out[1], "workers 0 │ no project");
}

#[test]
fn local_workers_that_ask_show_as_questions() {
    let _guard = themed(Variant::Dark, ColorDepth::TrueColor);
    let mut data = sample::data(true);
    data.workers[1].task_id = 2107;
    data.workers[1].phase = WorkerPhase::Running;
    let status = plain(&render_status(&data, &palette(), 200)[0]);
    assert!(
        status.contains("workers 3 ■ running · 1 ■ question"),
        "{status}"
    );
    let q = palette();
    assert_eq!(q.state(Status::Question).label, "Question");
}

#[test]
fn run_reports_come_from_the_sessions_tool_calls() {
    let _guard = themed(Variant::Dark, ColorDepth::TrueColor);
    let chrome = Chrome::new();
    chrome.set_snapshot(sample::meta(true));
    let status = |c: &Chrome| plain(&render_status(&c.data().unwrap(), &palette(), 160)[0]);
    assert!(status(&chrome).ends_with("unreported #2106"));

    let before = chrome.fingerprint();
    let call = |id: &str, ticket: i64, is_error: bool| {
        chrome.observe(&AgentEvent::ToolExecutionStart {
            tool_call_id: id.into(),
            tool_name: "amp_complete_task".into(),
            args: json!({"task_id": ticket}),
        });
        chrome.observe(&AgentEvent::ToolExecutionEnd {
            tool_call_id: id.into(),
            tool_name: "amp_complete_task".into(),
            result: rpi_agent::AgentToolResult::text("{}"),
            is_error,
        });
    };
    call("c1", 2106, true);
    assert!(status(&chrome).ends_with("report failed #2106"));
    assert_ne!(chrome.fingerprint(), before);
    call("c2", 2106, false);
    assert!(status(&chrome).ends_with("reported ✓ #2106"));

    // Without a current ticket, the latest report shows.
    let other = Chrome::new();
    other.set_snapshot(sample::meta(false));
    assert!(!status(&other).contains("reported"));
    other.record_report(2200, ReportState::Reported);
    assert!(status(&other).ends_with("reported ✓ #2200"));

    // The chat view feeds its events to the chrome.
    let mut view = ChatView::new();
    view.chrome().set_snapshot(sample::meta(true));
    view.apply(&AgentEvent::ToolExecutionStart {
        tool_call_id: "r1".into(),
        tool_name: "amp_report_run".into(),
        args: json!({"task_id": "#2106", "outcome": "abandoned"}),
    });
    view.apply(&AgentEvent::ToolExecutionEnd {
        tool_call_id: "r1".into(),
        tool_name: "amp_report_run".into(),
        result: rpi_agent::AgentToolResult::text("{}"),
        is_error: false,
    });
    assert_eq!(
        view.chrome().data().unwrap().reports.get(&2106),
        Some(&ReportState::Reported)
    );
}

/// #2186: header line 1 drops the protocol, then the left side (host, email, bands, project),
/// and keeps the health dot and its word to the last.
#[test]
fn the_header_keeps_the_dot_and_connected_longest() {
    let _guard = themed(Variant::Dark, ColorDepth::TrueColor);
    let p = palette();
    let data = sample::data(false);
    for width in 11..=160 {
        let line = plain(&render_header(&data, &p, width)[0]);
        assert!(visible_width(&line) <= width, "{width}: {line}");
        assert!(line.ends_with("● connected") || line.ends_with("● connected 2026-07-28"));
        if !line.ends_with("2026-07-28") && width >= 100 {
            panic!("{width}: the protocol went with room to spare: {line}");
        }
    }
    // Before "connected" goes, everything on the left has.
    let line = plain(&render_header(&data, &p, 30)[0]);
    assert_eq!(line.trim_start(), "● connected", "{line}");
    let line = plain(&render_header(&data, &p, 50)[0]);
    assert!(line.contains("AMP #2") && !line.contains('@'), "{line}");
}
