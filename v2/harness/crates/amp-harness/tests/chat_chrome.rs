//! #2186: leaner chrome at 80 columns. Driven through `--snapshot 80x24 --meta` against the mock
//! AMP (header, ticket line, AMP status line) on the faux provider, with a reply longer than the
//! screen so the transcript fills every row it has.

mod common;

use common::{MockAmp, Output, run, spawn, workdir};
use serde_json::json;

const FAUX_CONFIG: &str = r#"
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
"#;

/// Runs the chat at 80x24 with the metadata poller, `/ticket 2085` current (its panel closed),
/// and one prompt whose answer is a 40-row list.
async fn screen(name: &str, extra: &[&str]) -> Vec<String> {
    let amp = spawn(MockAmp::new(), None).await;
    let dir = workdir(name, Some(2));
    let config = dir.join("home/.config/amp-harness");
    std::fs::create_dir_all(&config).unwrap();
    std::fs::write(config.join("config.toml"), FAUX_CONFIG).unwrap();
    let rows: Vec<String> = (1..=40).map(|i| format!("- row {i:02}")).collect();
    std::fs::write(
        dir.join("script.json"),
        json!([{"text": rows.join("\n")}]).to_string(),
    )
    .unwrap();
    let script = dir.join("script.json");
    let state = dir.join("home/.local/state");
    let mut args = vec!["--snapshot", "80x24", "--meta"];
    args.extend_from_slice(extra);
    let out: Output = run(
        &dir,
        &amp.url,
        &args,
        &[
            ("AMP_HARNESS_FAUX_SCRIPT", script.to_str().unwrap()),
            ("XDG_STATE_HOME", state.to_str().unwrap()),
        ],
    )
    .await;
    assert_eq!(
        out.code, 0,
        "stderr: {}\nstdout: {}",
        out.stderr, out.stdout
    );
    if std::env::var_os("CHROME_SCREENS").is_some() {
        eprintln!("{}", out.stdout);
    }
    let lines: Vec<String> = out.stdout.lines().map(str::to_owned).collect();
    assert_eq!(lines.len(), 24, "{}", out.stdout);
    lines
}

#[tokio::test]
async fn eighty_by_twenty_four_leaves_at_least_fifteen_transcript_rows() {
    let lines = screen(
        "chrome-80x24",
        &[
            "--input",
            "/ticket 2085",
            "--input",
            "key:esc",
            "--input",
            "list",
        ],
    )
    .await;
    let text = lines.join("\n");
    // The header's two rows: line 1 keeps the dot and "connected"; line 2 is the ticket.
    assert!(lines[0].ends_with("● connected"), "{text}");
    assert!(lines[1].contains("#2085"), "{text}");
    // Every row between the header and the editor's top rule is the transcript's.
    let rule = lines
        .iter()
        .position(|l| l.starts_with("────"))
        .expect("the editor's top rule");
    let transcript = rule - 2;
    let visible = lines.iter().filter(|l| l.contains("• row ")).count();
    assert!(transcript >= 15, "{transcript} transcript rows:\n{text}");
    assert_eq!(visible, transcript, "{text}");
    // Below the editor: the AMP status line and a one-row footer, nothing else.
    assert_eq!(lines.len() - rule, 5, "{text}");
    let footer = &lines[23];
    assert!(footer.contains("• manager · #2085"), "{footer}");
    assert!(footer.contains("ctx "), "{footer}");
    assert!(footer.ends_with("faux-1 • thinking off"), "{footer}");
    // pi's unlabelled counters are gone (they are in /usage).
    for gone in ["↑", "↓", "(auto)", "%/"] {
        assert!(!footer.contains(gone), "{gone} in {footer}");
    }
    // The long test cwd is shortened from the left.
    assert!(footer.starts_with('…'), "{footer}");
}

#[tokio::test]
async fn the_banner_session_line_is_one_row_and_light_has_no_stray_background() {
    let lines = screen("chrome-banner", &["--ansi", "--theme", "light"]).await;
    let plain: Vec<String> = lines
        .iter()
        .map(|l| strip(l).trim_end().to_owned())
        .collect();
    let text = plain.join("\n");
    let at = plain
        .iter()
        .position(|l| l.starts_with(" session "))
        .expect("the banner's session row");
    assert!(plain[at].ends_with(" (/usage for path)"), "{text}");
    // The next row is the banner's last line, not the rest of a path.
    assert!(plain[at + 1].starts_with(" /help for commands"), "{text}");
    // The transcript sets foregrounds only: no background colour anywhere above the editor (the
    // review's yellow highlight was SGR 103, a truecolor red component replayed on its own).
    let rule = plain.iter().position(|l| l.starts_with("────")).unwrap();
    for line in &lines[..rule] {
        assert!(!has_background(line), "background in {line:?}");
    }
}

fn strip(s: &str) -> String {
    let mut out = String::new();
    let mut chars = s.chars().peekable();
    while let Some(c) = chars.next() {
        if c != '\x1b' {
            out.push(c);
            continue;
        }
        match chars.next() {
            Some('[') => {
                for c in chars.by_ref() {
                    if ('@'..='~').contains(&c) {
                        break;
                    }
                }
            }
            Some(']') => {
                while let Some(c) = chars.next() {
                    if c == '\x07' || (c == '\x1b' && chars.next_if_eq(&'\\').is_some()) {
                        break;
                    }
                }
            }
            _ => {}
        }
    }
    out
}

/// Whether any SGR sequence in `line` sets a background (40–47, 48;…, 100–107).
fn has_background(line: &str) -> bool {
    line.split("\x1b[").skip(1).any(|rest| {
        let Some(end) = rest.find(|c: char| ('@'..='~').contains(&c)) else {
            return false;
        };
        if !rest[end..].starts_with('m') {
            return false;
        }
        let params: Vec<&str> = rest[..end].split(';').collect();
        let mut i = 0;
        while i < params.len() {
            match params[i].parse::<u16>() {
                Ok(38) => {
                    i += if params.get(i + 1) == Some(&"2") {
                        5
                    } else {
                        3
                    };
                    continue;
                }
                Ok(40..=48 | 100..=107) => return true,
                _ => {}
            }
            i += 1;
        }
        false
    })
}
