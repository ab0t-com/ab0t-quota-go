package messages

import (
	"strings"
	"testing"
)

func TestBuilder_DefaultsFillBlanks(t *testing.T) {
	b := New(Templates{})
	got := b.Denied(Vars{Resource: "spend", Limit: "100", Used: "150", Tier: "pro"})
	if !strings.Contains(got, "spend") || !strings.Contains(got, "100") {
		t.Errorf("got %q", got)
	}
}

func TestBuilder_OverrideTemplate(t *testing.T) {
	b := New(Templates{Denied: "DENY {{.Resource}} > {{.Limit}}"})
	got := b.Denied(Vars{Resource: "api.calls", Limit: "1000"})
	if got != "DENY api.calls > 1000" {
		t.Errorf("got %q", got)
	}
}

func TestBuilder_BadTemplate_ReturnsRaw(t *testing.T) {
	b := New(Templates{Denied: "DENY {{ . }"}) // malformed
	got := b.Denied(Vars{})
	if !strings.Contains(got, "DENY") {
		t.Errorf("got %q", got)
	}
}

func TestBuilder_AllChannelsRender(t *testing.T) {
	b := New(Templates{})
	v := Vars{Resource: "X", Limit: "10", Used: "9", Tier: "pro", UpgradeURL: "u"}
	cases := []string{
		b.Denied(v),
		b.OverBurst(v),
		b.Warning(v),
		b.Critical(v),
		b.UpgradePrompt(v),
		b.UnknownTier(v),
		b.ShadowAllowed(v),
	}
	for i, s := range cases {
		if s == "" {
			t.Errorf("case %d empty", i)
		}
	}
}
