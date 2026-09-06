# Teleoperation Assurance

Teleop provides controller input and optional auditing for games, simulations, vehicles, and other controlled systems; optional strict actuation modules preserve evidence of what was observed, authorized, attempted, and acknowledged.

## Language

**Controlled System**:
The application, simulation, game, or physical machine interpreting controller-derived commands through an application-owned adapter.
_Avoid_: vessel (unless discussing a maritime deployment)

**Strict Profile**:
The domain-neutral configuration requiring all software interlocks provided by the Guard, without claiming the values are safe for a particular **Controlled System**.

**Assured Session**:
A controller session that owns input, the **Safety Authority**, authenticated evidence, external witnessing, readiness checks, and ordered shutdown.
_Avoid_: secure mode, safe wrapper

**Safety Authority**:
The sole software path that may authorize and submit a hazardous **Actuator Command**.
_Avoid_: gate, command service

**Actuator Command**:
A sequenced, expiring request for a **Controlled System** to enter a specified state.
_Avoid_: output, drive call

**Command Lease**:
The bounded interval during which an actuator may honor one **Actuator Command** before independently entering the **Engineered Safe State**.
_Avoid_: timeout, heartbeat

**Engineered Safe State**:
The system-specific fallback state selected by the consuming application, and by its safety analysis for physical hazards, when authority or certainty is lost.
_Avoid_: zero state, neutral

**Interlock Decision**:
The point-in-time authorization result linking observed operator input and safety conditions to an **Actuator Command**.
_Avoid_: permit flag, guard result

**Activity Evidence**:
An immutable record of an observed event, operator action, decision, command attempt, actuator acknowledgement, or failure.
_Avoid_: log line, telemetry

**Command Policy**:
The reviewed rules and authenticated permission required for the exact proposed
actuator, command envelope, mode and setpoint transition. It supplements the
**Interlock Decision**; it does not establish physical safety by itself.

**Operator Grant**:
An authenticated, expiring permission bound to one controller session and
specific command/actuator/mode scopes. Recorded operator labels are not grants.

**Evidence Session**:
The recorded sequence of **Activity Evidence** for one controller session, with mandatory authentication, durability, and witnessing in an **Assured Session**.
_Avoid_: audit file, event dump

**Witness Receipt**:
An acknowledgement from independently controlled storage that commits to an **Evidence Session** tree head.
_Avoid_: upload result, checkpoint status

**Transport Health**:
The backend-specific evidence that distinguishes a state change, a successful device poll, a confirmed disconnect, and unverifiable silence.
_Avoid_: liveness

## Relationships

- An **Assured Session** owns exactly one **Safety Authority** and one **Evidence Session**.
- Controller input and an **Evidence Session** can be used without a **Safety Authority** or **Assured Session**.
- An **Assured Session** requires a **Strict Profile**; the **Controlled System** supplies command meaning and receiver behavior.
- A **Safety Authority** produces one **Interlock Decision** for each **Actuator Command** attempt.
- An **Actuator Command** carries exactly one **Command Lease** and results in zero or more actuator acknowledgements.
- An **Engineered Safe State** is selected whenever an **Interlock Decision** inhibits or command outcome is uncertain.
- An **Evidence Session** contains every admitted **Activity Evidence** item and produces zero or more **Witness Receipts**; only the prefix through the latest acknowledged tree head is externally witnessed.
- **Transport Health** is one input to every **Interlock Decision**.
- Live **Actuator Commands** in an **Assured Session** require a **Command Policy** decision; **Operator Grant** expiry bounds the **Command Lease**.

## Example dialogue

> **Dev:** "Can the application send the requested thrust after the Guard permits it?"
> **Domain expert:** "Only the **Safety Authority** may send an **Actuator Command**. It first persists the **Interlock Decision**, attaches a **Command Lease**, and records the actuator acknowledgement in the **Evidence Session**."

## Flagged ambiguities

- "tamper-proof" previously mixed integrity, authenticity, survival, and completeness; the resolved target is origin-authenticated, tamper-evident, externally witnessed, durable, and omission-aware **Activity Evidence**.
- A **Witness Receipt** records successful return from the configured witness adapter; remote identity, custody, and trusted receipt time are deployment claims unless the adapter authenticates and retains its own protocol receipt.
- "neutral" previously meant both a zero controller value and a safe physical response; only a system-specific **Engineered Safe State** backed by safety analysis is a physical safety claim.
- `VesselCommand` and `MaritimeConfig` are compatibility spellings of domain-neutral `safety.Command` and `safety.StrictConfig`, not restrictions on the controlled system.
- `teleop.Command` records an application's assertion; `safety.Command` proposes intent to the **Safety Authority**. Recording alone neither authorizes nor executes it.
