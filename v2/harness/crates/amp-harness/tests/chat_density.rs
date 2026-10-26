//! #2346: the chat's density. The same scripted conversation (a thinking reply with a heading
//! and a list, AMP tool calls, a code block) is laid out through the hidden `--snapshot` mode at
//! 120x40 and 80x24, compact (the default) and comfortable (`[tui] density`). Compact shows at
//! least a quarter more of the conversation on the same screen, with no decorative blank rows,
//! one blank row between turns, a one-row footer and no bold outside headings and the person's
//! own prompt.
//!
//! `DENSITY_SCREENS=1` prints the screens (the before/after renderings on the ticket).

mod common;

use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::{Value, json};

const SIZES: [(usize, usize); 2] = [(120, 40), (80, 24)];

fn config(density: Option<&str>) -> String {
    let mut text = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
"#
    .to_owned();
    if let Some(d) = density {
        text.push_str(&format!("\n[tui]\ndensity = \"{d}\"\n"));
    }
    text
}

const PROMPTS: [&str; 3] = ["explain waves", "what's on the board?", "how do I run one?"];

/// Three turns: thinking and markdown; two AMP calls and a list; a code block.
fn script() -> Value {
    json!([
        {
            "thinking": "The person asks about waves. I explain the order and the gates.",
            "text": "## Waves\n\nWaves run in order. A wave starts once every ticket it depends on is done, and its tickets run in parallel.\n\n- **Gate**: a wave check reviews the wave before the next starts.\n- **Fixes**: a failed check adds fix tickets to the same wave.\n- **Merges**: the person merges when the batch is done.\n\nThat keeps each wave small enough to review."
        },
        {"tool_calls": [
            {"name": "amp_list_tasks", "arguments": {"project_id": 2, "format": "full"}},
            {"name": "amp_get_plan", "arguments": {"project_id": 2}}
        ]},
        {"text": "### Board\n\nSeven tickets: one ready, one in progress.\n\n1. #2085 M1-4 CLI is in progress.\n2. Two plans wait for your approval.\n3. Nothing has failed this week."},
        {"tool_calls": [{"name": "amp_get_task", "arguments": {"task_id": 2085}}]},
        {"text": "Run it from the harness folder:\n\n```sh\nmake -C v2/harness check\n```\n\nThen push the branch."}
    ])
}

async fn snapshot(density: Option<&str>, size: (usize, usize)) -> Vec<String> {
    snapshot_with(density, size, &[]).await
}

async fn snapshot_with(density: Option<&str>, size: (usize, usize), extra: &[&str]) -> Vec<String> {
    let amp = spawn(MockAmp::new(), None).await;
    let name = format!(
        "density-{}-{}x{}{}",
        density.unwrap_or("default"),
        size.0,
        size.1,
        extra.join("")
    );
    let dir = workdir(&name, Some(2));
    let config_dir = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config_dir).unwrap();
    std::fs::write(config_dir.join("config.toml"), config(density)).unwrap();
    let script_path = dir.join("script.json");
    std::fs::write(&script_path, script().to_string()).unwrap();
    let state = dir.join("home/.local/state");
    let geometry = format!("{}x{}", size.0, size.1);
    let mut args = vec!["--no-session", "--snapshot", geometry.as_str()];
    args.extend(extra);
    for prompt in PROMPTS {
        args.extend(["--input", prompt]);
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
    if std::env::var_os("DENSITY_SCREENS").is_some() {
        println!("--- {name}\n{}", out.stdout);
    }
    let rows: Vec<String> = out.stdout.lines().map(str::to_owned).collect();
    assert_eq!(rows.len(), size.1, "{}", out.stdout);
    rows
}

/// The rows of the conversation in sight: everything above the editor's top border.
fn transcript(rows: &[String]) -> &[String] {
    let editor = rows
        .iter()
        .position(|r| r.trim_start().starts_with('─'))
        .unwrap_or_else(|| panic!("no editor border in\n{}", rows.join("\n")));
    &rows[..editor]
}

/// The non-blank rows of the conversation in sight.
fn content_lines(rows: &[String]) -> usize {
    transcript(rows)
        .iter()
        .filter(|r| !r.trim().is_empty())
        .count()
}

