# Releasing dalforge

A release is a git tag. Everything else is automated by
`.github/workflows/release.yml`.

## What a release contains

| Artifact | Where | How users get it |
|---|---|---|
| `dalforge` binaries (linux, darwin × amd64, arm64) and `checksums.txt` | GitHub Releases, by GoReleaser (`.goreleaser.yaml`) | mise `"github:gisripa/dalforge"`, or download |
| the generator module, tag `vX.Y.Z` | the git tag you push | `go install github.com/gisripa/dalforge/cmd/dalforge@vX.Y.Z` |
| the runtime module `github.com/gisripa/dalforge/dal`, tag `dal/vX.Y.Z` | tagged by the workflow on the same commit | `go get github.com/gisripa/dalforge/dal@vX.Y.Z` |

Generated code and the runtime are released in lockstep: users run dalforge
`vX.Y.Z` with runtime `vX.Y.Z`.

## Steps

1. On `main`, with CI green, make sure the docs are current:
   `mise run docs && mise run check:all`.
2. Tag and push:

   ```sh
   git tag v0.1.0
   git push origin v0.1.0
   ```

   A tag with a pre-release suffix (`v0.2.0-rc.1`) is published as a GitHub
   pre-release.
3. The workflow runs CI, publishes the release with GoReleaser, and pushes
   `dal/v0.1.0`.
4. Check the release page, then try an install the way users will:

   ```sh
   mise exec github:gisripa/dalforge@0.1.0 -- dalforge version
   ```

## Before the first release

- The repository must be public, or users need `GOPRIVATE` and a token for
  `go install` and `go get`.
- Releases are `v0.x`: the IDL, the generated API and the runtime may still
  change between minor versions. Say so in the release notes when they do.

## Trying the release locally

```sh
mise exec goreleaser@latest -- goreleaser release --snapshot --clean   # builds into dist/, publishes nothing
```
