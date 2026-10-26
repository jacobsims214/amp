//! Text-format tool calls (#2179): some OpenAI-compatible models (MiMo, Qwen-coder and others
//! trained on the Hermes/Qwen chat templates) sometimes write a tool call as text instead of, or
//! next to, a structured `tool_calls` entry:
//!
//! ```text
//! <tool_call><function=ls><parameter=path>/repo</parameter></function></tool_call>
//! <tool_call>{"name": "ls", "arguments": {"path": "/repo"}}</tool_call>
//! <function=ls><parameter=path>/repo</parameter></function>
//! ```
//!
//! That markup is never shown. [`scrub_stream`] sits in [`crate::providers::ConfiguredProvider`]
//! (the same layer as the stall guard) and rewrites the event stream:
//! - Text and thinking deltas, their `End` contents and every partial message have the markup
//!   removed. Text that might be the start of a tag (`<tool`) is held back until it is decided, so
//!   a tag split across stream chunks never leaks.
//! - On `Done`: if the message has a structured tool call, the text calls are dropped. If it has
//!   none, each distinct text call becomes a real [`ToolCall`] at the end of the content and the
//!   stop reason becomes `ToolUse`.
//!
//! The parsed call is then an ordinary tool call in the final message, so the agent loop runs it
//! through the same `before_tool_call` hook (tool policy, approval guard) as any other call. There
//! is no separate execution path.
//!
//! Argument types come from the request's tool schema: a parameter declared `string` stays a
//! string, other declared types are read as JSON (falling back to the string when that fails).

use std::{
    collections::BTreeMap,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
    time::{SystemTime, UNIX_EPOCH},
};

use rpi_ai::{
    AssistantMessage, AssistantMessageEvent, AssistantMessageEventStream, StopReason,
    event_stream::create_assistant_message_event_stream,
    types::{Content, DoneReason, Tool, ToolCall, ToolCallType},
};
use serde_json::{Map, Value};

const TOOL_CALL_OPEN: &str = "<tool_call>";
const TOOL_CALL_CLOSE: &str = "</tool_call>";
const FUNCTION_OPEN: &str = "<function=";
const FUNCTION_CLOSE: &str = "</function>";
const PARAMETER_OPEN: &str = "<parameter=";
const PARAMETER_CLOSE: &str = "</parameter>";

/// A tool call parsed from text.
#[derive(Debug, Clone, PartialEq)]
pub struct TextCall {
    pub name: String,
    pub arguments: Value,
}

/// Incremental scrubber for one text or thinking block. Feed it the block's whole text so far
/// (it only grows); [`Scrubber::visible`] is what may be shown.
#[derive(Debug, Default, Clone)]
pub struct Scrubber {
    /// Bytes of the raw text already decided (shown or removed).
    consumed: usize,
    clean: String,
    calls: Vec<TextCall>,
}

enum Markup {
    /// Not markup: show the `<`.
    Text,
    /// Markup of this many bytes, removed; with the call it held, if it parsed.
    Block(usize, Option<TextCall>),
    /// Might be markup, but more text is needed to tell.
    Incomplete,
}

impl Scrubber {
    /// Process the block's text so far, holding back anything that may still become markup.
    pub fn feed(&mut self, raw: &str, tools: &[Tool]) {
        self.process(raw, false, tools);
    }

    /// The block is complete: decide what was held back.
    pub fn finish(&mut self, raw: &str, tools: &[Tool]) {
        self.process(raw, true, tools);
    }

    /// The text that may be shown.
    pub fn visible(&self) -> &str {
        &self.clean
    }

    /// Calls parsed so far.
    pub fn calls(&self) -> &[TextCall] {
        &self.calls
    }

