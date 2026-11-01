//! Links to AMP's web UI in tool results, made right before the model sees them (#2205).
//!
//! AMP puts web UI links in its results and refusals: `approve_url` from `amp_check_plan`, the
//! link in "… is done by a person in the AMP web UI, not by an agent or over MCP: <link>", a
//! ticket's board link. When amp-api has no `APP_PUBLIC_URL` (compose), they are relative
//! (`/project/68/plan`), and the model completed them with the only origin it knew, the MCP
//! server's: `http://localhost:18000/project/68/plan`, which is not the web UI (`:5173`).
//!
//! [`web_links`] rewrites, in a tool result's text:
//! - a relative page link (`/project/<n>…`, `/artifacts/<n>…`) at the start of a token (after
//!   whitespace, a quote or an opening bracket) to `<web_base>/project/<n>…`;
//! - the same page on the MCP origin (`http://localhost:18000/project/<n>…`) to the web base, when
//!   the web UI is somewhere else.
//!
//! Anything else (paths inside other URLs, file paths, `/project/abc`) is left alone. The web base
//! is [`amp_mcp::ServerSession::web_base`]: `AMP_WEB_URL` > `[server] web_url` > the MCP URL's
//! origin, the same origin `/open` and the status line use.

use std::borrow::Cow;

use serde_json::Value;

/// The web UI pages AMP links to.
const PAGES: &[&str] = &["/project/", "/artifacts/"];

/// `text` with every relative or MCP-origin AMP page link pointed at `web_base`. Borrowed when
/// nothing changed.
pub fn web_links<'a>(text: &'a str, web_base: &str, mcp_origin: &str) -> Cow<'a, str> {
    let web_base = web_base.trim_end_matches('/');
    if web_base.is_empty() || !PAGES.iter().any(|p| text.contains(p)) {
        return Cow::Borrowed(text);
    }
    let mcp_origin = mcp_origin.trim_end_matches('/');
    let rewrite_mcp = !mcp_origin.is_empty() && mcp_origin != web_base;

    let mut out = String::with_capacity(text.len() + 64);
    // `text[copied..]` is not in `out` yet.
    let mut copied = 0;
    let mut changed = false;
    let mut i = 0;
    while i < text.len() {
        let Some(page) = PAGES
            .iter()
            .find(|p| text[i..].starts_with(*p) && starts_with_digit(&text[i + p.len()..]))
        else {
            i += text[i..].chars().next().map_or(1, char::len_utf8);
            continue;
        };
        let before = &text[copied..i];
        if rewrite_mcp && before.ends_with(mcp_origin) && token_start(text, i - mcp_origin.len()) {
            out.push_str(&before[..before.len() - mcp_origin.len()]);
            out.push_str(web_base);
            copied = i;
            changed = true;
        } else if token_start(text, i) {
            out.push_str(before);
            out.push_str(web_base);
            copied = i;
            changed = true;
        }
        i += page.len();
    }
    if !changed {
        return Cow::Borrowed(text);
    }
    out.push_str(&text[copied..]);
    Cow::Owned(out)
}

/// [`web_links`] on every string in a JSON value (a result's structured content).
pub fn web_links_json(value: &mut Value, web_base: &str, mcp_origin: &str) {
    match value {
        Value::String(s) => {
            if let Cow::Owned(new) = web_links(s, web_base, mcp_origin) {
                *s = new;
            }
        }
        Value::Array(items) => items
            .iter_mut()
            .for_each(|v| web_links_json(v, web_base, mcp_origin)),
        Value::Object(map) => map
            .values_mut()
            .for_each(|v| web_links_json(v, web_base, mcp_origin)),
        _ => {}
    }
}

fn starts_with_digit(s: &str) -> bool {
    s.bytes().next().is_some_and(|b| b.is_ascii_digit())
}

/// A link starts a token at `at`: the start of the text, or after whitespace, a quote, an opening
/// bracket or `=` (`"approve_url":"/project/…"`, `over MCP: /project/…`, `(/project/…)`).
fn token_start(text: &str, at: usize) -> bool {
    match text[..at].chars().next_back() {
        None => true,
        Some(c) => c.is_whitespace() || matches!(c, '"' | '\'' | '`' | '(' | '<' | '[' | '{' | '='),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const WEB: &str = "http://localhost:5173";
    const MCP: &str = "http://localhost:18000";

    fn fix(text: &str) -> String {
        web_links(text, WEB, MCP).into_owned()
    }

    #[test]
    fn a_relative_approve_url_becomes_the_web_ui_link() {
        assert_eq!(
            fix(r#"{"approve_url":"/project/68/plan","awaiting_approval":[1]}"#),
            r#"{"approve_url":"http://localhost:5173/project/68/plan","awaiting_approval":[1]}"#
        );
        // Pretty-printed JSON too.
        assert_eq!(
            fix("{\n  \"approve_url\": \"/project/68/plan\"\n}"),
            "{\n  \"approve_url\": \"http://localhost:5173/project/68/plan\"\n}"
        );
    }

    #[test]
    fn a_refusal_link_becomes_the_web_ui_link() {
        let refusal = "tool error: amp_dispatch_task: tools/call amp_dispatch_task: Mcp error: \
            -32603: approving the plan is done by a person in the AMP web UI, not by an agent or \
            over MCP: /project/68/plan. Stop and ask your person to do it there.";
        assert!(fix(refusal).contains("over MCP: http://localhost:5173/project/68/plan. Stop"));
        assert_eq!(
            fix("see (/project/7/board?task=12) and [/artifacts/3]"),
            "see (http://localhost:5173/project/7/board?task=12) and [http://localhost:5173/artifacts/3]"
        );
    }

    #[test]
    fn mcp_origin_links_move_to_the_web_ui() {
        assert_eq!(
            fix(
                "Approve: http://localhost:18000/project/68/plan and \"http://localhost:18000/artifacts/4\""
            ),
            "Approve: http://localhost:5173/project/68/plan and \"http://localhost:5173/artifacts/4\""
        );
        // The MCP endpoint itself is not a page link.
        assert_eq!(
            fix("server http://localhost:18000/mcp"),
            "server http://localhost:18000/mcp"
        );
    }

    #[test]
    fn other_paths_and_links_are_left_alone() {
        for text in [
            "https://amp.simstech.cloud/project/68/plan",
            "http://localhost:5173/project/68/plan",
            "v2/project/12/file.rs",
            "/project/abc and /projects/1 and ./project/1",
            "http://localhost:180001/project/1",
            "no links here",
        ] {
            assert!(
                matches!(web_links(text, WEB, MCP), Cow::Borrowed(_)),
                "{text}"
            );
        }
    }

    #[test]
    fn same_origin_still_absolutizes_relative_links() {
        let hosted = "https://amp.simstech.cloud";
        assert_eq!(
            web_links(r#""approve_url":"/project/2/plan""#, hosted, hosted),
            r#""approve_url":"https://amp.simstech.cloud/project/2/plan""#
        );
    }

    #[test]
    fn json_strings_are_rewritten() {
        let mut v = serde_json::json!({"approve_url": "/project/68/plan", "n": 1, "list": ["/artifacts/9"]});
        web_links_json(&mut v, WEB, MCP);
        assert_eq!(
            v,
            serde_json::json!({"approve_url": "http://localhost:5173/project/68/plan", "n": 1, "list": ["http://localhost:5173/artifacts/9"]})
        );
    }

    #[test]
    fn non_ascii_text_is_kept() {
        assert_eq!(
            fix("⏸ plan → /project/1/plan ✓"),
            "⏸ plan → http://localhost:5173/project/1/plan ✓"
        );
    }
}
