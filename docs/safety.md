# Safety interlocks and assured actuation

`teleop` transports a remote operator's intent. It cannot determine whether a
particular physical action is safe. For hazardous actuation, the recommended
interface is an `assured.Session`: it owns one **Safety Authority**, its strict
strict interlocks, the actuator command path, and the **Evidence Session**.

These modules are optional. Controller input and auditing work independently
for games, simulations, and other applications without specifying an actuator
or safe state. The strict command seams are domain-neutral; the consuming
system supplies meaning and validation. See [integration choices](integration.md).

`safety.Guard` remains the low-level interlock primitive. Calling
`Guard.Evaluate` and then calling an actuator separately leaves a race and an
evidence gap between those operations. Do not use that split path for hazardous
actuation. `safety.Authority.Apply`, exposed as `assured.Session.Apply`, is the
serialized boundary that evaluates, records, sends, acknowledges, and falls
back.

> [!IMPORTANT]
> These packages are not a certified safety controller. They do not replace an
> independent hardware emergency stop, safety-rated interlocks, actuator-side
> expiry, or a system-specific hazard analysis. A consumer game controller,
> general-purpose operating system, and this process remain common-cause
> failure domains.

> [!NOTE]
> No producer-controlled file is literally tamper-proof. An Assured Session's
> evidence is locally durable, origin-authenticated, hash-linked,
> tamper-evident, externally witnessed, and explicit about known gaps and
> witness coverage. It cannot detect an event a compromised producer omitted
> before admission. Retention, trusted key custody, WORM policy, and witness
> independence remain deployment duties.

## Why silence needs its own interlock

The dangerous failure is often an absent update: the operator releases a
control, but the release never reaches the application, or the last actuator
command remains in effect after the control process stops.

Three measurements answer different questions and must not be conflated:

| Measurement | Question answered | Relevant fields |
| --- | --- | --- |
| State change | When did the canonical controller value last change? | `StateMeta.LastStateChangeMonotonic` |
| Observation age | How old is the most recently published physical observation? | `StateMeta.ReceivedMonotonic`, `Decision.InputAge` |
| Transport Health | When did the backend last independently confirm its documented OS/framework connection condition? | `TransportCheckSequence`, `LastTransportCheckMonotonic`, `TransportSilenceVerifiable` |

A change-driven backend may emit nothing while an operator holds a control
steady. Repeating the old observation or publishing an application heartbeat
would create activity, but it would not prove even an OS-level connection
condition. A source implementing `teleop.TransportHealthSource` instead
advances a sequence for each fresh check at a documented backend seam and
reports disconnects there separately. For the Xbox provider, Linux checks
current kernel recognition of the retained evdev attachment with `EVIOCGID`;
macOS checks exact retained-controller membership in
`GCController.controllers`. Neither is a physical-controller or radio
challenge, and both inherit OS disconnect-detection latency.

The strict profile uses `TransportTimeout` as the silence deadline.
It does not trip solely because an unchanged state has an old observation age,
or because observation-age metadata is marked stale, while independent
Transport Health remains fresh. If stale handling synthesizes a neutral
controller state, that state is still rejected as synthetic. If Transport
Health is missing, unverifiable, stopped, malformed, or disconnected, the
strict profile fails closed.

`teleop.WithLiveness` publishes observation and transport ages for evidence and
can mark observation metadata stale. It does not generate observations and
does not manufacture Transport Health. The legacy `safety.New` profile still
uses `WithCommandTimeout` against observation age; a steady change-driven
source can therefore time out under that profile.

## Domain-neutral strict profile

Start from `safety.DefaultStrictConfig`, then select values appropriate to the
controlled system (from its hazard analysis for physical actuation):

```go
profile := safety.DefaultStrictConfig(xbox.RightBumper)
profile.CommandTimeout = 250 * time.Millisecond
profile.TransportTimeout = 150 * time.Millisecond
profile.LoopWatchdog = 100 * time.Millisecond
profile.DeadManReactuation = 30 * time.Second
profile.ArmStickTolerance = 0.05
profile.ArmTriggerTolerance = 0.02

guard, err := safety.NewStrict(profile)
if err != nil {
    return err
}
```

`NewStrict` rejects incomplete or weakening configuration rather than
silently substituting permissive behavior. Configuration validation requires:

- positive command, transport, loop-watchdog, and dead-man re-actuation
  deadlines;
- a non-empty dead-man control; and
- finite arming tolerances in `[0, 1)`.

