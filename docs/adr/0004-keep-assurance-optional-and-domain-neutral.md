# Keep assurance optional and domain-neutral

Controller input and auditing remain independent of actuation; optional Safety
Authority and Assured Session modules support application-owned adapters for
games, simulations, vehicles, and other controlled systems. Use domain-neutral
Command and Strict Profile names while retaining maritime compatibility aliases;
Assured Session retains every existing strict guarantee, and rejects ambiguous
dual configuration instead of merging profiles or choosing precedence.

This clarifies the maritime scope of ADR-0001 without changing its serialized
actuation or evidence contracts. Physical receiver specifications, approved
limits, and emergency mechanisms are deployment evidence, not prerequisites
for developing the controller/audit library. The v1 provenance wire field
`maritime` retains its spelling and records the effective Strict Profile for
either configuration name so existing incident tools continue to work.