    fn process(&mut self, raw: &str, at_end: bool, tools: &[Tool]) {
        if raw.len() < self.consumed || !raw.is_char_boundary(self.consumed) {
            // The provider rewrote the block instead of appending to it: start over.
            *self = Self::default();
        }
        loop {
            let rest = &raw[self.consumed..];
            if rest.is_empty() {
                return;
            }
            match rest.find('<') {
                None => {
                    self.clean.push_str(rest);
                    self.consumed = raw.len();
                    return;
                }
                Some(0) => {}
                Some(i) => {
                    self.clean.push_str(&rest[..i]);
                    self.consumed += i;
                    continue;
                }
            }
            match markup_at(rest, at_end, tools) {
                Markup::Text => {
                    self.clean.push('<');
                    self.consumed += 1;
                }
                Markup::Block(len, call) => {
                    self.consumed += len;
                    self.calls.extend(call);
                }
                Markup::Incomplete => return,
            }
        }
    }
}

/// What the text at `rest` (which starts with `<`) is.
fn markup_at(rest: &str, at_end: bool, tools: &[Tool]) -> Markup {
    // A stray closing tag (after a bare `<function=…>` block, say).
    if rest.starts_with(TOOL_CALL_CLOSE) {
        return Markup::Block(TOOL_CALL_CLOSE.len(), None);
    }
    for (open, close) in [
        (TOOL_CALL_OPEN, TOOL_CALL_CLOSE),
        (FUNCTION_OPEN, FUNCTION_CLOSE),
    ] {
        if let Some(after_open) = rest.strip_prefix(open) {
            let (len, block) = match after_open.find(close) {
                Some(j) => {
                    let len = open.len() + j + close.len();
                    (len, &rest[..len])
                }
                None if at_end => (rest.len(), rest),
                None => return Markup::Incomplete,
            };
            let call = if open == TOOL_CALL_OPEN {
                let body = &block[open.len()..];
                parse_tool_call_body(body.strip_suffix(close).unwrap_or(body), tools)
            } else {
                parse_function(block, tools)
            };
            return Markup::Block(len, call);
        }
    }
    let maybe_tag = [TOOL_CALL_OPEN, TOOL_CALL_CLOSE, FUNCTION_OPEN]
        .iter()
        .any(|tag| tag.starts_with(rest));
    if maybe_tag && !at_end {
        Markup::Incomplete
    } else {
        Markup::Text
    }
}

/// The inside of `<tool_call>…</tool_call>`: Hermes JSON or Qwen-coder `<function=…>` markup.
fn parse_tool_call_body(inner: &str, tools: &[Tool]) -> Option<TextCall> {
    let inner = inner.trim();
    if inner.starts_with('{') {
        let value: Value = serde_json::from_str(inner).ok()?;
        let name = value.get("name")?.as_str()?.trim().to_owned();
        if name.is_empty() {
            return None;
        }
        let arguments = match value.get("arguments").or_else(|| value.get("parameters")) {
            Some(Value::String(s)) => serde_json::from_str(s).ok()?,
            Some(v @ Value::Object(_)) => v.clone(),
            None | Some(Value::Null) => Value::Object(Map::new()),
            Some(_) => return None,
        };
        return Some(TextCall { name, arguments });
    }
    let start = inner.find(FUNCTION_OPEN)?;
    parse_function(&inner[start..], tools)
}

