//! Canonical JSON (RFC 8785, JCS) under the protocol's integers-only I-JSON profile
//! (protocol.md section 3.3), and the strict parser every receiver uses.
//!
//! - [`parse_strict`] refuses duplicate keys, any number with a fraction or an exponent (and so
//!   NaN/Infinity, which JSON can't spell anyway), integers outside ±(2^53−1), lone surrogates and
//!   trailing text. The result is a [`serde_json::Value`].
//! - [`canonical`] writes JCS: no whitespace, members sorted by their keys' UTF-16 code units,
//!   strings escaped exactly as ECMAScript's `JSON.stringify` does, integers in plain decimal.
//!
//! Signatures always cover `canonical(parse_strict(received))`, never the received bytes.

use std::fmt;

use serde::de::{self, Deserialize, Deserializer, MapAccess, SeqAccess, Visitor};
use serde_json::{Map, Number, Value};

/// The largest integer the profile allows (2^53 − 1).
pub const MAX_SAFE_INTEGER: i64 = 9_007_199_254_740_991;

/// Why a text is not in the profile. The message is for logs and tests: it never echoes content.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("malformed: {0}")]
pub struct Malformed(pub String);

/// Parses `text` under the profile.
pub fn parse_strict(text: &[u8]) -> Result<Value, Malformed> {
    let mut de = serde_json::Deserializer::from_slice(text);
    let value = Strict::deserialize(&mut de).map_err(|e| Malformed(e.to_string()))?;
    de.end().map_err(|e| Malformed(e.to_string()))?;
    Ok(value.0)
}

/// Checks that `value` (built in code, not parsed) is in the profile: integers only, within range.
pub fn check_profile(value: &Value) -> Result<(), Malformed> {
    match value {
        Value::Number(n) => check_number(n),
        Value::Array(items) => items.iter().try_for_each(check_profile),
        Value::Object(map) => map.values().try_for_each(check_profile),
        _ => Ok(()),
    }
}

fn check_number(n: &Number) -> Result<(), Malformed> {
    if let Some(i) = n.as_i64() {
        if (-MAX_SAFE_INTEGER..=MAX_SAFE_INTEGER).contains(&i) {
            return Ok(());
        }
    } else if let Some(u) = n.as_u64()
        && u <= MAX_SAFE_INTEGER as u64
    {
        return Ok(());
    }
    Err(Malformed(
        "a number is not a safe integer (no floats in signed content)".into(),
    ))
}

/// The JCS form of `value`. `value` must be in the profile ([`check_profile`]); a float is
/// written as its integer part only if it has none, so check first.
pub fn canonical(value: &Value) -> String {
    let mut out = String::new();
    write_value(&mut out, value);
    out
}

/// [`canonical`] after [`check_profile`].
pub fn canonical_checked(value: &Value) -> Result<String, Malformed> {
    check_profile(value)?;
    Ok(canonical(value))
}

fn write_value(out: &mut String, value: &Value) {
    match value {
        Value::Null => out.push_str("null"),
        Value::Bool(true) => out.push_str("true"),
        Value::Bool(false) => out.push_str("false"),
        Value::Number(n) => {
            if let Some(i) = n.as_i64() {
                out.push_str(&i.to_string());
            } else if let Some(u) = n.as_u64() {
                out.push_str(&u.to_string());
            } else {
                // Not in the profile; check_profile refuses it before signing.
                out.push_str(&n.to_string());
            }
        }
        Value::String(s) => write_string(out, s),
        Value::Array(items) => {
            out.push('[');
            for (i, item) in items.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                write_value(out, item);
            }
            out.push(']');
        }
        Value::Object(map) => write_object(out, map),
    }
}

fn write_object(out: &mut String, map: &Map<String, Value>) {
    let mut keys: Vec<&String> = map.keys().collect();
    keys.sort_by(|a, b| a.encode_utf16().cmp(b.encode_utf16()));
    out.push('{');
    for (i, key) in keys.iter().enumerate() {
        if i > 0 {
            out.push(',');
        }
        write_string(out, key);
        out.push(':');
        write_value(out, &map[key.as_str()]);
    }
    out.push('}');
}

