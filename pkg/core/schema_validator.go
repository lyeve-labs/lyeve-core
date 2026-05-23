package core

import (
	"fmt"
	"regexp"
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
// A name a plugin supplies wins over the built-in interpreter, which is what
// makes a plugin able to give "postcode" meaning beyond a pattern.
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

// LookupCustomValidator returns the validator for name, falling back to the
// built-in pattern interpreter.
//
// The fallback is what makes custom_validators usable without writing a
// plugin: a schema entry whose body is a regular expression validates against
// it. Go's regexp is RE2, so a pattern runs in time linear in the subject and
// a schema author cannot write one that hangs the engine.
func LookupCustomValidator(name string) (CustomValidatorFunc, bool) {
	customValidators.mu.RLock()
	fn, ok := customValidators.fn[name]
	customValidators.mu.RUnlock()
	if ok {
		return fn, true
	}
	return patternValidator, true
}

// patternCache holds compiled bodies. A schema's validators are fixed between
// writes and every row validated re-reads the same handful of patterns, so
// compiling once per body rather than once per value is the difference between
// a validator and a bottleneck.
var patternCache sync.Map // body string -> *regexp.Regexp or error

// patternValidator reads the body as a regular expression and requires the
// value to match it whole.
func patternValidator(value any, body string, _ map[string]any) CustomValidatorResult {
	if body == "" {
		return CustomValidatorResult{
			Error: "this validator has no body, so there is nothing to check the value against",
		}
	}
	re, err := compilePattern(body)
	if err != nil {
		return CustomValidatorResult{Error: fmt.Sprintf("the validator's pattern is not usable: %v", err)}
	}
	s, ok := value.(string)
	if !ok {
		if value == nil {
			return CustomValidatorResult{OK: true}
		}
		s = fmt.Sprint(value)
	}
	if !re.MatchString(s) {
		return CustomValidatorResult{Error: fmt.Sprintf("value does not match %s", body)}
	}
	return CustomValidatorResult{OK: true}
}

// compilePattern anchors the body so a validator means the whole value, which
// is what a schema author writing "[A-Z]{2}" intends, and caches the result.
func compilePattern(body string) (*regexp.Regexp, error) {
	if v, ok := patternCache.Load(body); ok {
		if re, ok := v.(*regexp.Regexp); ok {
			return re, nil
		}
		return nil, v.(error)
	}
	anchored := body
	if len(anchored) == 0 || anchored[0] != '^' {
		anchored = "^" + anchored
	}
	if anchored[len(anchored)-1] != '$' {
		anchored += "$"
	}
	re, err := regexp.Compile(anchored)
	if err != nil {
		patternCache.Store(body, err)
		return nil, err
	}
	patternCache.Store(body, re)
	return re, nil
}
