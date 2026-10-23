//! `amp-harness whoami`: who AMP thinks you are (`amp_whoami`).

use amp_core::tools::{ToolCallError, call_as};
use amp_mcp::AmpMcpClient;
use serde::{Deserialize, Serialize};

use crate::conn::{CliError, ConnArgs, connect, print_json};

/// `whoami --json`. Add fields, never rename or remove them.
#[derive(Debug, Clone, Serialize)]
pub struct Whoami {
    pub user_id: Option<i64>,
    pub email: Option<String>,
    /// `human` or `agent`.
    pub account: Option<String>,
    pub global_admin: bool,
    pub via_api_token: bool,
    pub orgs: Vec<Org>,
}

#[derive(Debug, Clone, Serialize)]
pub struct Org {
    pub id: i64,
    pub name: String,
    pub slug: Option<String>,
    /// Your role in the org (`owner`, `admin`, `member`).
    pub role: Option<String>,
    pub teams: Vec<Team>,
}

#[derive(Debug, Clone, Serialize)]
pub struct Team {
    pub id: i64,
    pub name: String,
    pub role: Option<String>,
}

#[derive(Deserialize)]
struct RawWhoami {
    #[serde(default)]
    user_id: Option<i64>,
    #[serde(default)]
    email: Option<String>,
    #[serde(default)]
    account: Option<String>,
    #[serde(default)]
    global_admin: bool,
    #[serde(default)]
    via_api_token: bool,
    #[serde(default)]
    orgs: Option<Vec<RawOrg>>,
}

#[derive(Deserialize)]
struct RawOrg {
    id: i64,
    #[serde(default)]
    name: String,
    #[serde(default)]
    slug: Option<String>,
    #[serde(default)]
    my_role: Option<String>,
    #[serde(default)]
    teams: Option<Vec<RawTeam>>,
}

/// AMP packs a team's id and name into a member record's `user_id` and `email`.
#[derive(Deserialize)]
struct RawTeam {
    #[serde(default)]
    user_id: i64,
    #[serde(default)]
    email: String,
    #[serde(default)]
    role: Option<String>,
}

fn non_empty(s: Option<String>) -> Option<String> {
    s.filter(|s| !s.trim().is_empty())
}

impl From<RawWhoami> for Whoami {
    fn from(raw: RawWhoami) -> Self {
        Self {
            user_id: raw.user_id.filter(|id| *id != 0),
            email: non_empty(raw.email),
            account: non_empty(raw.account),
            global_admin: raw.global_admin,
            via_api_token: raw.via_api_token,
            orgs: raw
                .orgs
                .unwrap_or_default()
                .into_iter()
                .map(|o| Org {
                    id: o.id,
                    name: o.name,
                    slug: non_empty(o.slug),
                    role: non_empty(o.my_role),
                    teams: o
                        .teams
                        .unwrap_or_default()
                        .into_iter()
                        .map(|t| Team {
                            id: t.user_id,
                            name: t.email,
                            role: non_empty(t.role),
                        })
                        .collect(),
                })
                .collect(),
        }
    }
}

/// Calls `amp_whoami`.
pub async fn fetch(client: &AmpMcpClient) -> Result<Whoami, ToolCallError> {
    let raw: RawWhoami = call_as(client, "amp_whoami", serde_json::json!({})).await?;
    Ok(raw.into())
}

impl Whoami {
    /// `email (user N)`, or whatever AMP gave.
    pub fn short(&self) -> String {
        match (&self.email, self.user_id) {
            (Some(email), Some(id)) => format!("{email} (user {id})"),
            (Some(email), None) => email.clone(),
            (None, Some(id)) => format!("user {id}"),
            (None, None) => "unknown user".to_owned(),
        }
    }
}

pub async fn run(conn: ConnArgs, json: bool) -> Result<(), CliError> {
    let settings = conn.settings()?;
    let client = connect(&settings).await?;
    let me = fetch(&client).await;
    let auth = client.session().auth.clone();
    client.close().await;
    let me = me?;
    if json {
        return print_json(&me);
    }
    println!("{}", me.short());
    println!("  server:  {}", settings.url);
    println!("  auth:    {auth}");
    if let Some(account) = &me.account {
        let mut extra = Vec::new();
        if me.global_admin {
            extra.push("global admin");
        }
        if me.via_api_token {
            extra.push("API token");
        }
        if extra.is_empty() {
            println!("  account: {account}");
        } else {
            println!("  account: {account} ({})", extra.join(", "));
        }
    }
    for org in &me.orgs {
        let role = org.role.as_deref().unwrap_or("member");
        println!("  org:     {} (#{}, {role})", org.name, org.id);
        for team in &org.teams {
            let role = team.role.as_deref().unwrap_or("member");
            println!("           team {} (#{}, {role})", team.name, team.id);
        }
    }
    Ok(())
}
