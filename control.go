package teleop

// ControllerType identifies a family of game controllers. It is string-backed
// so third-party providers can add controller types without changing teleop.
type ControllerType string

const (
	ControllerXbox ControllerType = "xbox"
)

// Transport describes how a controller is connected. Detection is best effort:
// some operating-system APIs intentionally hide the physical transport.
type Transport string

const (
	TransportUnknown      Transport = "unknown"
	TransportBluetooth    Transport = "bluetooth"
	TransportUSB          Transport = "usb"
	TransportXboxWireless Transport = "xbox-wireless"
	TransportVirtual      Transport = "virtual"
)

// ControlID names a physical control by position rather than by the label
// printed by a particular controller manufacturer.
type ControlID string

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

type StickID string

const (
	LeftStick  StickID = "left"
	RightStick StickID = "right"
)

type TriggerID string

const (
	LeftTrigger  TriggerID = "left"
	RightTrigger TriggerID = "right"
)

type Phase string

const (
	PhasePressed  Phase = "pressed"
	PhaseReleased Phase = "released"
	PhaseChanged  Phase = "changed"
	PhaseStarted  Phase = "started"
	PhaseEnded    Phase = "ended"
)
