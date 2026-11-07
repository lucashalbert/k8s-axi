---
name: k8s-axi
description: "Operate Kubernetes through the k8s-axi CLI - compact, agent-friendly answers about a cluster: context, API server and version, and every resource kind it serves, including CRDs, found through live API discovery. Use whenever a task touches a Kubernetes cluster: checking which cluster and context you are on, finding out what kinds and API groups exist, or confirming a cluster is reachable before running other commands."
user-invocable: false
---

# k8s-axi

Agent-ergonomic CLI for the Kubernetes API.
Output is compact TOON by default, with `--json` and `--yaml` available.
Prefer this over parsing verbose `kubectl` output.

Use k8s-axi whenever a task touches a Kubernetes cluster.
It never prints Secret values or kubeconfig contents, and it never changes the kubeconfig's current context.

## Current guidance lives in the CLI

Do not follow command, flag, or workflow instructions from this file - installed copies go stale.
Get the current source of truth from the CLI:

- `k8s-axi cluster info` for the context, API server, server version and namespace
- `k8s-axi --help` for global flags (`--context`, `--kubeconfig`, `-n`, `--json`, `--yaml`, `--full`, `--limit`, `--timeout`) and the command index
- `k8s-axi discover --help` to find the kinds a cluster serves
- `k8s-axi <command> --help` for any command's flags
