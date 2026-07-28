package teleop

// ControllerType identifies a family of game controllers. It is string-backed
// so third-party providers can add controller types without changing teleop.
type ControllerType string

const (
	// ControllerXbox identifies the built-in Xbox-compatible provider family.
	ControllerXbox ControllerType = "xbox"
)

// Transport describes how a controller is connected. Detection is best effort:
// some operating-system APIs intentionally hide the physical transport.
type Transport string

const (
	// TransportUnknown means the backend cannot determine the connection.
	TransportUnknown Transport = "unknown"
	// TransportBluetooth identifies a Bluetooth connection.
	TransportBluetooth Transport = "bluetooth"
	// TransportUSB identifies a USB connection.
	TransportUSB Transport = "usb"
	// TransportXboxWireless identifies Microsoft's proprietary wireless link.
	TransportXboxWireless Transport = "xbox-wireless"
	// TransportVirtual identifies a synthetic or replay device.
	TransportVirtual Transport = "virtual"
)

// ControlID names a physical control by position rather than by the label
// printed by a particular controller manufacturer.
type ControlID string

// Standard control identifiers name physical positions independently of the
// labels printed by a controller manufacturer.
const (
	ButtonFaceSouth ControlID = "button.face.south"
	ButtonFaceEast  ControlID = "button.face.east"
	ButtonFaceWest  ControlID = "button.face.west"
	ButtonFaceNorth ControlID = "button.face.north"

	ButtonBumperLeft  ControlID = "button.bumper.left"
	ButtonBumperRight ControlID = "button.bumper.right"
	ButtonStickLeft   ControlID = "button.stick.left"
	ButtonStickRight  ControlID = "button.stick.right"

	ButtonMenuPrimary   ControlID = "button.menu.primary"
	ButtonMenuSecondary ControlID = "button.menu.secondary"
	ButtonSystem        ControlID = "button.system"
	ButtonCapture       ControlID = "button.capture"

	ButtonPaddle1 ControlID = "button.paddle.1"
	ButtonPaddle2 ControlID = "button.paddle.2"
	ButtonPaddle3 ControlID = "button.paddle.3"
	ButtonPaddle4 ControlID = "button.paddle.4"

	DPadUp    ControlID = "button.dpad.up"
	DPadDown  ControlID = "button.dpad.down"
	DPadLeft  ControlID = "button.dpad.left"
	DPadRight ControlID = "button.dpad.right"

	StickLeft    ControlID = "stick.left"
	StickRight   ControlID = "stick.right"
	TriggerLeft  ControlID = "trigger.left"
	TriggerRight ControlID = "trigger.right"
)

var standardButtons = []ControlID{
	ButtonFaceSouth,
	ButtonFaceEast,
	ButtonFaceWest,
	ButtonFaceNorth,
	ButtonBumperLeft,
	ButtonBumperRight,
	ButtonStickLeft,
	ButtonStickRight,
	ButtonMenuPrimary,
	ButtonMenuSecondary,
	ButtonSystem,
	ButtonCapture,
	ButtonPaddle1,
	ButtonPaddle2,
	ButtonPaddle3,
	ButtonPaddle4,
	DPadUp,
	DPadDown,
	DPadLeft,
	DPadRight,
}

// StandardButtonIDs returns the standard physical buttons in a stable order.
func StandardButtonIDs() []ControlID {
	return append([]ControlID(nil), standardButtons...)
}

// StickID selects one of the two standard analog sticks.
type StickID string

const (
	// LeftStick selects the left analog stick.
	LeftStick StickID = "left"
	// RightStick selects the right analog stick.
	RightStick StickID = "right"
)

// TriggerID selects one of the two standard analog triggers.
type TriggerID string

const (
	// LeftTrigger selects the left analog trigger.
	LeftTrigger TriggerID = "left"
	// RightTrigger selects the right analog trigger.
	RightTrigger TriggerID = "right"
)

// Phase describes an event edge or lifecycle transition.
type Phase string

const (
	// PhasePressed reports a digital press edge.
	PhasePressed Phase = "pressed"
	// PhaseReleased reports a digital release edge.
	PhaseReleased Phase = "released"
	// PhaseChanged reports an analog value change.
	PhaseChanged Phase = "changed"
	// PhaseStarted reports the start of a stateful derived event.
	PhaseStarted Phase = "started"
	// PhaseEnded reports the end of a stateful derived event.
	PhaseEnded Phase = "ended"
)
