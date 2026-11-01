//! Redacting URLs before they are shown (#2308).
//!
//! An MCP server URL can carry a secret: `${env:KEY}` expanded into the query
//! (`https://mcp.exa.ai/mcp?exaApiKey=...`) or a password in its userinfo. reqwest's errors print
//! the whole request URL ("error sending request for url (...)"), and those errors reach the
//! transcript, `/mcp`, `doctor` and `run -p` stderr. Everything that prints an MCP or provider URL
//! goes through [`redact_url`] (one URL) or [`redact_urls`] (free text that may contain URLs).
//!
//! The rule: userinfo is dropped, every query value becomes [`REDACTED`] (the parameter names
//! stay, so `?exaApiKey=<redacted>` still says which parameter was sent) and the fragment is
//! dropped. Scheme, host, port and path are kept.

/// What a redacted value is replaced with.
pub const REDACTED: &str = "<redacted>";

/// `url` without userinfo, with every query value replaced by [`REDACTED`] and no fragment.
/// Text that is not a URL comes back with the same rule applied as far as it can be
/// (anything after `?` redacted, anything after `#` dropped).
pub fn redact_url(url: &str) -> String {
    let url = url.trim();
    let (before_fragment, _) = url.split_once('#').unwrap_or((url, ""));
    let (base, query) = match before_fragment.split_once('?') {
        Some((b, q)) => (b, Some(q)),
        None => (before_fragment, None),
    };
    let mut out = strip_userinfo(base);
    if let Some(query) = query {
        out.push('?');
        let pairs: Vec<String> = query
            .split('&')
            .filter(|p| !p.is_empty())
            .map(|pair| match pair.split_once('=') {
                Some((name, _)) => format!("{name}={REDACTED}"),
                // A bare `?token` could be the secret itself.
                None => REDACTED.to_owned(),
            })
            .collect();
        out.push_str(&pairs.join("&"));
    }
    out
}

/// `scheme://user:pass@host/path` -> `scheme://host/path`.
fn strip_userinfo(base: &str) -> String {
    let Some(i) = base.find("://") else {
        return base.to_owned();
    };
    let start = i + 3;
    let authority_end = base[start..].find('/').map_or(base.len(), |j| start + j);
    match base[start..authority_end].rfind('@') {
        Some(at) => format!("{}{}", &base[..start], &base[start + at + 1..]),
        None => base.to_owned(),
    }
}

/// `text` with every `http://`, `https://`, `ws://` and `wss://` URL in it passed through
/// [`redact_url`]. A URL ends at whitespace or at one of `)` `]` `>` `"` `'` `` ` `` `,` `;`
/// (how error messages and logs wrap them).
pub fn redact_urls(text: &str) -> String {
    let lower = text.to_ascii_lowercase();
    let mut out = String::with_capacity(text.len());
    let mut i = 0;
    while i < text.len() {
        let next = ["https://", "http://", "wss://", "ws://"]
            .iter()
            .filter_map(|scheme| lower[i..].find(scheme).map(|j| i + j))
            .min();
        let Some(start) = next else {
            out.push_str(&text[i..]);
            break;
        };
        out.push_str(&text[i..start]);
        let end = text[start..]
            .find(|c: char| c.is_whitespace() || ")]>\"'`,;".contains(c))
            .map_or(text.len(), |j| start + j);
        out.push_str(&redact_url(&text[start..end]));
        i = end;
    }
    out
}

/// `text` with each of `values` (secret values the caller knows about, e.g. what `${env:..}`
/// expanded to) replaced by [`REDACTED`]. Values shorter than 4 characters are left alone (they
/// would match ordinary text and are not credentials).
pub fn scrub_values<S: AsRef<str>>(text: &str, values: &[S]) -> String {
    let mut out = text.to_owned();
    let mut values: Vec<&str> = values
        .iter()
        .map(AsRef::as_ref)
        .filter(|v| v.len() >= 4)
        .collect();
    // Longest first, so a value that contains another is replaced whole.
    values.sort_by_key(|v| std::cmp::Reverse(v.len()));
    for v in values {
        if out.contains(v) {
            out = out.replace(v, REDACTED);
        }
        // Percent-encoded too: a URL carries the value encoded.
        let encoded: String = url_encode(v);
        if encoded != v && out.contains(&encoded) {
            out = out.replace(&encoded, REDACTED);
        }
    }
    out
}

fn url_encode(v: &str) -> String {
    let mut out = String::new();
    for b in v.bytes() {
        if b.is_ascii_alphanumeric() || b"-._~".contains(&b) {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn urls_lose_userinfo_query_values_and_fragment() {
        for (given, want) in [
            (
                "https://mcp.exa.ai/mcp?exaApiKey=SECRET123",
                "https://mcp.exa.ai/mcp?exaApiKey=<redacted>",
            ),
            (
                "https://u:p4ss@host:8443/a/b?x=1&token=SECRET#frag",
                "https://host:8443/a/b?x=<redacted>&token=<redacted>",
            ),
            ("https://host/path", "https://host/path"),
            (
                "http://127.0.0.1:1/x?SECRET",
                "http://127.0.0.1:1/x?<redacted>",
            ),
            ("https://user@host", "https://host"),
            ("not a url?k=SECRET", "not a url?k=<redacted>"),
        ] {
            assert_eq!(redact_url(given), want, "{given}");
        }
    }

    #[test]
    fn urls_inside_text_are_redacted() {
        let text = "connect to http://127.0.0.1:1: error sending request for url \
                    (http://127.0.0.1:1/x?k=SECRET&b=2): Connection refused; also \
                    HTTPS://u:SECRET@h/p?q=SECRET, and \"wss://h/s?t=SECRET\"";
        let out = redact_urls(text);
        assert!(!out.contains("SECRET"), "{out}");
        assert!(out.contains("(http://127.0.0.1:1/x?k=<redacted>&b=<redacted>)"));
        assert!(out.contains("Connection refused"));
        assert!(out.contains("HTTPS://h/p?q=<redacted>,"));
        assert_eq!(redact_urls("no urls here"), "no urls here");
    }

    #[test]
    fn known_values_are_scrubbed_raw_and_encoded() {
        let out = scrub_values(
            "https://h/s3cr+t/x and s3cr%2Bt and abc",
            &["s3cr+t", "abc"],
        );
        assert_eq!(out, "https://h/<redacted>/x and <redacted> and abc");
    }
}
