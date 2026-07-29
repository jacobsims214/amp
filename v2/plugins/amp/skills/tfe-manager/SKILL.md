---
name: tfe-manager
description: Use when creating, configuring, or investigating Terraform Cloud/Enterprise workspaces — covers workspace lifecycle, variable sets, run types, run investigation, and VCS integration.
---

# TFE Manager

## Workspace Lifecycle

| Step | Action |
|------|--------|
| 1. Create | `terraform_create_workspace` — name, org, `execution_mode` |
| 2. Configure | Set `execution_mode` (remote/agent/local) and `terraform_version` |
| 3. Variables | Attach variable sets for shared config; add workspace vars for overrides |
| 4. VCS | Connect repo via `vcs_repo_oauth_token_id` + `vcs_repo_identifier` |
| 5. Run | `terraform_create_run` with `run_type: "plan_only"` first |
| 6. Monitor | `terraform_get_run_details` until status reaches a terminal state |

Terminal statuses: `planned_and_finished`, `applied`, `errored`, `canceled`, `discarded`.

## Variable Precedence

Workspace variables **override** variable set values for the same key.

| Type | Use for |
|------|---------|
| Variable sets | Shared credentials, org-wide config, common tags |
| Workspace vars | Environment-specific overrides, per-workspace secrets |
| Sensitive | Credentials — write-only after creation, never readable via API |

## Run Types

| Type | Use when |
|------|----------|
| `plan_only` | Preview changes; always use on first run of a new workspace |
| `plan_and_apply` | Standard apply |
| `refresh_state` | Detect drift without changing infrastructure |
| `allow_empty_apply` | Force apply when no changes are detected |

## Run Investigation

1. `terraform_get_run_details(run_id)` → check `status` field
2. If `errored`: read `message` field for root cause
3. Plan errors (provider config, syntax) → in the plan log
4. Apply errors (resource creation failures) → in the apply log
5. No run ID? Use `terraform_list_runs` filtered by `status: ["errored"]`

| Error pattern | Likely cause |
|---------------|-------------|
| `Provider configuration not present` | Missing credentials in workspace or variable set |
| `No valid credential sources found` | Provider credentials not attached |
| `Invalid value for variable` | Required variable missing from workspace |
| `Error acquiring the state lock` | Another run in progress or state is locked |
| `Error reading VCS repository` | OAuth token expired or repo access revoked |

## VCS Integration

| Rule | Do | Don't |
|------|-----|-------|
| OAuth first | OAuth token must exist before workspace creation | Create workspace then try to add VCS |
| Identifier | `org/repo` format | Full URL |
| Monorepos | Set `working_directory` to `.tf` file location | Run from repo root |
| Triggers | Trigger on branch | Trigger on tags for regular workspaces |

## Do / Don't

| Do | Don't |
|----|-------|
| `plan_only` first on every new workspace | `plan_and_apply` on the first run |
| Tag workspaces for discoverability | Leave workspaces untagged |
| Use variable sets for credentials | Repeat the same vars across workspaces |
| Require human approval in production | `auto_apply: true` in production |
| `terraform_list_workspaces` before creating | Create duplicate workspaces |

## Sensitive Variables

Once a sensitive variable is written, its value **cannot be read back** via API.
To rotate: delete the variable and recreate it with the new value.

```
terraform_create_workspace_variable(
  ..., key="API_TOKEN", value="secret", sensitive=true, category="env"
)
```

## State Lock

If a workspace is locked, check for a stuck run before forcing an unlock.
List recent runs: `terraform_list_runs(workspace_name=..., status=["planning","applying"])`
