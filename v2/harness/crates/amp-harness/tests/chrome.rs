//! M3-3 (#2106): `amp-harness debug render-chrome` prints the chat's AMP header and status line,
//! from the built-in sample or (`--live`) from the metadata poller against the mock AMP.

mod common;

use common::{MockAmp, run, spawn, workdir};

/// A URL nothing listens on: the sample never connects.
const NO_AMP: &str = "http://127.0.0.1:9/mcp";

#[tokio::test]
async fn render_chrome_draws_the_sample() {
    let dir = workdir("chrome-sample", None);
    let out = run(
        &dir,
        NO_AMP,
        &["debug", "render-chrome", "--width", "160", "--plain"],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    let lines: Vec<&str> = out.stdout.lines().collect();
    assert_eq!(lines.len(), 3, "{}", out.stdout);
    assert!(
        lines[0].starts_with("▀▀▀▀▀▀▀▀ amp.simstech.cloud · ada@example.com · simstech › AMP #2")
    );
    assert!(lines[0].ends_with("● connected 2026-07-28"), "{}", lines[0]);
    assert!(lines[1].starts_with("■ In progress #2106 M3-3 Header and AMP status line"));
    assert!(lines[1].ends_with("▌ AMP Harness › M3 theme + metadata"));
    assert!(lines[2].starts_with("■  2 QUESTIONS  │ workers 1 ■ running · 1 queued"));
    assert!(lines[2].ends_with("unreported #2106"), "{}", lines[2]);
    assert!(lines.iter().all(|l| l.chars().count() <= 160));

    // No ticket: the header's second line is hidden.
    let out = run(
        &dir,
        NO_AMP,
        &[
            "debug",
            "render-chrome",
            "--width",
            "80",
            "--plain",
            "--no-ticket",
        ],
        &[],
    )
    .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert_eq!(out.stdout.lines().count(), 2, "{}", out.stdout);
}

#[tokio::test]
async fn render_chrome_colours_and_links() {
    let dir = workdir("chrome-colours", None);
    let args = |color: &'static str| {
        [
            "debug",
            "render-chrome",
            "--width",
            "120",
            "--theme",
            "light",
            "--color",
            color,
        ]
    };
    let out = run(&dir, NO_AMP, &args("truecolor"), &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(out.stdout.contains("\x1b[38;2;"), "truecolor");
    assert!(
        out.stdout
            .contains("\x1b]8;;https://amp.simstech.cloud/project/2/board?task=2106\x07")
    );
    assert!(
        out.stdout
            .contains("\x1b]8;;https://amp.simstech.cloud/project/2\x07")
    );

    let out = run(&dir, NO_AMP, &args("256"), &[]).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(out.stdout.contains("\x1b[38;5;"));
    assert!(!out.stdout.contains("38;2;") && !out.stdout.contains("48;2;"));

    let out = run(
        &dir,
        NO_AMP,
        &["debug", "render-chrome", "--theme", "/no/such/theme.json"],
        &[],
    )
    .await;
    assert_eq!(out.code, 2, "a bad theme is a usage error: {}", out.stderr);
}

#[tokio::test]
async fn render_chrome_live_reads_the_poller() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir("chrome-live", Some(2));
    let out = run(
        &dir,
        &amp.url,
        &[
            "debug",
            "render-chrome",
            "--live",
            "--ticket",
            "2085",
            "--width",
            "140",
            "--plain",
        ],
        &[],
    )
    .await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let lines: Vec<&str> = out.stdout.lines().collect();
    assert_eq!(lines.len(), 3, "{}", out.stdout);
    let host = amp
        .url
        .trim_start_matches("http://")
        .trim_end_matches("/mcp");
    assert!(lines[0].contains(host), "{}", lines[0]);
    assert!(
        lines[0].contains("local-dev@example.com · AMP v2 #2"),
        "{}",
        lines[0]
    );
    assert!(lines[0].contains("● connected"), "{}", lines[0]);
    assert!(
        lines[1].starts_with("■ In progress #2085 M1-4 CLI"),
        "{}",
        lines[1]
    );
    assert!(lines[1].ends_with("▌ AMP Harness › M1 CLI"), "{}", lines[1]);
    assert!(
        lines[2].starts_with(
            // #2204: #2085 is in progress elsewhere (not a worker of this chat): the board counts
            // it, the workers count doesn't.
            "■  1 QUESTION  │ workers 0 │ ■ 1 In progress · □ 1 Ready · ■ 1 Scheduled"
        ),
        "{}",
        lines[2]
    );
    // The poller's own calls (the model is not involved).
    assert_eq!(amp.mock.calls_to("amp_whoami").len(), 1);
    assert_eq!(amp.mock.calls_to("amp_get_task").len(), 1);
    assert_eq!(amp.mock.calls_to("amp_list_tasks")[0]["format"], "compact");
}

#[tokio::test]
async fn render_chrome_live_gives_up_after_wait() {
    let dir = workdir("chrome-live-down", Some(2));
    let out = run(
        &dir,
        NO_AMP,
        &["debug", "render-chrome", "--live", "--wait", "1"],
        &[],
    )
    .await;
    assert_ne!(out.code, 0, "no AMP: {}", out.stdout);
}
