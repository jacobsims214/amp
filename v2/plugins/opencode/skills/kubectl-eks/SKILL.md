---
name: kubectl-eks
description: Use when you need to interact with Kubernetes clusters (especially EKS) using kubectl — viewing pods, reading logs, exec into containers, restarting deployments, and troubleshooting. Covers context switching, pod management, log retrieval, exec into containers, rollout restart, EKS-specific commands with aws-sso wrapper, and common debugging patterns.
---

# Kubectl (EKS)

This skill covers using `kubectl` to interact with Kubernetes clusters, with a focus on EKS clusters managed through AWS SSO authentication. Use this when you need to inspect cluster state, read logs, debug failing pods, or restart deployments.

## Prerequisites

AWS must be authenticated first. Load the `aws-sso-auth` skill if credentials aren't already set:
```
skill("aws-sso-auth")
```

## Context management

```bash
# List all available contexts
kubectl config get-contexts

# Switch to a specific context
kubectl config use-context <context-name>

# Show the current context
kubectl config current-context

# Get cluster info
kubectl cluster-info
```

## Common cluster commands

### Nodes and general state

```bash
kubectl get nodes
kubectl get nodes -o wide
kubectl top nodes  # if metrics-server is installed
```

### Pods

```bash
# List pods in a namespace
kubectl get pods -n <namespace>

# All namespaces
kubectl get pods -A

# Wide output (shows IPs and node assignments)
kubectl get pods -n <namespace> -o wide

# Describe a pod (events, conditions, volumes)
kubectl describe pod <pod-name> -n <namespace>
```

### Logs

```bash
# Last N lines from a specific pod
kubectl logs -n <namespace> <pod-name> --tail=200

# All containers in a pod (multi-container pods)
kubectl logs -n <namespace> <pod-name> --all-containers=true --tail=200

# Follow logs
kubectl logs -n <namespace> <pod-name> --tail=50 -f

# Filter for errors
kubectl logs -n <namespace> <pod-name> --tail=500 | grep -iE "error|panic|exception|fail"

# Previous container's logs (after a crash/restart)
kubectl logs -n <namespace> <pod-name> --previous
```

### Exec into pods

```bash
# Run a single command
kubectl exec -n <namespace> <pod-name> -- ls -la

# Interactive shell
kubectl exec -it -n <namespace> <pod-name> -- /bin/sh
kubectl exec -it -n <namespace> <pod-name> -- /bin/bash

# Specific container in a multi-container pod
kubectl exec -n <namespace> <pod-name> -c <container-name> -- <command>
```

### Deployments and rollouts

```bash
# Restart a deployment
kubectl rollout restart deployment <deploy-name> -n <namespace>

# Check rollout status
kubectl rollout status deployment <deploy-name> -n <namespace>

# Scale a deployment
kubectl scale deployment <deploy-name> -n <namespace> --replicas=<N>

# Get deployment details
kubectl get deployment <deploy-name> -n <namespace> -o wide
kubectl describe deployment <deploy-name> -n <namespace>
```

### Resources and YAML

```bash
# Get common resources
kubectl get svc,ingress,configmap,secret,pvc -n <namespace>

# Get a resource as YAML
kubectl get <resource> <name> -n <namespace> -o yaml

# Get events (sorted by time)
kubectl get events -n <namespace> --sort-by='.lastTimestamp'
```

## EKS + aws-sso pattern

When AWS credentials aren't already in your shell environment, wrap `kubectl` calls in `aws-sso exec`:

```bash
aws-sso exec --profile <account>:<role> -- kubectl get nodes
```

This works for any `kubectl` command — exec, logs, rollout restart, etc. — as long as the kubeconfig for the cluster has already been written locally (see the `aws-sso-auth` skill's `update-kubeconfig` command for how to get one).

## Troubleshooting

- **"Unable to connect to the server"**: AWS credentials expired — re-auth with `aws-sso exec`.
- **"executable aws failed"**: AWS CLI not in PATH, or credentials expired.
- **Pod in CrashLoopBackOff**: Check the failing container's logs with `kubectl logs --previous`.
- **"No resources found"**: Wrong namespace or context — verify with `kubectl config current-context`.
- **Pod stuck Pending**: Check events with `kubectl describe pod` — usually a scheduling issue (nodeSelector, resource requests, PVC binding).

