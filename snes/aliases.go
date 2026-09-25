// Package snes names SNES buttons by their printed labels. Device access is
// provided by generic, with profiles.SNES and a device-specific mapping.
package snes

import "github.com/open-ships/teleop"

const (
	ButtonB       = teleop.ButtonFaceSouth
	ButtonA       = teleop.ButtonFaceEast
	ButtonY       = teleop.ButtonFaceWest
	ButtonX       = teleop.ButtonFaceNorth
	LeftShoulder  = teleop.ButtonBumperLeft
	RightShoulder = teleop.ButtonBumperRight
	Start         = teleop.ButtonMenuPrimary
	Select        = teleop.ButtonMenuSecondary
)
