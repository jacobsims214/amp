//! Secret redaction before signing (protocol.md section 6.4, R-M11, T-H-5).
//!
//! Every string of every outgoing event body passes through [`Redactor::redact_value`]:
//! - **known values**: the AMP OAuth tokens, the connect token, provider keys and secret header
//!   values, the pairing secret, and the values of secret-looking environment variables
//!   (`*_KEY`, `*_TOKEN`, `*_SECRET`, `*PASSWORD*`, …; `AMP_TOKEN` included), each replaced
//!   wherever it appears;
//! - **token shapes**: well-known key and token formats (Anthropic/OpenAI `sk-…`, GitHub `ghp_…` /
//!   `github_pat_…`, Slack `xox?-…`, AWS `AKIA…`, Google `AIza…`, AMP `amp_…` and `rct_…`, JWTs,
//!   `Bearer …` values, PEM private keys), whether or not the harness knows the value.
//!
//! The replacement is [`REDACTED`]. A secret split across two streamed deltas is caught by
//! [`DeltaGuard`], which holds back a trailing token-like word, and any tail that could be the
//! start of a known value whatever its characters (#2391, L1), until the next delta of the same
//! message (or a flush) completes it; a field the feed cut short loses its last partial word.
//!
//! The known values come from sources ([`Redactor::add_source`]) that are read again whenever the
//! epoch ([`Redactor::set_epoch`]) moves: a key stored with `/connect`, an MCP secret read or
//! changed, a `/reload` (#2391, M5).

use std::sync::{Arc, RwLock};

use regex::Regex;
use serde_json::Value;

/// What a secret is replaced with (section 6.4).
pub const REDACTED: &str = "[redacted]";

/// Known values shorter than this are not redacted (they would match ordinary text).
pub const MIN_SECRET_LEN: usize = 8;

/// Environment variable names whose values count as secrets.
pub fn is_secret_env_name(name: &str) -> bool {
    let n = name.to_ascii_uppercase();
    const HINTS: [&str; 9] = [
        "KEY",
        "TOKEN",
        "SECRET",
        "PASSWORD",
        "PASSWD",
        "CREDENTIAL",
        "AUTH",
        "PRIVATE",
        "COOKIE",
    ];
    HINTS.iter().any(|h| n.contains(h))
        && !matches!(
            n.as_str(),
            "SSH_AUTH_SOCK" | "GPG_AGENT_INFO" | "AMP_HARNESS_KEYCHAIN_PROMPTS"
        )
}

/// A source of known secret values, read when the link starts and again on demand.
pub type SecretSource = Arc<dyn Fn() -> Vec<String> + Send + Sync>;

/// A cheap counter that moves whenever a source may have new values ([`Redactor::set_epoch`]).
pub type SecretEpoch = Arc<dyn Fn() -> u64 + Send + Sync>;

fn patterns() -> &'static [Regex] {
    static PATTERNS: std::sync::OnceLock<Vec<Regex>> = std::sync::OnceLock::new();
    PATTERNS.get_or_init(|| {
        [
            r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z0-9 ]*PRIVATE KEY-----|$)",
            r"\bsk-ant-[A-Za-z0-9_\-]{10,}",
            r"\bsk-(?:proj-|or-v1-|live_|test_)?[A-Za-z0-9_\-]{20,}",
            r"\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}",
            r"\bgithub_pat_[A-Za-z0-9_]{20,}",
            r"\bxox[abposr]-[A-Za-z0-9\-]{10,}",
            r"\b(?:AKIA|ASIA)[0-9A-Z]{16}\b",
            r"\bAIza[0-9A-Za-z_\-]{30,}",
            r"\bamp_[A-Za-z0-9_\-]{16,}",
            r"\brct_[A-Za-z0-9_\-]{20,}",
            r"\beyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}",
        ]
        .iter()
        .map(|p| Regex::new(p).expect("a valid redaction pattern"))
        .collect()
    })
}

