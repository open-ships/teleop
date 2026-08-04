# Auditing controller input

Attach a recorder when opening a controller:

```go
file, err := os.OpenFile(
    "controller.jsonl",
    os.O_CREATE|os.O_EXCL|os.O_WRONLY,
    0o600,
)
if err != nil {
    return err
}
defer file.Close()

recorder := audit.NewRecorder(file)
defer recorder.Close()

controller, err := provider.Open(
    ctx,
    deviceID,
    teleop.WithAuditSink(recorder),
)
```

The audit log is versioned newline-delimited JSON. Each record contains its
event kind, full event payload, controller session and sequence, observation
time, causes, and a cryptographic hash linked to the preceding record.

Recorder admission is strict: the event must serialize faithfully, its JSON
header must equal `Event.Header`, each stream must begin at sequence one and be
contiguous, every cause must already exist, and the controller session cannot
change. Invalid or ambiguous events fail the recorder instead of being reduced
to a misleading header-only record. When attached to a Controller, Recorder's
`CanonicalEventSink` path receives immutable bytes captured before asynchronous
handoff.

`audit.ReadAll` rejects missing hashes, broken links, reordering, deletion, and
truncation of a completed stream. Its default unkeyed SHA-256 chain provides
integrity against corruption, not authenticity: an attacker able to rewrite
the file can recompute the entire chain. `audit.WithHMAC(key)` authenticates a
writer to another holder of the same key; verify it with
`audit.ReadAuthenticated(reader, key)`. Generate at least 32 random key bytes
with a cryptographically secure source and keep them outside the log. Where the
history may become evidence, prefer asymmetric signing plus an independent
witness so verification does not grant signing capability and destruction is
detectable.

Use `audit.ReadPartial` only when inspecting an interrupted or currently open
log; it returns the integrity-checked prefix and does not require signature
coverage. Unchained streams require the explicit unverified read option.

Audit files contain a single header/footer-delimited stream. Create a unique
file per session; `O_EXCL` prevents an accidental overwrite. Appending a new
stream to an old one is not supported.

Logs are intentionally append-only and are not auto-rotated: splitting a
stream changes its verification boundary. For long-running deployments, rotate
at a supervisory/session boundary, close and verify each file, and enforce a
filesystem quota outside the controller process.

`audit.Descriptor` and `audit.Observations` extract the controller metadata and
canonical observations. Pass those observations to `testkit.NewReplaySource`
to exercise code against canonical input without controller hardware. Use
`audit.Events` when exact forensic playback is required: it decodes the
recorded canonical, gesture, action, command, clock, and lifecycle events
directly. Re-running processors from `Observations` is a new derivation and is
not a substitute for the recorded derived-event stream.

Input alone is not a record of what the application commanded. At the actuator
decision boundary call `controller.RecordCommand` with an application-defined
name, JSON-serializable payload, authorization result, and the already
published event IDs that caused it. A command queue overflow means that command
was not admitted to the audit pipeline and must be handled as an application
safety fault.

`RecordCommand` reports bounded queue admission, not durability. The concrete
`*teleop.Controller.RecordCommandSync` waits for every sink callback and all
earlier FIFO records; Recorder's default callback synchronizes a sync-capable
writer. Cancellation after controller-queue admission returns
`teleop.ErrCommandPublicationUncertain`, because already admitted work may
still be recorded. `teleop.WithSynchronousAudit()` applies the callback barrier
to every event before Snapshot, subscriber, or processor exposure. Hazardous
applications should use `assured.Session`, which requires that mode and checks
`Recorder.EvidenceStatus` before actuation.

Finite `testkit.ReplaySource` instances should be opened with
`teleop.WithDeferredStart()`; the first subscription then attaches before the
replay source starts returning observations.

## What each mechanism actually proves

The mechanisms below are not interchangeable. Each closes a different gap, and
adding a stronger one does not make a weaker one redundant.

