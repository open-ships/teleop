# Choose the modules your application needs

Teleop is a controller interface with optional auditing and optional strict
actuation. It does not require a vessel, physical receiver, or hazard analysis
to use or develop the input/audit modules.

| Use case | Modules | Application responsibility |
| --- | --- | --- |
| Game input, UI, controller recorder | `teleop`, a provider, optional `gesture` / `action` / `audit` | Interpret input and choose recording guarantees |
| Simulator or RC control with interlocks | Above plus optional `safety` | Command schema, policy, receiver, expiry and fallback behavior |
| Strict audited actuation | `assured` composing `safety` and `audit` | All required adapters and configuration; independent validation for physical hazards |

Use case alone does not select an assurance level: an RC car can be hazardous,
and a boat simulator need not use strict actuation. Controller input and `audit`
do not import `safety`, `assured`, `policy`, or `simulation`.

## Controller and auditing only

Attach `audit.NewRecorder(writer)` with `teleop.WithAuditSink(recorder)` when
opening a controller. Add gesture/action processors only when needed. No
actuator, policy, dead-man, signer, or witness is required; sampled input sources
are accepted and retain their actual audit grade. Close/drain the controller
before closing the recorder, then close the writer and check every error.

`teleop.Command` and `RecordCommand` / `RecordCommandSync` only record what the
application says happened. They never execute commands or independently verify
the caller's `Authorized` assertion. See the executable
[controller-only integration test](../audit/controller_only_test.go) for input
→ game action → causal command record → verified decoding without actuation.

A plain hash chain detects edits relative to its chain but cannot authenticate
the producer or prevent whole-log replacement. A writer without `Sync` supplies
no crash-durability guarantee. Signing, trusted-key verification, independent
witnessing, and retention are separately selectable audit capabilities; see
[the audit guide](audit.md). Opting out of Assured does not imply equivalent
evidence guarantees.

## Optional command authority

`safety.Command` contains a name and application-defined JSON payload. A
`safety.Actuator` adapter interprets the command and enforces session identity,
sequence, expiry, and matching acknowledgments at the receiver. A
`safety.CommandPolicy` supplies semantic checks and permission; `safety.Evidence`
supplies the durability barrier. Low-level Authority without a policy only
enforces input interlocks.

The command protocol fits renewable states/setpoints such as steering,
throttle, or a player's movement vector. Lease expiry cannot undo an
irreversible one-shot action such as firing or making a purchase. The frozen
fallback can be a command to enter a system-owned safe mode; the library does
not calculate a dynamic physical safe state. A game's pause is not a boat's
safe state, and an acknowledgment is a receiver claim, not physical proof.

Two contrasting adapters exercise these seams without physical hardware:

- [Signed vehicle receiver integration](../assured/policy_integration_test.go)
  uses `simulation.Receiver`, scalar units/limits, and independently signed grants.
- [In-process game integration](../safety/game_adapter_test.go) uses player
  identity, movement vectors and buttons with a pause fallback, checking exact
  evidence, matching acknowledgments, expiry, replay rejection, and shutdown.

These are test models. Their explicitly advanced clocks and in-memory state
are not production watchdogs or evidence storage.

## Generic strict profile and compatibility

```go
profile := safety.DefaultStrictConfig(teleop.ButtonBumperRight)
// Select deadlines and tolerances appropriate to your controlled system.
guard, err := safety.NewStrict(profile)
```

For `assured.OpenSource` or `assured.OpenProvider`, set `assured.Config.Safety`
to that profile. All other required Assured configuration remains required:
durable store, signer, external witness, provenance, policy, safe command,
receiver lease, applied acknowledgments, and bounded deadlines. Strict input
requires exact backend recording and verifiable transport-health evidence;
ordinary sampled game input should use the controller/audit path instead.
Assured retains its sealed system clock; deterministic simulation time belongs
to the low-level Authority/model seams.

| Preferred interface | Compatibility spelling |
| --- | --- |
| `safety.Command` | `safety.VesselCommand` (type alias) |
| `safety.StrictConfig` | `safety.MaritimeConfig` (type alias) |
| `safety.DefaultStrictConfig` | `safety.DefaultMaritimeConfig` (same preset) |
| `safety.NewStrict` | `safety.NewMaritime` (same constructor behavior) |
| `safety.DefaultStrictLoopWatchdog` | `safety.DefaultMaritimeLoopWatchdog` |
| `assured.Config.Safety` | `assured.Config.Maritime` |

Set only one nonzero Assured profile field. Both fields are rejected even when
equal; partial profiles are never merged. Existing keyed configuration literals
and type assignments continue to work. Use keyed literals when migrating:
adding `Safety` changes the shape of an unkeyed `assured.Config` literal, and
reflection reports the new canonical type names for aliases.

The `teleop.assured.v1` provenance entry still stores its effective interlocks
under the historical JSON key `maritime`, regardless of configuration spelling.
This is wire compatibility, not a domain restriction. Command/evidence wire
schemas and strict guarantees are unchanged.

Physical deployments must independently validate command limits, receiver
enforcement, fallback suitability, emergency mechanisms and feedback. Those
facts belong to the integrating system, not hard-coded controller defaults.
