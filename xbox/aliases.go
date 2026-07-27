// Package xbox provides Xbox controller discovery and controller-specific
// labels on top of teleop's controller-neutral API.
package xbox

import "github.com/open-ships/teleop"

const (
	ButtonA = teleop.ButtonFaceSouth
	ButtonB = teleop.ButtonFaceEast
	ButtonX = teleop.ButtonFaceWest
	ButtonY = teleop.ButtonFaceNorth

	LeftBumper  = teleop.ButtonBumperLeft
	RightBumper = teleop.ButtonBumperRight
	LeftStick   = teleop.ButtonStickLeft
	RightStick  = teleop.ButtonStickRight

	Menu  = teleop.ButtonMenuPrimary
	View  = teleop.ButtonMenuSecondary
	Xbox  = teleop.ButtonSystem
	Share = teleop.ButtonCapture

	Paddle1 = teleop.ButtonPaddle1
	Paddle2 = teleop.ButtonPaddle2
	Paddle3 = teleop.ButtonPaddle3
	Paddle4 = teleop.ButtonPaddle4
)
