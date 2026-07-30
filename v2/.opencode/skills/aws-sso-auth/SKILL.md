---
name: aws-sso-auth
description: Use when you need to authenticate with AWS via SSO and run AWS CLI commands against AWS accounts and services (EKS, S3, CloudWatch, IAM, etc.). Covers aws-sso exec authentication flow, credential management, common AWS CLI commands, EKS cluster token, and troubleshooting auth issues.
---

# AWS SSO Authentication

This skill covers authenticating with AWS via SSO (aws-sso CLI) and running AWS CLI commands against AWS accounts and services. Use this when you need to run any `aws` command against a cloud environment — listing resources, describing clusters, managing S3, querying CloudWatch, or getting an EKS kubeconfig token.

## Authentication flow

### 1. Check current auth status

```bash
aws sts get-caller-identity
```

If this returns your identity (account ID, ARN, user ID), you're already authenticated. If it fails, proceed to step 2.

### 2. Authenticate via aws-sso

Use `aws-sso exec` to run any command with temporary SSO credentials injected as environment variables:

```bash
aws-sso exec --profile <account>:<role> -- <command>
```

To start an interactive shell with credentials set for the session:
```bash
aws-sso exec --profile <account>:<role>
```

### 3. Run AWS CLI commands

Once authenticated, run AWS CLI commands directly:

```bash
aws sts get-caller-identity
aws eks list-clusters --region <region>
aws s3 ls
aws logs describe-log-groups --region <region>
```

### 4. Get an EKS kubeconfig token for kubectl

```bash
aws eks get-token --cluster-name <cluster-name> --region <region>
```

Or write the full kubeconfig for a cluster:

```bash
aws eks update-kubeconfig --name <cluster-name> --region <region>
```

## Common commands by service

### EKS

```bash
# List clusters
aws eks list-clusters --region <region>

# Describe a cluster (endpoint, version, networking)
aws eks describe-cluster --name <cluster-name> --region <region>

# List nodegroups
aws eks list-nodegroups --cluster-name <cluster-name> --region <region>
```

### S3

```bash
# List buckets
aws s3 ls

# List objects in a bucket (recursive)
aws s3 ls s3://<bucket>/<prefix>/ --recursive

# Download a file
aws s3 cp s3://<bucket>/<key> ./<local-file>
```

### CloudWatch

```bash
# List log groups
aws logs describe-log-groups --region <region>

# Get recent log events from a group
aws logs get-log-events --log-group-name <group> --log-stream-name <stream> --region <region>
```

### IAM

```bash
# List users
aws iam list-users

# Get role details
aws iam get-role --role-name <role-name>
```

## Key environment variables set by aws-sso exec

When using `aws-sso exec`, these are automatically injected into the command's environment:
- `AWS_ACCESS_KEY_ID`
- `AWS_SECRET_ACCESS_KEY`
- `AWS_SESSION_TOKEN`
- `AWS_DEFAULT_REGION`
- `AWS_PROFILE`

## Troubleshooting

- **"Unable to locate credentials"**: Run `aws-sso exec` with your command — don't try to run `aws` bare without SSO credentials set.
- **"Token expired"**: `aws-sso exec` handles token refresh automatically on each invocation — just re-run the command.
- **Profile not found**: Check available profiles in `~/.aws/config` — the `aws-sso` config file format uses `[profile account:role]` sections.
- **Wrong account/region**: Double-check the profile name and `--region` flag — it's easy to hit the wrong region and get "resource not found" for things that exist elsewhere.

