package quota

// Claim 4 / D-17 (finding QG-07) — Setup must FAIL CLOSED on a non-default
// redis_key_prefix. Python hardcodes the "quota" head; any other Go prefix
// forks the keyspace, so it is rejected at startup rather than warned about.

import (
	"context"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

func TestSetup_RejectsNonDefaultKeyPrefix_D17(t *testing.T) {
	cfg := minimalConfig()
	cfg.Storage = config.StorageConfig{RedisKeyPrefix: "myapp"}

	_, err := Setup(context.Background(), Options{ConfigOverride: cfg})
	if err == nil {
		t.Fatal("D-17: Setup must reject a non-\"quota\" redis_key_prefix, but it succeeded")
	}
	if !strings.Contains(err.Error(), "redis_key_prefix") || !strings.Contains(err.Error(), "parity") {
		t.Errorf("D-17: error should name the knob and the parity contract, got: %v", err)
	}
}

func TestSetup_AcceptsDefaultAndEmptyPrefix_D17(t *testing.T) {
	for _, p := range []string{"", "quota"} {
		cfg := minimalConfig()
		cfg.Storage = config.StorageConfig{RedisKeyPrefix: p}
		q, err := Setup(context.Background(), Options{ConfigOverride: cfg})
		if err != nil {
			t.Fatalf("prefix %q should be accepted, got: %v", p, err)
		}
		_ = q.Close(context.Background())
	}
}
