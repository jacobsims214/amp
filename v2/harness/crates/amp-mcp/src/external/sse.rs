//! Legacy HTTP+SSE (MCP 2024-11-05) for third-party servers (H5, #2238).
//!
//! rmcp 3.6.0 has no client for it ("legacy" there means the `initialize` era over Streamable
//! HTTP), so this is ours, on the same reqwest client and `sse-stream` parser:
//!
//! 1. `GET <url>` with `Accept: text/event-stream` opens the server-to-client stream.
//! 2. The server's first `endpoint` event names the URL to POST to (usually
//!    `/messages?sessionId=..`). It must be on the stream's origin: a server can't send our
//!    headers and tokens to another host.
//! 3. Every client message is a `POST` of one JSON-RPC message to that URL (202 Accepted).
//!    Responses, requests and notifications from the server arrive as `message` events.
//!
//! Headers, `bearer_env` and a stored OAuth login come from [`super::http::prepare`], so an SSE
//! server authenticates exactly like a Streamable HTTP one: the configured headers on every GET
//! and POST, `Authorization: Bearer` from the env or from rmcp's refreshing `AuthClient`.
//!
//! Used for `type = "sse"` ([`HttpServerOptions::legacy_sse`]) and as `type = "http"`'s fallback
//! when the server refuses the Streamable HTTP POST with 400, 404 or 405
//! ([`super::http::wants_sse_fallback`]). The stream can't be resumed (the session lives on it),
//! so when it ends the client is closed and the hub (H6) reconnects.

use std::time::Duration;

use bytes::Bytes;
use futures::{Stream, StreamExt};
use reqwest::{
    StatusCode, Url,
    header::{ACCEPT, CONTENT_TYPE, HeaderMap, WWW_AUTHENTICATE},
};
use rmcp::{
    RoleClient,
    model::{ClientJsonRpcMessage, ServerJsonRpcMessage},
    service::ClientInitializeError,
    transport::{
        DynamicTransportError, Transport,
        auth::{AuthClient, AuthError},
        streamable_http_client::AuthRequiredError,
    },
};
use sse_stream::SseStream;
use tokio::{sync::mpsc, task::JoinHandle};

use super::{
    ExternalClient, ExternalOptions, Secret, auth_hint,
    http::{HttpServerOptions, PreparedHttp, prepare_bounded},
};
use crate::{CredentialBackend, McpError, ProtocolMode};

/// The largest single event we accept (as rmcp's Streamable HTTP client): a bigger one ends the
/// stream instead of growing memory without bound.
pub const MAX_SSE_EVENT_BYTES: usize = 16 * 1024 * 1024;

/// How long the server may take, once the stream is open, to send its `endpoint` event. Legacy
/// servers send it at once; a Streamable HTTP server's GET stream never does.
pub const ENDPOINT_TIMEOUT: Duration = Duration::from_secs(10);

/// Server messages waiting for rmcp.
const INBOUND_BUFFER: usize = 64;

