//! Provider errors as one sentence a person can read (#2207).
//!
//! rpi formats a refused HTTP request as `http error <status>: <response body>`, and the body is
//! the provider's whole JSON error. OpenRouter's repeats the message once per upstream attempt
//! (`metadata.previous_errors`) and carries the account's `user_id`. Printed as is, that was
//! about ten wrapped lines, twice.
//!
//! - [`summarize`] turns it into `OpenRouter refused the request (400): <message>`, plus a next
//!   step where one exists. It is what the chat transcript, `run -p` and worker reports show.
//! - [`redact`] keeps the full text but removes `user_id` (at any depth of a JSON body). The
//!   provider wrapper applies it to every error message, so the session log keeps the full body
//!   without the id, and `--verbose` prints it.
//!
//! Neither ever prints a key: provider bodies don't echo the request's headers, and the summary
//! only takes the message fields.

use serde_json::Value;

/// The longest message a summary keeps (characters); a longer one is cut with "...".
const MAX_MESSAGE: usize = 300;

/// A provider id as people know it: `openrouter` -> `OpenRouter`. Unknown ids are returned as is.
pub fn provider_label(provider: &str) -> String {
    let known = match provider.to_ascii_lowercase().as_str() {
        "openrouter" => "OpenRouter",
        "anthropic" => "Anthropic",
        "openai" => "OpenAI",
        "deepseek" => "DeepSeek",
        "moonshot" | "moonshotai" => "Moonshot",
        "zai" | "z-ai" => "Z.ai",
        "xai" => "xAI",
        "google" | "gemini" => "Google",
        "mistral" => "Mistral",
        "groq" => "Groq",
        "ollama" => "Ollama",
        _ => "",
    };
    if known.is_empty() {
        provider.to_owned()
    } else {
        known.to_owned()
    }
}

/// `raw` (an assistant message's `error_message`, or a run's error text) as one line.
///
/// - `http error <status>: <body>` becomes `<Provider> <verb> (<status>): <message>` with the
///   message taken from the body (`error.message`, OpenAI / OpenRouter / Anthropic shapes;
///   upstream errors that say something else are added once), and a next step for the statuses
///   that have one.
/// - Anything else (a stall, a transport error, a mid-stream error message) is kept, on one line.
///
/// `provider` is the config id (`openrouter`), or "" when unknown.
pub fn summarize(provider: &str, raw: &str) -> String {
    let raw = raw.trim();
    let label = if provider.trim().is_empty() {
        "The provider".to_owned()
    } else {
        provider_label(provider.trim())
    };
    if let Some((status, body)) = split_http(raw) {
        let message = body_message(body);
        let mut line = format!("{label} {} ({status})", verb(status));
        if !message.is_empty() {
            line.push_str(": ");
            line.push_str(&message);
        }
        if let Some(next) = next_step(status, provider.trim()) {
            if !line.ends_with(['.', '!', '?']) {
                line.push('.');
            }
            line.push(' ');
            line.push_str(&next);
        }
        return line;
    }
    if let Some(rest) = raw.strip_prefix("http error: ") {
        return format!(
            "{label} could not be reached: {}",
            clip(&one_line(strip_transport(rest)))
        );
    }
    // rpi's `Provider finish_reason: <reason>`: the stream ended with a reason it doesn't take
    // as a normal end (#2334). OpenRouter sends `error` when the upstream failed mid-reply.
    if let Some(reason) = raw.strip_prefix("Provider finish_reason:") {
        return finish_reason(&label, reason.trim());
    }
    // A mid-stream error body (`{"error":{...}}`) or plain text.
    if raw.starts_with('{') {
        let message = body_message(raw);
        if !message.is_empty() {
            return format!("{label} returned an error: {message}");
        }
    }
    clip(&one_line(&redact(raw)))
}

