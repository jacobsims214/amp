//! #2176: transcript text word-wraps in the chat. Driven through the hidden `--snapshot` mode
//! at 80x24 and 127x46 (the sizes of the real-terminal review, #2175) with the review's texts:
//! a long answer with a list, thinking, thinking with a numbered list, `/help`, and a URL longer
//! than the row. No word is split across rows, no continuation row starts with a stray space,
//! and a token wider than the row still breaks.

mod common;

use std::collections::HashSet;

use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::{Value, json};

const FAUX_CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
"#;

/// The answer from the review's 80x24 capture (13-long-80x24-t3).
const ANSWER: &str = "Content from guide + skill:\n\n\
- Work flows in waves: parallel implementation tickets, then a review gate ticket (wave check) that the next wave depends on.\n\
- Wave check: lightweight verification — build passes, acceptance criteria met. Not full code review. Creates fix tasks for issues, never fails.\n\
- Dependencies (dependency_ids) enforce order; AMP dispatches everything whose deps are done in parallel.\n\
- Code review once per story at the end.\n\
- Example: our CONTRIBUTING.md plan is a one-wave example: #141 implementation, #142 wave check. Or a two-wave example: API + repo layer in wave 1, check, then handlers + UI in wave 2, check, then code review.\n\n\
Example for 300-word version: feature with wave 1 (model + repo), wave 1 check, wave 2 (API + UI), wave 2 check, code review.";

/// Thinking from the review (13-long-80x24-t1).
const THINKING: &str = "The user is asking two similar questions — one ~300 words with an example, one ~200 words and \"check the guide first.\" These look like two variants of the same request pasted together. I should probably answer one coherent response... Actually, both are in one message. The second says \"Check the guide first\" — I should call amp_guide to look this up before answering. Then write an explanation that satisfies both: about 200-300 words with an example. Hmm, two conflicting lengths. The safest is to answer both requests — or treat it as one combined request. Actually they're contradictory in word count. I think the pragmatic approach: check the guide first (as instructed), then give one explanation ~200-300 words with a brief example. Or answer them as two separate deliverables? That seems silly and repetitive.";

/// Thinking with a numbered list (08-plan-t5).
const THINKING_STEPS: &str = "Steps:\n\
1. Search KB for contributing guidelines or repo conventions.\n\
2. Look at the repo briefly (ls) to know what it is — the CONTRIBUTING.md content matters (how to contribute to what?). Actually I shouldn't implement; but knowing the repo shape helps write a good ticket. I can use ls (allowed).\n\
3. Create epic? Or add story to existing epic? The existing epic \"Try the harness\" is completed and is about trying the harness. This is a new tiny feature — create a new epic? For a tiny feature, maybe a story under a new epic like \"Repo hygiene\" or just... hierarchy is enforced: Project → Epic → Story → Task. I need an epic.";

/// Longer than a row at both sizes.
const URL: &str = "https://amp.simstech.cloud/project/45/board?task=132&view=timeline&filter=assignee:amp-worker-backend&sort=updated&group=story&columns=state,owner,updated";

const SIZES: [(usize, usize); 2] = [(80, 24), (127, 46)];

fn setup(name: &str, script: Value) -> std::path::PathBuf {
    let dir = workdir(name, Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), FAUX_CONFIG).unwrap();
    std::fs::write(dir.join("script.json"), script.to_string()).unwrap();
    dir
}

async fn snapshot(name: &str, script: Value, size: (usize, usize), inputs: &[&str]) -> Vec<String> {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup(&format!("{name}-{}x{}", size.0, size.1), script);
    let script_path = dir.join("script.json");
    let state = dir.join("home/.local/state");
    let geometry = format!("{}x{}", size.0, size.1);
    let mut args = vec!["--snapshot", geometry.as_str()];
    for input in inputs {
        args.extend(["--input", input]);
    }
    let out: Output = run(
        &dir,
        &amp.url,
        &args,
        &[
            ("AMP_HARNESS_FAUX_SCRIPT", script_path.to_str().unwrap()),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
        ],
    )
    .await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let rows: Vec<String> = out.stdout.lines().map(str::to_owned).collect();
    assert_eq!(rows.len(), size.1, "{}", out.stdout);
    rows
}

/// Words split across two rows: a row's last token and the next row's first token that only
/// make a word of `sources` together. Tokens wider than the row are expected to break.
fn split_words(rows: &[String], sources: &[&str], width: usize) -> Vec<String> {
    let vocab: HashSet<&str> = sources.iter().flat_map(|s| s.split_whitespace()).collect();
    rows.windows(2)
        .filter_map(|pair| {
            let a = pair[0].split_whitespace().last()?;
            let b = pair[1].split_whitespace().next()?;
            let joined = format!("{a}{b}");
            (joined.chars().count() < width
                && vocab.contains(joined.as_str())
                && !(vocab.contains(a) && vocab.contains(b)))
            .then(|| format!("{a:?} / {b:?}"))
        })
        .collect()
}

