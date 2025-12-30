# AGENTS

## Development principles

1. Always follow `spec.md`. Implementation MUST match the behavior defined there.
2. Design for robustness. Keep modules aligned with SRP.
3. Use a ubiquitous language. Keep terminology consistent across code and docs.
4. Test-first. Prefer TDD when changing behavior.
5. Run golangci-lint v2. When changing code, run `make lint` and fix findings.
6. Use `cmp.Diff` in tests. Compare expected vs actual with `github.com/google/go-cmp/cmp.Diff`.
7. Make logging diagnosable. Logging level MUST be configurable (for example via env vars).
8. Prefer lnd libraries. Use lnd-provided APIs/libraries before re-implementing.

# `spec.md` authoring guidelines (WYSIWID Architecture)

This project uses Eagon Meng's "What You See Is What It Does (WYSIWID)" pattern for design docs.
Reference: `./WhatYouSeeIsWhatItDoes.md`

Behavior is expressed as independent Concepts and the Synchronizations that connect them.

These docs are the ubiquitous language for the project.
Keep them precise and consistent.

## General principles

- Flat structure. Avoid unnecessary nesting.
- Scannable. Prefer headings, short lists, and signatures over long prose.
- Ubiquitous language. Keep doc terms aligned with identifiers in code when possible.

## Section structure

Write `spec.md` sections in this order.

### Security & Architectural Constraints

Define permanent invariants first: security, deployment constraints, prohibitions, and architectural limits.

Writing rules:
- Use normative wording (MUST, MUST NOT).
- Add a brief rationale for each invariant.

### Concepts

Concepts are independent functional units with a specific purpose.
A Concept MUST NOT depend on other Concepts.

Heading: `### [Concept Name]`

Content:
- Purpose: responsibility in 1–2 lines.
- Domain Model (State): data structures the Concept owns.
- Actions: operations as signatures.
- Operational Principle (optional): implementation notes, libraries, stack choices.

Domain Model formatting:
- Prefer object-like shapes (example: `Mention: text, user_id, ts`).

Actions formatting:
- Format: `action_name(input_args) -> output`
- Note side effects and error cases.

Important prohibition:
- A Concept MUST NOT call actions from other Concepts.
- A Concept MUST NOT reference other Concepts' types.
- All cross-Concept coordination MUST happen in Synchronizations.

### Synchronizations

Synchronizations combine Concepts into end-to-end stories.

Heading: `### sync [Sync Name]`

Content:
- Summary: one sentence.
- Flow: `when / where / then`.

Flow guidance:
- `when`: triggering event.
- `where`: preconditions and data gathering.
- `then`: action chain, including error branches.

Diagrams:
- For complex flows/state transitions, include Mermaid sequence diagrams or state charts.

-----

## Example

```markdown
### SlackMessaging
Purpose: Send and receive messages on Slack.
Domain Model:
- `Mention`: user_id, text, ts
Actions:
- `post_reply(channel, text) -> ts`: Post a message.

### sync handle_mention
When: `SlackMessaging` receives a mention.
Then:
1. `Store.check_duplicate` checks for duplicates.
2. `GooseExecution.execute_slack_thread` generates a reply.
3. `SlackMessaging.post_reply` posts the reply.
```