/// A reply the provider ended with `reason` (its `finish_reason`), with a next step.
fn finish_reason(label: &str, reason: &str) -> String {
    match reason {
        "error" | "" => {
            format!("{label} ended the reply with an error; try again, or /model to switch.")
        }
        "content_filter" => format!(
            "{label} stopped the reply (its content filter); rephrase the request, or /model to \
             switch."
        ),
        other => format!(
            "{label} ended the reply unexpectedly (finish_reason {}); try again, or /model to \
             switch.",
            clip(&one_line(other))
        ),
    }
}

/// A failed run's error (rpi's `HarnessRunOutcome::Failed { error, final_message }`) as one line.
/// rpi's code `error` means "the last assistant message ended in an error": its message is the
/// provider's, summarized for `provider` (the final message's provider). Other codes
/// (`no_assistant`, ...) keep the code in front.
pub fn run_failure(code: &str, message: &str, provider: &str) -> String {
    if code == "error" || code.is_empty() {
        summarize(provider, message)
    } else {
        format!("{code}: {}", summarize(provider, message))
    }
}

/// `raw` with every `user_id` removed from its JSON body (any depth). Text that has no JSON body
/// is returned unchanged; a body that doesn't parse has `"user_id":"..."` pairs removed textually.
pub fn redact(raw: &str) -> String {
    if !raw.contains("user_id") {
        return raw.to_owned();
    }
    let (prefix, body) = match raw.find('{') {
        Some(i) => raw.split_at(i),
        None => return raw.to_owned(),
    };
    match serde_json::from_str::<Value>(body.trim_end()) {
        Ok(mut value) => {
            strip_user_ids(&mut value);
            format!("{prefix}{value}")
        }
        Err(_) => format!("{prefix}{}", strip_user_id_text(body)),
    }
}

/// `http error 400: {...}` -> `(400, "{...}")`.
fn split_http(raw: &str) -> Option<(u16, &str)> {
    let rest = raw.strip_prefix("http error ")?;
    let (status, body) = rest.split_once(':')?;
    let status = status.trim().parse::<u16>().ok()?;
    Some((status, body.trim()))
}

fn verb(status: u16) -> &'static str {
    match status {
        401 | 403 => "rejected the API key",
        402 => "needs payment",
        404 => "does not know this model or endpoint",
        408 | 504 => "timed out",
        429 => "rate-limited the request",
        500..=599 => "had a server error",
        _ => "refused the request",
    }
}

fn next_step(status: u16, provider: &str) -> Option<String> {
    let id = if provider.is_empty() {
        "<provider>"
    } else {
        provider
    };
    match status {
        401 | 403 => Some(format!("Check the key: amp-harness connect {id}.")),
        402 => Some("Add credits to the account, or pick another model.".to_owned()),
        404 => Some("Check the model id: amp-harness config show.".to_owned()),
        429 => Some("Wait a minute and try again, or pick another model.".to_owned()),
        500..=599 => Some("Try again in a moment.".to_owned()),
        _ => None,
    }
}

/// The message in an error body: `error.message` (OpenAI, OpenRouter, Anthropic), `message`, or
/// the body's first line when it isn't JSON. OpenRouter's upstream errors
/// (`error.metadata.previous_errors[].message`, `error.metadata.raw`) are added once each when
/// they say something the main message doesn't.
fn body_message(body: &str) -> String {
    let body = body.trim();
    if body.is_empty() {
        return String::new();
    }
    let Ok(value) = serde_json::from_str::<Value>(body) else {
        if body.starts_with('<') {
            // An HTML error page from a proxy says nothing useful on one line.
            return String::new();
        }
        return clip(&one_line(&redact(body)));
    };
    let error = value.get("error").unwrap_or(&value);
    let main = match error {
        Value::String(s) => s.clone(),
        _ => error
            .get("message")
            .or_else(|| value.get("message"))
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned(),
    };
    let mut parts: Vec<String> = Vec::new();
    let mut push = |m: &str| {
        let m = one_line(m);
        if !m.is_empty() && !parts.iter().any(|p| p == &m || p.contains(&m)) {
            parts.push(m);
        }
    };
    push(&main);
    if let Some(meta) = error.get("metadata") {
        if let Some(previous) = meta.get("previous_errors").and_then(Value::as_array) {
            for p in previous {
                if let Some(m) = p.get("message").and_then(Value::as_str) {
                    push(m);
                }
            }
        }
        if let Some(raw) = meta.get("raw").and_then(Value::as_str) {
            // `raw` is the upstream's own body, often JSON again.
            let inner = body_message(raw);
            push(&inner);
        }
    }
    let mut message = match parts.split_first() {
        Some((first, rest)) if !rest.is_empty() => format!("{first} (also: {})", rest.join("; ")),
        Some((first, _)) => first.clone(),
        None => String::new(),
    };
    if let Some(via) = error
        .get("metadata")
        .and_then(|m| m.get("provider_name"))
        .and_then(Value::as_str)
        .filter(|s| !s.trim().is_empty())
        && !message.is_empty()
    {
        message = format!("{message} [via {via}]");
    }
    clip(&message)
}