/// `<function=NAME><parameter=KEY>VALUE</parameter>…</function>` (the closing tags may be
/// missing when the stream ended early).
fn parse_function(block: &str, tools: &[Tool]) -> Option<TextCall> {
    let after = block.strip_prefix(FUNCTION_OPEN)?;
    let name_end = after.find('>')?;
    let name = after[..name_end].trim();
    if name.is_empty() || name.contains(char::is_whitespace) {
        return None;
    }
    let mut body = &after[name_end + 1..];
    if let Some(end) = body.find(FUNCTION_CLOSE) {
        body = &body[..end];
    }
    let schema = tools
        .iter()
        .find(|t| t.name == name)
        .map(|t| &t.parameters.0);
    let mut arguments = Map::new();
    while let Some(start) = body.find(PARAMETER_OPEN) {
        let after = &body[start + PARAMETER_OPEN.len()..];
        let Some(key_end) = after.find('>') else {
            break;
        };
        let key = after[..key_end].trim();
        let rest = &after[key_end + 1..];
        let (value, next) = match (rest.find(PARAMETER_CLOSE), rest.find(PARAMETER_OPEN)) {
            (Some(c), Some(o)) if o < c => (&rest[..o], &rest[o..]),
            (Some(c), _) => (&rest[..c], &rest[c + PARAMETER_CLOSE.len()..]),
            (None, Some(o)) => (&rest[..o], &rest[o..]),
            (None, None) => (rest, ""),
        };
        if !key.is_empty() {
            // Qwen's template puts the value on its own lines: drop one newline each side.
            let value = value.strip_prefix('\n').unwrap_or(value);
            let value = value.strip_suffix('\n').unwrap_or(value);
            arguments.insert(key.to_owned(), typed(value, property_types(schema, key)));
        }
        body = next;
    }
    Some(TextCall {
        name: name.to_owned(),
        arguments: Value::Object(arguments),
    })
}

/// The JSON types a schema declares for one property (empty when unknown).
fn property_types(schema: Option<&Value>, key: &str) -> Vec<String> {
    let Some(ty) = schema
        .and_then(|s| s.get("properties"))
        .and_then(|p| p.get(key))
        .and_then(|p| p.get("type"))
    else {
        return Vec::new();
    };
    match ty {
        Value::String(s) => vec![s.clone()],
        Value::Array(a) => a
            .iter()
            .filter_map(|t| t.as_str().map(str::to_owned))
            .collect(),
        _ => Vec::new(),
    }
}

fn typed(raw: &str, types: Vec<String>) -> Value {
    if types.iter().any(|t| t == "string") {
        return Value::String(raw.to_owned());
    }
    // A declared non-string type, or no schema: JSON if it parses as something other than a
    // string, else the text as given.
    match serde_json::from_str::<Value>(raw.trim()) {
        Ok(Value::String(_)) | Err(_) => Value::String(raw.to_owned()),
        Ok(v) => v,
    }
}

fn block_text(content: &Content) -> Option<&str> {
    match content {
        Content::Text(t) => Some(&t.text),
        Content::Thinking(t) => Some(&t.thinking),
        _ => None,
    }
}

fn set_block_text(content: &mut Content, text: &str) {
    match content {
        Content::Text(t) => t.text = text.to_owned(),
        Content::Thinking(t) => t.thinking = text.to_owned(),
        _ => {}
    }
}

/// A unique id with `prefix`: `<prefix>_<ms hex>_<n>`.
fn unique_id(prefix: &str) -> String {
    static NEXT: AtomicU64 = AtomicU64::new(0);
    let ms = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis())
        .unwrap_or_default();
    format!("{prefix}_{ms:x}_{}", NEXT.fetch_add(1, Ordering::Relaxed))
}

/// A unique id for a call parsed from text.
fn new_call_id() -> String {
    unique_id("call_text")
}

/// Whether a structured call can't be dispatched: no tool name. rpi-agent rejects it, and the
/// rejection's tool result is what poisons the session (#2203).
fn unusable(call: &ToolCall) -> bool {
    call.name.trim().is_empty()
}

/// Repair the structured tool calls of a finished message (#2203). Some OpenAI-compatible
/// providers (OpenRouter with MiMo) stream a `tool_calls` entry with no id and no name next to a
/// text-format call. rpi-agent rejects such a call with a tool result whose `tool_call_id` is
/// empty, and every later request is then refused (400 "tool messages must include a non-empty
/// string tool_call_id"). So: a call with no name is dropped, and a named call with no id gets a
/// generated one (`call_amp_…`). When no call is left, a `ToolUse` stop reason becomes `Stop`.
/// Returns how many calls were dropped.
pub fn repair_tool_calls(message: &mut AssistantMessage) -> usize {
    let before = message.content.len();
    message
        .content
        .retain(|c| !matches!(c, Content::ToolCall(call) if unusable(call)));
    let dropped = before - message.content.len();
    let mut any = false;
    for content in &mut message.content {
        if let Content::ToolCall(call) = content {
            any = true;
            if call.id.trim().is_empty() {
                call.id = unique_id("call_amp");
            }
        }
    }
    if !any && message.stop_reason == StopReason::ToolUse {
        message.stop_reason = StopReason::Stop;
    }
    dropped
}