/// What went wrong on the legacy SSE transport. Never holds a header value or token; URLs are
/// shown as origins.
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum SseError {
    /// The server answered with a non-success status. A 401/403 carries the
    /// `WWW-Authenticate` challenge, when there is one, as its source.
    #[error("legacy SSE: HTTP {status}")]
    Status {
        status: StatusCode,
        #[source]
        challenge: Option<AuthRequiredError>,
    },
    /// The GET was answered, but not with an event stream.
    #[error("legacy SSE: the server answered with {0}, not an event stream")]
    NotEventStream(String),
    /// The stream opened but no `endpoint` event came (it ended, or [`ENDPOINT_TIMEOUT`] passed).
    #[error("legacy SSE: {0}")]
    NoEndpoint(String),
    #[error("legacy SSE: request failed")]
    Request(#[source] reqwest::Error),
    /// The stored OAuth login could not give a token (expired and not refreshable).
    #[error("legacy SSE: OAuth")]
    OAuth(#[source] AuthError),
    /// The server broke the protocol (a bad or foreign endpoint, an oversized event).
    #[error("legacy SSE: {0}")]
    Protocol(String),
}

impl SseError {
    /// The server does not speak legacy SSE at all (as opposed to speaking it and failing): for
    /// `type = "http"`'s fallback, the Streamable HTTP error is the one to report then.
    fn is_not_sse(&self) -> bool {
        match self {
            Self::Status { status, .. } => {
                *status != StatusCode::UNAUTHORIZED && *status != StatusCode::FORBIDDEN
            }
            Self::NotEventStream(_) | Self::NoEndpoint(_) => true,
            _ => false,
        }
    }
}

/// The HTTP client and credentials, shared by the GET and every POST.
#[derive(Clone)]
struct Requester {
    http: reqwest::Client,
    headers: HeaderMap,
    bearer: Option<Secret>,
    oauth: Option<AuthClient<reqwest::Client>>,
}

impl Requester {
    fn new(prepared: &PreparedHttp) -> Self {
        Self {
            http: prepared.http.clone(),
            headers: prepared
                .headers
                .iter()
                .map(|(k, v)| (k.clone(), v.clone()))
                .collect(),
            bearer: prepared.bearer.clone(),
            oauth: prepared.oauth.clone(),
        }
    }

    /// The configured headers and `Authorization` (an OAuth token is refreshed when due).
    async fn authorize(
        &self,
        request: reqwest::RequestBuilder,
    ) -> Result<reqwest::RequestBuilder, SseError> {
        let request = request.headers(self.headers.clone());
        // `bearer_auth` marks the header sensitive.
        if let Some(oauth) = &self.oauth {
            let token = oauth.get_access_token().await.map_err(SseError::OAuth)?;
            return Ok(request.bearer_auth(token));
        }
        Ok(match &self.bearer {
            Some(token) => request.bearer_auth(token.expose()),
            None => request,
        })
    }
}

fn status_error(response: &reqwest::Response) -> SseError {
    let status = response.status();
    let challenge = matches!(status, StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN)
        .then(|| response.headers().get(WWW_AUTHENTICATE))
        .flatten()
        .and_then(|v| v.to_str().ok())
        .map(|v| AuthRequiredError::new(v.to_owned()));
    SseError::Status { status, challenge }
}

/// The client side of one legacy SSE session: rmcp's [`Transport`] over the open event stream
/// and the server's message endpoint.
pub struct LegacySseTransport {
    requester: Requester,
    endpoint: Url,
    post_timeout: Duration,
    inbound: mpsc::Receiver<ServerJsonRpcMessage>,
    reader: Option<JoinHandle<()>>,
}

impl std::fmt::Debug for LegacySseTransport {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("LegacySseTransport")
            .field("endpoint", &crate::url_origin(self.endpoint.as_str()))
            .finish_non_exhaustive()
    }
}

impl Drop for LegacySseTransport {
    fn drop(&mut self) {
        if let Some(reader) = self.reader.take() {
            reader.abort();
        }
    }
}

impl LegacySseTransport {
    /// Opens the event stream at `prepared.url` and waits (up to [`ENDPOINT_TIMEOUT`]) for the
    /// `endpoint` event. Each later POST is bounded by `post_timeout`.
    pub async fn open(
        server: &str,
        prepared: &PreparedHttp,
        post_timeout: Duration,
    ) -> Result<Self, SseError> {
        let url = Url::parse(&prepared.url)
            .map_err(|_| SseError::Protocol("the server URL does not parse".into()))?;
        let requester = Requester::new(prepared);
        let request = requester
            .authorize(
                requester
                    .http
                    .get(url.clone())
                    .header(ACCEPT, "text/event-stream"),
            )
            .await?;
        let response = request
            .send()
            .await
            .map_err(|e| SseError::Request(e.without_url()))?;
        if !response.status().is_success() {
            return Err(status_error(&response));
        }
        let content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_ascii_lowercase();
        if !content_type.starts_with("text/event-stream") {
            return Err(SseError::NotEventStream(if content_type.is_empty() {
                "no content type".to_owned()
            } else {
                format!("content type {content_type}")
            }));
        }

        let mut events = Box::pin(SseStream::from_bytes_stream(limit_event_size(
            response.bytes_stream(),
            MAX_SSE_EVENT_BYTES,
        )));
        let wait = async {
            while let Some(event) = events.next().await {
                let event = event.map_err(|e| SseError::Protocol(format!("event stream: {e}")))?;
                if event.event.as_deref() == Some("endpoint") {
                    return endpoint_url(&url, event.data.as_deref().unwrap_or(""));
                }
                tracing::debug!(server, event = ?event.event, "legacy SSE: event before `endpoint` ignored");
            }
            Err(SseError::NoEndpoint(
                "the event stream ended before the `endpoint` event".into(),
            ))
        };
        let endpoint = tokio::time::timeout(ENDPOINT_TIMEOUT, wait)
            .await
            .map_err(|_| {
                SseError::NoEndpoint(format!(
                    "no `endpoint` event within {}s",
                    ENDPOINT_TIMEOUT.as_secs()
                ))
            })??;

        let (tx, inbound) = mpsc::channel(INBOUND_BUFFER);
        let reader = tokio::spawn(read_messages(server.to_owned(), events, tx));
        Ok(Self {
            requester,
            endpoint,
            post_timeout,
            inbound,
            reader: Some(reader),
        })
    }
}

/// The `endpoint` event's URL, resolved against the stream's URL. It must be on the same origin:
/// our headers and tokens go to it.
fn endpoint_url(base: &Url, data: &str) -> Result<Url, SseError> {
    let endpoint = base
        .join(data.trim())
        .map_err(|_| SseError::Protocol("the `endpoint` event does not hold a URL".into()))?;
    let same = endpoint.scheme() == base.scheme()
        && endpoint.host_str() == base.host_str()
        && endpoint.port_or_known_default() == base.port_or_known_default();
    if !same {
        return Err(SseError::Protocol(format!(
            "the `endpoint` event points to another origin ({}); refusing to send requests there",
            crate::url_origin(endpoint.as_str())
        )));
    }
    Ok(endpoint)
}

/// Feeds `message` events (and unnamed ones) to rmcp until the stream ends or rmcp is gone.
async fn read_messages<S>(server: String, mut events: S, tx: mpsc::Sender<ServerJsonRpcMessage>)
where
    S: Stream<Item = Result<sse_stream::Sse, sse_stream::Error>> + Unpin,
{
    while let Some(event) = events.next().await {
        let event = match event {
            Ok(event) => event,
            Err(e) => {
                tracing::debug!(server, error = %e, "legacy SSE: the event stream failed");
                break;
            }
        };
        match event.event.as_deref() {
            None | Some("message") => {
                let Some(data) = event.data else { continue };
                match serde_json::from_str::<ServerJsonRpcMessage>(&data) {
                    Ok(message) => {
                        if tx.send(message).await.is_err() {
                            break;
                        }
                    }
                    Err(e) => tracing::warn!(
                        server,
                        error = %e,
                        "legacy SSE: skipped a message that is not JSON-RPC"
                    ),
                }
            }
            Some(other) => tracing::debug!(server, event = other, "legacy SSE: event ignored"),
        }
    }
    tracing::debug!(server, "legacy SSE: the event stream ended");
}

impl Transport<RoleClient> for LegacySseTransport {
    type Error = SseError;

    fn name() -> std::borrow::Cow<'static, str> {
        "legacy SSE".into()
    }

    fn send(
        &mut self,
        item: ClientJsonRpcMessage,
    ) -> impl Future<Output = Result<(), SseError>> + Send + 'static {
        let requester = self.requester.clone();
        let endpoint = self.endpoint.clone();
        let timeout = self.post_timeout;
        async move {
            let body = serde_json::to_vec(&item)
                .map_err(|e| SseError::Protocol(format!("encode a message: {e}")))?;
            let request = requester
                .authorize(
                    requester
                        .http
                        .post(endpoint)
                        .header(CONTENT_TYPE, "application/json")
                        .timeout(timeout)
                        .body(body),
                )
                .await?;
            let response = request
                .send()
                .await
                .map_err(|e| SseError::Request(e.without_url()))?;
            if response.status().is_success() {
                Ok(())
            } else {
                Err(status_error(&response))
            }
        }
    }

    async fn receive(&mut self) -> Option<ServerJsonRpcMessage> {
        self.inbound.recv().await
    }

    async fn close(&mut self) -> Result<(), SseError> {
        if let Some(reader) = self.reader.take() {
            reader.abort();
        }
        self.inbound.close();
        Ok(())
    }
}

