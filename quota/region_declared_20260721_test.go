package quota

// T-G2 REDs (GO-02 + GO-08, pack 20260721_shared_lib_declared_not_discovered).
//
// GO-02: setup.go invented TWO DIFFERENT AWS regions — us-west-2 in
// newDDBClient and us-east-1 in newSNSClient — so with no region declared,
// which region a table landed in depended on which code path provisioned it.
// Contract: declared region wins; otherwise the AWS SDK's own chain (env /
// profile / IMDS — platform contract, design §2.3-SDK, out of scope by
// ruling); a region NOBODY resolves is a typed config error, never an
// invented default.
//
// GO-08: ddbSignalPresent used AWS_ENDPOINT_URL as a FEATURE FLAG ("is
// DynamoDB configured at all?"). The SDK carve-out covers endpoints, not
// intent — whether the subsystem exists is decided from declared config only.

import (
	"context"
	"strings"
	"testing"

	"github.com/ab0t-com/ab0t-quota-go/config"
)

// clearAWSRegionSources makes the SDK chain resolve NOTHING: env cleared,
// config files pointed at nothing, IMDS disabled (this box may be EC2 — an
// IMDS answer would silently supply a region and fake a green).
func clearAWSRegionSources(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent-aws-config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent-aws-credentials")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func TestNoRegionDeclaredIsAnError(t *testing.T) {
	clearAWSRegionSources(t)

	t.Run("ddb_us_west_2_fallback_gone", func(t *testing.T) {
		_, err := newDDBClient(context.Background(), config.StorageConfig{})
		if err == nil {
			t.Fatal("GO-02: no region declared and none resolvable by the SDK chain — " +
				"newDDBClient must return a typed config error, not invent us-west-2")
		}
		for _, tok := range []string{"storage.dynamodb_region", "AWS_REGION"} {
			if !strings.Contains(err.Error(), tok) {
				t.Errorf("the refusal must name %q verbatim; got: %v", tok, err)
			}
		}
	})

	t.Run("sns_us_east_1_fallback_gone", func(t *testing.T) {
		_, err := newSNSClient(context.Background(), config.OutboxConfig{})
		if err == nil {
			t.Fatal("GO-02: no region declared and none resolvable by the SDK chain — " +
				"newSNSClient must return a typed config error, not invent us-east-1")
		}
		for _, tok := range []string{"outbox.sns_region", "AWS_REGION"} {
			if !strings.Contains(err.Error(), tok) {
				t.Errorf("the refusal must name %q verbatim; got: %v", tok, err)
			}
		}
	})
}

func TestDeclaredAndSDKRegionsHonored(t *testing.T) {
	clearAWSRegionSources(t)

	t.Run("declared_config_region_wins", func(t *testing.T) {
		c, err := newDDBClient(context.Background(), config.StorageConfig{DynamoDBRegion: "eu-west-1"})
		if err != nil {
			t.Fatalf("declared storage.dynamodb_region must be honoured: %v", err)
		}
		if got := c.Options().Region; got != "eu-west-1" {
			t.Errorf("DDB client region = %q, want the declared eu-west-1", got)
		}
		s, err := newSNSClient(context.Background(), config.OutboxConfig{SNSRegion: "eu-central-1"})
		if err != nil {
			t.Fatalf("declared outbox.sns_region must be honoured: %v", err)
		}
		if got := s.Options().Region; got != "eu-central-1" {
			t.Errorf("SNS client region = %q, want the declared eu-central-1", got)
		}
	})

	t.Run("ambient_AWS_REGION_is_SDK_contract", func(t *testing.T) {
		// The deletion test (§2.3-SDK): the SDK resolves AWS_REGION by its own
		// documented mechanism — the library defers and logs, never overrides.
		t.Setenv("AWS_REGION", "ap-southeast-2")
		c, err := newDDBClient(context.Background(), config.StorageConfig{})
		if err != nil {
			t.Fatalf("AWS_REGION set — the SDK chain must resolve it: %v", err)
		}
		if got := c.Options().Region; got != "ap-southeast-2" {
			t.Errorf("DDB client region = %q, want the SDK-resolved ap-southeast-2 "+
				"(today an invented us-west-2 overrides the platform's own contract)", got)
		}
		s, err := newSNSClient(context.Background(), config.OutboxConfig{})
		if err != nil {
			t.Fatalf("AWS_REGION set — the SDK chain must resolve it: %v", err)
		}
		if got := s.Options().Region; got != "ap-southeast-2" {
			t.Errorf("SNS client region = %q, want the SDK-resolved ap-southeast-2", got)
		}
	})
}

func TestEndpointVarIsNotAFeatureFlag(t *testing.T) {
	// GO-08: a LocalStack endpoint set for some OTHER service in the same
	// environment must not change ab0t-quota's decision about whether
	// DynamoDB is configured at all.
	t.Setenv("AWS_ENDPOINT_URL", "http://localhost:4566")
	if ddbSignalPresent(&config.Config{}) {
		t.Error("GO-08: AWS_ENDPOINT_URL alone must not make the library believe DynamoDB " +
			"is configured — the variable is an endpoint override, not an intent signal")
	}

	t.Setenv("AWS_ENDPOINT_URL", "")
	declared := map[string]*config.Config{
		"outbox.store=ddb":       {Outbox: config.OutboxConfig{Store: "ddb"}},
		"storage.dynamodb_table": {Storage: config.StorageConfig{DynamoDBTable: "t"}},
		"outbox.ddb_table":       {Outbox: config.OutboxConfig{DDBTable: "t"}},
	}
	for name, c := range declared {
		if !ddbSignalPresent(c) {
			t.Errorf("declared %s must still signal DDB intent", name)
		}
	}
}