/// The `Done` reason that matches a repaired message: `ToolUse` exactly when it has calls left
/// (a `Length` or other reason is kept).
fn done_reason(reason: DoneReason, message: &AssistantMessage) -> DoneReason {
    let has_calls = message
        .content
        .iter()
        .any(|c| matches!(c, Content::ToolCall(_)));
    match reason {
        DoneReason::Stop if has_calls => DoneReason::ToolUse,
        DoneReason::ToolUse if !has_calls => DoneReason::Stop,
        other => other,
    }
}

/// Scrub a finished message. With `parse_calls`, text calls become real calls when the message has
/// no structured one; returns whether it did.
///
/// Structured calls are repaired first ([`repair_tool_calls`]), so a call with no name doesn't
/// count as structured: the text calls are parsed in its place.
pub fn scrub_message(message: &mut AssistantMessage, tools: &[Tool], parse_calls: bool) -> bool {
    repair_tool_calls(message);
    let mut calls: Vec<TextCall> = Vec::new();
    let content = std::mem::take(&mut message.content);
    for mut block in content {
        if let Some(raw) = block_text(&block) {
            let mut scrubber = Scrubber::default();
            scrubber.finish(raw, tools);
            if scrubber.visible().len() != raw.len() {
                for call in scrubber.calls {
                    if !calls.contains(&call) {
                        calls.push(call);
                    }
                }
                let keep = !scrubber.clean.trim().is_empty()
                    || matches!(&block, Content::Thinking(t) if t.thinking_signature.is_some());
                if !keep {
                    continue;
                }
                set_block_text(&mut block, &scrubber.clean);
            }
        }
        message.content.push(block);
    }
    let structured = message
        .content
        .iter()
        .any(|c| matches!(c, Content::ToolCall(_)));
    if !parse_calls || structured || calls.is_empty() {
        return false;
    }
    for call in calls {
        message.content.push(Content::ToolCall(ToolCall {
            kind: ToolCallType,
            id: new_call_id(),
            name: call.name,
            arguments: call.arguments,
            thought_signature: None,
            namespace: None,
        }));
    }
    if message.stop_reason == StopReason::Stop {
        message.stop_reason = StopReason::ToolUse;
    }
    true
}

/// Rewrites a provider's events so text-format tool calls are never shown, and become real calls
/// when no structured call came with them. See the module docs.
pub struct StreamScrubber {
    tools: Vec<Tool>,
    blocks: BTreeMap<usize, Scrubber>,
    /// Scrub and parse text-format calls. Without it only the final message's structured calls
    /// are repaired ([`repair_tool_calls`]); every other event passes through.
    text: bool,
}

impl StreamScrubber {
    pub fn new(tools: Vec<Tool>) -> Self {
        Self {
            tools,
            blocks: BTreeMap::new(),
            text: true,
        }
    }

    /// Repair structured calls only (providers that never write text calls: Anthropic).
    pub fn calls_only() -> Self {
        Self {
            tools: Vec::new(),
            blocks: BTreeMap::new(),
            text: false,
        }
    }

