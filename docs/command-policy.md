# Command policy and receiver simulation

Assured Sessions require `Authority.Policy`. A policy denial, callback panic,
timeout, missing identity or expired permission inhibits live intent and
independently attempts the Engineered Safe State. The exact requested bytes,
policy decision, grant proof and input decision share one durable decision
record, which parents intent and the actuator outcome. Grant expiry limits the
transmitted lease; evidence latency cannot renew it. Ordinary newer input
before any live send returns `ErrIntentSuperseded`, substitutes a higher-sequence
safe command, and preserves standing arm only after successful fallback.
Uncertainty after a send, and real interlock faults, still inhibit.

## Strict reference policy

`policy.New` snapshots an explicit configuration. There are no vessel defaults.
Each command name has one actuator, mode, exact set of scalar fields, units,
bounds, explicitly permitted predecessor commands, and optional setpoint-rate
limits. Unknown, duplicate, case-mismatched, missing or null fields are refused.
Scalar values use canonical finite float64 JSON (`encoding/json` applied to
`policy.Scalar`), rejecting lossy integer rounding, underflow and noncanonical
spellings such as `1.0` instead of `1`. Other-system encoders must preserve this
representation rather than relying on a different numeric interpretation.
Every live command needs a known acknowledged baseline. Expired or uncertain
live output is not assumed to remain the baseline; an acknowledged safe
transaction must re-establish it.

Reference payload shape (illustrative simulation values only):

```json
{"actuator":"sim","mode":"manual","values":{"power":{"value":0.25,"unit":"fraction"}}}
```

`policy.SignGrant` belongs in a separate authorization service. It issues
Ed25519 credentials for an exact controller session, operator, time interval
and list of `(command, actuator, mode)` scopes. Use `Session.ID()` when requesting
a grant and `policy.WithGrant(ctx, signedGrant)` on live `Apply` calls. The
controller must not hold the grant-issuer private key. Policy configuration
pins its independently provisioned public key and maximum grant lifetime.
Offline grants do not provide immediate revocation: use short lifetimes or a
reviewed online policy with fail-closed revocation. The receiver must check
authorization again after transport delay. Recorded provenance labels alone
are still not credentials.

Retain `policy.Configuration()` in `Provenance.Config`; decisions include a
digest-derived policy identity and the signed grant. Protect this evidence as
operator-sensitive data. Custom policies must retain equivalently reconstructable
configuration/proof, fail closed, avoid side effects, and return bounded results.
The built-in policy measures setpoint delta against elapsed time, **not physical
actuator slew or vessel dynamics**. Mode names and acknowledged baselines are
software/adapter assertions. Independently verify actual mode, position, speed,
mapping sign and physical envelopes where the hazard analysis requires them.

Safe commands intentionally bypass live grants. Their exact bytes and effects
must be reviewed before startup and accepted only as the approved safe state
by the receiver. An authorization outage must never block emergency stopping.

## Explicit simulation boundary

`simulation.NewReceiver` creates an in-memory receiver with a fresh boot epoch,
pinned sender key, independent policy and approved safe envelope. `Advance`
is its monotonic clock driver. Advancing across expiry applies simulated safe
output at the original deadline even when no new packet arrives. This is **not
a real-time timer, physical feedback or hardware watchdog**.

Its in-process `Actuator` adapter fits the existing `safety.Actuator` seam. The
sender signs the exact payload bytes, session, sequence, lease, evidence IDs,
boot epoch and independent operator grant. Startup requires a sequence-one
safe bootstrap. Receiver restart needs a fresh epoch obtained over an
authenticated channel; a new session cannot steal an existing boot-session.
Receipt never starts a fresh TTL: expiry is reduced by the configured worst
clock skew. Invalid authenticated newer commands consume their sequence and
force safe output, so an older queued intent cannot resurrect afterward.

Combine this model with `testkit` input sources and low-level Authority clock
seams for deterministic scenarios. Do not advertise a test source as physical
transport evidence. Production `assured.OpenSource`/`OpenProvider` continues to
require its sealed system-clock pipeline. Gestures/actions can be installed
through `Config.Processors`; replay and simulation have no automatic path into
a physical output adapter.

## Receiver/adapter acceptance scenarios

Run these against every future simulator, vessel driver and transport adapter:

- Correct safe bootstrap; wrong signer, session, boot epoch and false-safe bytes.
- Duplicate/reordered packets; delayed delivery; short grants and expired leases.
- No sender packets at expiry; controller crash; receiver restart; clock error.
- Missing/stale/tampered grants; wrong units, fields, ranges, modes and setpoint deltas.
- Higher-sequence fallback after Send/Await loss; late old callbacks/packets.
- Physical feedback lost, stuck or contradictory; distinguish receipt from effect.
- Disk full, failed sync/witness, corrupt evidence, and shutdown during each phase.

The automated model scenarios are in `simulation/receiver_test.go`; policy and
Authority fault suites are separate so neither silently substitutes for the
other. A real transport must preserve signed payload bytes (ordinary JSON
reformatting changes them) and authenticate acknowledgements. Hardware-in-the-loop,
power-loss and sea-trial evidence remain installation requirements.
