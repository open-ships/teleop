// Package assured composes strict controller interlocks, serialized actuator
// authority, locally durable activity evidence, origin-authenticated audit
// heads, and external witnessing into one owned session.
//
// It deliberately exposes neither the raw controller nor the actuator adapter,
// so application-requested vessel commands cannot bypass the evidence-before-
// actuation path through this API. Deployment still owns vessel-specific
// command policy, hardware interlocks, actuator-side lease enforcement,
// physical feedback, key custody, and trustworthy witness storage.
package assured
