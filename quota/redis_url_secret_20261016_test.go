package quota

// Ticket 20261016 review C-06: the Setup refusal for an unparseable redis_url
// carried go-redis's parse error, which embeds the whole URL — password
// included. RED on v0.1.9.

import (
	"context"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

func TestSetup_UnparseableRedisURLErrorHasNoPassword_20261016(t *testing.T) {
	cfg := minimalConfig()
	retry := 0.0
	cfg.Storage.RedisURL = config.Declare("redis://:s3cr/et+base64==@cache.internal:6379/0")
	cfg.Storage.ConnectRetrySeconds = &retry
	_, err := Setup(context.Background(), Options{ConfigOverride: cfg, SkipBackgroundLoops: true})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if strings.Contains(err.Error(), "s3cr") {
		t.Fatalf("the refusal leaks the Redis password: %v", err)
	}
}
