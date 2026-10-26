//! #2207: an OpenRouter refusal through the shipped `openrouter` provider (rpi's real
//! OpenAI-compatible provider, real HTTP, inside [`ConfiguredProvider`]). The message's
//! `error_message` (what the session log keeps) holds the whole body without the account's
//! `user_id`, and [`provider_error::summarize`] makes it one sentence.

use std::sync::Arc;

use amp_core::{
    models::{ModelsConfig, ModelsFile},
    provider_error,
    providers::ConfiguredProvider,
};
use axum::{Router, http::StatusCode, response::IntoResponse, routing::post};
use rpi_ai::{
    Context, Message, Model, Provider, SimpleStreamOptions, StopReason,
    providers::openai_completions::OpenAiCompletionsProvider, types::UserMessage,
};
use tokio::net::TcpListener;

/// QA pass 1's body (#2173): the message, four upstream repeats, and the account id.
const BODY: &str = r#"{"error":{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400,"metadata":{"provider_name":null,"previous_errors":[{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400},{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400},{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400},{"message":"messages[9]: tool messages must include a non-empty string tool_call_id","code":400}]}},"user_id":"user_3FPq9ZxAbCdEf"}"#;

async fn refuse() -> impl IntoResponse {
    (
        StatusCode::BAD_REQUEST,
        [("content-type", "application/json")],
        BODY,
    )
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_openrouter_400_keeps_its_body_without_the_user_id() {
    let app = Router::new().route("/api/v1/chat/completions", post(refuse));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}/api/v1", listener.local_addr().unwrap());
    let server = tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });

    let models = ModelsConfig::from_files(ModelsFile::defaults(), None, "test", &|_| None).unwrap();
    let cfg = &models.providers["openrouter"];
    let mut model: Model = cfg.models.values().next().unwrap().clone();
    model.base_url = url.clone();
    let http = rpi_ai::http::client_builder_for(&url, None, None)
        .unwrap()
        .build()
        .unwrap();
    let inner = OpenAiCompletionsProvider::with_models_without_env_api_key(
        cfg.id.clone(),
        Some("sk-or-mock".into()),
        http,
        vec![model.clone()],
    );
    let provider: Arc<dyn Provider> = Arc::new(ConfiguredProvider::new(cfg, Arc::new(inner)));

    let ctx = Context {
        system_prompt: Some("test".into()),
        messages: vec![Message::User(UserMessage::new("hi", 0))],
        tools: Vec::new(),
    };
    let opts = SimpleStreamOptions {
        max_retries: Some(0),
        ..SimpleStreamOptions::default()
    };
    let message = provider
        .stream_simple(&model, &ctx, &opts)
        .await
        .result()
        .await
        .unwrap();
    server.abort();

    assert_eq!(message.stop_reason, StopReason::Error);
    let raw = message.error_message.as_deref().expect("an error message");
    assert!(raw.starts_with("http error 400: {"), "{raw}");
    assert!(
        raw.contains("previous_errors"),
        "the full body is kept: {raw}"
    );
    assert!(
        !raw.contains("user_id") && !raw.contains("user_3FP"),
        "{raw}"
    );
    assert!(!raw.contains("sk-or-mock"), "{raw}");

    assert_eq!(
        provider_error::summarize(&message.provider, raw),
        "OpenRouter refused the request (400): messages[9]: tool messages must include a \
         non-empty string tool_call_id"
    );
    assert_eq!(
        provider_error::run_failure("error", raw, &message.provider),
        provider_error::summarize("openrouter", raw)
    );
}
