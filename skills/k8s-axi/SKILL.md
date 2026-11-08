---
name: k8s-axi
description: "Operate Kubernetes through the k8s-axi CLI - compact, agent-friendly answers about a cluster: context and version, the kinds it serves (CRDs included), listing and inspecting any object, events, and bounded redacted logs. Use whenever a task touches a Kubernetes cluster: checking which cluster you are on, listing or reading pods, deployments, nodes or custom resources, finding out why something is unhealthy, reading events, or reading container logs."
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
- `k8s-axi get --help` to list or read objects of any kind
- `k8s-axi inspect --help` for one object's facts, conditions, owners and events
- `k8s-axi events --help` for recent events of a namespace or object
- `k8s-axi logs --help` for bounded, redacted logs of a pod or workload
- `k8s-axi <command> --help` for any command's flags