At runtime, binding also verifies that the controller advertises the dead-man
as a digital button or D-pad control. Authorization requires independently
verifiable Transport Health at the source's documented seam and a timely
control-loop heartbeat. Faults always latch; the strict profile cannot disable
latching.

`NewStrict` requires a positive `CommandTimeout`, but strict authorization
keeps `Decision.InputAge` diagnostic and uses independent Transport Health,
rather than observation churn, to decide whether silence is safe. The legacy
profile enforces `CommandTimeout` directly. Library defaults are starting
points, not certified limits.

`MaritimeConfig`, `DefaultMaritimeConfig`, and `NewMaritime` retain the original
preset as compatibility spellings with identical strict behavior. In
`assured.Config`, prefer `Safety: profile`; legacy `Maritime: profile` remains
accepted, but setting both nonzero fields is an error.

A Guard binds to one controller session only. It cannot carry armed state,
dead-man history, or timing proof across a reconnect or operator handover.
`Reset` always returns the lifecycle to inhibited/idle; it never preserves
authority from an armed state.

## Arming and re-arming sequence

The order is a safety property:

1. **Release** — observe the dead-man control released on the current physical
   input session. A dead-man already held at startup or after a fault is not
   engagement proof.
2. **Neutral** — release every digital control. Keep each stick's radial
   magnitude and each trigger within the configured arming tolerances. These
   tolerances affect arming only; they do not alter live input or create a
   control dead zone.
3. **Arm** — call `Session.Arm` or `Authority.Arm`. Arming records both the
   attempt and outcome. It does not itself authorize a requested actuator
   command while the dead-man remains released.
4. **Fresh press** — press the dead-man after the successful Arm. Its controller
   receipt time must be strictly later than the Arm epoch; an edge received
   before Arm but published afterward does not qualify. A held state, repeated
   report, synthetic edge, or press from another stream cannot satisfy this
   step.
5. **Apply** — submit intent through `Session.Apply` or `Authority.Apply`. The
   Safety Authority makes a new point-in-time Interlock Decision for that
   command.

In shorthand: **release → neutral → arm → fresh dead-man press → apply**.

Releasing the dead-man during ordinary live operation inhibits immediately but
does not latch; a fresh press can resume while the lifecycle remains armed. A
fault, explicit Disarm, Emergency Stop, Reset, or session change clears the
engagement proof and requires the safe arming sequence again. A dead-man held
past `DeadManReactuation` is treated as defeated and trips the Guard.

An Emergency Stop cannot be cleared by Arm. Call `Reset`, establish the safe
baseline again, Arm, and then provide a fresh dead-man press. A software
Emergency Stop complements the independent physical stop; it is not a
substitute for it.

## Recommended hazardous-actuation path

An Assured Session composes the strict Guard, Safety Authority, synchronous
local evidence barrier, signed checkpoints, external witness, readiness checks,
and ordered shutdown. It intentionally does not expose the raw controller or a
raw actuator path.

The following fragment assumes:

- `provider` is a `teleop.Provider` whose selected backend reports
  `teleop.AuditExactBackendStream` and independently verifiable Transport
  Health at a documented OS/framework seam;
- `signer` is a protected Ed25519 `crypto.Signer`;
- `anchor` is an independently administered `audit.Anchor`;
- `actuator` is a `safety.Actuator` whose receiver enforces session identity,
  increasing sequence numbers, Command Lease expiry, and vessel-specific hard
  command limits;
- `operatorID` and `voyageAuthorization` have been authenticated and enforced
  outside teleop; and
- `vesselPolicy` implements `safety.CommandPolicy` for the vessel's mapping,
  units, limits, transitions, setpoint-rate limits and operator grants;
- `policyConfiguration` retains its actual effective configuration and `ctx`
  carries the current authenticated grant whenever live intent is submitted.

