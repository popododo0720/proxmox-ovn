# Development

Run the Go and web checks with:

```sh
make test
make build
make package-check
make deb
npm --prefix web ci
npm --prefix web test -- --run
npm --prefix web run build
```

The unit tests use in-memory control state and fake PVE/OVS command runners.
Host integration tests require a disposable PVE 9 cluster; do not run them on
a node carrying unmanaged production networking.

The PVE UI hook has no supported upstream plugin ABI. When adding a PVE build,
add an exact fixture and loader/injector test before expanding the allowlist.

`make package-check` validates maintainer scripts, the systemd dependency
graphs, local activation and readiness gates, strict supported-PVE9 UI
verification, the no-provider-rewire invariant, and the no-auto-central-enable
invariant. `make deb` builds static Go executables with the Debian package
version embedded, repeats checks against the archive, and verifies that the
manager, agent, UI, schema, examples, scripts, targets, and OVN drop-ins are
present. CI performs these builds in a Debian 13 container. Static unit
verification is not a substitute for a staged PVE 9 host test.
