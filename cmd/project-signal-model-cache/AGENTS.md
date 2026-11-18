# Project Signal model cache

- Keep model-cache-owned code under this directory.
- Keep `main.go` small; move implementation under `internal/` when the binary grows beyond straightforward command wiring.
- Do not import another `cmd/` tree or create shared helpers for behavior used only by this binary.
- Preserve digest verification, immutable model identity, and fail-closed cache behavior.
- Add focused tests beside the implementation.
