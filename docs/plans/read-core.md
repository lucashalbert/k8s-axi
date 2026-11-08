# Read-only core: get, inspect, events, logs

Status: plan for the `0.1.x` slice of the command table in [design.md](../design.md).
Inputs: the approved design and the PRD sections 6.4 (clients), 6.5 (inspection), 6.7 (logs and events), 6.9 (output) and 9 (safety).
Items marked (my call) were chosen by the author of this plan.

## Intent

Four read-only commands that answer the common "what is in the cluster, what is wrong with this object, what happened, what did it say" questions in one call each.
They share one resolution path (kind argument to a resource), one read path (typed clients for built-in kinds, the dynamic client for everything else) and one scrubbing path (nothing secret-shaped is printed).

## Non-goals

- `diagnose`, `plan`, `scale`, `rollout restart`, `apply`, `delete` and everything later.
- Following logs (`-f`), watching, `exec`, `port-forward`.
- Revealing Secret values.
  There is no flag for it, now or later in this slice.

## Shared behaviour

### Kind argument

`KIND` is `resource[.version][.group]`, in the style of `kubectl`, or `[group/]version/resource`.
It matches the plural, singular, kind or short name, ignoring case.

| Input | Resolves to |
|---|---|
| `pods`, `pod`, `po` | core `v1` pods |
| `deploy`, `deployments.apps` | `apps/v1` deployments |
| `widgets.example.com` | the CRD's preferred served version |
| `widgets.v1beta1.example.com`, `example.com/v1beta1/widgets` | that exact version |

Resolution goes through the REST mapper built from live discovery, so scope (namespaced or cluster) and version come from the cluster, not from a table.
Short names are expanded with the discovery short-name expander.
A kind served only in a non-preferred version resolves to that version.
An unknown kind is a usage error (exit 2) that points at `k8s-axi discover --kind <name>`.

`NAME` follows as a second argument or after a slash: `inspect pod web` and `inspect pod/web` are the same.

### Read path

A small table maps the resolved group-version-resource to a typed client call for the built-in kinds the PRD lists:
Pods, Deployments, StatefulSets, DaemonSets, ReplicaSets, Jobs, CronJobs, Services, Endpoints, EndpointSlices, Nodes, Namespaces, ConfigMaps, Secrets, PersistentVolumes, PersistentVolumeClaims and Events.
Every other resource, and any version the table does not name, goes through the dynamic client.
Both paths return `unstructured.Unstructured`, so summaries are written once.
Typed results are converted and stamped with the resolved `apiVersion` and `kind`.

### Output

TOON by default, `--json` and `--yaml` render the same structure, `--full` adds detail.
Every list states `count` and `truncated`.
Empty results are stated with a `note` that says what was searched.
A truncated result says how many were shown, how many exist when the server told us, and which flag shows more.
Compact output ends with `next`, one or two commands worth running next.
Errors use the shared error model: operation, target, message, hint, exit code 1 for API errors and 2 for usage.
API errors get a hint for the common causes: not found, forbidden (name the verb and resource) and unauthorized.

### Scrubbing

Applied to every object that leaves a command in full form (`--full`), and to inspect labels and annotations:

1. Secrets: `data` and `stringData` values become `<N bytes>`.
   Compact `get` and `inspect` of a Secret show the type and key names with sizes only.
2. `metadata.managedFields` and the `kubectl.kubernetes.io/last-applied-configuration` annotation are dropped on every kind, because the annotation repeats Secret values and env literals verbatim.
3. Literal container `env` entries and ConfigMap `data` or `binaryData` keys whose names look like credentials (`password`, `passwd`, `secret`, `token`, `api_key`, `credential`, `private_key`) have their values replaced by `[REDACTED]`.
4. Every remaining string passes through the SDK `redact.Mask`, which removes bearer tokens, JWTs, `token=`/`password=` pairs, URL passwords and private keys.

The kubeconfig is only ever read.

## Commands

### `get`

```text
k8s-axi get KIND [NAME] [-n NS | -A] [-l SELECTOR] [--field-selector SEL] [--limit N] [--full]
```

- `-A` / `--all-namespaces` lists across namespaces and adds a `namespace` column.
  Cluster-scoped kinds ignore `-n` and `-A`.
- `--limit` is the existing global flag (default 50, 0 means no limit).
  It is sent to the API server as the list limit, so a truncated list is detected from the continue token and never fetched in full.
- With `NAME`, the same envelope carries one row, and a missing object is a runtime error (exit 1).

Output:

