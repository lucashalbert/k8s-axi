# Usage guide

A short tour of the way an agent, or a person, uses k8s-axi on a cluster.
The command reference is in the [README](../README.md), and every command has `--help`.

## 1. Know where you are

```sh
k8s-axi cluster info
```

Prints the context, API server, server version and namespace, and whether the server answered.
Pick another cluster for one command with `--context NAME`.
k8s-axi never changes the kubeconfig's current context.
A kubeconfig with no current context is a usage error (exit 2) that lists the contexts you can choose.

## 2. Find and read things

```sh
k8s-axi discover --kind widget          # does this cluster serve the kind? which version?
k8s-axi get pods -n prod -l app=web     # a compact table
k8s-axi inspect deploy web -n prod      # facts, conditions, owners, newest events
k8s-axi events -n prod --type Warning
k8s-axi logs deploy/web -n prod --since 15m
```

Lists are cut at `--limit` rows (default 50) and say how many were left out.
An empty result says what was searched, so an empty answer is never silence.
Add `--full` for complete objects, and `--json` or `--yaml` for parsing.
Secret values are never printed in any format.

## 3. Ask why something is unhealthy

```sh
k8s-axi diagnose deploy/web -n prod
```

One call reads the object, its owners, its pods, warning events and a few log tails.
It prints a status, a one-sentence conclusion and ranked findings.
Each finding is `observed` (read from the cluster) or `inferred` (derived by a rule) and lists its evidence.
Exit code 0 means a report was produced, not that the workload is healthy.

## 4. Change something, safely

```sh
k8s-axi plan scale deploy/web --replicas 5 -n prod    # nothing changes
k8s-axi scale deploy/web --replicas 5 -n prod
k8s-axi plan apply -f manifests/ -n prod
k8s-axi apply -f manifests/ -n prod
k8s-axi plan delete deploy/web -n prod                # prints the exact --confirm token
k8s-axi delete deploy/web -n prod --confirm deployment/web
```

Read the plan first.
It names the context, namespace, resource and change, and ends with the command that carries it out.
`--dry-run=server` sends the real request with `dryRun=All`.
Delete is the only destructive command and needs the exact token.
A namespaced change without `-n` or a namespace on the context is refused.

## 5. When something goes wrong

| Exit code | Meaning | What to do |
|---|---|---|
| 0 | Success | |
| 1 | Runtime or API error: unreachable server, timeout, RBAC denial, a failed object in `apply` | Read the `hint` in the error; raise `--timeout` for a slow server |
| 2 | Usage error: unknown flag, missing `-n`, wrong `--confirm`, no current context | Fix the command; nothing was sent |

Errors are short and structured: the operation, the target, a message and a hint.
They never contain credentials or Secret values.

## 6. Keep it current

```sh
k8s-axi version   # also --version, -v and -V print just the version
k8s-axi update       # downloads the latest release and verifies its SHA-256 checksum
```

A cached once-a-day update check prints a one-line notice on stderr.
Structured errors (`op`, `message`, `hint`, `exit`) go to stdout in the same format as normal output; the exit code is unchanged.
It never delays or fails a command, and `K8S_AXI_NO_UPDATE_CHECK=1` turns it off.
A failed or interrupted `update` leaves the installed binary untouched.

## Limits

Every command bounds what it reads and prints.
The budgets are in [performance.md](performance.md), and the supported Kubernetes versions are in [compatibility.md](compatibility.md).
