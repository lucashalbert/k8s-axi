# apply and delete

Status: plan for the `0.4.x` slice of the command table in [design.md](../design.md#command-surface).
Inputs: the approved design, PRD sections 6.8 (mutations) and 9 (safety), the shipped [plan, scale and rollout restart](mutations.md) code, and the `mutate` package of axi-sdk-go v0.2.0.
Items marked (my call) were chosen by the author of this plan.

## Intent

`apply` creates or updates objects from manifests with server-side apply.
`delete` removes one named object and is the first destructive command.
Both reuse the mutation path that `scale` and `rollout restart` established: resolve, read live state, build a `mutate.Plan`, let `mutate.Run` authorize and print it (target first), then act.

## Non-goals

- Client-side apply, three-way merge and `--prune`.
- Delete by selector, by `--all`, or by wildcard.
  `--all`, `--selector` and `-l` exist only to give a usage error that says so.
- Fetching manifests from a URL, recursive directories and `kustomize`.
- Waiting for rollouts or for deleted objects to disappear.
- Client-side dry-run (`--dry-run=client` is refused, as for the other mutations).

## Commands

```text
k8s-axi apply -f FILE|DIR|- [-f ...] [-n NS] [--force-conflicts] [--dry-run=server]
k8s-axi delete KIND/NAME --confirm KIND/NAME [-n NS] [--propagation background|foreground|orphan] [--dry-run=server]
k8s-axi plan apply -f ...
k8s-axi plan delete KIND/NAME
```

`plan apply` and `plan delete` are the same code in dry-run mode, built from the same constructors, like the other `plan` subcommands.

### Shared rules

- The kubeconfig is only read, and the selected `--context` is never made current.
- The plan is printed before anything changes and carries `scope` with `context`, `server` and `namespace`.
- `--dry-run=server` sends the real requests with `dryRun=All` and never applies.
- Everything runs under `--timeout`; objects are processed one at a time, so concurrency is 1.
- Namespace rule as before: a namespaced object without a namespace needs `-n` or a namespace on the kubeconfig context; falling back to `default` is a usage error.

### apply

- Input: `-f` takes a file, a directory (`.yaml`, `.yml`, `.json`, non-recursive, sorted) or `-` for stdin, and may repeat.
  Documents may be multi-document YAML, JSON, or a `List` with `items`.
  Input size is capped (16 MiB) so stdin is bounded.
- Pre-flight, all before the plan is printed and all usage errors (exit 2): unparsable document, missing `apiVersion`, `kind` or `metadata.name` (`generateName` alone is not enough, server-side apply needs a name), two documents naming the same object, an object namespace that disagrees with `-n`, an ambiguous namespace, and a kind the cluster does not serve (unless the manifest creates the CRD that serves it).
- Resolution goes through discovery and the REST mapper by the object's own `apiVersion` and `kind`, so built-ins and CRDs take one path.
  The shared dynamic client does all I/O.
- Per object the plan phase reads the live object and sends a server-side apply with `dryRun=All` under the field manager `k8s-axi`.
  That gives the exact result the server would store, so the plan can say `create`, `configure` (with the changed field paths) or `unchanged` without guessing.
- A field conflict is shown in the plan (`conflict deployment/web: .spec.replicas owned by "helm"`).
  `--force-conflicts` is a separate flag and the only way to take ownership; it is never implied.
- Unchanged objects are not sent (PRD: avoid unnecessary changes).
- Apply order: Namespaces, then CRDs, then the rest in manifest order.
  An object whose namespace or CRD is created by the same manifest is planned as `pending` and applied after it; for a CRD the command waits (bounded by `--timeout`) until discovery serves the new kind.
- Partial failure: every object is attempted, each gets its own status in the result, and any failure makes the exit code 1.
- Secrets: values are never printed.
  A Secret in the plan shows its keys and byte sizes.
  Errors for a Secret have its values removed before they are printed, on top of the SDK masking.
  Live objects and the proposed result are only ever diffed by path, never printed.

Result document: `status` per object (`created`, `configured`, `unchanged`, `failed`, or `would-create`, `would-configure` in a dry run), counts, `next` hints.
A plan with nothing to change is a no-op: only the plan, then the result listing every object as `unchanged`.

### delete

- Resolves the target like the other mutations and builds a destructive `mutate.Plan` whose `Target` is `kind/name` in lower case, for example `deployment/web`.
- `--confirm` must equal that target exactly.
  A missing or mismatched token is a usage error naming the expected token and happens before the object is read, so nothing is deleted or even fetched.
  `deploy/web` does not match `deployment/web`; that is intended, the token is the resolved identity.
- Names with `*` or `?`, a missing name, `--all`, `--selector` and `-l` are usage errors.
- `--propagation` default is `background`.
- The delete carries a UID precondition from the live read, so a recreated object of the same name is not removed by a stale plan.
- Deleting a Namespace or a CustomResourceDefinition adds a `WARNING:` line to the plan that says what else goes with it.
  Finalizers on the live object are reported as `notable:`.
- Result: `deleted` when a follow-up read finds nothing, `terminating` when the object is still there (finalizers, namespaces), `validated` for a dry run, with `next` hints.

## Package layout

New files in `internal/kube`, one concern each:

| File | Content |
|---|---|
| `apply_manifest.go` | reading files, directories and stdin; decoding; shape validation |
| `apply_diff.go` | changed-path diff, conflict parsing, Secret key listing and value scrubbing |
| `apply.go` | the `apply` command, assessment, ordering, result |
| `delete.go` | the `delete` command |

Root wiring is `mutationCmds` and `planCmd` each gaining the new constructors.

## Tasks

Each task is test first, then code, then one conventional commit.

1. **Plan** (this document).
2. **Manifest loading.**
   Tests: single and multi-document YAML, JSON, `List`, empty documents, directory ordering and filtering, stdin, size cap, missing kind, name or apiVersion, `generateName` only, duplicates.
3. **Diff helpers.**
   Tests: changed paths ignore server-owned fields and status, lists, conflict causes, Secret keys and sizes, Secret value scrubbing.
4. **apply.**
   Tests: create, configure, unchanged, conflict then `--force-conflicts`, `--dry-run=server` persists nothing, `--dry-run=client` rejected, field manager and force reach the server, namespace rules, CRD kind through the mapper, pending namespace and CRD, partial failure and exit code, Secret values absent from every output format, kubeconfig untouched.
5. **delete.**
   Tests: missing and mismatched token (usage error, nothing read or deleted), match deletes, UID precondition, propagation values, namespace and CRD warnings, `--all`, `-l` and wildcard refused, dry run, terminating object, not found, forbidden hint.
6. **plan apply and plan delete**, wiring, and help text.
7. **Integration tests on kind** across the five-minor matrix: CRD plus custom resource in one manifest and in a directory, a conflict and its `--force-conflicts` resolution, multi-document apply, unchanged re-apply, `--dry-run=server` persisting nothing, delete with a missing and a mismatched token (object still there), delete with the right token, kubeconfig bytes and current context unchanged.
8. **Docs.** README, `docs/compatibility.md`, the embedded skill.

## Risks and decisions

- One plan per command, not per object (my call).
  `mutate.Plan` has one `Target`, so a single object uses its id and several use `N objects`; each object is a line in `changes`.
- A plan phase that already sent dry-run requests costs one extra read and one extra apply per object (my call).
  It is the only way to tell create from configure from unchanged exactly, and it surfaces conflicts before anything is written.
- Server-side apply of an object whose content is unchanged would still record field ownership for a new manager.
  Skipping it is the PRD's rule and keeps the command free of writes on a re-run; the first real change claims ownership then.
- Scrubbing Secret values out of errors matches values of four or more characters, so a very short value could survive in a server message.
  The commands never print a manifest or a live object, which is the main protection; scrubbing is the second layer.
