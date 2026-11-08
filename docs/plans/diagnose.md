# Diagnose

Status: plan for the `0.2.x` slice of the command table in [design.md](../design.md).
Inputs: the approved design and the PRD sections 6.6 (workload diagnostics), 6.9 (output), 9 (safety) and phase 4.
Style follows [read-core.md](read-core.md).
Items marked (my call) were chosen by the author of this plan.

## Intent

One command that answers "why is this workload or service unhealthy" in a single call.
It reads the object, its owners, its pods, the relevant events and a few bounded log tails, applies deterministic rules, and prints ranked findings with a short conclusion and next-step commands.
Each finding is labelled `observed` (read directly from the cluster) or `inferred` (derived by a rule) and carries the evidence it used.

## Non-goals

- Any mutation: no plan, scale, rollout restart, apply or delete.
- Any call outside the Kubernetes API, and any model call.
  Rules are plain Go functions over a snapshot of objects, so they are tested against fixtures.
- Following logs, watching, or diagnosing kinds other than the six below.

## Command

```text
k8s-axi diagnose KIND NAME | KIND/NAME [-n NS] [--full]
```

Supported kinds: deployments, statefulsets, daemonsets, pods, jobs and services (any alias the resolver accepts).
Any other kind is a usage error (exit 2) that lists the supported kinds.
Resolution, clients, scrubbing, redaction, error model and output rendering are the existing ones.
Exit code is 0 whenever a report was produced, healthy or not, so a report is never mistaken for a tool failure.
Reading the target failing is exit 1 with the usual API error hints.

## Evidence gathered

Bounds are constants, and every read honours the global `--timeout` per request.
The whole command also stops after three times `--timeout` (my call), keeping what it has and reporting the rest as gaps.

| Step | Read | Bound |
|---|---|---|
| target | get the object | 1 call |
| owners | owner chain, existing `ownerChain` | 5 levels, 10 rows |
| pods | list by `spec.selector` (Jobs also `batch.kubernetes.io/job-name`); a Pod target is itself | 100 pods, one page |
| replica sets | Deployment only: list by selector, keep those it owns | 100 |
| events | warning events in the namespace, matched locally to the target, its pods and replica sets by UID | 1000 scanned, existing `Events` |
| nodes | nodes of unhealthy pods, for readiness and pressure | 3 gets |
| quotas | ResourceQuotas in the namespace, only when replicas are missing | 1 list |
| endpoints | Service only: EndpointSlices by `kubernetes.io/service-name`, and namespace pods to explain a selector mismatch | 1 list each, 200 pods |
| logs | container logs of pods that crash, fail to become ready or ended in error, previous instance when the container restarted | 3 streams (5 with `--full`), 20 lines (100), 4 KiB (16 KiB) each |

Independent reads run in parallel with a concurrency cap of 4 (my call).
A read that fails (forbidden, not found, timed out) never fails the command.
It is listed under `gaps` with a short reason, so a missing check is visible and not mistaken for a pass.
Log text goes through the existing `readStream` (tail, byte budget, `redact.Mask`).
Event messages, condition messages and evidence strings are masked the same way.
No environment values, command arguments or Secret data are read into evidence.

## Findings

A finding has `rank`, `severity` (`critical`, `warning`, `info`), `basis` (`observed`, `inferred`), `rule`, `subject`, `summary` and `evidence`.
Findings of one rule that differ only by pod are merged into one with a pod count and at most 5 names.
Ranking is severity, then `observed` before `inferred`, then a fixed rule order that puts causes before symptoms, then subject.
It is deterministic and never depends on map order or time.

`status` is `unhealthy` when any finding is critical, `degraded` when the worst is a warning, otherwise `healthy`.
`conclusion` is one sentence built from the top finding, or says nothing was found and what was checked.

An observed fact and its inferred cause are separate findings, so the label is never blurred.

