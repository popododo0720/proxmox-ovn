# Development

Run the Go, native UI, and package checks with:

```sh
make test
make build
make package-check
make deb
make ui-test
```

The unit tests use in-memory control state and fake PVE/OVS command runners.
Host integration tests require a disposable PVE 9 cluster; do not run them on
a node carrying unmanaged production networking.

The PVE UI hook has no supported upstream plugin ABI. When adding a PVE build,
add an exact fixture and loader/injector test before expanding the allowlist.

`make package-check` validates maintainer scripts, the systemd dependency
graphs, local activation and readiness gates, strict supported-PVE9 UI
verification, the no-provider-rewire invariant, and the no-auto-central-enable
invariant. Its rolling-update tests also exercise inactive topology-only
clusters at 1, 3, and 5 nodes and prove that a persisted/live Corosync doctor
failure prevents the next node mutation. `make deb` builds static Go
executables with the Debian package version embedded, repeats checks against
the archive, and verifies that the manager, agent, UI, schema, examples,
scripts, targets, and OVN drop-ins are present. CI performs these builds in a
Debian 13 container. Static unit verification is not a substitute for a staged
PVE 9 host test.

## Canonical releases

`make deb` builds a local development/test package. It may be used for staged
PVE validation, but it is not a canonical public artifact and must not be
uploaded to GitHub Releases. `make release` is reserved for the tag-triggered
GitHub Actions workflow and fails outside GitHub Actions.

The canonical publisher uses the pinned Debian image
`docker.io/library/debian@sha256:34cd9e9fd437c0a095ec39cb2e73422c9f30821b0d0848ed74fd0d43bae4d958`,
Go 1.24.13, Node 24.18.0 for native UI tests, and dpkg 1.22.22. The DEB uses
deterministic xz level 6 with one compressor thread; commit ID, build date, and
`SOURCE_DATE_EPOCH` come from the tagged commit.

After updating every version field and the Debian changelog, commit the clean
tree and push the new annotated `vVERSION` tag. Do not create, upload, edit,
delete, or publish a release manually. GitHub Actions claims the bot-authored
draft before building, verifies the exact seven assets and their SHA-256
values, and publishes it. Retry the same workflow against its existing draft;
never move or replace a release tag or published asset.

Enable GitHub's **Immutable releases** repository setting before publishing
and keep it enabled. The workflow also treats every published release as
immutable and accepts a rerun only when the bot-authored assets exactly match
the rebuild.
