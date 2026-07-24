package config

// The Go dependency resolver (pack 20260721; design §2.2 precedence, §5
// error contract). One resolution path, provenance attached, no generic env
// names, no invented values — declared, not discovered.

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Requirement expresses how an undeclared dependency is judged (design §2.3).
type Requirement string

const (
	Required Requirement = "required" // undeclared ⇒ typed ConfigError before any I/O
	Optional Requirement = "optional" // absent ⇒ unset (feature off); null ⇒ declared-off
)

// Spec declares one dependency for resolution.
type Spec struct {
	Name        string   // human name, e.g. "Redis counter store URL"
	ConfigKey   string   // dotted key, e.g. "storage.redis_url"
	Env         []string // documented NAMESPACED env vars, in order — never a generic name
	Requirement Requirement
	Code        string // stable error code (QUOTA-CFG-001…), for runbooks + doc-lint
	Previously  string // what earlier versions silently did — the migration line
	Remedy      string // the one action, concretely
	Docs        string // doc anchor + pre-deploy verification hint
}

// Resolved is a resolution outcome with provenance.
type Resolved struct {
	Value  string
	Source string // "config <key>" | "env <NAME>" | "off (declared null)" | "unset"
}

// ConfigError is the typed §5 refusal: one error, one cause, one remedy.
type ConfigError struct {
	Code    string
	Message string
}

func (e *ConfigError) Error() string { return e.Message }

// ResolveString resolves one string dependency per the §2.2 precedence:
// config declared (non-null, non-empty) → namespaced env (also consulted on
// explicit null — a consumer-set QUOTA_* var is a declaration, not
// discovery) → for Required, a typed startup error; for Optional,
// declared-off / unset. There is no further tier: no generic env var and no
// invented default, ever.
func ResolveString(d Declared[string], spec Spec) (Resolved, error) {
	if v, ok := d.Get(); ok && v != "" {
		return Resolved{Value: v, Source: "config " + spec.ConfigKey}, nil
	}
	for _, name := range spec.Env {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return Resolved{Value: v, Source: "env " + name}, nil
		}
	}
	if spec.Requirement == Optional {
		if d.IsNull() {
			return Resolved{Source: "off (declared null)"}, nil
		}
		return Resolved{Source: "unset"}, nil
	}
	state := "key absent"
	switch {
	case d.IsNull():
		state = "declared null"
	case !d.IsAbsent():
		// Present but empty — e.g. an unset ${QUOTA_*} interpolation.
		state = "declared empty"
	}
	return Resolved{}, NewUndeclaredError(spec, state)
}

// NewUndeclaredError builds the §5-shaped refusal. The message names the
// config key and the accepted namespaced env var(s) VERBATIM — an operator
// who has never seen this library's source can act on it.
func NewUndeclaredError(spec Spec, state string) *ConfigError {
	env := "(none)"
	if len(spec.Env) > 0 {
		env = strings.Join(spec.Env, " / ") + "        (not set)"
	}
	msg := fmt.Sprintf(
		"ab0t-quota-go config error [%s] — %s is not declared.\n\n"+
			"  config key : %s      (quota-config.json: %s)\n"+
			"  env        : %s\n"+
			"  previously : %s\n"+
			"  remedy     : %s\n\n"+
			"  docs: %s",
		spec.Code, spec.Name, spec.ConfigKey, state, env,
		spec.Previously, spec.Remedy, spec.Docs)
	return &ConfigError{Code: spec.Code, Message: msg}
}

// RedactURL strips userinfo from a URL for logs and errors (design §6.3):
// scheme/host/port/db survive, credentials never do.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String() + " (userinfo redacted)"
}
