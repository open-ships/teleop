package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"

	"github.com/open-ships/teleop"
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

	// GoVersion, OS, and Arch identify the recording runtime.
	GoVersion string `json:"go_version,omitempty"`
	OS        string `json:"os,omitempty"`
	Arch      string `json:"arch,omitempty"`
	// Host and PID identify the recording process.
	Host string `json:"host,omitempty"`
	PID  int    `json:"pid,omitempty"`

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

// Clone recursively isolates ordinary reference-backed configuration values.
// Recorder construction additionally normalizes Config through JSON once so
// custom marshalers with hidden mutable state cannot change a manifest later.
func (p Provenance) Clone() Provenance {
	if p.Config != nil {
		config := make(map[string]any, len(p.Config))
		visited := make(map[cloneVisit]reflect.Value)
		for key, value := range p.Config {
			cloned := cloneProvenanceValue(reflect.ValueOf(value), visited)
			if cloned.IsValid() {
				config[key] = cloned.Interface()
			} else {
				config[key] = nil
			}
		}
		p.Config = config
	}
	p.Platform = maps.Clone(p.Platform)
	return p
}

// freezeProvenance crosses the open-ended Config callback boundary exactly
// once, then decodes the resulting JSON into reference-isolated, JSON-native
// values. Reflection alone cannot clone reference-backed unexported fields
// consulted by a custom Marshaler, and invoking such a Marshaler separately
// for hashing and writing could produce two different manifests.
func freezeProvenance(provenance Provenance) (frozen Provenance, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			frozen = Provenance{}
			err = fmt.Errorf(
				"%w: provenance JSON callback: %v",
				teleop.ErrCallbackPanic,
				recovered,
			)
		}
	}()
	encoded, err := json.Marshal(provenance)
	if err != nil {
		return Provenance{}, fmt.Errorf("marshal audit provenance: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&frozen); err != nil {
		return Provenance{}, fmt.Errorf("normalize audit provenance: %w", err)
	}
	return frozen, nil
}

// cloneVisit identifies reference-backed values while recursively cloning a
// provenance configuration. Config is intentionally open to application-defined
// JSON values, so a shallow map clone would still let a caller mutate a nested
// map, slice, or pointer after the manifest was configured.
type cloneVisit struct {
	typeOf  reflect.Type
	pointer uintptr
	length  int // slices sharing an address can represent different JSON values
}

func cloneProvenanceValue(value reflect.Value, visited map[cloneVisit]reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}

	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneProvenanceValue(value.Elem(), visited)
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result

	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typeOf: value.Type(), pointer: value.Pointer()}
		if cloned, ok := visited[visit]; ok {
			return cloned
		}
		result := reflect.New(value.Type().Elem())
		visited[visit] = result
		result.Elem().Set(cloneProvenanceValue(value.Elem(), visited))
		return result

	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typeOf: value.Type(), pointer: value.Pointer()}
		if cloned, ok := visited[visit]; ok {
			return cloned
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		visited[visit] = result
		iterator := value.MapRange()
		for iterator.Next() {
			key := cloneProvenanceValue(iterator.Key(), visited)
			item := cloneProvenanceValue(iterator.Value(), visited)
			result.SetMapIndex(key, item)
		}
		return result

	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typeOf: value.Type(), pointer: value.Pointer(), length: value.Len()}
		if cloned, ok := visited[visit]; ok {
			return cloned
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		visited[visit] = result
		for index := range value.Len() {
			result.Index(index).Set(cloneProvenanceValue(value.Index(index), visited))
		}
		return result

	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for index := range value.Len() {
			result.Index(index).Set(cloneProvenanceValue(value.Index(index), visited))
		}
		return result

	case reflect.Struct:
		// Copy the whole value first so immutable structs with unexported state,
		// such as time.Time, retain that state. Exported reference-backed fields
		// are then recursively isolated.
		result := reflect.New(value.Type()).Elem()
		result.Set(value)
		for index := range value.NumField() {
			if result.Field(index).CanSet() && value.Type().Field(index).IsExported() {
				result.Field(index).Set(cloneProvenanceValue(value.Field(index), visited))
			}
		}
		return result

	default:
		return value
	}
}