    /// The events to send in place of `event` (usually one; an `End` may add the delta it
    /// released first).
    pub fn on_event(&mut self, event: AssistantMessageEvent) -> Vec<AssistantMessageEvent> {
        use AssistantMessageEvent as E;
        match event {
            E::Done {
                reason,
                mut message,
            } => {
                if self.text {
                    scrub_message(&mut message, &self.tools, true);
                } else {
                    repair_tool_calls(&mut message);
                }
                let reason = done_reason(reason, &message);
                vec![E::Done { reason, message }]
            }
            E::Error { reason, mut error } => {
                // #2207: the provider's body goes to the session log without the account id.
                if let Some(message) = error.error_message.as_mut() {
                    *message = crate::provider_error::redact(message);
                }
                if self.text {
                    scrub_message(&mut error, &self.tools, false);
                } else {
                    repair_tool_calls(&mut error);
                }
                vec![E::Error { reason, error }]
            }
            other if !self.text => vec![other],
            E::Start { partial } => vec![E::Start {
                partial: self.update(&partial, None),
            }],
            E::TextStart {
                content_index,
                partial,
            } => vec![E::TextStart {
                content_index,
                partial: self.update(&partial, None),
            }],
            E::ThinkingStart {
                content_index,
                partial,
            } => vec![E::ThinkingStart {
                content_index,
                partial: self.update(&partial, None),
            }],
            E::TextDelta {
                content_index,
                partial,
                ..
            } => {
                let before = self.visible_len(content_index);
                let partial = self.update(&partial, None);
                let delta = self.visible_from(content_index, before);
                vec![E::TextDelta {
                    content_index,
                    delta,
                    partial,
                }]
            }
            E::ThinkingDelta {
                content_index,
                partial,
                ..
            } => {
                let before = self.visible_len(content_index);
                let partial = self.update(&partial, None);
                let delta = self.visible_from(content_index, before);
                vec![E::ThinkingDelta {
                    content_index,
                    delta,
                    partial,
                }]
            }
            E::TextEnd {
                content_index,
                content,
                partial,
            } => {
                let before = self.visible_len(content_index);
                let partial = self.update_end(&partial, content_index, &content);
                let released = self.visible_from(content_index, before);
                let content = self.visible_or(content_index, content);
                let mut out = Vec::new();
                if !released.is_empty() {
                    out.push(E::TextDelta {
                        content_index,
                        delta: released,
                        partial: Arc::clone(&partial),
                    });
                }
                out.push(E::TextEnd {
                    content_index,
                    content,
                    partial,
                });
                out
            }
            E::ThinkingEnd {
                content_index,
                content,
                partial,
            } => {
                let before = self.visible_len(content_index);
                let partial = self.update_end(&partial, content_index, &content);
                let released = self.visible_from(content_index, before);
                let content = self.visible_or(content_index, content);
                let mut out = Vec::new();
                if !released.is_empty() {
                    out.push(E::ThinkingDelta {
                        content_index,
                        delta: released,
                        partial: Arc::clone(&partial),
                    });
                }
                out.push(E::ThinkingEnd {
                    content_index,
                    content,
                    partial,
                });
                out
            }
            E::ToolCallStart {
                content_index,
                partial,
            } => vec![E::ToolCallStart {
                content_index,
                partial: self.update(&partial, None),
            }],
            E::ToolCallDelta {
                content_index,
                delta,
                partial,
            } => vec![E::ToolCallDelta {
                content_index,
                delta,
                partial: self.update(&partial, None),
            }],
            E::ToolCallEnd {
                content_index,
                tool_call,
                partial,
            } => vec![E::ToolCallEnd {
                content_index,
                tool_call,
                partial: self.update(&partial, None),
            }],
        }
    }

    fn visible_len(&self, index: usize) -> usize {
        self.blocks.get(&index).map_or(0, |s| s.clean.len())
    }

    fn visible_from(&self, index: usize, from: usize) -> String {
        self.blocks
            .get(&index)
            .and_then(|s| s.clean.get(from..))
            .unwrap_or_default()
            .to_owned()
    }

    fn visible_or(&self, index: usize, fallback: String) -> String {
        self.blocks
            .get(&index)
            .map_or(fallback, |s| s.clean.clone())
    }