fn strip_user_ids(value: &mut Value) {
    match value {
        Value::Object(map) => {
            map.remove("user_id");
            for v in map.values_mut() {
                strip_user_ids(v);
            }
        }
        Value::Array(items) => items.iter_mut().for_each(strip_user_ids),
        _ => {}
    }
}

/// Removes `"user_id": "..."` (and a comma next to it) from text that isn't valid JSON.
fn strip_user_id_text(text: &str) -> String {
    let mut out = text.to_owned();
    while let Some(start) = out.find("\"user_id\"") {
        let after_key = start + "\"user_id\"".len();
        let rest = &out[after_key..];
        let Some(colon) = rest.find(':') else { break };
        let value = rest[colon + 1..].trim_start();
        let skipped = rest.len() - value.len();
        let value_len = if let Some(v) = value.strip_prefix('"') {
            match v.find('"') {
                Some(end) => end + 2,
                None => value.len(),
            }
        } else {
            value.find([',', '}']).unwrap_or(value.len())
        };
        let mut end = after_key + skipped + value_len;
        let mut begin = start;
        if out[end..].starts_with(',') {
            end += 1;
        } else if let Some(comma) = out[..start].trim_end().strip_suffix(',') {
            begin = comma.len();
        }
        out.replace_range(begin..end, "");
    }
    out
}

/// rpi's `http transport error: ` prefix adds nothing after "could not be reached".
fn strip_transport(text: &str) -> &str {
    text.strip_prefix("http transport error: ").unwrap_or(text)
}

/// Whitespace runs (newlines included) as one space.
fn one_line(text: &str) -> String {
    text.split_whitespace().collect::<Vec<_>>().join(" ")
}

