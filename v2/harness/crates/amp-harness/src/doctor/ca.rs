//! doctor's `ca` check: does this machine trust the AMP server's TLS certificate (D2's `--ca-file`)?
//!
//! Before connecting it validates the CA file (readable, PEM certificates, inside their validity
//! dates). After connecting, a TLS failure is classified (unknown issuer, expired, wrong host name,
//! missing serverAuth EKU) so the fix it prints is the one that applies.

use std::path::Path;

use amp_mcp::{Settings, Source, load_ca_file};
use serde::Serialize;
use x509_parser::pem::Pem;

use super::{CheckStatus, Finding};

/// Warn when a CA certificate expires within this many days.
const EXPIRY_WARN_DAYS: i64 = 14;

/// `doctor --json` `.ca`. Add fields, never rename or remove them.
#[derive(Debug, Default, Serialize)]
pub struct CaReport {
    /// `system` (the OS roots), `system + file`, or `none` (plain http).
    pub trust: &'static str,
    pub file: Option<String>,
    pub file_source: Option<String>,
    /// Certificates in the file (when it could be read).
    pub certificates: Vec<CertInfo>,
    /// Why the server's certificate was rejected, when connecting failed on TLS.
    pub tls_error: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct CertInfo {
    pub subject: String,
    /// `YYYY-MM-DD`.
    pub not_after: String,
    pub expired: bool,
}

/// The pre-connect half: the CA file itself. `now` is a Unix timestamp (tests pin it).
pub fn check_file(settings: &Settings, now: i64) -> (CaReport, Finding) {
    let https = settings.url.starts_with("https://");
    // Where the CA file setting came from, phrased as the thing to change.
    let source = match &settings.ca_file_source {
        Some(Source::Env(var)) => var.clone(),
        Some(Source::ConfigFile(path)) => format!("[server] ca_file in {}", path.display()),
        _ => "--ca-file".to_owned(),
    };
    let mut report = CaReport {
        trust: if https { "system" } else { "none" },
        file: settings.ca_file.as_ref().map(|p| p.display().to_string()),
        file_source: settings.ca_file_source.as_ref().map(ToString::to_string),
        ..CaReport::default()
    };
    let Some(path) = settings.ca_file.as_deref() else {
        let finding = if https {
            Finding::new(CheckStatus::Ok, "the OS trust store (no CA file)")
        } else {
            Finding::new(CheckStatus::Skip, "plain http: no TLS")
        };
        return (report, finding);
    };
    if !https {
        let finding = Finding::new(
            CheckStatus::Warn,
            format!(
                "CA file {} is unused: the server URL is plain http",
                path.display()
            ),
        )
        .fix(format!(
            "drop the CA file ({source}), or use the server's https:// URL"
        ));
        return (report, finding);
    }
    report.trust = "system + file";

    if let Err(e) = load_ca_file(path) {
        let finding = Finding::new(CheckStatus::Fail, e.to_string()).fix(format!(
            "point {source} at the server's CA certificate in PEM form (docker-desktop cluster: \
             amp-local-ca.crt, exported as in the KB how-to/local-dev-setup.md)"
        ));
        return (report, finding);
    }
    report.certificates = read_certs(path, now);

    let expired: Vec<&CertInfo> = report.certificates.iter().filter(|c| c.expired).collect();
    if let Some(first) = expired.first() {
        let finding = Finding::new(
            CheckStatus::Fail,
            format!(
                "CA certificate {} in {} expired on {}",
                first.subject,
                path.display(),
                first.not_after
            ),
        )
        .fix("export the server's current CA certificate again and point --ca-file / AMP_CA_FILE at it");
        return (report, finding);
    }
    let soon = report
        .certificates
        .iter()
        .find(|c| days_left(&c.not_after, now).is_some_and(|d| d <= EXPIRY_WARN_DAYS));
    let summary = summarize(&report.certificates, path);
    let finding = match soon {
        Some(c) => Finding::new(
            CheckStatus::Warn,
            format!("{summary}; {} expires on {}", c.subject, c.not_after),
        )
        .fix("renew the CA soon and export it again; logins will fail once it expires"),
        None => Finding::new(
            CheckStatus::Ok,
            format!("{summary} on top of the OS trust store"),
        ),
    };
    (report, finding)
}

/// The post-connect half: when connecting failed on TLS, what to do about it. `None` when the error
/// is not a certificate problem.
pub fn classify_tls(error: &str, settings: &Settings) -> Option<Finding> {
    let lower = error.to_ascii_lowercase();
    let tls = [
        "invalid peer certificate",
        "certificate verify failed",
        "unknownissuer",
        "unknown issuer",
        "self-signed",
        "self signed",
    ]
    .iter()
    .any(|needle| lower.contains(needle));
    if !tls {
        return None;
    }
    let host = url_host(&settings.url);
    let (detail, fix) = if lower.contains("eku") || lower.contains("extended key usage") {
        (
            format!(
                "{host}'s certificate has no serverAuth extended key usage, which this OS rejects"
            ),
            "reissue the server certificate with `usages: [server auth]` (cert-manager); the AMP \
             chart's amp-ca-issuer does this since #2086"
                .to_owned(),
        )
    } else if lower.contains("expired") {
        (
            format!("{host}'s certificate (or its CA) has expired"),
            "renew the server certificate; if you pass a CA file, export the current CA again"
                .to_owned(),
        )
    } else if lower.contains("notvalidforname") || lower.contains("not valid for name") {
        (
            format!("{host}'s certificate does not name {host}"),
            "connect with the host name the certificate was issued for (docker-desktop cluster: \
             https://amp.local/mcp)"
                .to_owned(),
        )
    } else if let Some(path) = &settings.ca_file {
        (
            format!(
                "{host}'s certificate is not signed by the CA in {}",
                path.display()
            ),
            "export the CA that signed the server's current certificate and point --ca-file / \
             AMP_CA_FILE at it"
                .to_owned(),
        )
    } else {
        (
            format!("{host}'s certificate is not signed by a CA this machine trusts"),
            "pass --ca-file <the server's CA, PEM> or set AMP_CA_FILE (docker-desktop cluster: \
             amp-local-ca.crt, see the KB how-to/local-dev-setup.md)"
                .to_owned(),
        )
    };
    Some(Finding::new(CheckStatus::Fail, detail).fix(fix))
}

fn url_host(url: &str) -> &str {
    let rest = url.split_once("://").map_or(url, |(_, r)| r);
    rest.split(['/', '?', '#']).next().unwrap_or(rest)
}

fn read_certs(path: &Path, now: i64) -> Vec<CertInfo> {
    let Ok(bytes) = std::fs::read(path) else {
        return Vec::new();
    };
    Pem::iter_from_buffer(&bytes)
        .filter_map(Result::ok)
        .filter(|pem| pem.label == "CERTIFICATE")
        .filter_map(|pem| {
            let cert = pem.parse_x509().ok()?;
            let not_after = cert.validity().not_after;
            let subject = cert
                .subject()
                .iter_common_name()
                .next()
                .and_then(|cn| cn.as_str().ok())
                .map(|cn| format!("CN={cn}"))
                .unwrap_or_else(|| cert.subject().to_string());
            Some(CertInfo {
                subject,
                not_after: not_after.to_datetime().date().to_string(),
                expired: not_after.timestamp() < now,
            })
        })
        .collect()
}

fn summarize(certs: &[CertInfo], path: &Path) -> String {
    match certs {
        [one] => format!(
            "{} from {} (valid until {})",
            one.subject,
            path.display(),
            one.not_after
        ),
        many => format!("{} certificates from {}", many.len(), path.display()),
    }
}

/// Whole days from `now` until the end of `date` (`YYYY-MM-DD`).
fn days_left(date: &str, now: i64) -> Option<i64> {
    let mut parts = date.splitn(3, '-').map(|p| p.parse::<i64>().ok());
    let (y, m, d) = (parts.next()??, parts.next()??, parts.next()??);
    Some((days_from_civil(y, m, d) * 86_400 - now) / 86_400)
}

/// Days since 1970-01-01 (Howard Hinnant's algorithm).
fn days_from_civil(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = y.div_euclid(400);
    let yoe = y - era * 400;
    let doy = (153 * (m + if m > 2 { -3 } else { 9 }) + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + doe - 719_468
}

#[cfg(test)]
mod tests {
    use super::*;
    use amp_mcp::{Overrides, Settings};

    fn settings(url: &str, ca: Option<&Path>) -> Settings {
        Settings::resolve_with_env(
            &Overrides {
                url: Some(url.to_owned()),
                ca_file: ca.map(Path::to_path_buf),
                ..Default::default()
            },
            &|_| None,
        )
        .unwrap()
    }

    #[test]
    fn civil_days() {
        assert_eq!(days_from_civil(1970, 1, 1), 0);
        assert_eq!(days_from_civil(2026, 10, 22), 20_748);
        assert_eq!(days_left("2026-10-25", 20_748 * 86_400), Some(3));
    }

    #[test]
    fn no_ca_file() {
        let (r, f) = check_file(&settings("https://amp.example/mcp", None), 0);
        assert_eq!((f.status, r.trust), (CheckStatus::Ok, "system"));
        let (r, f) = check_file(&settings("http://localhost:8000/mcp", None), 0);
        assert_eq!((f.status, r.trust), (CheckStatus::Skip, "none"));
    }

    #[test]
    fn tls_errors_get_the_matching_fix() {
        let s = settings("https://amp.local/mcp", None);
        let f = classify_tls(
            "connect to x: ... invalid peer certificate: UnknownIssuer",
            &s,
        )
        .unwrap();
        assert_eq!(f.status, CheckStatus::Fail);
        assert!(f.fix.as_deref().unwrap().contains("--ca-file"), "{f:?}");
        assert!(f.detail.contains("amp.local"), "{f:?}");

        let f = classify_tls("invalid peer certificate: Other(OtherError(EkuError))", &s).unwrap();
        assert!(f.fix.unwrap().contains("server auth"));
        let f = classify_tls("invalid peer certificate: Expired", &s).unwrap();
        assert!(f.detail.contains("expired"));
        let f = classify_tls("invalid peer certificate: NotValidForName", &s).unwrap();
        assert!(f.detail.contains("does not name"));

        let with_file = settings("https://amp.local/mcp", Some(Path::new("/x/ca.pem")));
        let f = classify_tls("invalid peer certificate: UnknownIssuer", &with_file).unwrap();
        assert!(f.detail.contains("/x/ca.pem"), "{f:?}");

        assert!(classify_tls("connection refused", &s).is_none());
    }
}