    fn update_end(
        &mut self,
        partial: &Arc<AssistantMessage>,
        index: usize,
        content: &str,
    ) -> Arc<AssistantMessage> {
        let tools = &self.tools;
        self.blocks.entry(index).or_default().finish(content, tools);
        self.update(partial, Some(index))
    }

    /// Feed every text and thinking block of `partial` and return it with the markup removed
    /// (the same `Arc` when there was nothing to remove).
    fn update(
        &mut self,
        partial: &Arc<AssistantMessage>,
        finished: Option<usize>,
    ) -> Arc<AssistantMessage> {
        let mut differs = false;
        for (i, block) in partial.content.iter().enumerate() {
            let Some(raw) = block_text(block) else {
                continue;
            };
            let scrubber = self.blocks.entry(i).or_default();
            if finished == Some(i) {
                scrubber.finish(raw, &self.tools);
            } else {
                scrubber.feed(raw, &self.tools);
            }
            differs |= scrubber.clean.len() != raw.len();
        }
        if !differs {
            return Arc::clone(partial);
        }
        let mut message = (**partial).clone();
        for (i, block) in message.content.iter_mut().enumerate() {
            if let Some(scrubber) = self.blocks.get(&i)
                && block_text(block).is_some()
            {
                set_block_text(block, &scrubber.clean);
            }
        }
        Arc::new(message)
    }
}

/// Forward `inner` through a [`StreamScrubber`]. `tools` are the request's tools (for argument
/// types).
pub fn scrub_stream(
    inner: AssistantMessageEventStream,
    tools: Vec<Tool>,
) -> AssistantMessageEventStream {
    forward(inner, StreamScrubber::new(tools))
}

/// Forward `inner`, repairing only the final message's structured tool calls
/// ([`repair_tool_calls`], #2203).
pub fn repair_stream(inner: AssistantMessageEventStream) -> AssistantMessageEventStream {
    forward(inner, StreamScrubber::calls_only())
}

fn forward(
    mut inner: AssistantMessageEventStream,
    mut scrubber: StreamScrubber,
) -> AssistantMessageEventStream {
    let (mut producer, consumer) = create_assistant_message_event_stream();
    tokio::spawn(async move {
        while let Some(event) = inner.next().await {
            let terminal = event.is_terminal();
            for out in scrubber.on_event(event) {
                if !producer.push(out) {
                    // The consumer went away; dropping `inner` tells the provider.
                    return;
                }
            }
            if terminal {
                return;
            }
        }
    });
    consumer
}

#[cfg(test)]
mod tests {
    use super::*;
    use rpi_ai::types::{Api, Schema, TextContent, ThinkingContent};
    use serde_json::json;

    /// The line from the UX review (#2175, finding 4), as MiMo streamed it.
    const MIMO: &str = "<tool_call><function=ls><parameter=path>/Volumes/x/harness-play</parameter></function></tool_call>";

    fn tools() -> Vec<Tool> {
        vec![
            Tool {
                name: "ls".into(),
                description: "list".into(),
                parameters: Schema(json!({
                    "type": "object",
                    "properties": {"path": {"type": "string"}, "limit": {"type": "integer"}}
                })),
                constrained_sampling: None,
            },
            Tool {
                name: "bash".into(),
                description: "bash".into(),
                parameters: Schema(json!({
                    "type": "object",
                    "properties": {"command": {"type": "string"}}
                })),
                constrained_sampling: None,
            },
        ]
    }

    fn ls_call() -> TextCall {
        TextCall {
            name: "ls".into(),
            arguments: json!({"path": "/Volumes/x/harness-play"}),
        }
    }

    /// Feed `text` one byte boundary at a time; with `strict`, the visible text must never show
    /// markup at any point.
    fn feed_bytewise(text: &str, strict: bool) -> Scrubber {
        let mut s = Scrubber::default();
        let tools = tools();
        for end in (1..=text.len()).filter(|&e| text.is_char_boundary(e)) {
            s.feed(&text[..end], &tools);
            if !strict {
                continue;
            }
            for tag in ["<tool", "<func", "<param", "</"] {
                assert!(
                    !s.visible().contains(tag),
                    "markup visible after {end} bytes: {:?}",
                    s.visible()
                );
            }
        }
        s.finish(text, &tools);
        s
    }

