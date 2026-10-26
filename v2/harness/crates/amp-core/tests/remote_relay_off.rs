//! #2390: with the relay off, `/relay/` reaches the web app (a login redirect, or its HTML). The
//! harness's relay client must not follow the redirect with its token, and must say "isn't
//! available" instead of "an unexpected shape".

use std::sync::{
    Arc,
    atomic::{AtomicI64, Ordering},
};

use amp_core::remote::{
    crypto::KeyPair,
    relay::{NOT_RELAY, RelayClient},
};

/// With the relay off, `/relay/` reaches the web app, which redirects to its login (or serves
/// its HTML). The client neither follows the redirect (with its token) nor reports "an
/// unexpected shape".
#[tokio::test]
async fn web_app_answers_are_not_the_relay() {
    use std::sync::atomic::AtomicUsize;
    let followed = Arc::new(AtomicUsize::new(0));
    let f = Arc::clone(&followed);
    let app = axum::Router::new()
        .route(
            "/login",
            axum::routing::any(move || {
                f.fetch_add(1, Ordering::SeqCst);
                async { "login page" }
            }),
        )
        .route(
            "/relay/v1/sessions",
            axum::routing::post(|| async {
                (
                    axum::http::StatusCode::TEMPORARY_REDIRECT,
                    [("location", "/login")],
                )
            }),
        )
        .route(
            "/relay/v1/sessions/{id}/token",
            axum::routing::post(|| async {
                (
                    [("content-type", "text/html")],
                    "<!doctype html><title>AMP</title>",
                )
            }),
        )
        .route(
            "/relay/v1/sessions/{id}/commands",
            axum::routing::get(|| async {
                (
                    [("content-type", "text/html")],
                    "<!doctype html><title>AMP</title>",
                )
            }),
        );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", listener.local_addr().unwrap());
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    let c = RelayClient::new(&base, None).unwrap();

    let e = c
        .register("tok", "h", "p", "host", "os", "0", Some(1))
        .await
        .unwrap_err();
    assert_eq!((e.status, e.code.as_deref()), (307, Some(NOT_RELAY)));
    assert_eq!(
        e.register_message(),
        "Remote control isn't available on this AMP server."
    );
    assert_eq!(followed.load(Ordering::SeqCst), 0, "followed the redirect");

    let key = KeyPair::generate();
    let e = c.refresh("rs_x", "tok", &key).await.unwrap_err();
    assert_eq!((e.status, e.code.as_deref()), (200, Some(NOT_RELAY)));

    let e = c
        .command_stream("rs_x", "rct", Arc::new(AtomicI64::new(0)), || {}, |_| true)
        .await
        .unwrap_err();
    assert_eq!(e.code.as_deref(), Some(NOT_RELAY));
}
