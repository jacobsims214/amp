//! #2176: transcript text word-wraps in the chat. Driven through the hidden `--snapshot` mode
//! at 80x24 and 127x46 (the sizes of the real-terminal review, #2175) with the review's texts:
//! a long answer with a list, thinking, thinking with a numbered list, `/help`, and a URL longer
//! than the row. No word is split across rows, no continuation row starts with a stray space,
//! and a token wider than the row still breaks.

mod common;

use std::collections::HashSet;

use common::{MockAmp, Output, run, spawn, unpad, workdir};
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

fn setup(name: &str, script: Value, extra_config: &str) -> std::path::PathBuf {
    let dir = workdir(name, Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(
        config.join("config.toml"),
        format!("{FAUX_CONFIG}{extra_config}"),
    )
    .unwrap();
    std::fs::write(dir.join("script.json"), script.to_string()).unwrap();
    dir
}

async fn snapshot(name: &str, script: Value, size: (usize, usize), inputs: &[&str]) -> Vec<String> {
    snapshot_env(name, script, size, inputs, &[], "").await
}

/// [`snapshot`] with more environment (`AMP_HARNESS_SHOW_THINKING`).
async fn snapshot_env(
    name: &str,
    script: Value,
    size: (usize, usize),
    inputs: &[&str],
    env: &[(&str, &str)],
    extra_config: &str,
) -> Vec<String> {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = setup(
        &format!("{name}-{}x{}", size.0, size.1),
        script,
        extra_config,
    );
    let script_path = dir.join("script.json");
    let state = dir.join("home/.local/state");
    let geometry = format!("{}x{}", size.0, size.1);
    let mut args = vec!["--snapshot", geometry.as_str()];
    for input in inputs {
        args.extend(["--input", input]);
    }
    let mut vars = vec![
        ("AMP_HARNESS_FAUX_SCRIPT", script_path.to_str().unwrap()),
        ("XDG_STATE_HOME", state.to_str().unwrap()),
    ];
    vars.extend_from_slice(env);
    let out: Output = run(&dir, &amp.url, &args, &vars).await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    let rows: Vec<String> = out.stdout.lines().map(str::to_owned).collect();
    assert_eq!(rows.len(), size.1, "{}", out.stdout);
    unpad(rows, size.0)
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

/// Thinking shown in full (#2177: collapsed by default): plain rows behind ` │ `, gutter removed
/// so the wrap checks see the text at the transcript's padding.
async fn full_thinking(
    name: &str,
    script: Value,
    size: (usize, usize),
    input: &str,
) -> Vec<String> {
    let screen = snapshot_env(
        name,
        script,
        size,
        &[input],
        &[("AMP_HARNESS_SHOW_THINKING", "full")],
        "",
    )
    .await;
    screen
        .iter()
        .map(|r| match r.strip_prefix(" │") {
            Some(rest) => format!(" {}", rest.strip_prefix(' ').unwrap_or(rest)),
            None => r.clone(),
        })
        .collect()
}

#[tokio::test]
async fn thinking_wraps_at_words() {
    for size in SIZES {
        let script = json!([{ "thinking": THINKING, "text": "Done." }]);
        let screen = full_thinking("wrap-thinking", script, size, "explain waves").await;
        check(&screen, &[THINKING], size.0);
        assert!(
            screen.iter().any(|r| r.contains("repetitive.")),
            "{screen:?}"
        );
    }
}

#[tokio::test]
async fn numbered_lists_in_thinking_hang_under_their_text() {
    for size in SIZES {
        let script = json!([{ "thinking": THINKING_STEPS, "text": "Planned." }]);
        let screen = full_thinking("wrap-steps", script, size, "plan it").await;
        check(&screen, &[THINKING_STEPS], size.0);
        // Plain text (no markdown list styling): pad 1 + "3. " (the gutter is stripped), so
        // continuations start at column 4.
        let item = rows_from(&screen, "3. Create epic?");
        assert!(item[0].starts_with(" 3. Create epic?"), "{item:?}");
        assert!(
            item[1].starts_with("    ") && !item[1].starts_with("     "),
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

// #2177: thinking is collapsed by default.

/// Rows that show a thinking header: `Thinking · 3s ▸` (or `Thinking ▸` without a duration).
fn thinking_headers(screen: &[String]) -> Vec<&String> {
    screen
        .iter()
        .filter(|r| {
            r.starts_with(" Thinking ·")
                || r.starts_with(" Thinking ▸")
                || r.starts_with(" Thinking ▾")
        })
        .collect()
}

/// Session files under the snapshot's state dir, read whole.
fn session_text(name: &str, size: (usize, usize)) -> String {
    let dir = std::path::Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join("amp-harness-cli")
        .join(format!("{name}-{}x{}", size.0, size.1))
        .join("home/.local/state/amp-harness/sessions");
    let mut text = String::new();
    let mut stack = vec![dir];
    while let Some(d) = stack.pop() {
        for entry in std::fs::read_dir(&d).into_iter().flatten().flatten() {
            let path = entry.path();
            if path.is_dir() {
                stack.push(path);
            } else if path.extension().is_some_and(|e| e == "jsonl") {
                text.push_str(&std::fs::read_to_string(&path).unwrap_or_default());
            }
        }
    }
    text
}

#[tokio::test]
async fn thinking_shows_one_line_per_turn_by_default() {
    for size in SIZES {
        let script = json!([
            { "thinking": THINKING, "text": "Done." },
            { "thinking": THINKING_STEPS, "text": "Planned." }
        ]);
        let screen = snapshot(
            "think-collapsed",
            script,
            size,
            &["explain waves", "plan it"],
        )
        .await;
        let text = screen.join("\n");
        let headers = thinking_headers(&screen);
        assert_eq!(headers.len(), 2, "one thinking line per turn:\n{text}");
        for h in &headers {
            // Collapsed once the answer started: the duration and ▸, no sentence after it.
            assert!(h.trim_end().ends_with('▸'), "{h:?}\n{text}");
        }
        for phrase in ["silly and repetitive", "The user is asking", "Create epic?"] {
            assert!(!text.contains(phrase), "{phrase:?} shown:\n{text}");
        }
        assert!(screen.iter().any(|r| r == " Done."), "{text}");
        assert!(screen.iter().any(|r| r == " Planned."), "{text}");
    }
    // The session file keeps the whole thinking.
    let session = session_text("think-collapsed", SIZES[0]);
    assert!(session.contains("silly and repetitive"), "{session}");
    assert!(session.contains("I need an epic."), "{session}");
}

#[tokio::test]
async fn ctrl_t_expands_the_last_thinking() {
    let size = SIZES[1];
    let script = json!([
        { "thinking": THINKING, "text": "Done." },
        { "thinking": THINKING_STEPS, "text": "Planned." }
    ]);
    let screen = snapshot(
        "think-expand",
        script.clone(),
        size,
        &["explain waves", "plan it", "key:ctrl+t"],
    )
    .await;
    let text = screen.join("\n");
    let headers = thinking_headers(&screen);
    assert_eq!(headers.len(), 2, "{text}");
    assert!(
        headers[0].trim_end().ends_with('▸'),
        "the first stays collapsed:\n{text}"
    );
    assert!(
        headers[1].trim_end().ends_with('▾'),
        "the last is expanded:\n{text}"
    );
    assert!(!text.contains("silly and repetitive"), "{text}");
    // Plain rows behind the gutter, not a markdown list.
    let item = rows_from(&screen, "3. Create epic?");
    assert!(item[0].starts_with(" │ 3. Create epic?"), "{item:?}");
    assert!(screen.iter().any(|r| r == " │ Steps:"), "{text}");

    // Pressed again, it collapses.
    let screen = snapshot(
        "think-expand-twice",
        script,
        size,
        &["explain waves", "plan it", "key:ctrl+t", "key:ctrl+t"],
    )
    .await;
    let text = screen.join("\n");
    assert!(!text.contains("Create epic?"), "{text}");
    assert!(
        thinking_headers(&screen)
            .iter()
            .all(|h| h.trim_end().ends_with('▸')),
        "{text}"
    );
}

#[tokio::test]
async fn show_thinking_hidden_in_config_shows_none_until_ctrl_t() {
    let size = SIZES[0];
    let hidden = "\n[tui]\nshow_thinking = \"hidden\"\n";
    let script = json!([{ "thinking": THINKING, "text": "Done." }]);
    let screen = snapshot_env(
        "think-hidden",
        script.clone(),
        size,
        &["explain waves"],
        &[],
        hidden,
    )
    .await;
    let text = screen.join("\n");
    assert!(thinking_headers(&screen).is_empty(), "{text}");
    assert!(!text.contains("Thinking"), "{text}");
    assert!(screen.iter().any(|r| r == " Done."), "{text}");

    let screen = snapshot_env(
        "think-hidden-shown",
        script,
        size,
        &["explain waves", "key:ctrl+t"],
        &[],
        hidden,
    )
    .await;
    let text = screen.join("\n");
    assert_eq!(thinking_headers(&screen).len(), 1, "{text}");
    assert!(text.contains("silly and repetitive"), "{text}");
}

/// #2180: a 4-column ticket table, like the board answer in the review (finding 5).
const TICKETS: &str = "Board:\n\n\
| # | Ticket | State | Owner |\n\
|--:|---|---|---|\n\
| 2176 | Transcript text breaks mid-word everywhere | done | amp-worker-backend |\n\
| 2177 | Thinking display: collapse by default with a live timer | done | amp-worker-backend |\n\
| 2180 | Render markdown tables correctly (box glyphs, padding, width fallback) | in progress | amp-worker-backend |\n\
| 2183 | Compact tool lines | ready | amp-worker-frontend |";

/// Too many columns for 80: every header word alone needs more than the row.
const WIDE: &str = "Plan:\n\n\
| Identifier | Description | Assignee | Dependencies | Acceptance | Priority | Estimate | Reviewer |\n\
|---|---|---|---|---|---|---|---|\n\
| T-1 | Parse tables | backend | none | snapshot tests | P2 | 2h | reviewer |\n\
| T-2 | Wrap cells | backend | T-1 | joints align | P2 | 1h | reviewer |";

/// The table's rows (top rule to bottom rule).
fn table_rows(screen: &[String]) -> &[String] {
    let top = screen
        .iter()
        .position(|r| r.trim_start().starts_with('┌'))
        .unwrap_or_else(|| panic!("no table\n{}", screen.join("\n")));
    let bottom = screen
        .iter()
        .position(|r| r.trim_start().starts_with('└'))
        .unwrap_or_else(|| panic!("no table bottom\n{}", screen.join("\n")));
    &screen[top..=bottom]
}

/// Column (in chars) of every box glyph that joins columns or closes a row.
fn joints(row: &str) -> Vec<usize> {
    row.trim_end()
        .chars()
        .enumerate()
        .filter(|(_, c)| "┌┬┐├┼┤└┴┘│".contains(*c))
        .map(|(i, _)| i)
        .collect()
}

fn assert_box_table(table: &[String], width: usize) {
    let text = table.join("\n");
    let top = joints(&table[0]);
    assert!(top.len() == 5, "4 columns: {text}");
    for row in table {
        assert_eq!(joints(row), top, "joints don't meet:\n{text}");
        assert!(!row.contains('|'), "ASCII | in a box table:\n{text}");
        assert!(row.trim_end().chars().count() <= width, "{text}");
    }
    // A rule under the header only: top, header, rule, body..., bottom.
    let rules = table.iter().filter(|r| r.contains('┼')).count();
    assert_eq!(rules, 1, "rules between body rows:\n{text}");
    // One space of padding each side of every cell.
    for row in table.iter().filter(|r| r.contains('│')) {
        let cells: Vec<&str> = row.trim().trim_matches('│').split('│').collect();
        for cell in cells {
            assert!(
                cell.starts_with(' ') && cell.ends_with(' '),
                "unpadded cell {cell:?}:\n{text}"
            );
        }
    }
}

#[tokio::test]
async fn tables_draw_with_box_glyphs_aligned_at_both_sizes() {
    for size in SIZES {
        let screen = snapshot("table", json!([{ "text": TICKETS }]), size, &["board?"]).await;
        if std::env::var_os("WRAP_SCREENS").is_some() {
            eprintln!("{}\n", screen.join("\n"));
        }
        let table = table_rows(&screen);
        assert_box_table(table, size.0);
        let text = table.join("\n");
        assert!(
            !screen.iter().any(|r| r.contains('|')),
            "{}",
            screen.join("\n")
        );
        if size.0 == 127 {
            // Natural widths: one row per ticket, the title whole.
            assert_eq!(table.len(), 4 + 4, "{text}");
            assert!(text.contains(
                "Render markdown tables correctly (box glyphs, padding, width fallback)"
            ));
        } else {
            // The title column shrinks and its cells wrap at words; the rest keep their width.
            assert!(table.len() > 8, "{text}");
            assert!(text.contains("amp-worker-frontend"), "{text}");
            let words: Vec<&str> = table
                .iter()
                .flat_map(|r| r.split(['│', ' ']))
                .filter(|w| !w.is_empty())
                .collect();
            for w in ["mid-word", "everywhere", "Render", "fallback)"] {
                assert!(words.contains(&w), "{w} split:\n{text}");
            }
        }
    }
}

#[tokio::test]
async fn a_table_too_wide_for_the_row_falls_back_to_records() {
    for size in SIZES {
        let screen = snapshot("table-wide", json!([{ "text": WIDE }]), size, &["plan?"]).await;
        if std::env::var_os("WRAP_SCREENS").is_some() {
            eprintln!("{}\n", screen.join("\n"));
        }
        let text = screen.join("\n");
        assert!(!screen.iter().any(|r| r.contains('|')), "{text}");
        if size.0 == 127 {
            let table = table_rows(&screen);
            let top = joints(&table[0]);
            assert_eq!(top.len(), 9, "{text}");
            for row in table {
                assert_eq!(joints(row), top, "{text}");
            }
        } else {
            assert!(!text.contains('┌') && !text.contains('┼'), "{text}");
            let records = rows_from(&screen, "• Identifier: T-1");
            assert!(records[1].trim() == "Description: Parse tables", "{text}");
            assert!(
                screen.iter().any(|r| r.contains("• Identifier: T-2")),
                "{text}"
            );
            assert!(
                screen.iter().any(|r| r.trim() == "Reviewer: reviewer"),
                "{text}"
            );
        }
    }
}
