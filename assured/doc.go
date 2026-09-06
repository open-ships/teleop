// Package assured composes strict controller interlocks, serialized actuator
// authority, locally durable activity evidence, origin-authenticated audit
// heads, and external witnessing into one owned session.
//
// It deliberately exposes neither the raw controller nor the actuator adapter,
// so application-requested commands cannot bypass the evidence-before-
// actuation path through this interface. Deployment still owns system-specific
// command policy, hardware interlocks, actuator-side lease enforcement,
// physical feedback, key custody, and trustworthy witness storage.
//
// This package is optional and deliberately strict, not the default way to open
// a game controller. Controller input with audit.Recorder needs no actuator,
// dead-man, signer, or witness. Simulation and game adapters can use the same
// command seams without claiming physical safety. safety.StrictConfig is
// domain-neutral; Assured still requires exact backend input, a sealed system clock, command
// policy, applied acknowledgments, and every evidence guarantee.
package assured