fn clip(text: &str) -> String {
    if text.chars().count() <= MAX_MESSAGE {
        return text.to_owned();
    }
    let cut: String = text.chars().take(MAX_MESSAGE).collect();
    format!("{}...", cut.trim_end())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// QA pass 1's body (#2173), as rpi formats it.
    const QA_400: &str = r#"http error 400: {"error":{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400,"metadata":{"provider_name":null,"previous_errors":[{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400},{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400},{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400},{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400}]}},"user_id":"user_3FPq9ZxAbCdEf"}"#;

    /// #2334: QA pass 3's first manager turn ended "Provider finish_reason: error".
    #[test]
    fn a_finish_reason_is_a_sentence_with_a_next_step() {
        assert_eq!(
            summarize("openrouter", "Provider finish_reason: error"),
            "OpenRouter ended the reply with an error; try again, or /model to switch."
        );
        assert_eq!(
            summarize("", "Provider finish_reason: content_filter"),
            "The provider stopped the reply (its content filter); rephrase the request, or \
             /model to switch."
        );
        assert_eq!(
            summarize("example", "Provider finish_reason: weird"),
            "example ended the reply unexpectedly (finish_reason weird); try again, or /model \
             to switch."
        );
    }

    #[test]
    fn openrouter_400_is_one_sentence() {
        let line = summarize("openrouter", QA_400);
        assert_eq!(
            line,
            "OpenRouter refused the request (400): messages[9]: tool messages must include a \
             non-empty string tool_call_id"
        );
        assert!(!line.contains("user_"));
    }

    #[test]
    fn distinct_upstream_errors_are_added_once() {
        let raw = r#"http error 502: {"error":{"message":"Provider returned error","code":502,"metadata":{"provider_name":"DeepInfra","raw":"{\"error\":{\"message\":\"model overloaded\"}}","previous_errors":[{"message":"upstream timeout"},{"message":"upstream timeout"}]}},"user_id":"user_x"}"#;
        assert_eq!(
            summarize("openrouter", raw),
            "OpenRouter had a server error (502): Provider returned error (also: upstream \
             timeout; model overloaded) [via DeepInfra]. Try again in a moment."
        );
    }

    #[test]
    fn statuses_with_a_next_step() {
        let cases = [
            (
                "anthropic",
                r#"http error 401: {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}"#,
                "Anthropic rejected the API key (401): invalid x-api-key. Check the key: \
                 amp-harness connect anthropic.",
            ),
            (
                "openrouter",
                r#"http error 429: {"error":{"message":"Rate limit exceeded","code":429}}"#,
                "OpenRouter rate-limited the request (429): Rate limit exceeded. Wait a minute \
                 and try again, or pick another model.",
            ),
            (
                "openrouter",
                "http error 404: ",
                "OpenRouter does not know this model or endpoint (404). Check the model id: \
                 amp-harness config show.",
            ),
            (
                "local",
                "http error 500: <html><body>Bad Gateway</body></html>",
                "local had a server error (500). Try again in a moment.",
            ),
            (
                "openai",
                "http error 400: plain text\nreason",
                "OpenAI refused the request (400): plain text reason",
            ),
        ];
        for (provider, raw, want) in cases {
            assert_eq!(summarize(provider, raw), want, "{raw}");
        }
    }

    #[test]
    fn other_errors_stay_but_on_one_line() {
        assert_eq!(
            summarize(
                "openrouter",
                "http error: http transport error: error sending request"
            ),
            "OpenRouter could not be reached: error sending request"
        );
        let stall = "openrouter/z-ai/glm-5.3: stream stalled, no data for 120s \
                     (stream_idle_timeout_secs); request cancelled";
        assert_eq!(summarize("openrouter", stall), stall);
        assert_eq!(
            summarize("", "Request was\n  aborted"),
            "Request was aborted"
        );
        assert_eq!(
            summarize(
                "openrouter",
                r#"{"error":{"message":"mid-stream"},"user_id":"u1"}"#
            ),
            "OpenRouter returned an error: mid-stream"
        );
    }

    #[test]
    fn long_messages_are_clipped() {
        let long = format!(
            r#"http error 400: {{"error":{{"message":"{}"}}}}"#,
            "x".repeat(1000)
        );
        let line = summarize("openrouter", &long);
        assert!(line.ends_with("..."));
        assert!(line.chars().count() < MAX_MESSAGE + 60, "{line}");
    }

    #[test]
    fn redact_removes_user_id_and_keeps_the_rest() {
        let redacted = redact(QA_400);
        assert!(redacted.starts_with("http error 400: {"));
        assert!(!redacted.contains("user_id"));
        assert!(!redacted.contains("user_3FP"));
        assert!(redacted.contains("previous_errors"));
        let nested =
            r#"http error 400: {"error":{"metadata":{"user_id":"a"}},"x":[{"user_id":"b"}]}"#;
        assert!(!redact(nested).contains("user_id"));
        // Truncated JSON still loses the id.
        let broken = r#"http error 400: {"error":{"message":"m"},"user_id":"user_3FP","x":1"#;
        let r = redact(broken);
        assert!(!r.contains("user_3FP"), "{r}");
        assert!(r.contains(r#""x":1"#), "{r}");
        let last = r#"{"a":1,"user_id":"user_3FP""#;
        assert_eq!(redact(last), r#"{"a":1"#);
        assert_eq!(redact("no ids here"), "no ids here");
    }
}
