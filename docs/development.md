# Development

Run the Go and web checks with:

```sh
make test
make build
npm --prefix web ci
npm --prefix web test -- --run
npm --prefix web run build
```

The unit tests use in-memory control state and fake PVE/OVS command runners.
Host integration tests require a disposable PVE 9 cluster; do not run them on
a node carrying unmanaged production networking.

The PVE UI hook has no supported upstream plugin ABI. When adding a PVE build,
add an exact fixture and loader/injector test before expanding the allowlist.
