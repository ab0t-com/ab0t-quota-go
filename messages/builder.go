// Package messages builds the human-facing denial / warning strings the
// engine attaches to denial responses. Config-driven from day one — the
// Python lib hard-codes copy in helpers.py; the Go port keeps everything
// in struct fields so consumers can override.
//
// Templates use Go text/template; a small fixed set of placeholders are
// substituted: {{.Resource}}, {{.Limit}}, {{.Used}}, {{.Tier}},
// {{.UpgradeURL}}.
package messages

import (
	"bytes"
	"strings"
	"text/template"
)

// Templates is the consumer-overridable copy. Zero-valued fields use the
// in-package defaults.
type Templates struct {
	Denied         string
	OverBurst      string
	Warning        string
	Critical       string
	UpgradePrompt  string
	UnknownTier    string
	ShadowAllowed  string
}

// Builder composes denial messages by rendering Templates.
type Builder struct {
	t Templates
}

// New returns a Builder with fallbacks for any blank template field.
func New(t Templates) *Builder {
	if t.Denied == "" {
		t.Denied = `Quota exceeded for {{.Resource}}: {{.Used}}/{{.Limit}} (tier: {{.Tier}}).`
	}
	if t.OverBurst == "" {
		t.OverBurst = `Burst allowance exceeded for {{.Resource}} (tier: {{.Tier}}).`
	}
	if t.Warning == "" {
		t.Warning = `Approaching quota for {{.Resource}}: {{.Used}}/{{.Limit}}.`
	}
	if t.Critical == "" {
		t.Critical = `Quota critical for {{.Resource}}: {{.Used}}/{{.Limit}}.`
	}
	if t.UpgradePrompt == "" {
		t.UpgradePrompt = `Upgrade your plan to increase this limit: {{.UpgradeURL}}.`
	}
	if t.UnknownTier == "" {
		t.UnknownTier = `Cannot resolve tier for this request (tier "{{.Tier}}" unknown).`
	}
	if t.ShadowAllowed == "" {
		t.ShadowAllowed = `Shadow mode: would-deny {{.Resource}} ({{.Used}}/{{.Limit}}) but allowing.`
	}
	return &Builder{t: t}
}

// Vars are the placeholders templates can reference.
type Vars struct {
	Resource   string
	Limit      string
	Used       string
	Tier       string
	UpgradeURL string
}

// Denied renders the denial message.
func (b *Builder) Denied(v Vars) string { return b.render(b.t.Denied, v) }

// OverBurst renders the burst-exceeded message.
func (b *Builder) OverBurst(v Vars) string { return b.render(b.t.OverBurst, v) }

// Warning renders the threshold-warning message.
func (b *Builder) Warning(v Vars) string { return b.render(b.t.Warning, v) }

// Critical renders the threshold-critical message.
func (b *Builder) Critical(v Vars) string { return b.render(b.t.Critical, v) }

// UpgradePrompt renders the upgrade-call-to-action.
func (b *Builder) UpgradePrompt(v Vars) string { return b.render(b.t.UpgradePrompt, v) }

// UnknownTier renders the unknown-tier message.
func (b *Builder) UnknownTier(v Vars) string { return b.render(b.t.UnknownTier, v) }

// ShadowAllowed renders the shadow-mode "would-deny" notice.
func (b *Builder) ShadowAllowed(v Vars) string { return b.render(b.t.ShadowAllowed, v) }

func (b *Builder) render(tmpl string, v Vars) string {
	if !strings.Contains(tmpl, "{{") {
		return tmpl
	}
	t, err := template.New("m").Parse(tmpl)
	if err != nil {
		return tmpl
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, v); err != nil {
		return tmpl
	}
	return buf.String()
}