    #[test]
    fn mimo_markup_split_anywhere_is_hidden_and_parsed() {
        let text = format!("Let me look.\n{MIMO}\nDone.");
        let s = feed_bytewise(&text, true);
        assert_eq!(s.visible(), "Let me look.\n\nDone.");
        assert_eq!(s.calls(), &[ls_call()]);
    }

    #[test]
    fn other_shapes_parse() {
        let tools = tools();
        let cases = [
            (
                r#"<tool_call>{"name": "ls", "arguments": {"path": "/r", "limit": 5}}</tool_call>"#,
                json!({"path": "/r", "limit": 5}),
            ),
            (
                r#"<tool_call>{"name": "ls", "arguments": "{\"path\": \"/r\"}"}</tool_call>"#,
                json!({"path": "/r"}),
            ),
            (
                "<function=ls>\n<parameter=path>\n/r\n</parameter>\n<parameter=limit>\n5\n</parameter>\n</function>\n</tool_call>",
                json!({"path": "/r", "limit": 5}),
            ),
            // A path that looks like a number stays a string: the schema says string.
            (
                "<tool_call><function=ls><parameter=path>123</parameter></function></tool_call>",
                json!({"path": "123"}),
            ),
        ];
        for (text, args) in cases {
            let mut s = Scrubber::default();
            s.finish(text, &tools);
            assert_eq!(s.visible().trim(), "", "{text}");
            assert_eq!(
                s.calls(),
                &[TextCall {
                    name: "ls".into(),
                    arguments: args
                }],
                "{text}"
            );
        }
    }

    #[test]
    fn ordinary_angle_brackets_are_left_alone() {
        for text in [
            "if a < b && c<d { <div> }",
            "Vec<String> and <tool> and <functional>",
            "ends with <tool",
            "ends with <",
        ] {
            let s = feed_bytewise(text, false);
            assert_eq!(s.visible(), text);
            assert!(s.calls().is_empty());
        }
    }

    #[test]
    fn an_unclosed_call_at_the_end_is_hidden_and_parsed_best_effort() {
        let s = feed_bytewise("ok <tool_call><function=ls><parameter=path>/r", true);
        assert_eq!(s.visible(), "ok ");
        assert_eq!(
            s.calls(),
            &[TextCall {
                name: "ls".into(),
                arguments: json!({"path": "/r"})
            }]
        );
    }

    fn message(content: Vec<Content>) -> AssistantMessage {
        let mut m = AssistantMessage::empty(Api::Other("t".into()), "t", "m", 0);
        m.content = content;
        m.stop_reason = StopReason::Stop;
        m
    }

    fn text(t: &str) -> Content {
        Content::Text(TextContent {
            kind: Default::default(),
            text: t.into(),
            text_signature: None,
        })
    }

    fn thinking(t: &str) -> Content {
        Content::Thinking(ThinkingContent {
            kind: Default::default(),
            thinking: t.into(),
            thinking_signature: None,
            redacted: false,
        })
    }

    #[test]
    fn a_call_in_thinking_and_text_runs_once() {
        let mut m = message(vec![thinking(&format!("plan: {MIMO}")), text(MIMO)]);
        assert!(scrub_message(&mut m, &tools(), true));
        let calls: Vec<_> = m
            .content
            .iter()
            .filter_map(|c| match c {
                Content::ToolCall(t) => Some(t),
                _ => None,
            })
            .collect();
        assert_eq!(calls.len(), 1);
        assert_eq!(m.stop_reason, StopReason::ToolUse);
        // The text block was only markup, so it is gone; the thinking keeps its words.
        assert_eq!(m.content.len(), 2);
        assert!(matches!(&m.content[0], Content::Thinking(t) if t.thinking == "plan: "));
    }
}
