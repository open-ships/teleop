# Teleoperation safety case

This document is the repository's claim-and-evidence index for safety-related
behavior. It is not a vessel certification, type approval, or substitute for a
hazard analysis performed against the actual propulsion, steering, power,
communications, and operating environment.

## Top-level claim

When an application uses an **Assured Session**, no application-requested,
non-fallback **Actuator Command** is intentionally transmitted unless the
software can establish the configured interlocks, preserve the exact input
cause, and durably admit the corresponding **Activity Evidence**. Loss of
authority or uncertainty selects the configured **Engineered Safe State** and
stops renewing the **Command Lease**.

This claim covers transaction integrity, not command semantics. Safety
Authority does not determine whether an application chose the correct actuator,
sign, units, range, rate, slew, vessel mode, or authorized operator.

That claim depends on deployment evidence outside this repository:

- an independent hardware emergency stop and safety-rated interlock;
- an actuator that rejects expired, duplicate, and out-of-order commands and
  reaches the engineered safe state without this process;
- vessel-specific analysis proving that state is safe in each operating mode;
- a vessel-command policy that validates input mappings, actuator identity and
  units, command envelopes, rate/slew limits, operating-mode constraints, and
  authenticated operator authorization before `Apply`, with independent
  receiver-side limits where required by the hazard analysis;
- authenticated actuator feedback capable of distinguishing receipt from
  physical effect;
- a non-exportable signing key and independently distributed public key;
- externally administered WORM or transparency storage with retention,
  capacity monitoring, trusted receipt time, and legal-hold procedures; and
- hardware-in-the-loop, failure-injection, and sea-trial evidence for the
  complete installed system.

## Safety requirements

| ID | Requirement | In-repository evidence | Deployment evidence |
| --- | --- | --- | --- |
| SR-01 | Invalid analog input never becomes an authorized nonzero command. | Invalid observations are atomically replaced by synthetic neutral state, emit a fault, and latch the safety lifecycle; boundary tests cover finite and non-finite values. | Backend fault injection. |
| SR-02 | A held or defeated dead-man control cannot establish authority at startup, rebind, or re-arm. | Authority requires a release, neutral controls, explicit arm, and a fresh post-arm press; session binding clears prior proof. | Physical switch anti-defeat analysis. |
| SR-03 | Known input loss, invalid state, disconnection, stale state, unsupported audit grade, or controller failure inhibits output. | The Guard maps each condition to a stable reason and latches production profiles. | Backend-specific transport-health evidence. |
| SR-04 | Every application-requested command transmitted through Authority has one point-in-time interlock decision and bounded lifetime. | The Safety Authority serializes lifecycle and apply operations and attaches a monotonically increasing sequence and Command Lease. | Actuator-side expiry and replay rejection. |
| SR-05 | Uncertainty cannot silently preserve the last live output. | Recorder, actuator, acknowledgement, application-time, or timeout failure selects a higher-sequence Engineered Safe State and stops lease renewal. | Independent watchdog and safe-state validation. |
| SR-06 | Observed input, Authority lifecycle calls, decisions, command attempts, sends, acknowledgements, refusals, failures, feedback output, and shutdown are reconstructable. | First-class immutable events share session identity, ordering, causality, and authenticated audit admission; terminal evidence has a dedicated contiguous stream after controller shutdown. | Identity provisioning and operating procedures. |
| SR-07 | A caller cannot mistake queue admission for durable evidence on the assured path. | Every admitted event waits every sink callback before state/subscriber/processor exposure and verifies local `fsync` high-water; status separately reports the externally witnessed prefix. | Storage `fsync`, power-loss, and remote-receipt tests. |
| SR-08 | A completed evidence history is resistant to undetected editing, replacement, truncation, and destruction. | Hash chaining, signed Merkle heads, trusted-key verification, timed checkpoints, and witness high-water tracking. | Independent key custody and WORM/transparency witness. |
| SR-09 | Audit degradation is visible and policy-controlled. | Sticky recorder failures, witness receipts/health, timed idle checkpoints, persisted transport liveness, gap events, and strict Assured Session startup/close validation. | Capacity alarms, redundant journal, on-call response. |
| SR-10 | Restart, reconnect, and handover never inherit previous authority. | One-shot session binding and reset-to-idle lifecycle semantics. | Operator handover procedure and integration tests. |

