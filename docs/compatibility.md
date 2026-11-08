# Kubernetes compatibility

k8s-axi supports the five newest stable Kubernetes minor releases.
The list below was resolved on 2026-11-07 from the official release list (`dl.k8s.io/release/stable.txt` and the `kubernetes/kubernetes` releases).

| Kubernetes | kind node image (pinned) |
|---|---|
| 1.37 | `kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5` |
| 1.36 | `kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed` |
| 1.35 | `kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0` |
| 1.34 | `kindest/node:v1.34.11@sha256:44e222ee2132dab25ff87301682f89eb82c7880ea3a1bf543bfe9708fd08d67d` |
| 1.33 | `kindest/node:v1.33.12@sha256:3f5c8443c620245e4d355cfe09e96a91ead32ceaa569d3f1ca9edf0cb2fe2ff4` |

The `integration` workflow runs the `integration`-tagged tests against a kind cluster (kind v0.33.0) for each row.
The 1.33 image comes from the kind v0.32.0 release, because v0.33.0 only publishes images for 1.34 and newer.

## client-go skew

k8s-axi is built with `k8s.io/client-go` v0.37, which matches Kubernetes 1.37.
Kubernetes supports clients within one minor version of the API server, so a client-go v0.37 is only formally supported against 1.36 to 1.38.
k8s-axi widens that window by not depending on a fixed resource list:

- Resources are found at run time through API discovery.
- Resources outside the typed table are read through the dynamic client, so a kind that is new or removed on one side does not need a code change.
- Kinds are resolved through the REST mapper built from live discovery, so a kind served only in a non-preferred version still resolves.
- Typed clients are used only for `v1` APIs of long-lived built-in kinds (core, `apps`, `batch`, `discovery.k8s.io`), which exist in all five supported minors.
- Everything else, including every CRD, goes through the dynamic client.
- The commands use stable surfaces only: `/version`, `/api` and `/apis` discovery, list and get, pod logs, and core events.

The kind matrix runs `get`, `inspect`, `events`, `logs` and `diagnose` against every minor, including a CRD served in a non-preferred version, a Secret whose value must never appear in any output format, credential redaction in logs, `--previous` logs of a restarted container, and the structured error for a service account without permissions.

`diagnose` is exercised against a pod that cannot pull its image, an unschedulable pod, a crash loop with its previous log, a failed Job, a failing readiness probe, services with a selector typo and with no ready endpoints, a Deployment over a ResourceQuota, and healthy workloads of every supported kind.
Its rules read documented status fields and conditions, plus the wording of a few scheduler, kubelet and controller messages, so a change in that wording only costs an inferred cause finding and never the observed finding next to it.
It also uses stable surfaces only: list and get of pods, replica sets, nodes, resource quotas, endpoint slices and events, and pod logs.

Every supported minor is exercised by the kind matrix, so a regression against 1.33 to 1.35 is caught in CI rather than assumed away.
Clusters outside the five supported minors may work but are not tested.

## Updating the matrix

When a new Kubernetes minor ships:

1. Add its kind node image, pinned by digest, to `.github/workflows/integration.yml` and drop the oldest minor.
2. Update the table above and the client-go version in `go.mod`.
3. Record the date the list was resolved.
