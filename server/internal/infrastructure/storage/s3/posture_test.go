package s3

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestBucketPolicyReading(t *testing.T) {
	public := `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`
	conditioned := `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"s3:GetObject",
		"Condition":{"StringEquals":{"aws:SourceVpce":"vpce-1"}}}]}`
	denySSEC := `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:PutObject",
		"Condition":{"Null":{"s3:x-amz-server-side-encryption-customer-algorithm":"false"}}}]}`
	if !policyAllowsEveryone(public) || policyAllowsEveryone(conditioned) || policyAllowsEveryone(denySSEC) {
		t.Fatal("public policy")
	}
	if !policyDeniesSSEC(denySSEC) || policyDeniesSSEC(public) || policyDeniesSSEC("not json") {
		t.Fatal("SSE-C deny")
	}
}

func TestLifecycleRulesReachingThePlatform(t *testing.T) {
	ours := []string{"staging", "ready"}
	for prefix, want := range map[string]bool{"": true, "ready/": true, "ready/t1/": true, "re": true, "logs/": false, "readyish/": false} {
		if _, got := overlaps(prefix, ours); got != want {
			t.Errorf("%q: %v", prefix, got)
		}
	}
	tagOnly := types.LifecycleRule{Filter: &types.LifecycleRuleFilter{Tag: &types.Tag{Key: aws.String("k"), Value: aws.String("v")}}}
	if _, prefixOnly := rulePrefix(tagOnly); prefixOnly {
		t.Error("a tag filter was read as a prefix")
	}
	and := types.LifecycleRule{Filter: &types.LifecycleRuleFilter{And: &types.LifecycleRuleAndOperator{Prefix: aws.String("ready/")}}}
	if p, prefixOnly := rulePrefix(and); p != "ready/" || !prefixOnly {
		t.Errorf("and filter: %q %v", p, prefixOnly)
	}
}