## Hazard log

| Hazard | Initiating condition | Software control | Residual risk |
| --- | --- | --- | --- |
| H-01 unintended full-scale motion | Corrupt backend value is clamped to a valid extreme. | Reject the complete observation, synthesize neutral, latch invalid-input reason. | Compromise after the software seam; actuator corruption. |
| H-02 unexpected motion on arming | Dead-man is taped down or controls are displaced before startup. | Release-neutral-arm-fresh-press sequence. | Mechanical sensor defeat not visible to the backend. |
| H-03 continued motion after link/process loss | Releases or neutral commands never arrive. | Short Command Lease, software watchdog, no renewal on uncertainty. | Actuator watchdog failure; unsafe safe-state selection. |
| H-04 command after emergency stop | Evaluate and actuator call race in separate goroutines. | Serialized Safety Authority lifecycle/apply interface. | Hardware path outside the adapter; process already compromised. |
| H-05 unaudited actuation | Caller actuates before or without recording. | Assured Session exposes the Safety Authority as its actuation interface and requires durable intent. | Deliberate bypass of the assured module in embedding code. |
| H-06 plausible rewritten incident history | Local unkeyed log is replaced or deleted. | Origin-authenticated signed heads and independent witness receipts. | Signer compromise, witness collusion, producer omission. |
| H-07 authority crosses sessions | Guard rebind retains dead-man timing or armed state. | One-shot identity binding and fresh engagement proof. | Incorrect external identity/handover data. |
| H-08 audit outage creates a second hazard | Disk full or witness outage blocks commands needed to remain safe. | External witnessing is not a prerequisite for attempting the Engineered Safe State; policy distinguishes local durability from external witnessing. | Local evidence, process, or actuator failure can still defeat the attempt; vessel-specific trade-off between continued control and evidence availability. |
| H-09 acknowledged command is not physically applied | Transport acknowledgement is mistaken for actuator effect. | Assured configuration rejects an accepted reply without an application time inside the lease; malformed, stale, or future claims are uncertainty and trigger fallback. | `AppliedAt` is still an adapter assertion; requires authenticated independent physical feedback and trustworthy clock bounds. |
| H-10 recent evidence is destroyed before external acknowledgement | The producer and its locally durable journal fail or are compromised after an event but before a witness receipt covers it. | Every Assured event is locally synchronized; signed checkpoints, receipt high-water tracking, health counters, configurable checkpoint frequency, exact startup witnessing, and an exact witnessed footer expose and reduce the window. | The suffix after the last receipt is not externally protected. Use independent low-latency witnesses, `CheckpointEvery: 1` where justified, redundant/WORM storage, and active lag alarms. |
| H-11 unsafe command passes every teleop interlock | Application mapping selects the wrong actuator, sign, units, range, rate/slew, or vessel mode, or relies on an unauthenticated operator/grant label. | No semantic control is claimed. Authority binds the exact application-supplied command bytes and current input cause to its decision, evidence, lease, and acknowledgement; it does not interpret the command or authenticate provenance fields. | Independently review and test the vessel-command policy; authenticate and enforce operator roles; enforce hard envelopes and mode constraints at the receiver; validate with hardware-in-the-loop and sea trials. |

## Evidence semantics

“Tamper-proof” is not an achievable property for a general-purpose producer. A
compromised producer can omit an event before any logging module sees it. The
assured target is therefore explicit:

1. **Immutable admission** — mutable caller objects are serialized once before
   sink handoff.
2. **Local durability barrier** — in an Assured Session, the recorder invokes
   the configured store's flush and `Sync` barrier for every admitted event
   before corresponding state, subscriber, processor, or actuator exposure.
   Crash durability is only as truthful as that store implementation and must
   be validated with power-loss tests on the deployed storage stack.
