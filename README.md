# k8s-axi

Agent-ergonomic CLI for the Kubernetes API.

`k8s-axi` answers operational questions about a cluster in few calls and few tokens.
It follows the [AXI principles](https://github.com/kunchenguid/axi) and is built on [axi-sdk-go](https://github.com/lucashalbert/axi-sdk-go).
Output is compact [TOON](https://github.com/toon-format/spec) by default, with `--json` and `--yaml` alternatives.

## Install

Download an archive for Linux, macOS or Windows (amd64 or arm64) from the [releases page](https://github.com/lucashalbert/k8s-axi/releases), or build from source:

```sh
go install github.com/lucashalbert/k8s-axi/cmd/k8s-axi@latest
```

## Usage

```sh
k8s-axi cluster info                  # context, API server, server version, namespace
k8s-axi discover --kind deploy        # kinds the cluster serves, built-in and custom
k8s-axi discover --group apps --full  # more columns
k8s-axi get pods -n kube-system       # compact table, any kind: pods, deploy, widgets.example.com
k8s-axi get deploy -A -l app=web      # all namespaces, label selector (also --field-selector)
k8s-axi inspect pod web-6d5f8         # key facts, conditions, owner chain, newest events
k8s-axi events -n prod --type Warning # newest first; add pod/web-6d5f8 for one object
k8s-axi logs deploy/web --since 15m   # bounded, redacted; -c, --previous, --tail, -l
k8s-axi diagnose deploy/web -n prod   # ranked findings with evidence, for workloads and services
k8s-axi version
k8s-axi update                        # install the latest release, verifying its SHA-256 checksum
```

Read commands:

- `get KIND [NAME]` lists a kind, or reads one object, as a few columns per kind.
  `KIND` is a plural, singular, kind or short name, optionally qualified as `resource.version.group` or `group/version/resource`.
  Kinds are resolved through live discovery, so CRDs and kinds served only in a non-preferred version work.
  Built-in kinds are read through typed clients and everything else through the dynamic client.
- `inspect KIND NAME` summarises one object: per-kind facts, conditions, the owner chain, and the newest events about it.
  `--labels` and `--annotations` add those maps.
- `events [KIND NAME]` shows events for a namespace (`-A` for all) or one object, newest first, with a warning count.
- `logs [TARGET]` reads a pod, or the pods of a deployment, statefulset, daemonset, replicaset or job (`KIND/NAME`), or pods chosen with `-l`.

Every list states `count` and `truncated`, empty results say what was searched, and each result ends with next-step commands.
Lists are cut at `--limit` rows (default 50, `0` for no limit), events are read up to 1000 per call, and logs are bounded by `--tail` (default 100 lines per container) and `--max-bytes` (default 64 KiB in total) over at most 5 pods.
`--full` prints complete objects.

Secret values are never printed, and there is no flag that reveals them: `get` and `inspect` show a Secret's type and key names with sizes only, also with `--full`, `--json` and `--yaml`.
Full objects also drop `managedFields` and the `last-applied-configuration` annotation, mask credential-looking env literals and ConfigMap `data` or `binaryData` keys, and mask secret-shaped strings.
Log text is redacted for bearer tokens, JWTs, `token=`/`password=` pairs, URL passwords and private keys, and the result says how many values were replaced.
Redaction is pattern based, so treat logs as best effort.

### Diagnose

`diagnose KIND NAME` (or `KIND/NAME`) answers "why is this unhealthy" in one call for pods, deployments, statefulsets, daemonsets, jobs and services.
It reads the object, its owner chain, its pods, the newest warning events and a few bounded log tails.
It then applies fixed rules and prints a `status` (`unhealthy`, `degraded` or `healthy`), a one-sentence `conclusion`, ranked `findings` and next-step commands.

Every finding is labelled `observed` (read directly from the cluster) or `inferred` (derived by a rule) and lists the evidence it used.
An observed fact and its inferred cause are separate findings, for example `image-pull` and `image-pull-cause`.
Findings cover image pull failures, crash loops with the last termination reason, out-of-memory kills, container and init failures, unschedulable pods and why, failed probes, evictions, unhealthy nodes, failed pod creation and exhausted quotas, stalled or blocked rollouts, unavailable replicas, failed jobs, and services with no pods, a selector mismatch, no ready endpoints or a wrong target port.
The rules are plain functions with no model calls and no network access beyond the Kubernetes API.

Reads are bounded: one page of at most 100 pods, 1000 events, 3 log streams of 20 lines and 4 KiB each, 3 nodes, and concurrency capped at 4.
`--full` raises the log tails and stream count, shows every pod row and complete messages, and adds the scrubbed target object.
A check that cannot run, such as a list denied by RBAC or a read that timed out, is listed under `gaps` and never fails the command.
The whole command stops after three times `--timeout` and reports what it has.
It exits 0 whenever it produced a report, healthy or not.
It only reads, and the same redaction and Secret rules apply as for the other commands.

Global flags: `--context`, `--kubeconfig`, `-n/--namespace`, `--json`, `--yaml`, `--full`, `--limit`, `--timeout`.
Each has a `K8S_AXI_<NAME>` environment variable, and `KUBECONFIG` keeps its standard meaning.
`--context` selects a kubeconfig context for one command, and k8s-axi never changes the current context.
It never prints Secret values, tokens or kubeconfig contents.
Exec credential plugins named in the kubeconfig (for example `aws eks get-token`) are the only external programs it starts.

A cached once-a-day update check prints a one-line notice on stderr.
Set `K8S_AXI_NO_UPDATE_CHECK=1` to turn it off.

## Agent setup

```sh
k8s-axi setup hooks          # add a session-start hook that runs 'cluster info'
k8s-axi setup hooks --print  # show the settings fragment, write nothing
k8s-axi setup skill          # install the agent skill
```

Hooks default to `$CLAUDE_CONFIG_DIR/settings.json`, or `~/.claude/settings.json`.
The skill defaults to `$CLAUDE_CONFIG_DIR/skills/k8s-axi`, or `~/.claude/skills/k8s-axi`.
Use `--settings` or `--dest` to point either command elsewhere.

## Development

```sh
go vet ./...
go test -race ./...
go test -tags integration ./...   # needs a reachable cluster, for example kind
```

## Releases

Releases are cut by semantic-release from conventional commits on `main`, and GoReleaser publishes the archives and `checksums.txt`.
The project stays on `0.x`: a breaking change bumps the minor version until `1.0` is declared.
Pull request titles must be conventional commits, and they become the squash-merge commit message.
The first slice reaches `0.0.1` only when its PR is squash-merged with a `fix:` title; see [docs/release-process.md](docs/release-process.md).

## Compatibility

The five newest stable Kubernetes minors are tested with kind, see [docs/compatibility.md](docs/compatibility.md).
The design is in [docs/design.md](docs/design.md).

## License

BSD 3-Clause, see [LICENSE](LICENSE).
