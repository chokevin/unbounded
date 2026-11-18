# Project Signal controller

- Keep controller-owned code under this directory.
- `main.go` is a minimal process wrapper. Runtime wiring and implementation belong under `internal/`.
- Do not import another `cmd/` tree.
- Keep exported identifiers under `internal/` limited to the entry point used by `main.go` and identifiers required by framework interfaces.
- Do not move controller helpers into repository-global `internal/` or `pkg/` packages to make them reusable.
- Public certification contracts belong in `api/signal/v1alpha1`; do not add controller behavior there.
- Add focused tests beside the implementation and preserve fail-closed admission, evidence, and qualification behavior.