| Property | Question it answers | Mechanism |
| --- | --- | --- |
| Integrity | Do the record bytes still match the stored chain? | hash chain |
| Immutable admission | Can later caller mutation change accepted bytes? | `CanonicalEventSink` |
| Local durability | Did accepted bytes cross the store's sync barrier? | `EvidenceStatus.LocallyDurableEvents` |
| Authenticity | Did a holder of the key write it? | `WithHMAC` |
| Origin authentication | Did the provisioned signing key attest the tree head? | `WithSigner` |
| External existence evidence | Did an external adapter acknowledge this prefix? | `WithAnchor` |
| Completion witness | Did an external adapter acknowledge the exact footer? | `WithRequiredWitness` |
| Disclosure | Can one record be proved without the rest? | Merkle inclusion proof |
| Reconstruction | What code and configuration interpreted this input? | `WithProvenance` |

## Signing and origin authentication

An HMAC chain proves that someone holding the key wrote the log. It cannot
establish who, because the verifier holds the same key that could have produced
it. `audit.WithSigner` signs the manifest, every checkpoint, and the footer
with Ed25519, so writing and verification are separate capabilities:

```go
recorder := audit.NewRecorder(file, audit.WithSigner(signer))
```

`signer` is any `crypto.Signer` with an Ed25519 key. Prefer an
Ed25519-capable hardware or remote signer whose private key is non-exportable.
Hardware non-exportability does not by itself establish device identity: bind
the public key to the device through trusted provisioning and retain the
key-lifecycle records needed by future verifiers.

Individual events are not signed. The hash chain and Merkle tree already bind
every event to the nearest signed head, so per-event signatures would add cost
without adding evidence.

For a completed signed log, prefer the fail-closed helper:

```go
records, verification, err := audit.ReadTrusted(reader, trustedKey)
```

`trustedKey` must be obtained outside the log. The equivalent configurable
call is:

```go
records, verification, err := audit.Read(reader, audit.VerifyOptions{
    RequireFooter:    true,
    PublicKey:        trustedKey, // obtained out of band
})
```

- `verification.Signed` means every record read is covered by a tree head
  verified against the key the log declares about itself. That is internal
  consistency only: whoever can rewrite the whole log can also replace the
  declared key.
- `verification.Trusted` means every record read is covered by a tree head
  verified against `PublicKey`, supplied from outside the log. Only this claim
  carries evidentiary weight.
- `SignedTreeSize` and `TrustedTreeSize` expose the exact prefix covered by the
  latest verified head.

Setting `PublicKey` makes `RequireSignature` implicit, requires format version
3, and withholds events from a streaming verifier until a signed checkpoint or
footer covers them. A mismatch between the declared key and the trusted key
fails with `ErrUntrustedKey` rather than being ignored.

`KeyID` is only a short display/index label. Never use it alone as a trust
decision; compare the complete public key or a certificate/attestation bound to
that key.

The withheld event buffer defaults to 64 MiB so a replayed manifest followed
by an attacker-controlled unsigned tail cannot cause unbounded memory growth.
If intentionally large events can exceed that between signed heads, set
`VerifyOptions.MaxPendingBytes` to a deployment-appropriate bound.

The package uses keys but does not manage their lifecycle. Hardware
provisioning, trusted public-key distribution, access control, rotation,
revocation, archival verification, and destruction belong to the deployment's
key-management system.

If a deployment deliberately combines `WithSigner` and `WithHMAC`, verification
needs both trust inputs. Use `audit.Read` with `RequireFooter`, `PublicKey`, and
`HMACKey`; `ReadTrusted` accepts only the public-key input.

## Checkpoints and external anchoring

An unkeyed hash chain detects accidental edits but can be recomputed by an
attacker. Even an authenticated chain says nothing about a log that was
deleted, truncated before its final records, or never written at all — the
failure most likely to matter when a session ends badly, and the one an
append-only structure inherently cannot see.

The recorder periodically emits a signed checkpoint carrying the Merkle head
over every record before it. `audit.WithAnchor` publishes each checkpoint to
storage this process does not control:

