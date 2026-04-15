# stub-plugin

Minimal ArtificialPlugin used to exercise the `pluginhost` loader end-to-end.
Registers one MCP tool (`stub_help`) that echoes its input back wrapped in a
predictable envelope.

Not part of the production build — lives under `scratch/` as a throwaway.

## Build

```sh
cd scratch/stub-plugin
GOWORK=off go build -o stub-plugin ./
```

`GOWORK=off` is required because this module is deliberately outside the
top-level `go.work` workspace — it stands alone so it can be built and run in
isolation without polluting `cmd-worker`'s dependency graph.

## Use

Run the pluginhost integration tests:

```sh
cd src/cmd-worker
go test -tags=integration ./internal/pluginhost/...
```

The tests build a fake `/api/plugins` endpoint via `httptest`, point it at
the built stub binary, and assert the full load → `Tools()` → `Execute` →
`Shutdown` chain.

## Manual spawn

```sh
# Pretend to be a worker.
# stub-plugin speaks the hashicorp/go-plugin handshake on stdio — running it
# directly will hang; it only makes sense launched as a subprocess by the
# pluginhost loader.
```

The stub intentionally implements the bare minimum of `ArtificialPlugin`
(`Name`, `Tools`, `Execute`) so it exercises the wire format without any
plugin-specific business logic.
