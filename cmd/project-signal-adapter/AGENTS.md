# Project Signal adapter

- Keep adapter-owned code under this directory.
- `main.go` is a minimal process wrapper. Runtime wiring and implementation belong under `internal/`.
- Do not import another `cmd/` tree.
- Keep exported identifiers under `internal/` limited to the entry point used by `main.go` and identifiers required by framework interfaces.
- Do not move execution, evidence, telemetry, or Kubernetes helpers into repository-global `internal/` or `pkg/` packages.
- Public wire contracts belong in `api/signal/v1alpha1`; adapter-specific parsing and rendering remain private here.
- Add focused tests beside the implementation and preserve bounded, fail-closed result handling.