```go
store, err := assured.CreateFileStore(evidencePath)
if err != nil {
    return err
}

profile := safety.DefaultStrictConfig(xbox.RightBumper)
profile.CommandTimeout = 250 * time.Millisecond
profile.TransportTimeout = 150 * time.Millisecond
profile.LoopWatchdog = 100 * time.Millisecond
profile.DeadManReactuation = 30 * time.Second

safeState := safety.Command{
    Name: "vessel.safe",
    Payload: map[string]any{
        "propulsion": 0,
        "steering":   0,
    },
}
commandTTL := 50 * time.Millisecond

session, err := assured.OpenProvider(ctx, provider, deviceID, assured.Config{
    EvidenceStore: store,
    Signer:        signer,
    Anchor:        anchor,
    Provenance: audit.Provenance{
        Application:        "bridge-teleop",
        ApplicationVersion: buildVersion,
        Operator:           operatorID,
        Authorization:      voyageAuthorization,
        Config: map[string]any{
            "safety":                profile,
            "command_ttl":           commandTTL.String(),
            "engineered_safe_state": safeState,
            "command_policy":        policyConfiguration,
        },
    },
    Safety: profile,
    Authority: safety.AuthorityConfig{
        EngineeredSafeState:          safeState,
        CommandTTL:                   commandTTL,
        RequireAppliedAcknowledgment: true,
        Policy:                       vesselPolicy,
    },
    Actuator:           actuator,
    CheckpointInterval: 2 * time.Second,
    CheckpointEvery:    512,
    WitnessTimeout:     time.Second,
    ShutdownTimeout:    2 * time.Second,
})
if err != nil {
    _ = store.Close() // safe even if startup rollback already closed it
    return err
}
defer session.Close()

// Start the one authoritative Apply loop before Arm and keep it running through
// every lifecycle transition. While unarmed, Apply can select only safeState;
// those transactions exercise the software/adapter path and obtain its
// acknowledgement, but do not prove physical actuator state.
ticker := time.NewTicker(10 * time.Millisecond) // derive from the hazard analysis
defer ticker.Stop()
armed := false
for {
    select {
    case <-ctx.Done():
        return ctx.Err()
    case <-ticker.C:
    }

    if !armed {
        result, err := session.Apply(ctx, safety.ApplyRequest{
            Intent: safeState,
            Detail: "pre-arm safe renewal",
        })
        if err != nil || !result.Fallback {
            return errors.Join(err, errors.New("unarmed loop did not select safe state"))
        }
        // Operator procedure: release the dead-man, center every control, wait
        // for that observation, and request Arm without stopping this loop.
        if !operatorRequestedArm() {
            continue
        }
        if err := session.Arm(ctx, "captain armed station"); err != nil {
            return err
        }
        armed = true
        continue // the operator must make a fresh post-Arm dead-man press
    }

    result, err := session.Apply(ctx, safety.ApplyRequest{
        Intent: safety.Command{
            Name:    "propulsion.set",
            Payload: map[string]any{"throttle": requestedThrottle},
        },
        Causes: []teleop.EventID{mappedActionEventID},
        Detail: "captain propulsion request",
    })
    if err != nil {
        return err
    }
    if result.Fallback {
        reportInhibit(result.Decision.Reasons)
    }
}
```

The Assured Session owns `EvidenceStore` once controller opening begins and
closes it during ordered finalization. Handle the returned `Close` error in
production rather than discarding it as the compact example does.
`ShutdownTimeout` bounds authority and controller work, but an in-process
writer, `Sync`, or signer call has no cancellable Go interface and may still
block recorder finalization. Use contract-compliant adapters and separate
process supervision when a hard evidence-service shutdown bound is required;
actuator-side lease expiry remains the physical fail-safe.

The continuous `Apply` loop must already be running before `Arm`; do not place a
human-paced pause between arming and the next iteration. Strict `Arm` performs
its own evidenced and acknowledged safe-state preflight and refreshes the loop
watchdog only after that transaction and the Arm outcome succeed, but this is a
handoff to the loop, not a replacement for it. Call `Apply` often enough to
renew both the software watchdog and Command Lease with margin for scheduler,
storage, network, and actuator acknowledgement latency. If the loop stops, the
receiver must expire the lease and enter the Engineered Safe State without help
from this process. `CommandTTL` must not exceed any configured safety deadline;
normally it should be shorter.

A nil `Apply` error means the selected command was durably evidenced, sent, and
positively acknowledged. Assured configuration requires the actuator adapter
to assert a nonzero `AppliedAt` inside the Command Lease; a missing, stale, or
future application time is uncertainty and triggers fallback. That assertion
is still only as trustworthy as the authenticated actuator adapter and its
clock. Independent physical feedback is required to prove the vessel actually
reached the requested state.

Success does **not** necessarily mean the requested command was selected: when
an Interlock Decision inhibits, Authority sends the Engineered Safe State and
returns `ApplyResult.Fallback == true`. Always inspect that field.

