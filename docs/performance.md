# Performance and resource budgets

k8s-axi talks to clusters of any size, so every command has a bound on what it reads and prints.
The bounds are enforced by unit tests that run against a server which honours `limit` and `continue`, so they hold without a cluster.
The benchmarks print the cost of the commands most likely to run against a large cluster.
They are informational: the `bench` job in the `ci` workflow runs them on every pull request, uploads the output as the `benchmarks` artifact and never fails a build.

## Budgets

| Command | Bound | Enforced by |
|---|---|---|
| `get` | One list request of `--limit` items (default 50); the server cuts the list and the result says how many remain. | `TestGetListIsCutAtTheServer` |
| `get --limit 0` | Chunks of 500 items per request, so no single response holds the whole collection. Memory still grows with the collection, because the rows are built after the last page. | `TestGetLimitZeroPagesTheList` |
| `discover` | Two discovery requests however many API groups exist (aggregated discovery), at most 4 requests in flight, CRDs read in pages of 100, wall time under 10 s for 1200 groups. | `TestDiscoverManyCRDsIsBounded` |
| `events` | One request of at most 1000 events; the result is cut at `--limit` and says that the newest may be missing. | `TestEventsReadOneBoundedPage` |
| `logs` | At most 4 pages of 500 pods per selector, 5 pods read, `--tail` lines and `--max-bytes` bytes in total, at most 4 MiB read from one container. The result says "at least N pods" when the scan stopped. | `TestLogsManyPodsIsBounded`, `logs_test.go` |
| `diagnose` | One page of 100 pods, 1000 events, 3 log streams of 20 lines and 4 KiB, concurrency 4, and three times `--timeout` for the whole command. | `TestDiagnoseManyPodsIsBounded` |

Every request is bounded by `--timeout` (default 30 s), and the client is rebuilt from discovery on each run, so nothing is cached on disk for correctness.

## Benchmarks

```sh
go test ./internal/kube -run '^$' -bench . -benchmem
```

| Benchmark | What it runs |
|---|---|
| `BenchmarkGetLargeList/limit=50` and `limit=0` | `get pods -A` over 20000 pods |
| `BenchmarkDiscoverManyCRDs` | `discover` over 1200 API groups, each with a CRD, through a local HTTP server |
| `BenchmarkEventsLargeNamespace` | `events` over 5000 events |
| `BenchmarkDiagnoseManyPods` | `diagnose` of a DaemonSet with 5000 crash-looping pods |

Typical results on a laptop, for orientation only (the CI numbers vary with the runner):

| Benchmark | Time | Allocated |
|---|---|---|
| `get` 20000 pods, `--limit 50` | 1 ms | 1.2 MB |
| `get` 20000 pods, `--limit 0` | 190 ms | 375 MB |
| `discover` 1200 groups | 415 ms | 15 MB |
| `events` 5000 events | 1 ms | 4 MB |
| `diagnose` 5000 pods | 10 ms | 10 MB |

The fake servers have no network latency, so the numbers show client cost only.
Against a real API server the number of requests, which the tests bound, dominates.

## Changing a bound

A bound that changes must change its test, this page and the command reference in the README together.
