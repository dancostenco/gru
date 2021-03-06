// +build linux

package resource

import (
	"errors"
	"fmt"

	"github.com/coreos/go-systemd/dbus"
	"github.com/coreos/go-systemd/util"
)

// ErrNoSystemd error is returned when the system is detected to
// have no support for systemd.
var ErrNoSystemd = errors.New("No systemd support found")

// Service type is a resource which manages services on a
// GNU/Linux system running with systemd.
//
// Example:
//   svc = resource.service.new("nginx")
//   svc.state = "running"
//   svc.enable = true
type Service struct {
	Base

	// If true then enable the service during boot-time
	Enable bool `luar:"enable"`

	// Systemd unit name
	unit string `luar:"-"`
}

// NewService creates a new resource for managing services
// using systemd on a GNU/Linux system
func NewService(name string) (Resource, error) {
	if !util.IsRunningSystemd() {
		return nil, ErrNoSystemd
	}

	s := &Service{
		Base: Base{
			Name:          name,
			Type:          "service",
			State:         "running",
			Require:       make([]string, 0),
			PresentStates: []string{"present", "running"},
			AbsentStates:  []string{"absent", "stopped"},
			Concurrent:    true,
		},
		Enable: true,
		unit:   fmt.Sprintf("%s.service", name),
	}

	return s, nil
}

// unitProperty retrieves the requested property for the service unit
func (s *Service) unitProperty(name string) (*dbus.Property, error) {
	conn, err := dbus.New()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	property, err := conn.GetUnitProperty(s.unit, name)

	return property, err
}

// unitIsEnabled checks if the unit is enabled or disabled
func (s *Service) unitIsEnabled() (bool, error) {
	unitState, err := s.unitProperty("UnitFileState")
	if err != nil {
		return false, err
	}

	value := unitState.Value.Value().(string)
	switch value {
	case "enabled", "static", "enabled-runtime", "linked", "linked-runtime":
		return true, nil
	case "disabled", "masked", "masked-runtime":
		return false, nil
	case "invalid":
		fallthrough
	default:
		return false, errors.New("Invalid unit state")
	}
}

// enableUnit enables the service unit during boot-time
func (s *Service) enableUnit() error {
	conn, err := dbus.New()
	if err != nil {
		return err
	}
	defer conn.Close()

	s.Log("enabling service\n")

	units := []string{s.unit}
	_, changes, err := conn.EnableUnitFiles(units, false, false)
	if err != nil {
		return err
	}

	for _, change := range changes {
		s.Log("%s %s -> %s\n", change.Type, change.Filename, change.Destination)
	}

	return nil
}

// disableUnit disables the service unit during boot-time
func (s *Service) disableUnit() error {
	conn, err := dbus.New()
	if err != nil {
		return err
	}
	defer conn.Close()

	s.Log("disabling service\n")

	units := []string{s.unit}
	changes, err := conn.DisableUnitFiles(units, false)
	if err != nil {
		return err
	}

	for _, change := range changes {
		s.Log("%s %s\n", change.Type, change.Filename)
	}

	return nil
}

// setUnitState enables or disables the unit
func (s *Service) setUnitState() error {
	enabled, err := s.unitIsEnabled()
	if err != nil {
		return err
	}

	if s.Enable && !enabled {
		if err := s.enableUnit(); err != nil {
			return err
		}
	} else {
		if err := s.disableUnit(); err != nil {
			return err
		}
	}

	return s.daemonReload()
}

// daemonReload instructs systemd to reload it's configuration
func (s *Service) daemonReload() error {
	conn, err := dbus.New()
	if err != nil {
		return err
	}
	defer conn.Close()

	return conn.Reload()
}

// Evaluate evaluates the state of the resource
func (s *Service) Evaluate() (State, error) {
	state := State{
		Current:  "unknown",
		Want:     s.State,
		Outdated: false,
	}

	// Check if the unit is started/stopped
	activeState, err := s.unitProperty("ActiveState")
	if err != nil {
		return state, err
	}

	// TODO: Handle cases where the unit is not found

	value := activeState.Value.Value().(string)
	switch value {
	case "active", "reloading", "activating":
		state.Current = "running"
	case "inactive", "failed", "deactivating":
		state.Current = "stopped"
	}

	enabled, err := s.unitIsEnabled()
	if err != nil {
		return state, err
	}

	if s.Enable != enabled {
		state.Outdated = true
	}

	return state, nil
}

// Create starts the service unit
func (s *Service) Create() error {
	conn, err := dbus.New()
	if err != nil {
		return err
	}
	defer conn.Close()

	s.Log("starting service\n")

	ch := make(chan string)
	jobID, err := conn.StartUnit(s.unit, "replace", ch)
	if err != nil {
		return err
	}

	result := <-ch
	s.Log("systemd job id %d result: %s\n", jobID, result)

	return s.setUnitState()
}

// Delete stops the service unit
func (s *Service) Delete() error {
	conn, err := dbus.New()
	if err != nil {
		return err
	}
	defer conn.Close()

	s.Log("stopping service\n")

	ch := make(chan string)
	jobID, err := conn.StopUnit(s.unit, "replace", ch)
	if err != nil {
		return err
	}

	result := <-ch
	s.Log("systemd job id %d result: %s\n", jobID, result)

	return s.setUnitState()
}

// Update updates the service unit state
func (s *Service) Update() error {
	return s.setUnitState()
}

func init() {
	item := RegistryItem{
		Type:      "service",
		Provider:  NewService,
		Namespace: DefaultNamespace,
	}

	Register(item)
}
