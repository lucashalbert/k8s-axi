# Hardening

Status: plan for the `0.5.x` slice of the command table in [design.md](../design.md).
The sample non-Kubernetes module already shipped in axi-sdk-go v0.2.0, so it is not part of this slice.
Inputs: the design, the PRD sections 10 (performance), 11 (compatibility) and 12 (testing), and the review-focus list of the first-slice plan.

## Intent

No new commands and no behaviour changes, except defects the new tests expose.
The slice makes the compatibility claim checkable on every command, puts numbers on resource use, and pins the robustness cases that are only implied today.

## Tasks

Each task is TDD (a failing test or benchmark first) and one conventional commit.

1. **Smoke matrix.**
   A unit test fails when a leaf command is missing from the smoke list, so a new command cannot skip the matrix.
   One `integration`-tagged test runs every command once against the kind cluster: `version`, `update` (offline), `cluster info`, `discover`, `get`, `inspect`, `events`, `logs`, `diagnose`, `plan` (scale, rollout restart, apply, delete), `scale`, `rollout restart`, `apply` and `delete`.
   The CRD non-preferred-version, Secret-redaction and delete-confirmation cases stay in the matrix.
   Per-version defects the matrix exposes are fixed in their own commits.
2. **Compatibility docs.**
   `docs/compatibility.md` gets the supported versions, the client-go skew policy, known per-version differences and how the matrix is bumped, updated for the smoke matrix.
3. **Performance tests.**
   Fake-server tests for bounds, plus Go benchmarks:
   `get` on a large list (the list request carries `--limit`, rows and memory stay bounded),
   `discover` with many CRDs and API groups (concurrency capped, bounded wall time, CRD listing paged),
   `events` and `logs` byte and row bounds,
   `diagnose` on a workload with many pods (pod page, log streams and concurrency capped).
   Unbounded behaviour the tests expose is fixed.
4. **Benchmark CI job and budgets.**
   A `bench` job runs the benchmarks and uploads the output; it is informational and never blocks.
   `docs/performance.md` documents the budgets.
5. **Robustness tests** for the review-focus cases: no current context, unreachable API server bounded by `--timeout`, `update` interrupted or checksum-mismatched leaving the binary untouched, an offline update check not changing output or exit code, and empty results stated.
   Cases already pinned elsewhere are referenced rather than duplicated.
6. **Docs pass.**
   README command reference for all commands, the embedded skill in sync, and a short usage guide in `docs/usage.md`.
   CHANGELOG and generated release files are not touched.
