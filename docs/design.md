# k8s-axi and axi-sdk-go - design

Status: approved by the captain 2026-11-07.
Source inputs: the captain's request, the PRD at `~/Downloads/kubernetes-axi-prd.md`, `proxmox-axi`, the AXI repo and its CONTRIBUTING.md.
Items marked (my call) were chosen by the author and were not stated by the captain.

## Intent and why

Agents that operate Kubernetes spend tokens and turns on verbose, human-shaped `kubectl` output and on correlating several commands by hand.
`k8s-axi` is an agent-first command line tool for Kubernetes that answers operational questions in few calls and few tokens.
It follows the AXI principles from https://github.com/kunchenguid/axi and is modelled on https://github.com/lucashalbert/proxmox-axi.
The captain also plans more AXI tools, such as `argocd-axi`, so the reusable part is built as its own public Go SDK.

## Deliverables

1. `github.com/lucashalbert/axi-sdk-go`: a public Go SDK with no Kubernetes dependency.
2. `github.com/lucashalbert/k8s-axi`: the public Go CLI, binary name `k8s-axi`, built on the SDK and on `client-go`.

Both repositories are public, BSD 3-Clause licensed (captain's decision, 2026-11-07), created with `gh-axi` under the captain's account.
The captain gave explicit consent on 2026-11-07 for exactly: `lucashalbert/axi-sdk-go` and `lucashalbert/k8s-axi`, public, BSD 3-Clause.

## Goals

- Read-only inspection, diagnostics and status first, then mutations, delivered across many small releases starting at `0.0.1`.
- Runtime detection of the cluster version and API capabilities through discovery and REST mapping, not a hardcoded resource list.
- Built-in resources through typed clients and everything else, including CRDs, through the dynamic client.
- Kubeconfig contexts honoured, so an agent can choose which cluster a command talks to.
- Compact TOON output by default, with `--json`, `--yaml` and `--full` available.
- Safe, deterministic, non-interactive behaviour, with stable exit codes.
- `k8s-axi version`, `k8s-axi update` and a background update notice.
- CI, and automated releases through semantic-release plus GoReleaser.

## Non-goals

- Replacing every `kubectl` feature, a TUI or dashboard, a hosted service, a plugin marketplace.
- Full ArgoCD, Kargo or EKS integrations in the initial releases.
- CRD schema validation beyond what the API server's server-side dry-run provides.

## Anti-goals (a failure even if it technically works)

- Printing Secret values, tokens or kubeconfig contents by default.
- Silently changing the kubeconfig's current context, or silently widening a mutation's scope.
- Any interactive prompt in a normal run.
- The SDK importing any Kubernetes library, or the CLI re-implementing SDK concerns.
- An update check that slows, blocks or fails a command.

## Decisions made by the captain

- Language is Go, using `client-go`.
- Default output is TOON.
- The shared runtime is a separate public repository, `axi-sdk-go`.
- Releases use semantic-release for versioning plus GoReleaser for artifacts, starting at `0.0.1`, one release per slice of the PRD.
- Supported Kubernetes versions: the latest five minor releases, tested with `kind` in a GitHub Actions matrix, with fake clients for unit tests.
- `k8s-axi update` fetches and installs the latest GitHub Release, verified by its SHA-256 checksum.
- Each run performs a cached update check at most once per day and prints a one-line notice on stderr.
  `K8S_AXI_NO_UPDATE_CHECK=1` turns the check off.
- A skill plus a `k8s-axi setup hooks` subcommand wires a session-start dashboard for agents, mirroring `proxmox-axi`.
- `--context` selects a kubeconfig context, and the tool never switches the current context.
- Contributions go through `no-mistakes`, and the AXI catalog entry is added after the first release, as CONTRIBUTING.md describes.
- The PRD's phases are all in scope, delivered in order.

## Decisions made by the author (my call)

- Names: binary, module and project are `k8s-axi`. The PRD's `kaxi` and the `kubernetes-axi` in the request are treated as older names.
- CLI framework is Cobra.
- TOON encoding uses `github.com/toon-format/toon-go` if it passes a quality check by the builder (correct output on the TOON spec examples, maintained, permissive license).
  Otherwise the builder reports before writing an encoder.
- Global flags: `--context`, `--kubeconfig`, `-n/--namespace`, `--json`, `--yaml`, `--full`, `--limit`, `--timeout`.
  Environment variables use the `K8S_AXI_` prefix, and `KUBECONFIG` keeps its standard meaning.
- Secret values are never output by default.
  Secret reads show keys and sizes only.
  A flag to reveal values is not part of the first releases.
- Destructive commands (`delete`) need `--confirm <kind/name>` matching the resolved target exactly.
  Other mutations need explicit verbs, support `--dry-run=server`, and print the target cluster context, namespace and resource.
- Diagnostics label each finding `observed` (read from the cluster) or `inferred` (derived), with the evidence it used.
- Update check: GitHub Releases API, 2 second timeout, cached under the user cache directory, never fails or delays the command beyond that timeout.
- `version` prints release version, commit and build date, injected at build time by GoReleaser ldflags.
- Platforms: linux, darwin and windows on amd64 and arm64.
- Module and CI: GitHub Actions, `golangci-lint`, `go vet`, `go test -race`, `govulncheck`.
- Go version: the current stable release at the time of scaffolding, pinned in `go.mod` and CI.

## Repositories and layout

`axi-sdk-go` (no Kubernetes imports, enforced by a CI check on the import graph):
- command registration and module composition
- configuration loading
- output and serialization (TOON, JSON, YAML)
- structured errors and stable exit codes
- redaction helpers
- mutation planning and authorization (plan, confirm token, dry-run)
- version and self-update helpers, update-check cache
- agent setup helpers (skill and hooks installers)

`k8s-axi`:
- `cmd/k8s-axi`: the binary, wiring the SDK and the Kubernetes module
- Kubernetes module: client configuration, version and API discovery, REST mapper, typed and dynamic client factories, resource operations, diagnostics, mutations
- `skills/k8s-axi`: the installable skill
- `docs/`: this design, the compatibility policy and command reference

The exact internal package layout is left to the builder, subject to the SDK having no Kubernetes imports.

## Command surface

Delivered in this order, names may be refined during the specification step:

| Release | Commands |
|---|---|
| 0.0.x | `version`, `update`, `cluster info`, `discover` |
| 0.1.x | `get`, `inspect`, `events`, `logs` |
| 0.2.x | `diagnose` for deployments, statefulsets, daemonsets, pods, jobs and services |
| 0.3.x | `plan`, `scale`, `rollout restart` |
| 0.4.x | `apply`, `delete` with safeguards |
| 0.5.x | sample non-Kubernetes module in the SDK, compatibility matrix hardening, performance tests |

The first slice also includes `setup hooks` and the skill, so agents can use it from day one.
Breaking changes stay in `0.x` until the captain declares `1.0` (my call; the release tooling is configured so a breaking commit does not jump to 1.0.0).

## Releases and CI

- Conventional commits drive semantic-release.
  A baseline tag `v0.0.0` makes the first fix release `0.0.1`.
- One release workflow runs semantic-release, which runs GoReleaser through the exec plugin with the computed version.
  This avoids relying on a tag pushed by the default token triggering a second workflow.
- GoReleaser builds archives per platform, a `checksums.txt`, and publishes the GitHub Release.
- CI on every PR: format check, lint, vet, unit tests with the race detector, `govulncheck`, a build of all targets, a commitlint check on the PR title.
- Integration job: `kind` clusters for each of the five supported minors, running the integration and acceptance tests.
- The SDK repository has the same pipeline minus the cluster jobs.

## Safety and behaviour contracts

- Read-only commands need no confirmation.
- Errors are structured and short, with the failed operation, the target, and a remediation hint, and never include credentials or secret values.
- Exit codes: 0 success, 1 runtime or API error, 2 usage error, with further codes documented by the builder.
- Output that is truncated says so explicitly and says how to see more.
- Empty results are stated, not silent.
- Requests are bounded by timeouts and cancellation, and concurrency is capped.

## Delivery and process

- Delivery mode is no-mistakes for both repositories, since the captain asked for it and CONTRIBUTING.md requires it.
- The first worker creates the repositories only after the captain's yes, with the design committed under `docs/`.
- Workers use the Go skills: golang-patterns, go-review, go-build, go-test, and ponytail.
- Work is split into jobs, each sized to the model that fits (author's heuristic):
  architecture, SDK interfaces and diagnostics logic use the strongest model,
  CI, GoReleaser and release plumbing use the default model,
  docs, skill text and mechanical test fixtures use a lighter model.
- Nothing merges without the captain's explicit word.

## Calls settled during the builder read (my call)

- License is BSD 3-Clause (captain's decision, overriding my MIT call).
  `toon-format/toon-go` is MIT, which is compatible, and is not archived (last pushed 2026-10-09).
- Kubeconfig exec credential plugins (for example `aws eks get-token`) are allowed, because EKS and similar clusters need them.
  They are the standard `client-go` mechanism and are documented as the only external programs the tool can start.
  This does not conflict with the PRD rule against running arbitrary shell commands for resource operations.
- `apply` uses server-side apply with the field manager name `k8s-axi`, and `--force-conflicts` is a separate explicit flag.
- FIPS-validated builds are not part of the first releases.
  GoReleaser can add them later through the Go FIPS 140 module setting if the captain asks.
- Supporting five Kubernetes minors with one `client-go` version relies on discovery and the dynamic client.
  The builder documents the `client-go` skew and tests every minor in the kind matrix.
- This design document is committed to `docs/` in `k8s-axi` by the first worker.

## Left to the builder

- Exact Go package layout and interface names.
- Specific Cobra structure and flag spellings beyond those listed.
- The compatibility policy text and the exact five Kubernetes minors on the day of scaffolding.
- Details of the TOON schema for each command, provided output is compact, consistent and states counts, truncation and next steps.
- Choice of test libraries.
