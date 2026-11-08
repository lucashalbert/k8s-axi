# First mutations: plan, scale, rollout restart

Status: plan for the `0.3.x` slice of the command table in [design.md](../design.md#command-surface).
Inputs: the approved design, the PRD sections 6.8 (mutations) and 9 (safety), and the `mutate` package of axi-sdk-go v0.2.0.
Items marked (my call) were chosen by the author of this plan.

## Intent

Three commands that change a workload's size or restart it, and show exactly what they will do first.
They share one safety path: resolve the target, read its live state, build a `mutate.Plan`, and let `mutate.Run` authorize, print and carry it out.

## Non-goals

- `apply`, `delete`, `diagnose` and everything later.
- A confirm token.
  Scale and restart are not destructive, so `mutate.Plan.Destructive` is false and no `--confirm` exists.
- Client-side dry-run.
  The PRD says it is not a substitute for server validation, so `--dry-run=client` is rejected rather than faked.

## Commands

```text
k8s-axi scale KIND/NAME --replicas N [-n NS] [--dry-run=server]
k8s-axi rollout restart KIND/NAME [-n NS] [--dry-run=server]
k8s-axi plan scale KIND/NAME --replicas N [-n NS]
k8s-axi plan rollout restart KIND/NAME [-n NS]
```

`KIND/NAME` uses the same target syntax and resolution as the read commands (`splitTarget`, `Clients.Resolve`).

### Shared rules

- The plan is printed before anything changes, target first, with `scope` carrying `context`, `server`, `namespace` and `resource`.
- The kubeconfig is only read.
  The selected `--context` is used for the clients and is never made current.
- No context to pick (none given, none current) is already a usage error from `Load`.
- Namespace must be unambiguous (my call).
  A namespaced target needs `-n` or a namespace set on the kubeconfig context.
  Falling back to the silent `default` is a usage error for mutations, because the PRD forbids silently widening or redirecting a change.
  A cluster-scoped target with `-n` is also a usage error.
- `--dry-run` is `none` (default, applies) or `server`.
  `server` sends the real request with `dryRun=All`, through `mutate.DryRun` and its `Validate` hook, and never calls apply.
  `client` and anything else are usage errors that point at `--dry-run=server` and `plan`.
- Requests are bounded: the whole command runs under `--timeout`, which also bounds each request.
- Verbs are explicit: `get` and `update` on the `scale` subresource, `get` and `patch` on the workload.
- `--json`, `--yaml` and `--full` behave as for the read commands.
  The plan document comes first, then one result document, so JSON output is two objects in sequence.
- Concurrency (PRD 6.8): scale sends the `resourceVersion` it read.
  That version is the workload's own, which controllers bump with every status write, so a conflict is retried (at most 3 times) against a fresh read while the replica count is still the one the plan was made from.
  If another writer changed the replica count, the plan is stale and the conflict is reported with a retry hint instead of overwriting their change.
  Restart is a merge patch of one annotation, which is how `kubectl` does it, and does not need a version.
- A requested state that already holds yields an empty `changes`, which `mutate.Run` prints as a no-op without calling the API again.

### `scale`

- Works on anything with a `scale` subresource.
  The subresource is found through discovery (`<plural>/scale` in the resource's group version), so deployments, statefulsets, replicasets and CRDs that declare one all work, with no table.
  A kind without it is a usage error that says so.
- Reads the `autoscaling/v1` Scale through the dynamic client, so the same code serves every kind and every Kubernetes version.
- `--replicas` is required and must be zero or more.
- Changes: `replicas 3 -> 5`.
  Scaling to zero adds a second line `notable: scales to zero, no pods will run and the workload stops serving` (the brief asks for it to be reported as notable).
- Result after apply: desired replicas, the observed replica count from the response, and `next` hints (`inspect`, `events`).

### `rollout restart`

- Kinds: `deployments`, `statefulsets`, `daemonsets` in the `apps` group.
  Anything else is a usage error naming the three.
- Sets `spec.template.metadata.annotations["kubectl.kubernetes.io/restartedAt"]` to the plan's timestamp through a JSON merge patch, so the plan shows the exact value that is applied.
- A paused deployment is refused, as `kubectl` does, because the restart would not roll out.
- Changes: the annotation line, and how many pods the rollout replaces.
  Strategies that surprise are called out as `notable:` lines: `Recreate` on a deployment (downtime), `OnDelete` on a statefulset or daemonset (pods do not restart until deleted), and zero replicas (nothing to restart).
- Result after apply: the timestamp and `next` hints.

### `plan`

`plan scale` and `plan rollout restart` run the same code in `DryRun` mode only, so they cannot change anything and take no `--dry-run` flag.
They use a server-side dry-run request where the API supports it.
A resource that does not support dry-run is not an error for `plan`: the result says `validated: false` with a note, and the plan reflects the live state only.
The result carries a `next` hint with the exact command that applies the plan.

## Output

```text
plan:
  operation: scale
  target: deployment/web
  scope: {context, server, namespace, resource}
  changes[1]: replicas 3 -> 0 | notable: ...
  destructive: false
  mode: dry-run | apply
result:
  target: deployment/web
  status: applied | validated | not-validated
  replicas / observed_replicas / restarted_at
  note: ...
next[2]: ...
```

Errors use the shared model: operation, target, message, hint.
A conflict, a forbidden verb and an unsupported dry-run each get a specific hint.

## Package layout

New files in `internal/kube`, one concern each:

| File | Content |
|---|---|
| `mutation.go` | shared: `--dry-run` parsing, namespace rule, scope, target preparation, result document, the `mutate.Run` wrapper |
| `scale.go` | `scale` command and the scale subresource logic |
| `restart.go` | `rollout restart` command |
| `plan.go` | the `plan` command tree, built from the same constructors |

Root wiring is one added line in `Module.Register`, and `Clients` gets one field (`NamespaceFromContext`) set by `Load`.

## Tasks

Each task is test first, then code, then one conventional commit.

1. **SDK bump** to axi-sdk-go v0.2.0 (done).
2. **Plan** (this document).
3. **Shared mutation plumbing.**
   Tests: `--dry-run` values, namespace rule, scope contents, no-op plan, timeout wiring.
4. **scale.**
   Tests: deployment, statefulset, replicaset and a CRD with a scale subresource, a kind without one, same replicas is a no-op, scale to zero is notable, negative replicas, missing `--replicas`, stale resourceVersion conflict, `--dry-run=server` sends `dryRun=All` and persists nothing, `--dry-run=client` rejected, forbidden hint, JSON and TOON, kubeconfig untouched.
5. **rollout restart.**
   Tests: the three kinds, annotation patch content, paused deployment, strategy notes, unsupported kind, dry-run persists nothing, the plan timestamp equals the applied one.
6. **plan.**
   Tests: both subcommands change nothing, unsupported dry-run degrades to `validated: false`, the next hint is the apply command.
7. **Integration tests on kind** for the five-minor matrix: scale a deployment up, to zero and back, scale a statefulset and a CRD with a scale subresource, `--dry-run=server` changes nothing, `rollout restart` produces new pods, `plan` changes nothing, a service account without `patch` gets the forbidden hint, and the kubeconfig bytes and current context are unchanged.
8. **Docs.**
   README, `docs/compatibility.md`, the embedded skill.

## Risks and decisions

- All mutation I/O goes through the dynamic client (my call).
  The scale subresource has one shape for every kind and version, so one code path covers built-ins and CRDs, and the typed table is not extended.
- Strict namespace rule (my call).
  It costs one `-n` on contexts without a namespace and removes a class of wrong-namespace mistakes.
- Two documents on stdout (plan, then result) follow from `mutate.Run` printing the plan first.
  Consumers read a stream, as the docs say.
- `plan` swallows only the "dry run unsupported" error.
  Every other dry-run error, including forbidden and invalid, fails the command, because it predicts a real failure.