/// A string as ECMAScript's `JSON.stringify` writes it.
fn write_string(out: &mut String, s: &str) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\u{08}' => out.push_str("\\b"),
            '\u{0C}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
}

/// A value parsed under the profile.
struct Strict(Value);

impl<'de> Deserialize<'de> for Strict {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        d.deserialize_any(StrictVisitor)
    }
}

struct StrictVisitor;

impl<'de> Visitor<'de> for StrictVisitor {
    type Value = Strict;

    fn expecting(&self, f: &mut fmt::Formatter) -> fmt::Result {
        f.write_str("JSON under the amp-rc/1 profile")
    }

    fn visit_bool<E: de::Error>(self, v: bool) -> Result<Strict, E> {
        Ok(Strict(Value::Bool(v)))
    }

    fn visit_i64<E: de::Error>(self, v: i64) -> Result<Strict, E> {
        if (-MAX_SAFE_INTEGER..=MAX_SAFE_INTEGER).contains(&v) {
            Ok(Strict(Value::from(v)))
        } else {
            Err(E::custom("an integer outside ±(2^53−1)"))
        }
    }

    fn visit_u64<E: de::Error>(self, v: u64) -> Result<Strict, E> {
        if v <= MAX_SAFE_INTEGER as u64 {
            Ok(Strict(Value::from(v)))
        } else {
            Err(E::custom("an integer outside ±(2^53−1)"))
        }
    }

    fn visit_f64<E: de::Error>(self, _: f64) -> Result<Strict, E> {
        Err(E::custom("a non-integer number"))
    }

    fn visit_str<E: de::Error>(self, v: &str) -> Result<Strict, E> {
        Ok(Strict(Value::String(v.to_owned())))
    }

    fn visit_string<E: de::Error>(self, v: String) -> Result<Strict, E> {
        Ok(Strict(Value::String(v)))
    }

    fn visit_unit<E: de::Error>(self) -> Result<Strict, E> {
        Ok(Strict(Value::Null))
    }

    fn visit_none<E: de::Error>(self) -> Result<Strict, E> {
        Ok(Strict(Value::Null))
    }

    fn visit_seq<A: SeqAccess<'de>>(self, mut seq: A) -> Result<Strict, A::Error> {
        let mut items = Vec::new();
        while let Some(Strict(v)) = seq.next_element()? {
            items.push(v);
        }
        Ok(Strict(Value::Array(items)))
    }

    fn visit_map<A: MapAccess<'de>>(self, mut map: A) -> Result<Strict, A::Error> {
        let mut out = Map::new();
        while let Some(key) = map.next_key::<String>()? {
            if out.contains_key(&key) {
                return Err(de::Error::custom("a duplicate key"));
            }
            let Strict(v) = map.next_value()?;
            out.insert(key, v);
        }
        Ok(Strict(Value::Object(out)))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn profile_rejections() {
        for bad in [
            r#"{"a":1,"a":2}"#,
            r#"{"a":1.0}"#,
            r#"{"a":1e3}"#,
            r#"{"a":9007199254740992}"#,
            r#"{"a":-9007199254740992}"#,
            r#"{"a":"\ud800"}"#,
            r#"{"a":1} x"#,
        ] {
            assert!(parse_strict(bad.as_bytes()).is_err(), "{bad}");
        }
        assert!(parse_strict(br#"{"a":9007199254740991,"b":-9007199254740991}"#).is_ok());
    }

    #[test]
    fn escapes_like_json_stringify() {
        let v =
            parse_strict("{\"b\":\"\\u0001\\u001f\\u007f\u{2028}é\",\"a\":[true,null]}".as_bytes())
                .unwrap();
        assert_eq!(
            canonical(&v),
            "{\"a\":[true,null],\"b\":\"\\u0001\\u001f\u{7f}\u{2028}é\"}"
        );
    }
}
