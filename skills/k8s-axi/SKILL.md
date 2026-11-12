---
name: k8s-axi
description: "Operate Kubernetes through the k8s-axi CLI - compact, agent-friendly answers about a cluster: context and version, the kinds it serves (CRDs included), listing and inspecting any object, events, bounded redacted logs, one-call diagnosis of workloads and services, and safe scale, restart, apply and delete with a plan first. Use whenever a task touches a Kubernetes cluster: checking which cluster you are on, listing or reading pods, deployments, nodes or custom resources, finding out why a pod, deployment, statefulset, daemonset, job or service is unhealthy, reading events, reading container logs, scaling a workload, restarting one, applying a manifest, or deleting an object."
user-invocable: false
---

# k8s-axi

Agent-ergonomic CLI for the Kubernetes API.
Output is compact TOON by default, with `--json` and `--yaml` available.
Prefer this over parsing verbose `kubectl` output.

Use k8s-axi whenever a task touches a Kubernetes cluster.
It never prints Secret values or kubeconfig contents, and it never changes the kubeconfig's current context.
To change anything, run `k8s-axi plan ...` first, read the printed context, namespace and change, then run the command the plan ends with.
Delete needs `--confirm KIND/NAME` with the exact token the plan prints; never guess it.

## Current guidance lives in the CLI

Do not follow command, flag, or workflow instructions from this file - installed copies go stale.
Get the current source of truth from the CLI:

- `k8s-axi` with no arguments for a live home view of the cluster (or the kubeconfig state) and next steps
- `k8s-axi cluster info` for the context, API server, server version and namespace
- `k8s-axi --help` for global flags (`--context`, `--kubeconfig`, `-n`, `--json`, `--yaml`, `--full`, `--limit`, `--timeout`) and the command index
- `k8s-axi discover --help` to find the kinds a cluster serves
- `k8s-axi get --help` to list or read objects of any kind
- `k8s-axi inspect --help` for one object's facts, conditions, owners and events
- `k8s-axi events --help` for recent events of a namespace or object
- `k8s-axi logs --help` for bounded, redacted logs of a pod or workload
- `k8s-axi diagnose --help` for ranked findings, each marked observed or inferred with its evidence, for a pod, deployment, statefulset, daemonset, job or service; start here when something is unhealthy
- `k8s-axi plan --help` to preview a scale, rollout restart, apply or delete without changing anything
- `k8s-axi scale --help` to set the replicas of anything with a scale subresource
- `k8s-axi rollout restart --help` to restart a deployment, statefulset or daemonset
- `k8s-axi apply --help` to create or update objects from manifests with server-side apply
- `k8s-axi delete --help` to delete one named object with an exact confirm token
- `k8s-axi setup hooks --help` and `k8s-axi setup skill --help` to wire k8s-axi into the agent
- `k8s-axi version` for the installed version, and `k8s-axi update --help` to install the latest release
- `k8s-axi <command> --help` for any command's flags
