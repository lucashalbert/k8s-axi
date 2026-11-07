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
k8s-axi version
k8s-axi update                        # install the latest release, verifying its SHA-256 checksum
```

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

Both default to `$CLAUDE_CONFIG_DIR`, or `~/.claude`, and can be pointed elsewhere with `--settings` and `--dest`.

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