3. **Origin authentication** — signed tree heads verify against a public key
   obtained outside the log.
4. **External witnessing** — an independently controlled adapter acknowledges
   a tree head and advances a witnessed high-water mark; readiness requires an
   acknowledgement and orderly close requires one for the exact footer. The
   in-process `WitnessReceipt` records adapter success; authenticated witness
   identity, durable remote custody, and trusted receipt time must be supplied
   and retained by the adapter/deployment.
5. **Completeness signals** — sequence validation, causes, gap events, lifecycle
   attempts, explicit outcomes, and a signed footer expose known omissions and
   interrupted sessions.

An evidence receipt must state the achieved level. “Queued,” “locally durable,”
and “externally witnessed” are not interchangeable claims.

External acknowledgement is intentionally not on the non-safe actuation path:
a witness outage must not prevent the Engineered Safe State. Consequently, the
locally durable suffix after the witnessed high-water mark remains a declared
residual risk until a later receipt covers it. Operators must alarm on that lag;
checkpoint frequency reduces the nominal window but cannot guarantee a bound
when the witness, network, or producer is unavailable.

## Operating constraints

- Treat sampled input backends and unverifiable silence according to an
  explicit policy; a periodic application ticker is not connection evidence.
  Even a fresh OS/framework attachment check establishes only that documented
  seam, not a physical controller or radio response.
- Do not use a zero controller state as a synonym for vessel safety. Configure
  an Engineered Safe State for each actuator/mode and test transitions into it.
- Do not treat an Authority permit as approval of vessel-command semantics.
  Before `Apply`, validate the input-to-command mapping, actuator/channel and
  units, permitted envelope, rate/slew, current vessel mode/state, and operator
  grant. Enforce safety-critical limits again outside this process where a
  single application fault cannot be accepted. Retain the effective policy or
  a content-addressed copy in `Provenance.Config`.
- Treat `Provenance.Operator` and `Provenance.Authorization` as caller-supplied
  labels, not authentication or an access-control decision. Bind them to an
  authenticated identity/session and retained authorization evidence, and fail
  closed when that external decision is absent, expired, or revoked.
- Never make an external network witness the only way to issue the Engineered
  Safe State. Loss of the witness must not trap the vessel in live output.
- Preserve at least the log-retention period required by the applicable flag,
  class, coastal state, insurer, and operating authorization. Capacity
  exhaustion must alarm before it inhibits ordinary operation.
- Verify completed logs continuously and after key rotation; retain public-key
  identity, revocation, witness receipts, software provenance, and authenticated
  operator/session and authorization evidence with the log.
- Require signer, witness, actuator, and evidence-store adapters to honor their
  cancellation/durability contracts. A blocked kernel write, `Sync`, or signer
  call cannot be forcibly interrupted in-process; supervise the control and
  evidence services separately where a hard shutdown bound is required.

## Standards context

The architecture is informed by the non-mandatory IMO MASS Code, resolution
MSC.595(111), effective 1 July 2026. Its goal-based provisions address data
logging for performance/failure/incident analysis, reconstructing remote and
automated decisions, monitoring connectivity degradation, and predefined,
testable fallback states. NIST SP 800-53 Rev. 5 audit controls inform capacity,
failure alerts, protection, retention, and defined degraded behavior. Neither
reference makes this library compliant or certified; that depends on the
installed system and governing regime.

- [IMO International Code of Safety for Maritime Autonomous Surface Ships, resolution MSC.595(111)](https://wwwcdn.imo.org/localresources/en/MediaCentre/Documents/MSC%20111-22-Annex%2016%20%28Secretariat%29.pdf)
- [IMO autonomous-shipping FAQ and status](https://www.imo.org/en/mediacentre/hottopics/pages/autonomous-shipping.aspx)
- [NIST SP 800-53 Rev. 5 and current release material](https://csrc.nist.gov/pubs/sp/800/53/r5/upd1/final)