```text
resource: pods            # plural.version.group of the resolved kind, group and version omitted for core v1
namespace: default        # "*" with -A, absent for cluster-scoped kinds
count: 3
truncated: false
remaining: 12             # only when truncated and the server reported it
note: ...                 # empty result, truncation hint
items[3]{name,ready,status,restarts,node,age}:
  ...
next[1]: k8s-axi inspect pods <first-name> -n default
```

Row columns are chosen by kind, the same columns for every row of one list:

| Kind | Columns |
|---|---|
| Pod | `name, ready, status, restarts, node, age` |
| Deployment, StatefulSet, DaemonSet, ReplicaSet | `name, ready, age` (`ready` is ready/desired) |
| Job | `name, complete, age` |
| Node | `name, status, roles, version, age` |
| Service | `name, type, cluster_ip, ports, age` |
| ConfigMap | `name, keys, age` |
| Secret | `name, type, keys, age` |
| anything else | `name, status, age` |

`status` for the generic row is `status.phase`, else the `Ready`, `Available`, `Established` or `Succeeded` condition, else empty.
With `-A` every row also starts with `namespace`.
`--full` replaces `items` with the scrubbed objects.

### `inspect`

```text
k8s-axi inspect KIND NAME [-n NS] [--labels] [--annotations] [--full]
```

Output:

```text
kind: Pod
resource: pods
name: web-6d5f8
namespace: default
created: 2026-11-07T10:00:00Z
age: 3h
details: {...}            # per-kind summary, see below
conditions[2]{type,status,reason,age,message}:
owners[2]{depth,kind,name,controller}:
events: {count, total, truncated, items[...]{age,type,reason,count,message}}
next[2]: k8s-axi events pod web-6d5f8 -n default | k8s-axi logs pod/web-6d5f8 -n default
```

- `conditions` and `owners` are always present, empty with a stated zero count.
- `owners` is the ownerReference chain upward, nearest owner first, at most 5 levels.
  An owner that cannot be read is listed with a `note` instead of failing the command.
- `events` are the newest 10 events whose involved object is this object (matched by UID, then by kind and name), newest first, with `total` and `truncated`.
- `--labels` and `--annotations` add the scrubbed maps, which are omitted by default because they are large.
- `--full` adds `object` with the scrubbed full object.

`details` by kind, as structs so the field order is stable:

| Kind | Fields |
|---|---|
| Pod | `status, node, pod_ip, qos, containers[name,image,ready,state,restarts]` (`status` is the kubectl-style STATUS) |
| Deployment, StatefulSet, ReplicaSet | `desired, ready, available, updated, strategy, selector` |
| DaemonSet | `desired, ready, available, updated, selector` |
| Job | `completions, succeeded, failed, active` |
| Node | `status, roles, version, os, capacity, allocatable, taints` |
| Service | `type, cluster_ip, ports, selector` |
| ConfigMap, Secret | `type (Secret), keys[name,bytes]` |
| anything else | top-level scalar fields of `spec` and `status`, at most 20, with a `note` pointing to `--full` |

### `events`

```text
k8s-axi events [KIND NAME | KIND/NAME] [-n NS | -A] [--type Normal|Warning] [--since DUR] [--limit N] [--full]
```

- With no object, the scope is the namespace (`-A` for all).
- With an object, only events about it (by UID when the object exists, else by kind and name).
  Cluster-scoped object scopes search all namespaces and ignore `-n` and `-A`.
- Sorted newest first by the latest of `lastTimestamp`, `eventTime`, `series.lastObservedTime` and `creationTimestamp`.
- The API server cannot sort events, so up to 1000 matching events are read and sorted here.
  If more exist, `truncated` is true and the note says the newest may be missing, with the narrowing flags.
- Warnings are highlighted by a leading `warnings: N` count in the envelope, and `--type Warning` filters to them.

Output:

```text
scope: namespace/default
count: 20
warnings: 3
truncated: false
note: ...
events[20]{age,type,reason,object,count,message}:
next[1]: ...
```

`message` is cut at 200 characters with an ellipsis in compact mode and complete with `--full`.
`--full` adds `first_seen, last_seen, component, uid`.

### `logs`

```text
k8s-axi logs [TARGET] [-n NS] [-c CONTAINER] [-l SELECTOR] [--previous] [--tail N] [--since DUR] [--max-bytes N]
```

`TARGET` is a pod name, or `KIND/NAME` for `pod`, `deployment`, `statefulset`, `daemonset`, `replicaset` or `job`.
A workload resolves to its pods through `spec.selector` (a Job also by its `batch.kubernetes.io/job-name` label).
`-l SELECTOR` with no target selects pods directly.