/// Fails the byte stream once one event (the bytes between blank lines) passes `max`.
fn limit_event_size<S>(stream: S, max: usize) -> impl Stream<Item = Result<Bytes, SseError>>
where
    S: Stream<Item = reqwest::Result<Bytes>>,
{
    let mut size = EventSize::default();
    stream.map(move |chunk| {
        let chunk = chunk.map_err(|e| SseError::Request(e.without_url()))?;
        size.observe(&chunk, max)?;
        Ok(chunk)
    })
}

/// Bytes of the current event, across chunks. Line ends are `\n`, `\r\n` or `\r`; an empty line
/// ends the event.
#[derive(Debug)]
struct EventSize {
    size: usize,
    line_start: bool,
    after_cr: bool,
}

impl Default for EventSize {
    fn default() -> Self {
        Self {
            size: 0,
            line_start: true,
            after_cr: false,
        }
    }
}

impl EventSize {
    fn observe(&mut self, chunk: &[u8], max: usize) -> Result<(), SseError> {
        for &byte in chunk {
            if std::mem::take(&mut self.after_cr) && byte == b'\n' {
                continue;
            }
            match byte {
                b'\r' | b'\n' => {
                    if self.line_start {
                        self.size = 0;
                    }
                    self.line_start = true;
                    self.after_cr = byte == b'\r';
                }
                _ => {
                    self.line_start = false;
                    self.size += 1;
                    if self.size > max {
                        return Err(SseError::Protocol(format!(
                            "an event is larger than {max} bytes"
                        )));
                    }
                }
            }
        }
        Ok(())
    }
}

