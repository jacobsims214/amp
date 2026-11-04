//! #2225: the repeat guard, end to end on the faux model against the mock AMP. QA pass 2 found a
//! manager that called `bash ls` about 30 times in a row (every call succeeded the same way) and
//! then told its user AMP was broken. Now the 3rd and 4th identical call carry a line telling the
//! model it is repeating itself, the 5th is not run and ends the run, and the person sees one
//! sentence: a stderr line from `run -p`, a line in the chat transcript. Different calls, or the
//! same tool with different arguments, are never stopped. (The worker case, an abandoned run with
//! a note saying why, is in tests/run_report.rs.)

mod common;

use std::path::Path;

use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::{Value, json};

const FAUX_CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
"#;

const SENTENCE: &str = "The model kept repeating `bash ls` and was stopped";

fn setup(name: &str, script: &Value) -> std::path::PathBuf {
    let dir = workdir(name, Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), FAUX_CONFIG).unwrap();
    std::fs::write(dir.join("script.json"), script.to_string()).unwrap();
    std::fs::write(dir.join("project/README.md"), "hello\n").unwrap();
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

fn bash(command: &str) -> Value {
    json!({"tool_calls": [{"name": "bash", "arguments": {"command": command}}]})
}

/// The QA loop: the same `bash ls`, over and over (more steps than the guard allows).
fn looping() -> Value {
    let mut steps: Vec<Value> = (0..10).map(|_| bash("ls")).collect();
    steps.push(json!({"text": "never reached"}));
    Value::Array(steps)
}

fn of_type<'a>(events: &'a [Value], ty: &str) -> Vec<&'a Value> {
    events.iter().filter(|e| e["type"] == ty).collect()
}

#[tokio::test]
async fn run_p_stops_a_repeated_call_with_one_stderr_line_and_runs_nothing_more() {
    let amp = spawn(MockAmp::new(), None).await;

    // Text mode: one line about it on stderr, exit 1.
    let dir = setup("repeat-text", &looping());
    let out = harness(&dir, &amp.url, &["run", "-p", "set up this project"]).await;
    assert_eq!(
        out.code, 1,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let lines: Vec<&str> = out
        .stderr
        .lines()
        .filter(|l| l.contains("kept repeating"))
        .collect();
    assert_eq!(lines.len(), 1, "{}", out.stderr);
    assert!(lines[0].contains(SENTENCE), "{}", lines[0]);
    // Five calls started; the 5th was blocked; nothing after it.
    let started = out
        .stderr
        .lines()
        .filter(|l| l.starts_with("[tool] bash"))
        .count();
    assert_eq!(started, 5, "{}", out.stderr);
    assert!(!out.stdout.contains("never reached"), "{}", out.stdout);

    // --json: the events say the same, and the result says why.
    let dir = setup("repeat-json", &looping());
    let out = harness(
        &dir,
        &amp.url,
        &["run", "-p", "set up this project", "--json"],
    )
    .await;
    assert_eq!(
        out.code, 1,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let events: Vec<Value> = out
        .stdout
        .lines()
        .map(|l| serde_json::from_str(l).unwrap())
        .collect();
    let ends = of_type(&events, "tool_end");
    assert_eq!(ends.len(), 5, "{ends:?}");
    let text = |i: usize| ends[i]["text"].as_str().unwrap().to_owned();
    for end in &ends[..4] {
        assert_eq!(end["is_error"], false, "{end}");
        assert!(
            end["text"].as_str().unwrap().contains("README.md"),
            "the call ran: {end}"
        );
    }
    // The 3rd and 4th tell the model; the first two don't.
    assert!(!text(1).contains("times in a row"), "{}", text(1));
    assert!(
        text(2).contains("this exact call 3 times in a row"),
        "{}",
        text(2)
    );
    assert!(text(3).contains("4 times in a row"), "{}", text(3));
    assert_eq!(ends[4]["is_error"], true);
    assert!(
        text(4)
            .starts_with("[amp-harness] Stopped a repeating model: `bash ls` was called 5 times"),
        "{}",
        text(4)
    );
    let result = of_type(&events, "result")[0];
    assert_eq!(result["outcome"], "stopped", "{result}");
    assert_eq!(
        result["error"],
        "the model kept repeating `bash ls` (5 identical calls in a row) and was stopped"
    );
    assert_eq!(
        out.stderr.lines().filter(|l| l.contains(SENTENCE)).count(),
        1,
        "{}",
        out.stderr
    );
}

#[tokio::test]
async fn different_calls_or_arguments_are_not_stopped() {
    let amp = spawn(MockAmp::new(), None).await;
    // Four `ls` (the most in a row), then `ls -a`, four `ls` again, four `amp_whoami`, a `read`
    // between two runs of four `ls`: 18 calls, none stopped.
    let mut steps: Vec<Value> = Vec::new();
    steps.extend((0..4).map(|_| bash("ls")));
    steps.push(bash("ls -a"));
    steps.extend((0..4).map(|_| bash("ls")));
    steps.extend((0..4).map(|_| json!({"tool_calls": [{"name": "amp_whoami"}]})));
    steps.extend((0..4).map(|_| bash("ls")));
    steps.push(json!({"tool_calls": [{"name": "read", "arguments": {"path": "README.md"}}]}));
    steps.push(json!({"text": "All done."}));
    let dir = setup("repeat-different", &Value::Array(steps));
    let out = harness(&dir, &amp.url, &["run", "-p", "look around", "--json"]).await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let events: Vec<Value> = out
        .stdout
        .lines()
        .map(|l| serde_json::from_str(l).unwrap())
        .collect();
    let ends = of_type(&events, "tool_end");
    assert_eq!(ends.len(), 18);
    assert!(ends.iter().all(|e| e["is_error"] == false), "{ends:?}");
    assert_eq!(amp.mock.calls_to("amp_whoami").len(), 4);
    let result = of_type(&events, "result")[0];
    assert_eq!(result["outcome"], "completed");
    assert_eq!(result["text"], "All done.");
    assert!(!out.stderr.contains("kept repeating"), "{}", out.stderr);
}

#[tokio::test]
async fn the_chat_shows_one_sentence_per_stop_and_a_new_message_starts_over() {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup("repeat-chat", &looping());
    let out = harness(
        &dir,
        &amp.url,
        &[
            "--no-session",
            "--snapshot",
            "120x40",
            "--input",
            "set up this project",
            "--input",
            "try again",
        ],
    )
    .await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let screen = out.stdout.split_whitespace().collect::<Vec<_>>().join(" ");
    // One sentence per stopped run. The person's next message starts the count again: the second
    // run makes five more calls of its own (the first is not blocked as "after the stop") and is
    // stopped the same way.
    assert_eq!(
        screen.matches("kept repeating").count(),
        2,
        "{}",
        out.stdout
    );
    assert!(
        screen.contains(
            "The model kept repeating `bash ls` and was stopped; try again, or /model to switch \
             model."
        ),
        "{}",
        out.stdout
    );
    assert!(!screen.contains("never reached"), "{}", out.stdout);
}