fn bearer() -> &'static Regex {
    static RE: std::sync::OnceLock<Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| {
        Regex::new(r"(?i)\b(bearer|basic)(\s+)[A-Za-z0-9._~+/=\-]{12,}")
            .expect("a valid redaction pattern")
    })
}

/// Redacts known secrets and token shapes. Cheap to clone; shared by the link's tasks.
#[derive(Clone, Default)]
pub struct Redactor {
    values: Arc<RwLock<Vec<String>>>,
    sources: Arc<RwLock<Vec<SecretSource>>>,
    /// The epoch and the value it had when the sources were last read.
    epoch: Arc<RwLock<Option<(SecretEpoch, u64)>>>,
}

impl std::fmt::Debug for Redactor {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Redactor")
            .field("values", &self.values.read().map(|v| v.len()).unwrap_or(0))
            .finish()
    }
}

impl Redactor {
    pub fn new() -> Self {
        Self::default()
    }

    /// Adds known secret values (blank and short ones are ignored).
    pub fn add_values<I, S>(&self, values: I)
    where
        I: IntoIterator<Item = S>,
        S: AsRef<str>,
    {
        let mut v = self.values.write().unwrap_or_else(|e| e.into_inner());
        for s in values {
            let s = s.as_ref().trim();
            if s.len() >= MIN_SECRET_LEN && !v.iter().any(|x| x == s) {
                v.push(s.to_owned());
            }
        }
        // Longest first, so a secret that contains another is replaced whole.
        v.sort_by_key(|s| std::cmp::Reverse(s.len()));
    }

    /// Adds a source of values ([`Redactor::refresh`] reads it).
    pub fn add_source(&self, source: SecretSource) {
        self.add_values(source());
        self.sources
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .push(source);
    }

    /// The values of secret-looking variables in `env`.
    pub fn add_env(&self, env: impl IntoIterator<Item = (String, String)>) {
        self.add_values(
            env.into_iter()
                .filter(|(k, _)| is_secret_env_name(k))
                .map(|(_, v)| v),
        );
    }

    /// Reads the sources again whenever `epoch` moves (checked before each redaction), so a key
    /// stored with `/connect`, an MCP secret, or a `/reload` after the link started is redacted
    /// too (#2391, M5).
    pub fn set_epoch(&self, epoch: SecretEpoch) {
        let now = epoch();
        *self.epoch.write().unwrap_or_else(|e| e.into_inner()) = Some((epoch, now));
    }

    /// [`Redactor::refresh`] if the epoch moved since the sources were last read.
    fn refresh_if_moved(&self) {
        let moved = {
            let guard = self.epoch.read().unwrap_or_else(|e| e.into_inner());
            match &*guard {
                Some((epoch, seen)) => {
                    let now = epoch();
                    (now != *seen).then_some(now)
                }
                None => None,
            }
        };
        let Some(now) = moved else {
            return;
        };
        if let Some((_, seen)) = self
            .epoch
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .as_mut()
        {
            *seen = now;
        }
        self.refresh();
    }

    /// Reads every source again (keys added with `/connect` since).
    pub fn refresh(&self) {
        let sources = self
            .sources
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .clone();
        for s in sources {
            self.add_values(s());
        }
    }

