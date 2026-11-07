# Release process

`k8s-axi` releases from `main` with semantic-release and GoReleaser. Pull request
titles are conventional commits, and the squash-merge commit message is the
commit semantic-release evaluates on `main`.

## First slice

The first release slice is `0.0.1`. That version depends on merging the first
slice PR by squash with a `fix:`-typed PR title, for example:

```text
fix: add version, update, cluster info and discover commands
```

The repository starts from a `v0.0.0` baseline. With that baseline, the feature
commits on the branch would release `0.1.0` if they landed on `main` as separate
`feat:` commits. Squash merging discards those branch commit subjects for release
analysis; they appear only in the squash commit body, while the PR title becomes
the analyzed commit subject. A `fix:` squash commit therefore produces the
required patch release, `0.0.1`.

Do not change `.releaserc.json` to special-case the first slice. Its `feat`
release rule is intentional: later feature commits release minor versions
starting at `0.1.x`, and breaking changes stay within `0.x` until `1.0` is
declared.

## Normal releases

After `0.0.1`, keep using conventional PR titles:

- `fix:` and `perf:` publish patch releases.
- `feat:` publishes minor releases while the project is in `0.x`.
- Breaking changes also publish minor releases while the project is in `0.x`.

The release workflow runs semantic-release on pushes to `main`. semantic-release
computes the next version, pushes the tag, and invokes GoReleaser through the
exec plugin to publish the archives and checksums.
