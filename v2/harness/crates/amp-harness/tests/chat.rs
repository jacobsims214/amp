//! M2-7 (#2097): `amp-harness` with no subcommand opens the chat with the manager. Driven through
//! the hidden `--snapshot` mode (the same shell, no terminal) against the in-process mock AMP on
//! the faux provider: the manager is built as `run -p` builds it, calls an AMP tool natively,
//! keeps the conversation across prompts, and the screen shows the transcript and pi's footer.

mod common;

use std::path::Path;

use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::{Value, json};

const FAUX_CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]
[providers.faux.models."faux-2"]

[roles.manager]
model = "faux/faux-1"
"#;

fn setup(name: &str, script: Value) -> std::path::PathBuf {
    let dir = workdir(name, Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), FAUX_CONFIG).unwrap();
    std::fs::write(dir.join("script.json"), script.to_string()).unwrap();
    dir
}

async fn harness(dir: &Path, url: &str, args: &[&str]) -> Output {
    let script = dir.join("script.json");
    let state = dir.join("home/.local/state");
    run(
        dir,
        url,
        args,
        &[
            ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
        ],
    )
    .await
}

#[tokio::test]
async fn the_chat_talks_to_the_manager() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup(
        "chat-manager",
        json!([
            {"text": "Checking who you are.", "tool_calls": [{"name": "amp_whoami", "id": "call-1"}]},
            {"text": "You are local-dev."},
            {"text": "Still here, same conversation."}
        ]),
    );

    let out = harness(
        &dir,
        &amp.url,
        &[
            "--snapshot",
            "220x60",
            "--input",
            "who am I?",
            "--input",
            "are you still there?",
            "--input",
            "/usage",
        ],
    )
    .await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let screen = &out.stdout;
    let lines: Vec<&str> = screen.lines().collect();
    assert_eq!(lines.len(), 60, "{screen}");

    // Banner: the manager, the project from .amp.json, the AMP server, the model.
    assert!(screen.contains("AMP harness"), "{screen}");
    assert!(screen.contains("· manager"), "{screen}");
    assert!(screen.contains("project #2"), "{screen}");
    assert!(screen.contains(amp.url.as_str()), "{screen}");
    assert!(screen.contains("faux/faux-1 • thinking"), "{screen}");
    assert!(screen.contains("session "), "{screen}");

    // Transcript: both prompts, the AMP tool call, both replies.
    for text in [
        "who am I?",
        "Checking who you are.",
        "amp_whoami",
        "You are local-dev.",
        "are you still there?",
        "Still here, same conversation.",
        "usage this session: 2 prompt(s), 3 turn(s), 1 tool call(s) (0 failed)",
    ] {
        assert!(screen.contains(text), "missing {text:?} in\n{screen}");
    }
    assert_eq!(amp.mock.calls_to("amp_whoami"), vec![json!({})]);

    // pi's footer: cwd • role, then usage … model • thinking (220 columns fit the long test cwd).
    assert!(lines[58].contains("• manager"), "{}", lines[58]);
    assert!(lines[59].contains("faux-1 • "), "{}", lines[59]);

    // The session was written to the JSONL state dir, like run -p.
    let sessions = dir.join("home/.local/state/amp-harness/sessions/amp-manager");
    assert!(
        std::fs::read_dir(&sessions).unwrap().count() == 1,
        "{}",
        sessions.display()
    );
}

#[tokio::test]
async fn model_switches_and_quit_in_the_chat() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup("chat-model", json!([]));
    let out = harness(
        &dir,
        &amp.url,
        &[
            "--no-session",
            "--snapshot",
            "100x30",
            "--input",
            "/model",
            "--input",
            "/model faux/faux-2",
            "--input",
            "/quit",
            "--input",
            "never sent",
        ],
    )
    .await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let screen = &out.stdout;
    assert!(
        screen.contains("available: faux/faux-1, faux/faux-2"),
        "{screen}"
    );
    assert!(screen.contains("model: faux/faux-2"), "{screen}");
    assert!(screen.contains("session in memory"), "{screen}");
    assert!(!screen.contains("never sent"), "{screen}");
    let last = screen.lines().last().unwrap();
    assert!(last.contains("faux-2 • "), "{last}");
}

#[tokio::test]
async fn without_a_terminal_it_points_to_run_p() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup("chat-no-tty", json!([]));
    let out = harness(&dir, &amp.url, &[]).await;
    assert_eq!(out.code, 2, "stderr: {}", out.stderr);
    assert!(out.stderr.contains("needs a terminal"), "{}", out.stderr);
    assert!(out.stderr.contains("run -p"), "{}", out.stderr);
    // Nothing reached AMP: the check happens before connecting.
    assert!(amp.mock.calls_to("amp_whoami").is_empty());
}

#[tokio::test]
async fn chat_flags_do_not_mix_with_subcommands() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup("chat-flags", json!([]));
    let out = harness(&dir, &amp.url, &["--role", "reviewer", "version"]).await;
    assert_eq!(out.code, 2, "stdout: {}", out.stdout);
}

/// M3-1 (#2103): the chat installs the AMP theme before the shell is built. `[tui]` in
/// config.toml picks it, `--theme` and AMP_HARNESS_COLOR override it, and a bad value is a usage
/// error before anything reaches AMP.
#[tokio::test]
async fn the_chat_uses_the_amp_theme() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup("chat-theme", json!([]));
    let config = dir.join("home/.config/amp-harness/config.toml");
    let mut text = std::fs::read_to_string(&config).unwrap();
    text.push_str("\n[tui]\ntheme = \"light\"\ncolor = \"256\"\n");
    std::fs::write(&config, text).unwrap();
    let screen_args = [
        "--no-session",
        "--snapshot",
        "100x30",
        "--ansi",
        "--input",
        "/usage",
    ];

    // amp-light in 256 colours: its dim (242) on the banner, no 24-bit colour anywhere.
    let out = harness(&dir, &amp.url, &screen_args).await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(out.stdout.contains("\x1b[38;5;242m"), "{}", out.stdout);
    assert!(!out.stdout.contains("38;2;"), "{}", out.stdout);

    // --theme dark with truecolor from the environment: amp-dark's dim #7C8796.
    let script = dir.join("script.json");
    let mut args = vec!["--theme", "dark"];
    args.extend(screen_args);
    let out = run(
        &dir,
        &amp.url,
        &args,
        &[
            ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
            ("AMP_HARNESS_COLOR", "truecolor"),
        ],
    )
    .await;
    assert_eq!(out.code, 0, "stderr: {}", out.stderr);
    assert!(out.stdout.contains("38;2;124;135;150"), "{}", out.stdout);

    // A bad theme fails before connecting.
    let calls = amp.mock.calls_to("amp_whoami").len();
    let out = harness(&dir, &amp.url, &["--theme", "neon", "--snapshot", "100x30"]).await;
    assert_eq!(out.code, 2, "stdout: {}", out.stdout);
    assert!(
        out.stderr.contains("unknown theme `neon`"),
        "{}",
        out.stderr
    );
    assert_eq!(amp.mock.calls_to("amp_whoami").len(), calls);
}
