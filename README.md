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

## Compatibility

The five newest stable Kubernetes minors are tested with kind, see [docs/compatibility.md](docs/compatibility.md).
The design is in [docs/design.md](docs/design.md).

## License

BSD 3-Clause, see [LICENSE](LICENSE).
