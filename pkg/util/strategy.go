package util

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// DeviceListStrategies represents the set of strategies used to pass the device
// list to the underlying runtime. Multiple strategies can be combined, for
// example "envvar" together with "cdi-annotations".
type DeviceListStrategies []string

// Includes returns true if the given strategy is part of the set.
func (s DeviceListStrategies) Includes(strategy string) bool {
	for _, x := range s {
		if x == strategy {
			return true
		}
	}
	return false
}

// AnyCDIEnabled returns true if any CDI based strategy is enabled.
func (s DeviceListStrategies) AnyCDIEnabled() bool {
	return s.Includes(DeviceListStrategyCDIAnnotations) || s.Includes(DeviceListStrategyCDICRI)
}

// AllCDIEnabled returns true if every enabled strategy is a CDI based strategy.
func (s DeviceListStrategies) AllCDIEnabled() bool {
	if len(s) == 0 {
		return false
	}
	for _, x := range s {
		if x != DeviceListStrategyCDIAnnotations && x != DeviceListStrategyCDICRI {
			return false
		}
	}
	return true
}

// Validate checks that the configured strategies are non-empty and known.
func (s DeviceListStrategies) Validate() error {
	if len(s) == 0 {
		return fmt.Errorf("deviceListStrategy cannot be empty")
	}
	for _, strategy := range s {
		switch strategy {
		case DeviceListStrategyEnvvar,
			DeviceListStrategyVolumeMounts,
			DeviceListStrategyCDIAnnotations,
			DeviceListStrategyCDICRI:
		default:
			return fmt.Errorf("unknown deviceListStrategy value: %q", strategy)
		}
	}
	return nil
}

// MarshalJSON emits a single string when only one strategy is configured and a
// list otherwise, keeping the serialized form backward compatible.
func (s DeviceListStrategies) MarshalJSON() ([]byte, error) {
	if len(s) == 1 {
		return json.Marshal(s[0])
	}
	return json.Marshal([]string(s))
}

// MarshalYAML emits a single string when only one strategy is configured and a
// list otherwise, keeping the serialized form backward compatible.
func (s DeviceListStrategies) MarshalYAML() (interface{}, error) {
	if len(s) == 1 {
		return s[0], nil
	}
	return []string(s), nil
}

// UnmarshalJSON accepts either a single string or a list of strings so that
// existing single-value configurations remain backward compatible.
func (s *DeviceListStrategies) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = DeviceListStrategies{single}
		return nil
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("deviceListStrategy must be a string or a list of strings: %w", err)
	}
	*s = DeviceListStrategies(list)
	return nil
}

// UnmarshalYAML accepts either a single string or a list of strings so that
// existing single-value configurations remain backward compatible.
func (s *DeviceListStrategies) UnmarshalYAML(value *yaml.Node) error {
	var single string
	if err := value.Decode(&single); err == nil {
		*s = DeviceListStrategies{single}
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return fmt.Errorf("deviceListStrategy must be a string or a list of strings: %w", err)
	}
	*s = DeviceListStrategies(list)
	return nil
}