```go
recorder := audit.NewRecorder(
    file,
    audit.WithSigner(signer),
    audit.WithCheckpoints(10*time.Second, 10000),
    audit.WithAnchor(audit.NewFileAnchor(remote)),
    audit.WithRequiredWitness(true),
)
```

Anchor to a different failure and custody domain than the log itself: object
storage under a WORM or legal-hold policy, a transparency log, or a host under
separate control. Anchoring beside the log proves very little.

A published checkpoint identifies its format version, record type, and
signature algorithm. Verify it against an independently trusted key:

```go
err := audit.VerifyCheckpoint(trustedKey, checkpoint)
```

Checkpoint publication is asynchronous and never blocks the input path. The
interval timer emits a head even when no later event arrives, so a steady held
control does not leave the final active suffix indefinitely uncheckpointed.
After an ordinary publish error or queue eviction, that timer permits one
bounded retry head for the same unchanged event prefix. Panics and timed-out
adapters are not retried automatically, and all failure/drop counters remain
sticky after a later success.
Because a later head supersedes an earlier one, a saturated anchor queue drops
the oldest pending checkpoint rather than the freshest. Drops and failures are
counted, never hidden:

```go
if stats := recorder.AnchorStats(); stats.Failed > 0 || stats.Dropped > 0 {
    // Recent heads were never witnessed outside this process. Those records
    // are integrity protected but not protected against destruction.
}
```

`Recorder.EvidenceStatus` reports accepted, locally durable, and witnessed
event counts separately. `WaitForWitness(ctx, count)` returns the receipt that
covers a requested event high-water mark. A successful return proves only that
the configured adapter acknowledged the head; custody, persistence, and trusted
receipt time are properties of that adapter.

`WitnessReceipt` is deliberately an in-process record of that successful
adapter return, not a witness-signed protocol object. Deployments that require
cryptographic proof of witness identity or trusted acknowledgement time must
make the Anchor verify and durably retain the remote system's authenticated
receipt before `Publish` returns success.

Until such a receipt arrives, the locally durable suffix after the witnessed
high-water mark is still vulnerable to destruction or replacement with the
producer. Checkpoint frequency limits the nominal lag only while the recorder,
network, and witness remain healthy; it is not a maximum bound during an
outage. Use a count threshold of one (`WithCheckpoints(interval, 1)` or
`assured.Config.CheckpointEvery`) where the added signing and witness load is
justified, and alarm on `EvidenceStatus` lag, failures, and drops.

`WithRequiredWitness(true)` makes `Close` fail unless every attempted
publication succeeded and the external adapter acknowledged the exact footer
(signed when a signer is configured). The locally written footer remains
structurally complete and verifiable at its configured integrity level even
when that policy fails. `audit.NewQuorumAnchor` can require a threshold across
several independent adapters; several endpoints in one account or storage
system are not independent custody domains.

An anchor failure does not stop recording. A network problem should not stop a
vessel; it should be visible. Call `recorder.Checkpoint(reason)` directly at
moments worth being able to prove later, such as arming, an emergency stop, or
an operator handover. The first recorded controller event binds the log's
session; a checkpoint attempted before that returns `audit.ErrSessionRequired`.

## Selective disclosure with Merkle proofs

Records form an RFC 6962 Merkle tree in addition to the linear chain. This
matters in discovery: proving a record authentic through a hash chain alone
requires handing over the entire log, which may contain other operators'
activity or telemetry that is not discoverable.

An inclusion proof establishes that one record belongs to a signed head in
O(log n) hashes without revealing any other record:

```go
proof, err := audit.InclusionProof(index, leaves)
err = audit.VerifyInclusion(index, size, leaves[index], root, proof)
```

A consistency proof establishes that a later head extends an earlier one, so a
log cannot be quietly rewritten between two disclosures:

```go
err := audit.VerifyConsistency(oldSize, newSize, oldRoot, newRoot, proof)
```

## Provenance

A record that an operator pressed a control is not by itself evidence: the same
input produces different actuation under a different build, dead zone, or
action binding. `audit.WithProvenance` records what interpreted the input.

