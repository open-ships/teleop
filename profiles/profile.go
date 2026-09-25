// Package profiles describes controller layouts independently of device wiring.
package profiles

import (
	"slices"

	"github.com/open-ships/teleop"
)

// Profile describes the physical controls and printed labels of a layout.
// The first generic provider supports digital buttons and D-pad directions.
// Device-specific button numbers and axis assignments belong to a mapping.
type Profile struct {
	ID       string                     `json:"id"`
	Controls []teleop.ControlDescriptor `json:"controls"`
}

// Clone isolates a profile from subsequent caller changes.
func (p Profile) Clone() Profile { p.Controls = slices.Clone(p.Controls); return p }

// SNES is the standard eight-button SNES layout, plus its four D-pad directions.
var SNES = Profile{
	ID: "snes",
	Controls: []teleop.ControlDescriptor{
		{ID: teleop.ButtonFaceSouth, Kind: teleop.ControlButton, Label: "B"},
		{ID: teleop.ButtonFaceEast, Kind: teleop.ControlButton, Label: "A"},
		{ID: teleop.ButtonFaceWest, Kind: teleop.ControlButton, Label: "Y"},
		{ID: teleop.ButtonFaceNorth, Kind: teleop.ControlButton, Label: "X"},
		{ID: teleop.ButtonBumperLeft, Kind: teleop.ControlButton, Label: "L"},
		{ID: teleop.ButtonBumperRight, Kind: teleop.ControlButton, Label: "R"},
		{ID: teleop.ButtonMenuPrimary, Kind: teleop.ControlButton, Label: "Start"},
		{ID: teleop.ButtonMenuSecondary, Kind: teleop.ControlButton, Label: "Select"},
		{ID: teleop.DPadUp, Kind: teleop.ControlDPad, Label: "D-pad up"},
		{ID: teleop.DPadDown, Kind: teleop.ControlDPad, Label: "D-pad down"},
		{ID: teleop.DPadLeft, Kind: teleop.ControlDPad, Label: "D-pad left"},
		{ID: teleop.DPadRight, Kind: teleop.ControlDPad, Label: "D-pad right"},
	},
}
