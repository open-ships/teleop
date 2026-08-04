# Teleoperation Assurance

Teleop carries a remote operator's intent to a vessel while preserving enough trustworthy evidence to reconstruct what was observed, authorized, attempted, and applied.

## Language

**Assured Session**:
A controller session that owns input, the **Safety Authority**, authenticated evidence, external witnessing, readiness checks, and ordered shutdown.
_Avoid_: secure mode, safe wrapper

**Safety Authority**:
The sole software path that may authorize and submit a hazardous **Actuator Command**.
_Avoid_: gate, command service

**Actuator Command**:
A sequenced, expiring request for a vessel actuator to enter a specified state.
_Avoid_: output, drive call

**Command Lease**:
The bounded interval during which an actuator may honor one **Actuator Command** before independently entering the **Engineered Safe State**.
_Avoid_: timeout, heartbeat

**Engineered Safe State**:
The vessel-specific actuator state selected by the safety analysis when authority or certainty is lost.
_Avoid_: zero state, neutral

**Interlock Decision**:
The point-in-time authorization result linking observed operator input and safety conditions to an **Actuator Command**.
_Avoid_: permit flag, guard result

**Activity Evidence**:
An immutable record of an observed event, operator action, decision, command attempt, actuator acknowledgement, or failure.
_Avoid_: log line, telemetry

**Evidence Session**:
The origin-authenticated, hash-linked sequence of **Activity Evidence** for one **Assured Session**.
_Avoid_: audit file, event dump

**Witness Receipt**:
An acknowledgement from independently controlled storage that commits to an **Evidence Session** tree head.
_Avoid_: upload result, checkpoint status

**Transport Health**:
The backend-specific evidence that distinguishes a state change, a successful device poll, a confirmed disconnect, and unverifiable silence.
_Avoid_: liveness

## Relationships

- An **Assured Session** owns exactly one **Safety Authority** and one **Evidence Session**.
- A **Safety Authority** produces one **Interlock Decision** for each **Actuator Command** attempt.
- An **Actuator Command** carries exactly one **Command Lease** and results in zero or more actuator acknowledgements.
- An **Engineered Safe State** is selected whenever an **Interlock Decision** inhibits or command outcome is uncertain.
- An **Evidence Session** contains every admitted **Activity Evidence** item and produces zero or more **Witness Receipts**; only the prefix through the latest acknowledged tree head is externally witnessed.
- **Transport Health** is one input to every **Interlock Decision**.

## Example dialogue

> **Dev:** "Can the application send the requested thrust after the Guard permits it?"
> **Domain expert:** "Only the **Safety Authority** may send an **Actuator Command**. It first persists the **Interlock Decision**, attaches a **Command Lease**, and records the actuator acknowledgement in the **Evidence Session**."

## Flagged ambiguities

- "tamper-proof" previously mixed integrity, authenticity, survival, and completeness; the resolved target is origin-authenticated, tamper-evident, externally witnessed, durable, and omission-aware **Activity Evidence**.
- A **Witness Receipt** records successful return from the configured witness adapter; remote identity, custody, and trusted receipt time are deployment claims unless the adapter authenticates and retains its own protocol receipt.
- "neutral" previously meant both a zero controller value and a safe vessel response; only the vessel-specific **Engineered Safe State** is a safety claim.
