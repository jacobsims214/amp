//! Colour helpers for the theme loader: `#RRGGBB` parsing and the nearest xterm-256 index.
//!
//! The nearest index is what a theme file gets for a role it leaves out of `colors256`. It uses
//! the same method as the #2078 research table: CIELAB distance to the 6x6x6 cube and the grey
//! ramp (indexes 16..=255; 0..=15 are left out because terminals redefine them).

/// Parses `#RRGGBB` (the `#` is required, case does not matter).
pub fn parse_hex(raw: &str) -> Option<(u8, u8, u8)> {
    let hex = raw.strip_prefix('#')?;
    if hex.len() != 6 || !hex.bytes().all(|b| b.is_ascii_hexdigit()) {
        return None;
    }
    let byte = |i: usize| u8::from_str_radix(&hex[i..i + 2], 16).ok();
    Some((byte(0)?, byte(2)?, byte(4)?))
}

/// The RGB value xterm uses for a 256-colour index in 16..=255.
pub fn xterm_rgb(index: u8) -> (u8, u8, u8) {
    const LEVELS: [u8; 6] = [0, 95, 135, 175, 215, 255];
    match index {
        16..=231 => {
            let i = index - 16;
            (
                LEVELS[(i / 36) as usize],
                LEVELS[(i / 6 % 6) as usize],
                LEVELS[(i % 6) as usize],
            )
        }
        232..=255 => {
            let v = 8 + 10 * (index - 232);
            (v, v, v)
        }
        // The 16 system colours vary by terminal; these are xterm's defaults.
        _ => {
            const SYSTEM: [(u8, u8, u8); 16] = [
                (0, 0, 0),
                (205, 0, 0),
                (0, 205, 0),
                (205, 205, 0),
                (0, 0, 238),
                (205, 0, 205),
                (0, 205, 205),
                (229, 229, 229),
                (127, 127, 127),
                (255, 0, 0),
                (0, 255, 0),
                (255, 255, 0),
                (92, 92, 255),
                (255, 0, 255),
                (0, 255, 255),
                (255, 255, 255),
            ];
            SYSTEM[index as usize]
        }
    }
}

/// The xterm-256 index (16..=255) nearest to `rgb` by CIELAB (CIE76) distance.
pub fn nearest_256(rgb: (u8, u8, u8)) -> u8 {
    let target = lab(rgb);
    (16..=255u8)
        .min_by(|&a, &b| {
            let da = dist2(target, lab(xterm_rgb(a)));
            let db = dist2(target, lab(xterm_rgb(b)));
            da.total_cmp(&db)
        })
        .unwrap_or(16)
}

/// WCAG 2 relative luminance (0 = black, 1 = white).
pub fn relative_luminance((r, g, b): (u8, u8, u8)) -> f64 {
    0.2126 * linear(r) + 0.7152 * linear(g) + 0.0722 * linear(b)
}

fn linear(c: u8) -> f64 {
    let c = f64::from(c) / 255.0;
    if c <= 0.04045 {
        c / 12.92
    } else {
        ((c + 0.055) / 1.055).powf(2.4)
    }
}

fn dist2(a: [f64; 3], b: [f64; 3]) -> f64 {
    (a[0] - b[0]).powi(2) + (a[1] - b[1]).powi(2) + (a[2] - b[2]).powi(2)
}

/// sRGB → CIELAB (D65).
fn lab((r, g, b): (u8, u8, u8)) -> [f64; 3] {
    let (r, g, b) = (linear(r), linear(g), linear(b));
    let x = (0.4124 * r + 0.3576 * g + 0.1805 * b) / 0.95047;
    let y = 0.2126 * r + 0.7152 * g + 0.0722 * b;
    let z = (0.0193 * r + 0.1192 * g + 0.9505 * b) / 1.08883;
    let f = |t: f64| {
        if t > 216.0 / 24389.0 {
            t.cbrt()
        } else {
            (24389.0 / 27.0 * t + 16.0) / 116.0
        }
    };
    let (fx, fy, fz) = (f(x), f(y), f(z));
    [116.0 * fy - 16.0, 500.0 * (fx - fy), 200.0 * (fy - fz)]
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hex() {
        assert_eq!(parse_hex("#E8ECF2"), Some((0xE8, 0xEC, 0xF2)));
        assert_eq!(parse_hex("#e8ecf2"), Some((0xE8, 0xEC, 0xF2)));
        assert_eq!(parse_hex("E8ECF2"), None);
        assert_eq!(parse_hex("#E8ECF"), None);
        assert_eq!(parse_hex("#E8ECFG"), None);
        assert_eq!(parse_hex("#E8ÉCF2"), None);
    }

    #[test]
    fn xterm_palette() {
        assert_eq!(xterm_rgb(16), (0, 0, 0));
        assert_eq!(xterm_rgb(231), (255, 255, 255));
        assert_eq!(xterm_rgb(124), (175, 0, 0));
        assert_eq!(xterm_rgb(232), (8, 8, 8));
        assert_eq!(xterm_rgb(255), (238, 238, 238));
    }

    #[test]
    fn nearest_exact_and_close() {
        // Exact palette entries map to themselves.
        assert_eq!(nearest_256((175, 0, 0)), 124);
        assert_eq!(nearest_256((255, 255, 255)), 231);
        assert_eq!(nearest_256((8, 8, 8)), 232);
        // The #2078 table's values for a few AMP tokens.
        assert_eq!(nearest_256(parse_hex("#E8ECF2").unwrap()), 255);
        assert_eq!(nearest_256(parse_hex("#CF4426").unwrap()), 124);
        assert_eq!(nearest_256(parse_hex("#16181B").unwrap()), 234);
    }

    #[test]
    fn luminance() {
        assert!((relative_luminance((255, 255, 255)) - 1.0).abs() < 1e-9);
        assert!(relative_luminance((0, 0, 0)).abs() < 1e-9);
        assert!(relative_luminance((0x06, 0x0A, 0x0C)) < 0.5);
    }
}
