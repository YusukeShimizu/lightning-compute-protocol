# Consolidate go-lcpd env config into `.envrc` and re-enable manifest resend interval

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This repository defines ExecPlan requirements in `.agent/PLANS.md`. Maintain this document in accordance with that file.

## Purpose / Big Picture

Developers and operators should configure `go-lcpd` using a single, consistent entrypoint: `go-lcpd/.envrc` (direnv), without a separate legacy env-file workflow. After this change, the background runner script `go-lcpd/scripts/lcpd-grpcd` does not source any env file.

Additionally, the environment variable `LCPD_LND_MANIFEST_RESEND_INTERVAL` becomes functional again. When set to a positive duration (for example `10s`), `go-lcpd` periodically re-sends `lcp_manifest` to currently connected peers at that interval. This is observable by enabling debug logs and/or by unit tests that assert multiple `SendCustomMessage(lcp_manifest)` calls occur over time/ticks.

## Progress

- [x] (2026-01-03 15:00Z) Wrote the ExecPlan and identified legacy env-file references.
- [x] (2026-01-03 15:10Z) Removed legacy env-file support from scripts and docs; deleted the sample artifacts.
- [x] (2026-01-03 15:25Z) Implemented periodic `lcp_manifest` resend behavior behind `LCPD_LND_MANIFEST_RESEND_INTERVAL` and added a unit test.
- [x] (2026-01-03 15:30Z) Updated `docs/go-lcpd/spec.md` and configuration docs to reflect the new behavior.
- [x] (2026-01-03 15:40Z) Ran `go test ./...` and `make lint`; confirmed the legacy filename no longer appears in the repo.

## Surprises & Discoveries

- Observation: The go-lcpd spec originally required `lcp_manifest` to be sent at most once per connection, which conflicted with the requested periodic re-send.
  Evidence: `docs/go-lcpd/spec.md` (before the change) described “MUST be sent at most once per connection” under `LNDPeerMessaging`.

## Decision Log

- Decision: Keep the “reply at most once” rule for inbound manifests, but allow optional periodic outbound re-sends controlled by `LCPD_LND_MANIFEST_RESEND_INTERVAL`.
  Rationale: Periodic outbound sends solve the requested operational need while still preventing manifest ping-pong loops.
  Date/Author: 2026-01-03 (Codex)

## Outcomes & Retrospective

- Outcome: The background runner script no longer supports sourcing an env file; go-lcpd configuration is consolidated into `.envrc` (direnv).
- Outcome: `LCPD_LND_MANIFEST_RESEND_INTERVAL` is active again; when set to a positive duration, `lcp_manifest` is periodically re-sent to connected peers.
- Validation: `cd go-lcpd && go test ./...` and `make lint` pass.

## Context and Orientation

Relevant paths in this repo:

- `go-lcpd/scripts/lcpd-grpcd`: Background runner script for `lcpd-grpcd` (does not source any env files).
- `go-lcpd/.envrc.sample`: Sample direnv configuration used for local development.
- `go-lcpd/internal/lndpeermsg/peermsg.go`: Manages lnd peer messaging, including sending `lcp_manifest`.
- `go-lcpd/internal/lndpeermsg/config.go`: Loads lnd-related configuration from environment variables.
- `docs/go-lcpd/spec.md`: The normative “What You See Is What It Does” spec for go-lcpd behavior. Code changes must match this doc.
- `docs/go-lcpd/docs/background*.md`: Docs for running `lcpd-grpcd` in the background.
- `docs/go-lcpd/docs/configuration*.md` and `docs/go-lcpd/docs/cli.md`: End-user configuration docs for environment variables.

Definitions:

- “direnv”: A tool that automatically loads environment variables when you `cd` into a directory containing `.envrc`.
- “lcp_manifest”: The LCP capability advertisement message exchanged over Lightning custom messages before job-scoped messages.
- “resend interval”: A duration string parseable by Go’s `time.ParseDuration` (examples: `10s`, `1m`, `250ms`).

## Plan of Work

1) Remove the legacy env-file workflow.

   - Delete the legacy env sample file under `go-lcpd/` and remove any repository-level references to the legacy filename.
   - Update `go-lcpd/scripts/lcpd-grpcd` to stop sourcing an env file and remove `LCPD_GRPCD_ENV_FILE` from its documented interface.
   - Update background-run docs (`docs/go-lcpd/docs/background.md` and `docs/go-lcpd/docs/background-ja.md`) to describe a single configuration approach via `.envrc` / direnv (and, for service managers, use `direnv exec` rather than `EnvironmentFile` / `source`).
   - Remove the legacy filename from `.gitignore` entries if present, so it is no longer part of the expected workflow.

2) Re-enable `LCPD_LND_MANIFEST_RESEND_INTERVAL`.

   - Update `go-lcpd/internal/lndpeermsg/config.go` to describe `ManifestResendInterval` as a supported feature (no longer deprecated).
   - Update `go-lcpd/internal/lndpeermsg/peermsg.go` to start a periodic loop when `ManifestResendInterval` is set and positive:
     - On each tick, send `lcp_manifest` to all currently connected peers (even if it was already sent earlier in the same connection).
     - Preserve the “reply to inbound manifest at most once per connection” behavior to avoid loops.
   - Add a unit test in `go-lcpd/internal/lndpeermsg/peermsg_test.go` that injects a deterministic tick channel and asserts multiple `SendCustomMessage(type=lcp_manifest)` calls occur for a connected peer.

3) Update specs and docs.

   - Update `docs/go-lcpd/spec.md` so that:
     - Default behavior remains “send once per connection”.
     - Optional periodic re-sends are allowed and controlled by `LCPD_LND_MANIFEST_RESEND_INTERVAL`.
     - Reply behavior remains “reply once if we have not sent ours yet”.
   - Update user docs (`docs/go-lcpd/docs/configuration*.md`, `docs/go-lcpd/docs/cli.md`, and `go-lcpd/.envrc.sample`) to describe the interval as active and document how to disable it (`0s` or unset).

## Concrete Steps

All commands below assume the repository root unless stated otherwise.

1) Find any lingering references to env-file support:

    rg -n "LCPD_GRPCD_ENV_FILE" -S .

2) Make code/doc edits (as described in “Plan of Work”).

3) Validate Go module tests:

    cd go-lcpd
    go test ./...

4) Optional lint (if the toolchain is available in the environment):

    cd go-lcpd
    make lint

5) Confirm no lingering references remain (search for the legacy env filename and ensure there are no results).

## Validation and Acceptance

This change is accepted when:

- There are no repository references to the legacy env filename (scripts, docs, samples).
- `./go-lcpd/scripts/lcpd-grpcd --help` does not mention `LCPD_GRPCD_ENV_FILE` or any env file sourcing.
- Setting `LCPD_LND_MANIFEST_RESEND_INTERVAL=10s` causes `go-lcpd` to periodically send `lcp_manifest` to connected peers.
- Unit tests in `go-lcpd/internal/lndpeermsg/peermsg_test.go` cover the resend behavior deterministically (fails before the change, passes after).

## Idempotence and Recovery

- Documentation and script edits are safe to repeat.
- The new resend loop is controlled entirely by `LCPD_LND_MANIFEST_RESEND_INTERVAL`; leaving it unset (or setting it to `0s`) disables periodic re-sends.

## Artifacts and Notes

- Expected help-string changes are localized to `go-lcpd/scripts/lcpd-grpcd`.
- Expected spec changes are localized to `docs/go-lcpd/spec.md` under `LNDPeerMessaging` and `sync lnd_peer_messaging_startup`.