For every permitted decision, Authority automatically appends the exact
canonical observation EventID identified by `Decision.InputSequence` and the
bound controller session. It preserves and deduplicates caller-provided
`ApplyRequest.Causes`; use those for additional action, mapping, or supervisor
events from which the intent was derived, as `mappedActionEventID` illustrates
above. Assured recording rejects unpublished, future, or cross-session causes
rather than recording a plausible but false causal chain.

Use `Session.Disarm`, `Session.EmergencyStop`, and `Session.Reset` for lifecycle
changes. Disarm, Emergency Stop, and Close revoke authority before waiting for
evidence or serialization, cancel an in-flight requested command, and attempt a
newer Engineered Safe State command. A safety-generation check also prevents an
earlier blocked Arm or Reset from overwriting a later safety request.

### Using Safety Authority without an Assured Session

`safety.Authority` is the minimum hazardous-output boundary, but assembling it
correctly is a deployment responsibility. Production code must create a strict
Guard with `NewStrict`, pass it to `NewAuthorityWithGuard`, attach
`Authority.Processor()` before controller ingest starts, call `Authority.Bind`,
and route every lifecycle operation and actuator request through Authority.
Its `Evidence` adapter must not return success before the record crosses the
required local durability barrier.

`safety.NewAuthority` constructs the configurable legacy Guard and is not a
replacement for `NewStrict` in the strict path. Prefer `assured.OpenProvider`
or `assured.OpenSource`; they validate and own the composition. If Assured
Session readiness rejects a sampled audit backend, unverifiable transport,
missing witness, or non-durable evidence store, do not silently downgrade a
hazardous operation to the split Guard/actuator path.

## Fail-closed reasons

An Interlock Decision may contain more than one reason. Strict faults latch the
lifecycle until the operator establishes the safe baseline and arms again.
`Arm` returns an `*safety.ArmError` containing all failed arming conditions.

| Reason | Meaning |
| --- | --- |
| `ReasonUnbound` | No controller session is bound. |
| `ReasonNotArmed` | The lifecycle was never armed, was explicitly disarmed/reset, or a fault tripped it. |
| `ReasonEmergencyStop` | A software Emergency Stop is latched. |
| `ReasonNoInput` | No physical observation has established that the input path works. |
| `ReasonInvalidInput` | An analog value was non-finite or outside its canonical range; the entire observation was rejected and neutralized. |
| `ReasonInputGap` | The backend reported known or suspected loss in the physical input stream. |
| `ReasonSourceError` | The source or controller pipeline reported an error. |
| `ReasonDisconnected` | The controller or Transport Health confirmed disconnection. |
| `ReasonSynthetic` | The current controller state was synthesized rather than physically observed. |
| `ReasonControllerFault` | The controller pipeline terminated. |
| `ReasonCommandTimeout` | Under the legacy profile, the newest physical observation exceeded the command timeout. |
| `ReasonInputStale` | Under the legacy profile, the controller marked observation-age metadata stale. Strict mode instead relies on Transport Health unless the stale state was synthesized. |
| `ReasonTransportUnverifiable` | Strict mode has no independent proof that silence is safe. |
| `ReasonTransportTimeout` | The latest independent transport check exceeded `TransportTimeout`. |
| `ReasonLoopStalled` | The Safety Authority/control loop has not produced a timely successful heartbeat. |
| `ReasonControlsNotNeutral` | Arm was refused because an analog control exceeded its arming tolerance or any digital control was active. |
| `ReasonDeadManReleaseRequired` | No qualifying released dead-man state has been observed for this session/recovery sequence, or Arm was attempted while it was held. |
| `ReasonDeadManReleased` | The armed operator is not currently holding the dead-man; this normally inhibits without latching. |
| `ReasonDeadManUnconfirmed` | The snapshot is held, but the Guard has not observed the qualifying post-arm press edge. |
| `ReasonDeadManStale` | The dead-man was held past its required re-actuation deadline. |
| `ReasonOperator` | An operator- or Safety Authority-originated inhibit was selected, including fallback evidence for invalid intent. |

`ReasonDeadManReleased` and an in-flight `ReasonDeadManUnconfirmed` are normal
engagement states and do not themselves latch. They still inhibit the requested
command. Input-integrity faults, Transport Health faults, and strict-profile
faults do latch. A later good observation can clear a transient condition flag,
but it cannot silently restore an already tripped lifecycle.

## Evidence and command ordering

For each `Apply`, Safety Authority owns this order:

1. evaluate the Guard and durably record the Interlock Decision;
2. durably record the requested intent and the command selected for application;
3. send a session-bound, sequenced Actuator Command carrying a Command Lease;
4. record transport acceptance and wait for the exact actuator
   acknowledgement; and