| Rule | Basis | Severity | Reads |
|---|---|---|---|
| `image-pull` | observed | critical | container waiting as ErrImagePull, ImagePullBackOff, InvalidImageName, ErrImageNeverPull |
| `image-pull-cause` | inferred | critical | the waiting message: tag or name not found, registry auth, registry unreachable |
| `crash-loop` | observed | critical | waiting CrashLoopBackOff, with restarts and the last termination reason and exit code |
| `container-failed` | observed | critical | a pod that failed with a container exiting non-zero |
| `container-error` | observed | critical | CreateContainerConfigError, CreateContainerError, RunContainerError and their messages |
| `init-failure` | observed | critical | init container stuck or terminated non-zero |
| `oom-killed` | observed | critical | last or current termination reason OOMKilled, with the memory limit |
| `oom-cause` | inferred | warning | the limit is reached; raise it or reduce use |
| `restarts` | observed | warning | restarts with a last termination, container currently running |
| `unschedulable` | observed | critical | PodScheduled false, reason Unschedulable, scheduler message |
| `unschedulable-cause` | inferred | critical | message parsed to insufficient cpu or memory, untolerated taint, node selector or affinity, unbound volume |
| `startup-failed` | observed | critical | FailedMount, FailedAttachVolume, FailedMapVolume and FailedCreatePodSandBox events |
| `probe-failed` | observed | warning | Unhealthy events: readiness, liveness, startup |
| `not-ready` | observed | warning | pod running but not Ready, when nothing more specific explains it |
| `evicted` | observed | critical | pod reason Evicted, with the message |
| `evicted-cause` | inferred | warning | node pressure named in the eviction message |
| `terminating-stuck` | inferred | warning | deletion timestamp more than 5 minutes old |
| `node-unhealthy` | observed | warning | node Ready not true, or Memory, Disk or PID pressure |
| `replica-failure` | observed | critical | ReplicaFailure condition or FailedCreate events from the controller |
| `quota-exceeded` | observed with a FailedCreate event naming a quota, inferred from quota usage alone | critical | the event, plus any ResourceQuota with used at hard |
| `rollout-stalled` | observed | critical | Progressing false with ProgressDeadlineExceeded |
| `rollout-progressing` | observed | info | updated below desired, or observedGeneration behind generation |
| `rollout-blocked` | inferred | warning | StatefulSet ordered update waiting on its first not-ready pod |
| `replicas-unavailable` | observed | critical if none available, else warning | desired, ready, available counts |
| `daemonset-unavailable` | observed | warning | desired, ready, unavailable and misscheduled counts |
| `paused` | observed | warning | Deployment `spec.paused` |
| `scaled-to-zero` | observed | info | desired replicas 0 |
| `job-failed` | observed | critical | Failed condition: BackoffLimitExceeded, DeadlineExceeded, with the message |
| `job-retrying` | observed | warning | failed pods so far against `backoffLimit`, job still active |
| `job-suspended` | observed | info | `spec.suspend` |
| `service-no-selector` | observed | info | selector empty, endpoints managed by hand |
| `service-no-pods` | observed | critical | selector matches no pod in the namespace |
| `service-selector-mismatch` | inferred | critical | pods in the namespace that share some selector keys but not their values |
| `service-no-ready-endpoints` | observed | critical | pods match, EndpointSlices have none ready |
| `service-partial-endpoints` | observed | warning | some endpoints not ready |
| `service-port-mismatch` | inferred | warning | `targetPort` not among the container ports the matching pods declare |
| `service-lb-pending` | observed | warning | type LoadBalancer with no ingress yet |

Classification of messages (cause rules) uses fixed substring tables, with unit tests per entry.
A message that matches nothing yields no cause finding, not a guess.

## Output

TOON by default; `--json`, `--yaml` and `--full` render the same structure.

```text
target: deployment/web
namespace: default
context: kind-dev
status: unhealthy
conclusion: 2 of 3 pods cannot pull image nginx:nope (ImagePullBackOff).
details: {desired, ready, available, updated, strategy, selector}   # existing per-kind summary
conditions[n]{type,status,reason,age,message}:
owners[n]{depth,kind,name,controller}:
findings[2]:
  - rank: 1
    severity: critical
    basis: observed
    rule: image-pull
    subject: pod/web-7d4-a, pod/web-7d4-b
    summary: ...
    evidence[3]: ...
pods: {total, shown, unhealthy, truncated, items[n]{name,ready,status,restarts,node,age}}   # unhealthy first
events: {count, total, truncated, items[n]{age,type,reason,object,count,message}}   # newest 5 warnings
logs[n]: pod, container, previous, lines, truncated, log[...]
gaps[n]: events: forbidden (RBAC denied 'list' on events)
checked: object, owners, pods (3), warning events (4), logs (1)
next[n]: k8s-axi logs pod/web-7d4-a --previous | k8s-axi events pod web-7d4-a
```