    /// `text` with every known value and token shape replaced.
    pub fn redact(&self, text: &str) -> String {
        self.refresh_if_moved();
        let mut out = text.to_owned();
        {
            let values = self.values.read().unwrap_or_else(|e| e.into_inner());
            for v in values.iter() {
                if out.contains(v.as_str()) {
                    out = out.replace(v.as_str(), REDACTED);
                }
            }
        }
        for re in patterns() {
            if re.is_match(&out) {
                out = re.replace_all(&out, REDACTED).into_owned();
            }
        }
        if bearer().is_match(&out) {
            out = bearer()
                .replace_all(&out, |c: &regex::Captures<'_>| {
                    format!("{}{}{REDACTED}", &c[1], &c[2])
                })
                .into_owned();
        }
        out
    }

    /// Where a streamed `text` can be cut so that no known value is split (#2391, L1): at most
    /// `cut`, never inside a whole known value, and never after the start of a tail that is the
    /// beginning of one (the rest may come in the next delta). Any characters, not only
    /// token-like ones. Always a char boundary.
    pub fn safe_cut(&self, text: &str, cut: usize) -> usize {
        self.refresh_if_moved();
        let values = self.values.read().unwrap_or_else(|e| e.into_inner());
        let bytes = text.as_bytes();
        let mut cut = cut.min(text.len());
        // A tail that a known value starts with.
        for v in values.iter() {
            let vb = v.as_bytes();
            let from = text.len().saturating_sub(vb.len() - 1);
            if let Some(i) = (from..text.len())
                .find(|&i| text.is_char_boundary(i) && vb.starts_with(&bytes[i..]))
            {
                cut = cut.min(i);
            }
        }
        // A whole value across the cut (moving the cut back may put another one across it).
        loop {
            let before = cut;
            for v in values.iter() {
                for (i, _) in text.match_indices(v.as_str()) {
                    if i < cut && cut < i + v.len() {
                        cut = i;
                    }
                }
            }
            if cut == before {
                break;
            }
        }
        while !text.is_char_boundary(cut) {
            cut -= 1;
        }
        cut
    }

    /// Redacts every string (keys too) of `value` in place.
    pub fn redact_value(&self, value: &mut Value) {
        match value {
            Value::String(s) => {
                let r = self.redact(s);
                if r != *s {
                    *s = r;
                }
            }
            Value::Array(items) => items.iter_mut().for_each(|v| self.redact_value(v)),
            Value::Object(map) => {
                let keys: Vec<String> = map.keys().cloned().collect();
                for k in keys {
                    let rk = self.redact(&k);
                    if rk != k
                        && let Some(v) = map.remove(&k)
                    {
                        map.insert(rk, v);
                    }
                }
                map.values_mut().for_each(|v| self.redact_value(v));
            }
            _ => {}
        }
    }
}

/// A token-like character: a split secret can only hide in a run of these.
fn token_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || matches!(c, '_' | '-' | '.' | '/' | '+' | '=' | '~')
}

/// The byte index where `s`'s trailing token-like word starts (`s.len()` when there is none).
pub fn trailing_word_start(s: &str) -> usize {
    let mut start = s.len();
    for (i, c) in s.char_indices().rev() {
        if token_char(c) {
            start = i;
        } else {
            break;
        }
    }
    start
}

/// `s` without its trailing partial word (for a field the feed cut short: a secret cut in half
/// would no longer match anything).
pub fn drop_partial_tail(s: &str) -> &str {
    &s[..trailing_word_start(s)]
}

/// Holds back the trailing token-like word of a delta stream, so redaction sees whole words.
#[derive(Debug, Default)]
pub struct DeltaGuard {
    /// `(message_id, thinking)` of the held text, and the text.
    held: Option<(String, bool, String)>,
    /// When the held text last grew.
    since: Option<std::time::Instant>,
}

impl DeltaGuard {
    /// The text to send now for a delta of `message_id`: what was held for it plus `delta`,
    /// without its trailing word or any tail that could start one of `redactor`'s known values
    /// (held until the next delta or a flush). Anything held for another message comes back in
    /// `flushed` first.
    pub fn push(
        &mut self,
        message_id: &str,
        thinking: bool,
        delta: &str,
        redactor: &Redactor,
    ) -> (Option<(String, bool, String)>, String) {
        let mut flushed = None;
        let mut text = String::new();
        match self.held.take() {
            Some((id, th, held)) if id == message_id && th == thinking => text = held,
            Some(other) => flushed = Some(other),
            None => {}
        }
        text.push_str(delta);
        self.since = Some(std::time::Instant::now());
        let cut = redactor.safe_cut(&text, trailing_word_start(&text));
        // A whole delta of one word (no boundary yet) is held, up to a bound.
        if cut == 0 && text.len() < 8 * 1024 {
            self.held = Some((message_id.to_owned(), thinking, text));
            return (flushed, String::new());
        }
        let cut = if cut == 0 { text.len() } else { cut };
        let rest = text.split_off(cut);
        if !rest.is_empty() {
            self.held = Some((message_id.to_owned(), thinking, rest));
        }
        (flushed, text)
    }

