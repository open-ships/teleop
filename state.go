package teleop

import "maps"

// Stick is a normalized two-dimensional stick position. Both axes are in
// [-1,+1]. Positive X is right and positive Y is up.
type Stick struct {
	X float32 `json:"x"`
	Y float32 `json:"y"`
}

// DPad permits diagonals by representing each direction independently.
type DPad struct {
	Up    bool `json:"up"`
	Down  bool `json:"down"`
	Left  bool `json:"left"`
	Right bool `json:"right"`
}

// Buttons contains the standard physical game-controller buttons. Extensions
// holds controls not represented by the standard fields.
type Buttons struct {
	FaceSouth bool `json:"face_south"`
	FaceEast  bool `json:"face_east"`
	FaceWest  bool `json:"face_west"`
	FaceNorth bool `json:"face_north"`

	BumperLeft  bool `json:"bumper_left"`
	BumperRight bool `json:"bumper_right"`
	StickLeft   bool `json:"stick_left"`
	StickRight  bool `json:"stick_right"`

	MenuPrimary   bool `json:"menu_primary"`
	MenuSecondary bool `json:"menu_secondary"`
	System        bool `json:"system"`
	Capture       bool `json:"capture"`

	Paddle1 bool `json:"paddle_1"`
	Paddle2 bool `json:"paddle_2"`
	Paddle3 bool `json:"paddle_3"`
	Paddle4 bool `json:"paddle_4"`

	Extensions map[ControlID]bool `json:"extensions,omitempty"`
}

// Pressed reports whether a digital button is currently pressed.
func (b Buttons) Pressed(id ControlID) bool {
	switch id {
	case ButtonFaceSouth:
		return b.FaceSouth
	case ButtonFaceEast:
		return b.FaceEast
	case ButtonFaceWest:
		return b.FaceWest
	case ButtonFaceNorth:
		return b.FaceNorth
	case ButtonBumperLeft:
		return b.BumperLeft
	case ButtonBumperRight:
		return b.BumperRight
	case ButtonStickLeft:
		return b.StickLeft
	case ButtonStickRight:
		return b.StickRight
	case ButtonMenuPrimary:
		return b.MenuPrimary
	case ButtonMenuSecondary:
		return b.MenuSecondary
	case ButtonSystem:
		return b.System
	case ButtonCapture:
		return b.Capture
	case ButtonPaddle1:
		return b.Paddle1
	case ButtonPaddle2:
		return b.Paddle2
	case ButtonPaddle3:
		return b.Paddle3
	case ButtonPaddle4:
		return b.Paddle4
	default:
		return b.Extensions[id]
	}
}

// Set updates a digital button. Drivers and tests generally build a State and
// call SetButton instead of calling this method directly.
func (b *Buttons) Set(id ControlID, pressed bool) {
	switch id {
	case ButtonFaceSouth:
		b.FaceSouth = pressed
	case ButtonFaceEast:
		b.FaceEast = pressed
	case ButtonFaceWest:
		b.FaceWest = pressed
	case ButtonFaceNorth:
		b.FaceNorth = pressed
	case ButtonBumperLeft:
		b.BumperLeft = pressed
	case ButtonBumperRight:
		b.BumperRight = pressed
	case ButtonStickLeft:
		b.StickLeft = pressed
	case ButtonStickRight:
		b.StickRight = pressed
	case ButtonMenuPrimary:
		b.MenuPrimary = pressed
	case ButtonMenuSecondary:
		b.MenuSecondary = pressed
	case ButtonSystem:
		b.System = pressed
	case ButtonCapture:
		b.Capture = pressed
	case ButtonPaddle1:
		b.Paddle1 = pressed
	case ButtonPaddle2:
		b.Paddle2 = pressed
	case ButtonPaddle3:
		b.Paddle3 = pressed
	case ButtonPaddle4:
		b.Paddle4 = pressed
	default:
		if !pressed {
			delete(b.Extensions, id)
			return
		}
		if b.Extensions == nil {
			b.Extensions = make(map[ControlID]bool)
		}
		b.Extensions[id] = pressed
	}
}

func (b Buttons) clone() Buttons {
	b.Extensions = maps.Clone(b.Extensions)
	return b
}

// State is a canonical, transport-independent controller snapshot.
type State struct {
	Buttons      Buttons `json:"buttons"`
	LeftStick    Stick   `json:"left_stick"`
	RightStick   Stick   `json:"right_stick"`
	LeftTrigger  float32 `json:"left_trigger"`
	RightTrigger float32 `json:"right_trigger"`
	DPad         DPad    `json:"dpad"`
}

// Clone returns an isolated copy of the controller state.
func (s State) Clone() State {
	s.Buttons = s.Buttons.clone()
	return s
}

// Button reports the current digital state of a button or D-pad direction.
func (s State) Button(id ControlID) bool {
	switch id {
	case DPadUp:
		return s.DPad.Up
	case DPadDown:
		return s.DPad.Down
	case DPadLeft:
		return s.DPad.Left
	case DPadRight:
		return s.DPad.Right
	default:
		return s.Buttons.Pressed(id)
	}
}

// SetButton updates a button or D-pad direction.
func (s *State) SetButton(id ControlID, pressed bool) {
	switch id {
	case DPadUp:
		s.DPad.Up = pressed
	case DPadDown:
		s.DPad.Down = pressed
	case DPadLeft:
		s.DPad.Left = pressed
	case DPadRight:
		s.DPad.Right = pressed
	default:
		s.Buttons.Set(id, pressed)
	}
}