/// Connects to a legacy HTTP+SSE server at `options.url` (the stream's URL, often `/sse`),
/// whatever [`HttpServerOptions::legacy_sse`] says. [`super::http::connect`] calls this for
/// `type = "sse"` and as `http`'s fallback.
pub async fn connect(
    options: &HttpServerOptions,
    backend: &CredentialBackend,
) -> Result<ExternalClient, McpError> {
    let prepared = prepare_bounded(options, backend).await?;
    connect_prepared(options.server(), &options.external, &prepared).await
}

/// The legacy SSE lifecycle on an already prepared client: open the stream, then `initialize`
/// (any version from 2024-11-05 to 2025-11-25 the server picks). `Auto` means `initialize` here:
/// no SSE server knows `server/discover`.
pub async fn connect_prepared(
    name: &str,
    external: &ExternalOptions,
    prepared: &PreparedHttp,
) -> Result<ExternalClient, McpError> {
    attempt(name, external, prepared).await.map_err(|f| f.error)
}

/// A failed [`attempt`]. `not_sse`: the server does not speak legacy SSE at all.
pub(crate) struct Failure {
    pub error: McpError,
    pub not_sse: bool,
}

pub(crate) async fn attempt(
    name: &str,
    external: &ExternalOptions,
    prepared: &PreparedHttp,
) -> Result<ExternalClient, Failure> {
    let target = crate::url_origin(&prepared.url);
    let kind = if prepared.transport == "http" {
        "sse"
    } else {
        "sse (unix socket)"
    };
    let post_timeout = external.call_timeout.max(external.connect_timeout);
    let opened = tokio::time::timeout(
        external.connect_timeout,
        LegacySseTransport::open(name, prepared, post_timeout),
    )
    .await;
    let transport = match opened {
        Err(_) => {
            return Err(Failure {
                error: McpError::ConnectTimeout {
                    url: target,
                    after: external.connect_timeout,
                },
                not_sse: false,
            });
        }
        Ok(Err(e)) => {
            let not_sse = e.is_not_sse();
            return Err(Failure {
                error: open_error(name, &target, &prepared.auth, e),
                not_sse,
            });
        }
        Ok(Ok(transport)) => transport,
    };
    let legacy = ExternalOptions {
        protocol: match external.protocol {
            ProtocolMode::Auto => ProtocolMode::Legacy,
            other => other,
        },
        ..external.clone()
    };
    let mut once = Some(transport);
    let mut client =
        ExternalClient::handshake_with(name, &target, kind, &prepared.auth, &legacy, move || {
            once.take().ok_or(McpError::Closed)
        })
        .await
        .map_err(|error| Failure {
            error,
            not_sse: false,
        })?;
    client.info.fell_back = external.protocol == ProtocolMode::Auto;
    Ok(client)
}