    /// Whatever is held: `(message_id, thinking, text)`.
    pub fn flush(&mut self) -> Option<(String, bool, String)> {
        self.held.take().filter(|(_, _, t)| !t.is_empty())
    }

    pub fn is_holding(&self) -> bool {
        self.held.is_some()
    }

    /// Whether the held text has waited at least `quiet` for more (then flush it: the stream
    /// paused or ended).
    pub fn is_stale(&self, quiet: std::time::Duration) -> bool {
        self.held.is_some() && self.since.is_some_and(|t| t.elapsed() >= quiet)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn known_values_and_shapes_are_redacted() {
        let r = Redactor::new();
        r.add_values(["my-provider-key-123456", "short"]);
        r.add_env([
            (
                "OPENROUTER_API_KEY".to_owned(),
                "or-secret-value-9876".to_owned(),
            ),
            ("HOME".to_owned(), "/Users/someone-long".to_owned()),
        ]);
        let text = "key my-provider-key-123456 and or-secret-value-9876 at /Users/someone-long; \
                    short stays; sk-ant-api03-abcdefghijklmnop, ghp_abcdefghijklmnopqrstuvwxyz, \
                    Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123, \
                    eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlLXBhcnQ";
        let out = r.redact(text);
        for secret in [
            "my-provider-key-123456",
            "or-secret-value-9876",
            "sk-ant-api03",
            "ghp_abcdef",
            "abcdefghijklmnopqrstuvwxyz0123",
            "eyJhbGci",
        ] {
            assert!(!out.contains(secret), "{secret} in {out}");
        }
        assert!(out.contains("/Users/someone-long"));
        assert!(out.contains("short stays"));
        assert!(out.contains("Bearer [redacted]"));
    }

    #[test]
    fn delta_guard_rejoins_a_split_secret() {
        let r = Redactor::new();
        r.add_values(["supersecretvalue42"]);
        let mut g = DeltaGuard::default();
        let (_, a) = g.push("m1", false, "the key is supersec", &r);
        let (_, b) = g.push("m1", false, "retvalue42 ok ", &r);
        let c = g.flush().map(|x| x.2).unwrap_or_default();
        let sent = format!("{}{}{}", r.redact(&a), r.redact(&b), r.redact(&c));
        assert_eq!(sent, "the key is [redacted] ok ");
        assert!(!a.contains("supersec"));
    }

    /// A tiny deterministic PRNG (xorshift64*), so the property test needs no extra crate and
    /// a failure is reproducible from its seed.
    struct Rng(u64);

    impl Rng {
        fn next(&mut self) -> u64 {
            self.0 ^= self.0 >> 12;
            self.0 ^= self.0 << 25;
            self.0 ^= self.0 >> 27;
            self.0.wrapping_mul(0x2545_F491_4F6C_DD1D)
        }

        fn below(&mut self, n: usize) -> usize {
            (self.next() % n as u64) as usize
        }

        fn pick(&mut self, from: &[char]) -> char {
            from[self.below(from.len())]
        }
    }

    /// L1: a known value made of any characters (`@`, `:`, `!`, spaces, non-ASCII), split
    /// across deltas at random points, never leaves whole or in pieces that rejoin.
    #[test]
    fn a_known_value_split_at_random_points_never_leaks() {
        let secret_chars: Vec<char> = "abcXYZ019@:!#$%&*()[]{}<>?,;'\" |^`é€ -_./+=~"
            .chars()
            .collect();
        let text_chars: Vec<char> = "abc xyz@:!.,\n é€".chars().collect();
        for seed in 1..=3000u64 {
            let mut rng = Rng(seed.wrapping_mul(0x9E37_79B9_7F4A_7C15) | 1);
            let len = MIN_SECRET_LEN + rng.below(24);
            let mut secret: String = (0..len).map(|_| rng.pick(&secret_chars)).collect();
            // A value is trimmed when added; keep its ends non-blank so it is used as written.
            secret = format!("p{}w", secret.trim());
            let r = Redactor::new();
            r.add_values([secret.as_str()]);
            let around = |rng: &mut Rng| -> String {
                (0..rng.below(20)).map(|_| rng.pick(&text_chars)).collect()
            };
            let mut stream = around(&mut rng);
            for _ in 0..1 + rng.below(3) {
                stream.push_str(&secret);
                stream.push_str(&around(&mut rng));
            }
            // Random split points (on char boundaries), 1-6 deltas.
            let bounds: Vec<usize> = stream.char_indices().map(|(i, _)| i).collect();
            let mut cuts: Vec<usize> = (0..rng.below(6))
                .map(|_| bounds[rng.below(bounds.len())])
                .collect();
            cuts.sort_unstable();
            cuts.dedup();
            let mut deltas = Vec::new();
            let mut at = 0;
            for c in cuts.into_iter().chain([stream.len()]) {
                if c > at {
                    deltas.push(&stream[at..c]);
                    at = c;
                }
            }
            let mut g = DeltaGuard::default();
            let mut sent = String::new();
            for d in &deltas {
                let (flushed, now) = g.push("m1", false, d, &r);
                assert!(flushed.is_none());
                sent.push_str(&r.redact(&now));
            }
            if let Some((_, _, rest)) = g.flush() {
                sent.push_str(&r.redact(&rest));
            }
            assert!(
                !sent.contains(&secret),
                "seed {seed}: {secret:?} leaked from {deltas:?} as {sent:?}"
            );
            assert_eq!(
                sent,
                r.redact(&stream),
                "seed {seed}: the deltas sent differ from the whole text redacted"
            );
        }
    }

    #[test]
    fn split_at_a_colon_or_at_sign_is_held() {
        let r = Redactor::new();
        r.add_values(["p@ss:w0rd!xyz123"]);
        let mut g = DeltaGuard::default();
        let (_, a) = g.push("m1", false, "login p@", &r);
        let (_, b) = g.push("m1", false, "ss:", &r);
        let (_, c) = g.push("m1", false, "w0rd!xyz123 done.", &r);
        let rest = g.flush().map(|x| x.2).unwrap_or_default();
        let sent = format!(
            "{}{}{}{}",
            r.redact(&a),
            r.redact(&b),
            r.redact(&c),
            r.redact(&rest)
        );
        assert_eq!(sent, "login [redacted] done.");
    }

    #[test]
    fn sources_are_read_again_when_the_epoch_moves() {
        use std::sync::atomic::{AtomicU64, Ordering};
        let keys = Arc::new(RwLock::new(vec!["first-secret-value".to_owned()]));
        let epoch = Arc::new(AtomicU64::new(1));
        let r = Redactor::new();
        {
            let keys = Arc::clone(&keys);
            r.add_source(Arc::new(move || keys.read().unwrap().clone()));
        }
        {
            let epoch = Arc::clone(&epoch);
            r.set_epoch(Arc::new(move || epoch.load(Ordering::SeqCst)));
        }
        assert_eq!(r.redact("a first-secret-value"), "a [redacted]");
        // A key stored after the link started (`/connect`): unknown until the epoch moves.
        keys.write().unwrap().push("connected-later-key".to_owned());
        assert_eq!(r.redact("b connected-later-key"), "b connected-later-key");
        epoch.fetch_add(1, Ordering::SeqCst);
        assert_eq!(r.redact("c connected-later-key"), "c [redacted]");
        assert_eq!(r.redact("d first-secret-value"), "d [redacted]");
    }
}
