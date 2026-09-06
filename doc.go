// Package teleop provides a small, controller-neutral interface for game
// controllers used in games, simulation, teleoperation, and autonomy systems.
//
// Platform providers normalize OS input into immutable State snapshots and an
// ordered Event stream. Controllers with rumble expose normalized low- and
// high-frequency motor strengths through GameController.SetRumble. The core
// package has no third-party dependencies and does not pair Bluetooth or
// wireless devices itself.
//
// Input and audit recording do not require an actuator or a safety profile.
// The optional safety package adds interlocks and leased command authority;
// assured composes those with mandatory durable, signed, witnessed evidence.
// Applications supply system-specific command meaning, limits, and receivers.
package teleop
