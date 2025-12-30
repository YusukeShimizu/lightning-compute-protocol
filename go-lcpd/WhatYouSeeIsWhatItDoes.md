# What You See Is What It Does (WYSIWID)

This repo uses the "What You See Is What It Does (WYSIWID)" pattern for design docs.
A WYSIWID doc should map directly to code behavior.

## Core idea

- Concepts describe independent capabilities.
- Synchronizations connect Concepts into end-to-end flows.

## How to write WYSIWID docs here

- Start with invariants (Security & Architectural Constraints). Use RFC 2119 language and include short rationales.
- Keep Concepts dependency-free. Do not reference other Concepts' types or call other Concepts' actions.
- Describe cross-Concept behavior only in Synchronizations using `when / where / then`.
- Prefer signatures and data shapes over long prose. Keep terms aligned with identifiers in code.

See `go-lcpd/spec.md` for a full example.
