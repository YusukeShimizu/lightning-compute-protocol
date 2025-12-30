# AGENTS.md: Guidelines for Designing an Application-Layer Protocol on Lightning

This doc is a checklist for designing an application-layer protocol on the Lightning Network.
It assumes you do not modify the Lightning specifications (BOLTs).

## 1. Core philosophy

* No modification to BOLTs: Do not propose or require changes to upstream BOLT behavior. Design protocols as extensions (plugins or external processes).
* Leverage, do not reinvent: Reuse Lightning primitives for authentication, encryption, and routing (for example Noise, onion routing, payment secrets).
* BOLT-quality documentation: Aim for BOLT-level rigor, formatting, and readability.

## 2. Leveraging BOLT features

New protocols should ride on top of standard BOLT mechanisms.

### A. Transport

* Onion messages: For non-payment data transport, prefer onion messages over a custom TCP transport.
* Custom messages: For direct peer-to-peer communication, use high-range custom message types (>= 32768) and follow BOLT #1 parity rules.

### B. Data structure

* TLV streams only: Define new message fields as TLVs. Avoid fixed-position fields.
* Extension areas: If attaching to existing messages (`init`, HTLC-related messages, etc.), use spec-defined extension TLVs.

### C. Privacy

* Blinded paths: If the protocol carries identifiers or route information, use route blinding / blinded paths to protect endpoint privacy.

## 3. Specification strictness

Match BOLT writing style closely so existing tooling (for example `extract-formats.py`) can parse it.

### Message definition syntax

Write message and TLV definitions in a parseable format.

```text
1. type: <TypeNumber> (`<message_name>`)
2. data:
   * [`<type>`:`<field_name>`]
   * [`<length>`*`<type>`:`<array_name>`]
```
