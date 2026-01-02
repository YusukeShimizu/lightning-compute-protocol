# Contributing to LCP

Contributions are welcome.
This project is in a PoC / MVP phase, so small and iterative PRs work best.

If you are new to Lightning, Go, or protocol work, open an issue or a draft PR and ask questions.

## Quick links

- Protocol draft (LCP v0.2): `docs/protocol/protocol.md`
- Reference implementation (daemon): `go-lcpd/`
- go-lcpd developer docs: `docs/go-lcpd/docs/`

## Docs (Mintlify)

This repo’s docs site is managed with Mintlify (`docs/docs.json`).
All Mintlify pages and assets live under `docs/` (Japanese pages are colocated with English and use a `-ja` suffix).

Local preview:

```sh
cd docs
npx --yes mintlify@4.2.255 dev --no-open
```

Validate navigation + check docs quality:

```sh
cd docs
node scripts/check-docs-json.mjs
npx --yes mintlify@4.2.255 a11y
npx --yes mintlify@4.2.255 broken-links
```

Language notes:
- A Japanese page usually mirrors its English page in the same directory, using a `-ja` suffix (example: `docs/go-lcpd/docs/regtest.md` ↔ `docs/go-lcpd/docs/regtest-ja.md`).
- For deep specs, the English docs are the SSOT; Japanese pages may be summaries.

Note: Prefer repo-root-relative links (for example `docs/go-lcpd/docs/regtest.md`, `docs/go-lcpd/docs/regtest-ja.md`) so the link checker resolves paths consistently.

## What you can help with

- Fix bugs, improve error handling, or simplify code
- Add tests (unit or integration) and improve reliability
- Improve docs (typos, clarity, new guides)
- Improve protocol spec wording, examples, or diagrams
- Add small features that are already described in the spec

If you're unsure what to pick up, open an issue with what you want to do and we'll point you to a good starting point.

## Ground rules (friendly + lightweight)

- Be kind and constructive. No gatekeeping.
- Prefer small PRs that are easy to review.
- If you're changing behavior, add/update tests when practical.
- When you touch the protocol spec, keep it compatible with Lightning/BOLT conventions.

## Development setup

This repo uses Nix flakes to keep the dev environment consistent.

Typical workflow:

```sh
cd go-lcpd
nix develop
make test
```

If you use `direnv`, `go-lcpd/.envrc.sample` can be copied to `go-lcpd/.envrc` to auto-enter the dev shell.

### Common commands (go-lcpd)

Run these from `go-lcpd/`:

```sh
make gen   # regenerate protobuf / generated code
make fmt   # gofmt (and related formatting)
make test  # unit + integration tests
make lint  # buf lint + golangci-lint
```

Notes:
- Generated Go files live under `go-lcpd/gen/`. Prefer editing `.proto` and running `make gen` instead of hand-editing generated code.
- If protobuf imports don't resolve in your IDE, `make proto-deps` exports dependency protos under `go-lcpd/proto/_deps/`.

## Making changes

### 1) Pick an issue (or create one)

For anything non-trivial, it helps to start with an issue describing:
- what you want to change
- why (bug, spec alignment, ergonomics, etc.)
- how you plan to test it

### 2) Open a PR early

Draft PRs are welcome — especially if you want feedback on direction before polishing.

### 3) Keep commits understandable

No strict rules here. A good default is:
- one topic per commit when possible
- messages that explain the intent (why), not just the change (what)

### 4) Run checks locally

Before requesting review:

```sh
cd go-lcpd
make test
make lint
```

If you changed protobuf definitions:

```sh
cd go-lcpd
make gen
make test
```

## Protocol spec contributions

Edits under `docs/protocol/` should:
- avoid requiring changes to core BOLT/LN behavior
- use TLV streams for extensibility
- keep the document format close to BOLT-style specs (tables, "MUST/SHOULD", clear message definitions)

If you're proposing a protocol change, include:
- motivation and threat model notes (even brief)
- an example flow (sequence diagram or step-by-step)
- backward/forward compatibility considerations

## PR checklist

Before you hit "Ready for review", it helps if:
- the PR description explains why the change is needed
- code is formatted (`make fmt` if relevant)
- tests added/updated for behavior changes
- `make test` and `make lint` are green (for `go-lcpd` changes)
- `(cd docs && npx --yes mintlify@4.2.255 broken-links)` is green (for docs changes)
- docs updated if behavior or flags changed

## Security

If you believe you've found a security issue, please avoid filing a public issue with exploit details.
Open a private report (e.g., GitHub Security Advisory) or contact the maintainers through a private channel.
