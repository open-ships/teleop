package audit

import (
	"os"
	"runtime"
	"runtime/debug"
)

// A record that an operator pressed a control is not by itself evidence of
// anything: the same input produces different actuation under a different
// build, a different dead zone, or a different action binding. Provenance ties
// the recorded input to the code and configuration that interpreted it, which
// is what makes a log reconstructable rather than merely authentic.

// Provenance identifies the software, host, configuration, and operator behind
// a recorded session. Fields the process can determine for itself are filled
// by CaptureProvenance; the rest are the application's to supply.
type Provenance struct {
	// Module is the main module path of the recording binary.
	Module string `json:"module,omitempty"`
	// Revision is the VCS revision the binary was built from.
	Revision string `json:"revision,omitempty"`
	// Modified reports a build made from a dirty working tree. A true value
	// means the revision does not fully describe the running code.
	Modified bool `json:"modified,omitempty"`
	// BuildVersion is the module version, when built as a dependency.
	BuildVersion string `json:"build_version,omitempty"`

	GoVersion string `json:"go_version,omitempty"`
	OS        string `json:"os,omitempty"`
	Arch      string `json:"arch,omitempty"`
	Host      string `json:"host,omitempty"`
	PID       int    `json:"pid,omitempty"`

	// Application and ApplicationVersion identify the program embedding
	// teleop, which the runtime cannot determine on its own.
	Application        string `json:"application,omitempty"`
	ApplicationVersion string `json:"application_version,omitempty"`

	// Operator identifies who held the controls. Recording it has privacy
	// consequences; see the audit guide before enabling it in a workplace.
	Operator string `json:"operator,omitempty"`
	// Authorization records the grant under which the session ran, such as a
	// work order, shift assignment, or ticket.
	Authorization string `json:"authorization,omitempty"`

	// Config captures the parameters that determine how input becomes
	// actuation: dead zones, gesture thresholds, action bindings, rate limits.
	// Without them a replay cannot reproduce the original commands.
	Config map[string]any `json:"config,omitempty"`

	// Platform records driver, kernel, and transport detail the application
	// can discover but teleop cannot portably determine.
	Platform map[string]string `json:"platform,omitempty"`
}

// CaptureProvenance fills the fields the process can determine for itself.
// Callers should set Application, Operator, and Config on the result.
func CaptureProvenance() Provenance {
	provenance := Provenance{
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		PID:       os.Getpid(),
	}
	if host, err := os.Hostname(); err == nil {
		provenance.Host = host
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return provenance
	}
	provenance.Module = info.Main.Path
	provenance.BuildVersion = info.Main.Version
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			provenance.Revision = setting.Value
		case "vcs.modified":
			provenance.Modified = setting.Value == "true"
		}
	}
	return provenance
}

// Clone returns a deep copy so a recorder cannot observe later mutation of a
// caller's maps.
func (p Provenance) Clone() Provenance {
	if p.Config != nil {
		config := make(map[string]any, len(p.Config))
		for key, value := range p.Config {
			config[key] = value
		}
		p.Config = config
	}
	if p.Platform != nil {
		platform := make(map[string]string, len(p.Platform))
		for key, value := range p.Platform {
			platform[key] = value
		}
		p.Platform = platform
	}
	return p
}
