//! The chat's side of the remote link (#2373): [`ShellRemote`] is the
//! [`amp_core::remote::RemoteHost`] a [`crate::Shell`] hands out with [`crate::Shell::remote_host`].
//! Its calls travel to the shell over the shell's own message channel and are applied in
//! [`crate::Shell::poll`] (the terminal loop's tick), so the shell stays single-owner.

use std::sync::{Arc, Mutex};

use amp_core::chat_event::PromptOrigin;
use amp_core::remote::{HostStatus, RemoteHost};
use tokio::sync::oneshot;

use crate::shell::RemoteSender;

type BoxFuture<'a, T> = std::pin::Pin<Box<dyn std::future::Future<Output = T> + Send + 'a>>;

/// One call from the remote link.
#[derive(Debug)]
pub enum RemoteCall {
    /// Start `text` as a user turn now (never a slash command); `false` while a run is busy.
    Start {
        prompt_id: String,
        text: String,
        origin: PromptOrigin,
        reply: oneshot::Sender<bool>,
    },
    /// Stop the running reply, as Esc / `/abort` do: it drops the queued prompts, here and in the
    /// link; what it does to workers depends on the dispatch mode (AMP #2858).
    Stop { reply: oneshot::Sender<()> },
}

/// The shell as the remote link sees it.
pub struct ShellRemote {
    tx: RemoteSender,
    status: Arc<Mutex<HostStatus>>,
}

impl ShellRemote {
    pub(crate) fn new(tx: RemoteSender, status: Arc<Mutex<HostStatus>>) -> Self {
        Self { tx, status }
    }
}

impl RemoteHost for ShellRemote {
    fn start_prompt(
        &self,
        prompt_id: String,
        text: String,
        origin: PromptOrigin,
    ) -> BoxFuture<'static, bool> {
        let (reply, rx) = oneshot::channel();
        let sent = self.tx.send(RemoteCall::Start {
            prompt_id,
            text,
            origin,
            reply,
        });
        Box::pin(async move { sent && rx.await.unwrap_or(false) })
    }

    fn stop(&self) -> BoxFuture<'static, ()> {
        let (reply, rx) = oneshot::channel();
        let sent = self.tx.send(RemoteCall::Stop { reply });
        Box::pin(async move {
            if sent {
                let _ = rx.await;
            }
        })
    }

    fn status(&self) -> HostStatus {
        self.status
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }
}