5. record acknowledgement, rejection, timeout, or uncertainty.

Before a requested command crosses `Send`, Authority re-evaluates both the
interlocks and the exact controller observation token (`InputSequence` plus the
canonical state). A newer observation revokes the old intent even when both
snapshots would independently permit output. The same change cancels a live
operation already waiting on its actuator receipt, allowing a higher-sequence
safe fallback promptly.

If the decision inhibits, the selected command in step 2 is the Engineered Safe
State. If evidence fails before requested actuation, an actuator outcome is
uncertain, or authority changes around acknowledgement, Authority inhibits and
attempts a higher-sequence Engineered Safe State command. Lifecycle attempts
and outcomes are also evidence records, including refused Arm requests.
Safe fallback decision, intent, and clock callbacks have budgets independent of
the physical `Send`/`Await` lease budget. A failed or context-ignoring evidence
adapter is reported, but cannot consume the only time reserved to attempt the
safe command. After a fallback crosses `Send`, its receipt is collected before
a later evidence callback can spend the remainder of that receiver lease.

The Assured Session's local evidence barrier synchronizes each Activity
Evidence record before its admission returns. The Interlock Decision and intent
cross that barrier before the requested command is sent. Signed checkpoints
and Witness Receipts extend the evidence to an independently controlled system.
External publication is not on every command's latency path: startup requires a
witnessed checkpoint, periodic/count checkpoints reduce the nominal unwitnessed
interval while the witness is healthy, and orderly close requires a witnessed
signed footer. They do not impose a maximum during an outage. Monitor
`Session.EvidenceStatus` for durability, witness progress, failures, drops, and
the remaining unwitnessed suffix; use a per-event checkpoint policy where the
cost and risk analysis justify it.

## Physical and deployment limits

Software interlocks reduce risk only when the installed system honors their
assumptions:

- Define the Engineered Safe State per vessel mode and hazard. Zero controller
  input is not automatically safe: stopping thrust may remove steerage, and
  centering steering may be wrong during docking or dynamic positioning.
- Treat every `ApplyRequest.Intent` as application-defined output. Authority
  preserves its exact bytes and input cause and, in Assured Sessions, requires
  a live `Authority.Policy` decision before intent. Configure that reviewed
  policy to validate mapping, units, limits, mode and authenticated grants, and
  enforce safety-critical hard limits independently at the receiver. Retain the
  effective policy or a content-addressed copy in `Provenance.Config`.
- `Provenance.Operator` and `Provenance.Authorization` are required recorded
  labels in an Assured Session, not identity authentication or permission
  enforcement. Bind them to an authenticated operator session and a current,
  externally enforced grant; retain that evidence for audit.
- Enforce Command Lease expiry, session identity, and strictly increasing
  sequence numbers at the actuator receiver using its local monotonic clock.
  Loss of this process, host, or network must stop lease renewal.
- Authenticate actuator acknowledgements and derive `AppliedAt` from the
  receiving system. Bound clock error tightly enough that the adapter can
  truthfully assert the application occurred within the transmitted lease.
- Put the hardware emergency stop and safety-rated interlock on an independent
  power and control path capable of removing or constraining hazardous energy.
- Treat consumer controls, USB/Bluetooth radios, drivers, the host OS, and this
  process as non-safety-rated components unless the installed system has
  separate evidence establishing otherwise.
- Provision the signing key as a non-exportable device identity where
  practical. Distribute its public key out of band. Place the witness and WORM
  retention in different administrative and failure domains from the vessel
  host and local evidence store.
- An exact backend stream proves what crossed that backend seam. It cannot
  record an input the device, radio, driver, kernel, or compromised producer
  omitted before the seam. Sampled-state backends are rejected by Assured
  Session readiness rather than represented as complete histories.
- Size deadlines from measured worst-case latency plus margin, then validate
  them with hardware-in-the-loop tests, power-loss tests, transport faults,
  stuck controls, full disks, witness outages, delayed acknowledgements,
  process termination, reconnects, and operator handovers.
- Provide independent authenticated physical feedback when incident analysis
  or control policy must distinguish command acceptance from actual rudder,
  throttle, contactor, or propulsion state.

See the [teleoperation safety case](safety-case.md) for the claim/evidence
matrix and residual hazards, and the [audit guide](audit.md) for verification,
key management, checkpoints, and Witness Receipt semantics.
