# kubectl-unbounded

- Keep CLI-owned implementation under this command tree.
- Command packages under `app/` may use private helpers under `internal/`.
- Do not expose CLI-only compilers or binding helpers through repository-global `internal/` or `pkg/` packages.
- Do not import another `cmd/` tree.
- Keep exported private-package identifiers limited to the command surface that consumes them.
- Public Kubernetes contracts belong under `api/`; rendering and authoring workflow behavior remain private to this binary.
- Add focused command and compiler tests for success, failure, and malformed input.
