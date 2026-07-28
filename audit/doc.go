// Package audit records, verifies, and replays append-only controller event
// logs.
//
// Logs use newline-delimited JSON and may provide several distinct security
// properties:
//
//   - the default SHA-256 chain detects accidental corruption;
//   - [WithHMAC] authenticates every record with a shared secret;
//   - [WithSigner] signs Merkle tree heads with Ed25519; and
//   - [WithAnchor] publishes tree heads outside the recorder's custody.
//
// These properties are deliberately separate. In particular, a public key
// declared inside a log proves only internal consistency. [ReadTrusted]
// requires a public key obtained through a separate trusted channel and is the
// preferred entry point for completed signed logs. [ReadAuthenticated] is the
// corresponding entry point for completed HMAC-authenticated logs. [ReadAll]
// verifies structural integrity and completeness but does not establish
// authorship.
//
// When [VerifyOptions.PublicKey] or [VerifyOptions.RequireSignature] is set,
// events are withheld from the streaming [Verify] callback until a verified
// signed tree head covers them. [Verification.SignedTreeSize] and
// [Verification.TrustedTreeSize] report the exact coverage boundary.
// [VerifyOptions.MaxPendingBytes] bounds this untrusted buffer.
//
// Key generation, hardware provisioning, public-key distribution, rotation,
// revocation, and destruction are deployment responsibilities. [GenerateKey]
// is intended for tests and development; production systems should generally
// supply an Ed25519-capable hardware or remote crypto.Signer to [WithSigner].
package audit