Empty `findings` is stated (`count: 0`) with the `checked` list.
`--full` keeps every pod row, longer log tails, complete messages and the scrubbed target object.
`next` holds at most 4 commands, each carrying `--context` and `--kubeconfig` as the other commands do.

## Package layout

New files in `internal/kube`; the only edit to existing code is adding `g.diagnoseCmd()` to the command list in `module.go`.

| File | Content |
|---|---|
| `diagnose.go` | the command, gathering with bounds and gaps, output types |
| `diagnose_model.go` | `finding`, `snapshot`, ranking, status and conclusion |
| `diagnose_logs.go` | log selection, bounded reads, next steps |
| `diagnose_pod.go` | pod and node rules, message classification tables |
| `diagnose_workload.go` | deployment, statefulset, daemonset, job, quota and event rules |
| `diagnose_service.go` | service and endpoint rules |
| `diagnose*_test.go` | rule tests over YAML fixtures, command tests over fake clients |
| `diagnose_integration_test.go` | kind tests |

Rules are `func(*snapshot) []finding` over unstructured objects and `corev1.Event`s.
They do no I/O and take no clock except the package `now`, so tests are fixtures and assertions.

## Tasks

Each task is test first, then code, then one conventional commit.

1. **Plan** (this document).
2. **Finding model and ranking.**
   Tests: severity then basis then rule order then subject, merge of per-pod findings with the 5-name cap, status and conclusion, determinism under shuffled input.
3. **Pod and node rules.**
   Tests per rule from fixtures: image pull and its causes, crash loop with last termination, OOM with the limit, container config error, init failure, restarts, unschedulable and each cause entry, probe events, not-ready, evicted, terminating, node conditions, and a healthy pod giving no finding.
4. **Workload rules.**
   Tests: Deployment (stalled, progressing, replica failure, paused, zero, unavailable), StatefulSet (blocked), DaemonSet, Job (failed, retrying, suspended), quota from events and from ResourceQuota objects.
5. **Service rules.**
   Tests: no selector, no pods, selector mismatch with a near-miss pod, no ready endpoints, partial, port mismatch (numeric and named), load balancer pending, a healthy service.
6. **Gathering and the command.**
   Tests with fake clients: every supported kind, unsupported kind usage error, missing target error, parallel reads bounded, a forbidden events list becoming a gap, the overall timeout keeping partial results, log reads capped and redacted, Secret and credential values never in any format, `--json`, `--yaml`, `--full`, TOON default.
7. **Integration tests on kind** for every kind, run by the existing `integration` workflow on all five minors:
   an image that cannot be pulled, a crash-looping container with a log line and a previous log, an unschedulable pod (impossible node selector), a failing readiness probe, a Job over its backoff limit, a Service with a selector mismatch and one with no ready endpoints, a Deployment that exceeds a ResourceQuota, and a healthy Deployment that reports healthy.
8. **Docs.**
   README usage, `docs/compatibility.md` (what the matrix now exercises), and the embedded skill.
   `docs/design.md` is left unchanged.

## Risks and decisions

- Event reasons and scheduler messages are free text.
  Cause rules are `inferred` for that reason, and the observed finding is always emitted alongside (my call).
- One page of 100 pods bounds cost on large workloads.
  The output says when it analysed fewer pods than exist, and unhealthy pods are chosen by a second pass over the page only (my call: no extra pagination).
- Warning events only are scanned, because the rules need Unhealthy, FailedScheduling, FailedCreate and similar, which are Warning events.
  Normal events remain available through `events`.
- Exit code 0 for an unhealthy report is deliberate, so scripts can tell "found a problem" from "could not run".
