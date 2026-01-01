# AGENTS

## Development principles

1. Design for robustness. Keep modules aligned with SRP.
2. Use a ubiquitous language. Keep terminology consistent across code and docs.
3. Test-first. Prefer TDD when changing behavior.
4. Run golangci-lint v2. When changing code, run `make lint` and fix findings.
5. Use `cmp.Diff` in tests. Compare expected vs actual with `github.com/google/go-cmp/cmp.Diff`.
6. Make logging diagnosable. Logging level MUST be configurable (for example via env vars).
7. Prefer lnd libraries. Use lnd-provided APIs/libraries before re-implementing.