#[tokio::test]
async fn compact_shows_more_of_the_same_conversation() {
    for size in SIZES {
        let compact = snapshot(None, size).await;
        let comfortable = snapshot(Some("comfortable"), size).await;
        let (before, after) = (content_lines(&comfortable), content_lines(&compact));
        if std::env::var_os("DENSITY_SCREENS").is_some() {
            println!("{size:?}: comfortable {before} content lines, compact {after}");
        }
        assert!(
            after * 100 >= before * 125,
            "{size:?}: compact {after} vs comfortable {before} content lines\n{}\n---\n{}",
            compact.join("\n"),
            comfortable.join("\n")
        );
    }
}

fn strip(s: &str) -> String {
    let mut out = String::new();
    let mut chars = s.chars().peekable();
    while let Some(c) = chars.next() {
        if c == '\x1b' {
            match chars.next() {
                Some('[') => {
                    for p in chars.by_ref() {
                        if ('@'..='~').contains(&p) {
                            break;
                        }
                    }
                }
                Some(']') => {
                    while let Some(p) = chars.next() {
                        if p == '\x07' {
                            break;
                        }
                        if p == '\x1b' && chars.peek() == Some(&'\\') {
                            chars.next();
                            break;
                        }
                    }
                }
                _ => {}
            }
        } else {
            out.push(c);
        }
    }
    out
}

#[tokio::test]
async fn compact_has_no_decorative_rows_a_one_row_footer_and_bold_only_for_headings() {
    for size in SIZES {
        let styled = snapshot_with(None, size, &["--ansi"]).await;
        let rows: Vec<String> = styled
            .iter()
            .map(|r| strip(r).trim_end().to_owned())
            .collect();
        let screen = rows.join("\n");
        // Never two blank rows in a row in the conversation: one between turns.
        let talk = transcript(&rows);
        let first = talk.iter().position(|r| !r.trim().is_empty()).unwrap();
        for pair in talk[first..].windows(2) {
            assert!(
                !(pair[0].trim().is_empty() && pair[1].trim().is_empty()),
                "{size:?}: two blank rows\n{screen}"
            );
        }
        // The person's prompt, one blank row, then the reply's first line.
        let at = rows
            .iter()
            .position(|r| r.trim() == "how do I run one?")
            .unwrap();
        assert!(rows[at + 1].trim().is_empty(), "{screen}");
        assert!(
            rows[at + 2].trim_start().starts_with("✓ get_task #2085"),
            "{screen}"
        );
        assert!(
            rows[at + 3].trim_start().starts_with("Run it from"),
            "{screen}"
        );
        // The footer is one row: the editor's bottom rule, the footer, the blank bottom row.
        let bottom = rows
            .iter()
            .rposition(|r| r.trim_start().starts_with('─'))
            .unwrap();
        assert!(rows[bottom + 1].contains("• manager"), "{screen}");
        assert!(rows[bottom + 1].ends_with("thinking off"), "{screen}");
        assert!(
            rows[bottom + 2..].iter().all(|r| r.trim().is_empty()),
            "{screen}"
        );
        // Bold only on headings (and the reply's own **bold**); headings aren't underlined.
        for (raw, row) in styled.iter().zip(&rows) {
            let t = row.trim();
            if raw.contains("\x1b[1m") {
                assert!(
                    ["Waves", "Board"].contains(&t)
                        || ["• Gate", "• Fixes", "• Merges"]
                            .iter()
                            .any(|g| t.starts_with(g)),
                    "{size:?}: bold in {t:?}"
                );
            }
            assert!(!raw.contains("\x1b[4m"), "{size:?}: underline in {t:?}");
        }
    }
}

#[tokio::test]
async fn comfortable_keeps_the_roomier_layout() {
    let rows = snapshot(Some("comfortable"), (120, 40)).await;
    let screen = rows.join("\n");
    // The bubble's padding rows and the gap: two blank rows above and below the prompt.
    let at = rows
        .iter()
        .position(|r| r.trim() == "how do I run one?")
        .unwrap();
    assert!(
        rows[at + 1].trim().is_empty() && rows[at + 2].trim().is_empty(),
        "{screen}"
    );
    // The two-row footer at 120 columns.
    let bottom = rows
        .iter()
        .rposition(|r| r.trim_start().starts_with('─'))
        .unwrap();
    assert!(rows[bottom + 1].contains("• manager"), "{screen}");
    assert!(rows[bottom + 2].ends_with("thinking off"), "{screen}");
}