/// Rows of transcript prose that start with a stray space: text indented by the one-column
/// padding plus a space carried over from the break.
fn stray_spaces(rows: &[String]) -> Vec<&String> {
    rows.iter()
        .filter(|r| {
            r.starts_with("  ")
                && !r.starts_with("   ")
                && r[2..].starts_with(|c: char| !c.is_whitespace() && !"│•✓✗●─▸☐☒".contains(c))
        })
        .collect()
}

fn check(screen: &[String], sources: &[&str], width: usize) {
    let text = screen.join("\n");
    if std::env::var_os("WRAP_SCREENS").is_some() {
        eprintln!("{text}\n");
    }
    let split = split_words(screen, sources, width);
    assert!(
        split.is_empty(),
        "words split across rows: {split:?}\n{text}"
    );
    let stray = stray_spaces(screen);
    assert!(
        stray.is_empty(),
        "rows starting with a stray space: {stray:?}\n{text}"
    );
}

/// The rows from the one containing `first` on (the visible part of a block).
fn rows_from<'a>(screen: &'a [String], first: &str) -> &'a [String] {
    let i = screen
        .iter()
        .position(|r| r.contains(first))
        .unwrap_or_else(|| panic!("no row with {first:?}\n{}", screen.join("\n")));
    &screen[i..]
}

#[tokio::test]
async fn a_long_answer_with_a_list_wraps_at_words() {
    for size in SIZES {
        let screen = snapshot(
            "wrap-answer",
            json!([{ "text": ANSWER }]),
            size,
            &["waves?"],
        )
        .await;
        check(&screen, &[ANSWER], size.0);
        // List continuations hang under the item's text (pad 1 + "  • ").
        let list = rows_from(&screen, "• Example: our");
        assert!(
            list[1].starts_with("     ") && !list[1].starts_with("      "),
            "{list:?}"
        );
        assert!(
            screen
                .iter()
                .any(|r| r.starts_with(" Example for 300-word version:"))
        );
    }
}

#[tokio::test]
async fn thinking_wraps_at_words() {
    for size in SIZES {
        let script = json!([{ "thinking": THINKING, "text": "Done." }]);
        let screen = snapshot("wrap-thinking", script, size, &["explain waves"]).await;
        check(&screen, &[THINKING], size.0);
        assert!(
            screen.iter().any(|r| r.contains("silly and repetitive.")),
            "{screen:?}"
        );
    }
}

#[tokio::test]
async fn numbered_lists_in_thinking_hang_under_their_text() {
    for size in SIZES {
        let script = json!([{ "thinking": THINKING_STEPS, "text": "Planned." }]);
        let screen = snapshot("wrap-steps", script, size, &["plan it"]).await;
        check(&screen, &[THINKING_STEPS], size.0);
        // pad 1 + "  3. ": continuations start at column 6.
        let item = rows_from(&screen, "3. Create epic?");
        assert!(item[0].starts_with("   3. Create epic?"), "{item:?}");
        assert!(
            item[1].starts_with("      ") && !item[1].starts_with("       "),
            "{item:?}"
        );
    }
}

#[tokio::test]
async fn help_wraps_at_words() {
    for size in SIZES {
        let screen = snapshot("wrap-help", json!([]), size, &["/help"]).await;
        let help: Vec<String> = amp_tui::COMMANDS
            .iter()
            .map(|c| format!("{} {}", c.usage(), c.about))
            .chain(
                amp_tui::KEYS
                    .iter()
                    .map(|(k, about)| format!("{k} {about}")),
            )
            .collect();
        let help: Vec<&str> = help.iter().map(String::as_str).collect();
        check(&screen, &help, size.0);
        assert!(
            screen.iter().any(|r| r.trim() == "Keys")
                && screen.iter().any(|r| r.contains("Enter run")),
            "{screen:?}"
        );
    }
}

#[tokio::test]
async fn a_token_wider_than_the_row_still_breaks() {
    let text = format!("Open {URL} to see it.");
    for size in SIZES {
        let screen = snapshot("wrap-url", json!([{ "text": text }]), size, &["link?"]).await;
        check(&screen, &[&text], size.0);
        // The URL starts its own row and fills it; the rest follows on the next rows.
        let rows = rows_from(&screen, "https://amp.simstech.cloud");
        assert_eq!(rows[0].trim(), &URL[..rows[0].trim().len()], "{rows:?}");
        let joined: String = rows.iter().take(3).map(|r| r.trim()).collect();
        assert!(joined.starts_with(URL), "{rows:?}");
        assert!(screen.iter().any(|r| r.trim() == "Open"), "{screen:?}");
    }
}
