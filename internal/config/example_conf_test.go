package config

import "testing"

// The shipped example conf must always parse — it's the first thing a new
// operator copies. (A # comment inside JSON broke it once.)
func TestExampleConfParses(t *testing.T) {
	c, err := Load("../../llm_gateway.conf")
	if err != nil {
		t.Fatalf("shipped example conf does not parse: %v", err)
	}
	if len(c.ModelPools) == 0 {
		t.Fatal("example conf lost its pool")
	}
	if c.Gateway.Port != 8033 {
		t.Fatalf("port = %d", c.Gateway.Port)
	}
}
