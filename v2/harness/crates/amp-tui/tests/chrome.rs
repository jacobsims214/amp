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
    chrome::{clean, link, plain, render_header, render_status, sample, shorten},
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
    // Narrow: the protocol, the story and the board counts go first; the name stays whole.
    let narrow = at(80);
    assert!(narrow[0].ends_with("● connected"), "{}", narrow[0]);
    assert_eq!(
        narrow[1],
        "■ In progress #2106 M3-3 Header and AMP status line                ▌ AMP Harness"
    );
    assert_eq!(
        narrow[2],
        "■  2 QUESTIONS  │ workers 3 ■ running · 1 queued │ plan: 2 awaiting approval"
    );
}

#[test]
fn every_width_fits() {
    let _guard = themed(Variant::Light, ColorDepth::Ansi256);
    for ticket in [true, false] {
        let data = sample::data(ticket);
        for width in 10..=220 {
            let out = lines(&data, width);
            assert_eq!(out.len(), if ticket { 3 } else { 2 });
            for l in &out {
                assert!(visible_width(l) <= width, "{width}: {}", visible(l));
            }
        }
    }
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

    // With one, the header is on top and the status line sits between the editor and pi's
    // footer (whose cwd line is empty here, so it is one line).
    view.chrome().set_snapshot(sample::meta(true));
    let screen = view.render_plain(160, 20);
    assert!(screen[0].starts_with("▀▀▀▀▀▀▀▀ amp.simstech.cloud"));
    assert!(screen[1].starts_with("■ In progress #2106"));
    assert!(screen[17].starts_with("────"), "{screen:#?}");
    assert!(
        screen[18].starts_with("■  2 QUESTIONS  │ workers 3 ■ running │"),
        "{screen:#?}"
    );
    assert!(screen[19].starts_with("Enter send"), "{screen:#?}");
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
        "workers 0 │ plan: 2 awaiting approval │ no open tickets"
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