/// A failed open as an [`McpError`]: a rejection is [`McpError::ServerAuth`] (with the
/// challenge), anything else a [`McpError::Connect`] whose causes say what happened.
fn open_error(name: &str, target: &str, auth: &str, error: SseError) -> McpError {
    let challenge = match &error {
        SseError::Status { status, challenge }
            if matches!(*status, StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN) =>
        {
            Some(
                challenge
                    .as_ref()
                    .map(|c| c.www_authenticate_header.clone()),
            )
        }
        SseError::OAuth(AuthError::AuthorizationRequired) => Some(None),
        _ => None,
    };
    if let Some(challenge) = challenge {
        return McpError::ServerAuth {
            server: name.to_owned(),
            reason: auth_hint(name, auth),
            challenge,
        };
    }
    McpError::Connect {
        url: target.to_owned(),
        source: Box::new(ClientInitializeError::TransportError {
            error: DynamicTransportError::new::<LegacySseTransport, RoleClient>(error),
            context: "open the legacy SSE stream".into(),
        }),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn event_size_counts_one_event_across_chunks_and_line_ends() {
        for sep in ["\n\n", "\r\n\r\n", "\r\r"] {
            let mut size = EventSize::default();
            // Two 6-byte events: fine at a limit of 6.
            let text = format!("data:1{sep}data:2{sep}");
            for chunk in text.as_bytes().chunks(3) {
                size.observe(chunk, 6)
                    .unwrap_or_else(|e| panic!("{sep:?}: {e}"));
            }
            // Two lines of one event add up.
            let mut size = EventSize::default();
            let text = format!("data:1{}data:2{sep}", &sep[..sep.len() / 2]);
            assert!(size.observe(text.as_bytes(), 6).is_err(), "{sep:?}");
        }
        // A split \r\n is one line end, not an empty line.
        let mut size = EventSize::default();
        size.observe(b"data:1\r", 12).unwrap();
        size.observe(b"\ndata:2\r\n", 12).unwrap();
        assert_eq!(size.size, 12);
    }

    #[test]
    fn the_endpoint_must_be_on_the_stream_origin() {
        let base = Url::parse("http://127.0.0.1:9000/sse").unwrap();
        assert_eq!(
            endpoint_url(&base, " /messages?sessionId=a\n")
                .unwrap()
                .as_str(),
            "http://127.0.0.1:9000/messages?sessionId=a"
        );
        assert_eq!(
            endpoint_url(&base, "http://127.0.0.1:9000/m")
                .unwrap()
                .as_str(),
            "http://127.0.0.1:9000/m"
        );
        let https = Url::parse("https://mcp.example/sse").unwrap();
        assert!(endpoint_url(&https, "https://mcp.example:443/m").is_ok());
        for foreign in [
            "http://127.0.0.1:9001/m",
            "https://127.0.0.1:9000/m",
            "http://evil.example:9000/m",
            "//evil.example/m",
        ] {
            let err = endpoint_url(&base, foreign).unwrap_err().to_string();
            assert!(err.contains("another origin"), "{foreign}: {err}");
        }
    }

    #[test]
    fn rejections_are_not_a_reason_to_report_the_http_error() {
        let status = |status| SseError::Status {
            status,
            challenge: None,
        };
        assert!(status(StatusCode::NOT_FOUND).is_not_sse());
        assert!(status(StatusCode::METHOD_NOT_ALLOWED).is_not_sse());
        assert!(!status(StatusCode::UNAUTHORIZED).is_not_sse());
        assert!(!status(StatusCode::FORBIDDEN).is_not_sse());
        assert!(SseError::NotEventStream("x".into()).is_not_sse());
        assert!(SseError::NoEndpoint("x".into()).is_not_sse());
        assert!(!SseError::Protocol("x".into()).is_not_sse());
    }
}