- Defaults: `--tail 100` lines per container (0 means up to 10000, at most 10000 allowed), `--max-bytes 65536` across the whole response.
- At most 5 pods are read (first by name).
  The note says how many matched and to name a pod or narrow `-l` to see others.
- Without `-c`, every regular container of each pod is read (init containers only when named with `-c`).
- The byte budget is split evenly over the streams and the oldest lines are dropped first.
  `truncated` is true when lines were dropped for the tail count or the byte budget, and the note says which.
- `--previous` reads the last terminated container.
  A container with none is reported on its stream, and the command fails only when no stream could be read.
- The whole text of each stream goes through `redact.Mask` before it is split into lines, so multi-line private keys are caught.
  `redacted` counts the replacements.

Output:

```text
target: deployment/web
namespace: default
pods: 3                  # pods matched
truncated: false
redacted: 0
note: ...
streams[2]:
  - pod: web-1
    container: app
    lines: 100
    truncated: true
    log[100]{line}:
      ...
next[1]: ...
```

## Package layout

All in `internal/kube`, one file per concern, tests next to them:

| File | Content |
|---|---|
| `resolve.go` | kind argument parsing, `Clients.Resolve`, the `Resource` type, target splitting |
| `read.go` | typed table, dynamic fallback, `list` and `get` returning unstructured, label and field selector handling |
| `scrub.go` | the scrubbing described above, Secret key and size summary |
| `apierr.go` | API error to `axierr` with hints |
| `rows.go` | row types, age formatting, per-kind summaries |
| `details.go` | per-kind summaries and conditions for inspect |
| `common.go` | namespace scoping, argument-count usage errors, next-step command lines |
| `get.go`, `inspect.go`, `events.go`, `logs.go` | the commands |

`Load` wraps the mapper with the short-name expander.
Tests build the mapper from fake discovery data, so no cluster is needed for unit tests.

## Tasks

Each task is test first, then code, then one conventional commit.

1. **Plan** (this document).
2. **Resolve kinds.**
   Tests: `po`, `pod`, `Deployment`, `deployments.apps`, `widgets.example.com`, exact version, slash form, a kind only in a non-preferred version, an unknown kind (usage error), cluster-scoped vs namespaced.
3. **Read and scrub.**
   Tests: typed path for a built-in kind, dynamic path for a CRD and for a built-in kind outside the table, list limit and continue token, selectors, Secret data never present, last-applied dropped, credential env masked, bearer token in a ConfigMap masked.
4. **get.**
   Tests: per-kind columns, `-A`, selector, `--limit` truncation note, empty note, single name, missing name, `--full`, `--json`, TOON default, Secret keys only.
5. **events.**
   Tests: sort order, object scope by kind/name and UID, `--type`, `--since`, `-A`, bound and truncation, warnings count, empty note.
6. **inspect.**
   Tests: Pod, Deployment, Secret and CRD details, condition rows, owner chain Pod to ReplicaSet to Deployment, unreadable owner, related events, `--labels`, `--annotations`, `--full`, Secret never leaks in any format.
7. **logs.**
   Tests: single pod, container selection, workload selector, `-l`, pod cap, byte budget and tail truncation (pure function), redaction including a private key block, `--previous` failure on one stream, all streams failing is an error.
8. **Integration tests on kind** for every command, run by the existing `integration` workflow on all five minors:
   a CRD served only in a non-preferred version with a custom resource read through `get` and `inspect`,
   a Secret with a sentinel value (also in a last-applied annotation) that must not appear in any format of `get` or `inspect`,
   a Pod with a credential-looking env literal,
   a Deployment owner chain,
   a warning event,
   a Pod that logs a token and a private key, with `--previous` on a restarted container.
9. **Docs.**
   README usage, `docs/compatibility.md` (what the matrix now exercises), the embedded skill, and `docs/design.md` is left unchanged.

## Risks and decisions

- Typed clients for 17 kinds add conversion code that the dynamic client alone would not need (my call: kept because the design and PRD ask for typed clients for built-ins, and it keeps list pagination and decoding on the well-trodden path for the common kinds).
- Events cannot be sorted or limited by the server in a way that returns the newest, so the 1000-event read cap is the honest bound (my call).
- Each container is read with `tail + 1` requested lines (the extra line shows that older lines exist) and a 4 MiB read limit, then cut to the byte budget here, so a chatty container cannot make the command read an unbounded stream.
- `redact.Mask` is pattern based and cannot catch every secret.
  Logs are best-effort redacted and the output says how many replacements were made.