```go
provenance := audit.CaptureProvenance() // build, VCS revision, host, Go version
provenance.Application = "harbor-tug"
provenance.ApplicationVersion = "2.4.0"
provenance.Operator = "operator-7"
provenance.Authorization = "work-order-8812"
provenance.Config = map[string]any{"dead_zone": 0.12, "rate_limit_hz": 50}

recorder := audit.NewRecorder(file, audit.WithProvenance(provenance))
```

`CaptureProvenance` fills what the process can determine for itself, including
`vcs.revision` and whether the build came from a dirty tree. A `Modified` build
means the revision does not fully describe the running code.

`Config` is the field most often omitted and most often needed. Without the
parameters that turn input into actuation, a replay reproduces the operator's
thumb but not the machine's behavior.

`Operator` and `Authorization` are caller-supplied labels. Recording and signing
them does not authenticate a person, validate a grant, or enforce access. Bind
them to external identity and authorization evidence. Likewise, recording a
mapping or rate limit in `Config` does not enforce that policy.

Recording `Operator` is workplace surveillance and is treated as personal data
under the GDPR and comparable regimes. Decide that deliberately, with a
retention policy, rather than enabling it by default.

## Time

Wall-clock timestamps come from a clock the recording host controls and can
step. Every header therefore also carries `Monotonic`, a session-relative
reading that no clock adjustment can move. Use it, not `ObservedAt` or
`ReceivedAt`, for any duration or ordering question, including how long a
control was held and how stale an observation was.

When the wall clock steps relative to the monotonic clock by more than
`teleop.DefaultClockStepThreshold`, the controller publishes a `ClockEvent`.
Wall-clock times recorded on either side of one are not comparable; monotonic
readings remain valid across it.

## Guarantee boundary

The recorder stores every event received from the selected backend. It cannot
store input that the operating system or transport did not expose.

The controller freezes an event and places it into the recorder's bounded queue
before publishing it to subscribers or committing a new Snapshot. Recording
and `fsync` remain asynchronous for ordinary input by default. A
`WithSynchronousAudit` controller waits for callback completion before either
form of live exposure. The recorder flushes every event by default. A
queue-admission failure suppresses live exposure and terminates the controller;
a callback failure discovered after asynchronous admission cannot retract an
event already received by a subscriber.

The synchronous command barrier waits for callback completion, but success is
meaningful only when the sink reports a real durability level. A writer without
`Sync` reaches `flush-every-record`, not `fsync-every-record`. Assured Session
forces the every-event callback barrier, rejects that weaker store, requires a
signer and external witness, and requires a witnessed footer on close.
For a custom store, `fsync-every-record` means its configured `Sync` method
returned success; verify the implementation and deployed filesystem with
power-loss testing rather than treating the interface name as hardware proof.

- `AuditExactBackendStream` means the backend consumes an OS event stream.
- `AuditSampledState` means intermediate transitions can occur between polls.
- `GapEvent` means the backend or a queue reported known input loss.

An application that requires an uninterrupted trail should treat a `GapEvent`,
subscription overflow, recorder error, or sampled backend as a safety event.
Teleop reports these conditions; the application decides the appropriate safe
state.

### What none of this provides

An audit log is evidence and post-incident learning. It is not a safety
function, and no amount of cryptography in this package makes a system safer.

Under their stated key, adapter, and custody assumptions, these mechanisms
detect **errors of transcription**: records altered, substituted, removed from
a covered prefix, or attributed to the wrong key. None detects an **error of
omission** — input the operating system, driver, radio, or polling API never
delivered. A log cannot record what was never observed, and a perfectly signed,
anchored, provable log of nothing is exactly what a silent transport failure
produces.

Omission is the failure most likely to injure someone. It is addressed by
command timeout, liveness, and a dead-man switch, not by recording. See
[the safety guide](safety.md). Backend Transport Health can narrow silent
failure at a documented OS/framework seam, but an evdev attachment or
GameController-registry check is not proof that a physical controller or radio
answered.
