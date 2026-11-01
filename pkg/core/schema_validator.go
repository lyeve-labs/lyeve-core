package core

import (
	"sort"
	"sync"
)

// CustomValidatorResult is the outcome of one custom validator run. Error is
// what the caller sees on a field, so it states what the value had to be
// rather than naming the validator.
type CustomValidatorResult struct {
	OK    bool
	Error string
}

// CustomValidatorFunc validates one field value. body is the schema's
// custom_validators entry for this name, params are the arguments the field's
// rule passed. Both may be empty.
type CustomValidatorFunc func(value any, body string, params map[string]any) CustomValidatorResult

// CustomValidatorProvider is an optional interface a plugin implements to
// supply validators a schema can name. The runtime collects them once, after
// activation, so a validator is available for every request a schema is
// validated on and never changes under one.
//
// A name a plugin supplies is the validator for that name, which is what
// makes a plugin able to give "postcode" a meaning of its own. A name no
// plugin supplies is left to the schema engine.
type CustomValidatorProvider interface {
	Plugin

	// CustomValidators returns this plugin's validators by the name a schema
	// uses in custom_validators.
	CustomValidators() map[string]CustomValidatorFunc
}

var customValidators = struct {
	mu sync.RWMutex
	fn map[string]CustomValidatorFunc
}{fn: map[string]CustomValidatorFunc{}}

// RegisterCustomValidator installs fn under name, replacing any validator of
// the same name. A nil fn or an empty name is ignored rather than stored,
// because a registry entry that cannot run is worse than an absent one: the
// schema would name a validator that exists and does nothing.
func RegisterCustomValidator(name string, fn CustomValidatorFunc) {
	if name == "" || fn == nil {
		return
	}
	customValidators.mu.Lock()
	defer customValidators.mu.Unlock()
	customValidators.fn[name] = fn
}

// RegisteredCustomValidators returns the registered names, sorted.
func RegisteredCustomValidators() []string {
	customValidators.mu.RLock()
	defer customValidators.mu.RUnlock()
	names := make([]string, 0, len(customValidators.fn))
	for name := range customValidators.fn {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResetCustomValidators empties the registry. For tests and for a runtime that
// re-activates its plugins.
func ResetCustomValidators() {
	customValidators.mu.Lock()
	defer customValidators.mu.Unlock()
	customValidators.fn = map[string]CustomValidatorFunc{}
}

// LookupCustomValidator returns the validator a plugin registered under name,
// and false when none has. What an unclaimed name means is the schema
// engine's to decide.
func LookupCustomValidator(name string) (CustomValidatorFunc, bool) {
	customValidators.mu.RLock()
	defer customValidators.mu.RUnlock()
	fn, ok := customValidators.fn[name]
	return fn, ok
}
