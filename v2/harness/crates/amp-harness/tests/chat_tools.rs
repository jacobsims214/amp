//! #2183: tool calls in the chat are one line each, with a human summary and a duration in ms;
//! consecutive calls share one block; Ctrl+O shows their details. Driven through the hidden
//! `--snapshot` mode at the review's sizes (80x24 and 127x46, #2175 finding 8: three AMP calls
//! used to take about 14 rows).

mod common;

use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::{Value, json};

const FAUX_CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
"#;

const SIZES: [(usize, usize); 2] = [(80, 24), (127, 46)];

const ANSWER: &str = "One ticket waits on your answer.";

fn calls() -> [Value; 3] {
    [
        json!({"name": "amp_list_tasks", "arguments": {"project_id": 2, "format": "full"}}),
        json!({"name": "amp_get_plan", "arguments": {"project_id": 2}}),
        json!({"name": "amp_get_task", "arguments": {"task_id": 2085}}),
    ]
}

/// The three calls in one model response (parallel tool use).
fn one_turn() -> Value {
    json!([{"tool_calls": calls()}, {"text": ANSWER}])
}

/// The three calls in three model responses with no text between them.
fn three_turns() -> Value {
    let [a, b, c] = calls();
    json!([{"tool_calls": [a]}, {"tool_calls": [b]}, {"tool_calls": [c]}, {"text": ANSWER}])
}

async fn snapshot(name: &str, script: Value, size: (usize, usize), inputs: &[&str]) -> Vec<String> {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir(&format!("{name}-{}x{}", size.0, size.1), Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), FAUX_CONFIG).unwrap();
    let script_path = dir.join("script.json");
    std::fs::write(&script_path, script.to_string()).unwrap();
    let state = dir.join("home/.local/state");
    let geometry = format!("{}x{}", size.0, size.1);
    let mut args = vec!["--no-session", "--snapshot", geometry.as_str()];
    for input in inputs {
        args.extend(["--input", input]);
    }
    let vars = [
        ("AMP_HARNESS_FAUX_SCRIPT", script_path.to_str().unwrap()),
        ("XDG_STATE_HOME", state.to_str().unwrap()),
    ];
    let out: Output = run(&dir, &amp.url, &args, &vars).await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    if std::env::var_os("TOOL_SCREENS").is_some() {
        println!("{name} {geometry}\n{}", out.stdout);
    }
    let rows: Vec<String> = out.stdout.lines().map(str::to_owned).collect();
    assert_eq!(rows.len(), size.1, "{}", out.stdout);
    rows
}

/// The rows the three calls take: from the blank row above the first to the last call's line.
fn tool_block(rows: &[String]) -> &[String] {
    let first = rows
        .iter()
        .position(|r| r.starts_with(" ✓ list_tasks"))
        .unwrap_or_else(|| panic!("no list_tasks line in\n{}", rows.join("\n")));
    let last = rows
        .iter()
        .position(|r| r.starts_with(" ✓ get_task #2085"))
        .unwrap_or_else(|| panic!("no get_task line in\n{}", rows.join("\n")));
    assert!(rows[first - 1].trim().is_empty(), "{}", rows.join("\n"));
    &rows[first - 1..=last]
}

#[tokio::test]
async fn three_amp_calls_take_at_most_four_rows() {
    for (name, script) in [
        ("tools-one-turn", one_turn()),
        ("tools-three-turns", three_turns()),
    ] {
        for size in SIZES {
            let rows = snapshot(name, script.clone(), size, &["what's on the board?"]).await;
            let screen = rows.join("\n");
            let block = tool_block(&rows);
            assert!(
                block.len() <= 4,
                "{name} {size:?}: {} rows\n{screen}",
                block.len()
            );
            assert_eq!(block.len(), 4, "{screen}");
            assert!(block[2].starts_with(" ✓ get_plan · "), "{screen}");
            for line in &block[1..] {
                // One line each, with a duration in ms (the mock answers well under a second),
                // fitted to the width.
                assert!(line.ends_with(" ms"), "{line:?}\n{screen}");
                assert!(line.chars().count() <= size.0, "{line:?}");
            }
            // The summaries are sentences, not the raw keys.
            assert!(
                block[1].contains("· 7 tickets: 1 ready, 1 in progress"),
                "{screen}"
            );
            assert!(block[2].contains("· 2 epics, 1 story"), "{screen}");
            assert!(block[3].contains("· M1-4 CLI · in progress"), "{screen}");
            assert!(!screen.contains("owner_filter"), "{screen}");
            assert!(!screen.contains("AMP · list_tasks"), "{screen}");
            // The answer follows after one blank row.
            let at = rows.iter().position(|r| r.contains(ANSWER)).unwrap();
            assert!(rows[at - 1].trim().is_empty(), "{screen}");
            assert!(rows[at - 2].starts_with(" ✓ get_task #2085"), "{screen}");
        }
    }
}

#[tokio::test]
async fn ctrl_o_shows_the_calls_arguments_and_results() {
    let rows = snapshot(
        "tools-expanded",
        one_turn(),
        (127, 46),
        &["what's on the board?", "key:ctrl+o"],
    )
    .await;
    let screen = rows.join("\n");
    // The arguments, then the result as pretty JSON, behind the gutter under each call.
    assert!(screen.contains("   │ {\"task_id\":2085}"), "{screen}");
    assert!(screen.contains("   │ {"), "{screen}");
    assert!(screen.contains("│   \"task\": {"), "{screen}");
    assert!(screen.contains("more lines"), "{screen}");

    // Pressed twice, it collapses again.
    let rows = snapshot(
        "tools-collapsed",
        one_turn(),
        (127, 46),
        &["what's on the board?", "key:ctrl+o", "key:ctrl+o"],
    )
    .await;
    assert_eq!(tool_block(&rows).len(), 4, "{}", rows.join("\n"));
}
