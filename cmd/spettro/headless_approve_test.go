package main

import (
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// The headless /help lists /approve, so it must work: with a plan pending it
// hands the plan to the coding agent, and without one it says so instead of
// "unknown command".
func TestHeadlessApprove(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	pending := "1. do X"
	plan, ok := takeApprovedPlan("/approve", &pending, &manifest)
	if !ok || plan != "1. do X" || pending != "" {
		t.Fatalf("takeApprovedPlan = %q, %v (pending now %q)", plan, ok, pending)
	}
	if _, ok := takeApprovedPlan("/approve", &pending, &manifest); ok {
		t.Fatal("a plan can be approved only once")
	}
	if _, ok := takeApprovedPlan("/approved-skill", &pending, &manifest); ok {
		t.Fatal("only /approve approves")
	}
	mode := "plan"
	cfg := config.UserConfig{}
	reply, _ := handleHeadlessCommand("/approve", &mode, &cfg, provider.NewManager(), &manifest)
	if !strings.Contains(reply, "no pending plan") {
		t.Fatalf("/approve without a plan: %q", reply)
	}
}
