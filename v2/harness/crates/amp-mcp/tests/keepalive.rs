//! #2380: the keep-alive client reuses one connection for call after call; the MCP client (no idle
//! pooling, like rmcp's own) opens one per call.

use std::sync::{
    Arc,
    atomic::{AtomicUsize, Ordering},
};

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

/// A tiny HTTP/1.1 server answering every request on a connection with `200 ok`, keeping the
/// connection open. Returns its URL and the count of accepted connections.
async fn serve() -> (String, Arc<AtomicUsize>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}/", listener.local_addr().unwrap());
    let accepted = Arc::new(AtomicUsize::new(0));
    let count = accepted.clone();
    tokio::spawn(async move {
        loop {
            let Ok((mut sock, _)) = listener.accept().await else {
                return;
            };
            count.fetch_add(1, Ordering::SeqCst);
            tokio::spawn(async move {
                let mut buf = Vec::new();
                let mut chunk = [0u8; 4096];
                loop {
                    let Ok(n) = sock.read(&mut chunk).await else {
                        return;
                    };
                    if n == 0 {
                        return;
                    }
                    buf.extend_from_slice(&chunk[..n]);
                    // One response per complete request head (the test requests have no body).
                    while let Some(end) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
                        buf.drain(..end + 4);
                        let reply = b"HTTP/1.1 200 OK\r\ncontent-length: 2\r\n\r\nok";
                        if sock.write_all(reply).await.is_err() {
                            return;
                        }
                    }
                }
            });
        }
    });
    (url, accepted)
}

async fn get_three(client: &reqwest::Client, url: &str) {
    for _ in 0..3 {
        let body = client.get(url).send().await.unwrap().text().await.unwrap();
        assert_eq!(body, "ok");
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn the_keepalive_client_reuses_one_connection() {
    let (url, accepted) = serve().await;
    let client = amp_mcp::build_keepalive_http_client(None).unwrap();
    get_three(&client, &url).await;
    assert_eq!(
        accepted.load(Ordering::SeqCst),
        1,
        "three calls, one connection"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn the_mcp_client_opens_a_connection_per_call() {
    let (url, accepted) = serve().await;
    let client = amp_mcp::build_http_client(None).unwrap();
    get_three(&client, &url).await;
    assert_eq!(
        accepted.load(Ordering::SeqCst),
        3,
        "no idle pooling, as rmcp's client"
    );
}
