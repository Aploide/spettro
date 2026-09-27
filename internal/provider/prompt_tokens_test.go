package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// promptTokensManager knows one local model without vision at an address
// nothing listens on. The tests below stop at the input budget check, before
// any request would be sent.
func promptTokensManager() (*Manager, string) {
	const endpoint = "http://127.0.0.1:1/v1"
	pm := NewManager()
	pm.AddLocalModels([]Model{{Provider: endpoint, Name: "m", Local: true}})
	return pm, endpoint
}

// sendEstimate returns the prompt estimate Send checked against the input
// budget, read from the budget error a budget of one token always causes.
func sendEstimate(t *testing.T, req Request) string {
	t.Helper()
	pm, endpoint := promptTokensManager()
	req.InputBudget = 1
	_, err := pm.Send(context.Background(), endpoint, "m", req)
	if err == nil || !strings.Contains(err.Error(), "token budget exceeded") {
		t.Fatalf("Send error = %v, want the input budget error", err)
	}
	_, rest, _ := strings.Cut(err.Error(), "estimated=")
	estimate, _, _ := strings.Cut(rest, " ")
	return estimate
}

// Send uses the caller's estimate instead of counting the request again.
func TestSendUsesPromptTokensHint(t *testing.T) {
	req := Request{Messages: []Message{{Role: RoleUser, Content: "hello"}}, PromptTokens: 5000}
	if got := sendEstimate(t, req); got != "5000" {
		t.Fatalf("Send checked estimate %s, want the caller's 5000", got)
	}
}

// Without a hint Send estimates the request itself.
func TestSendEstimatesWithoutHint(t *testing.T) {
	req := Request{Messages: []Message{{Role: RoleUser, Content: strings.Repeat("x", 400)}}}
	want := fmt.Sprint(EstimateRequestTokens(req))
	if got := sendEstimate(t, req); got != want {
		t.Fatalf("Send checked estimate %s, want its own %s", got, want)
	}
}

// When Send strips images for a model without vision, the request it sends
// is not the one the hint measured, so it estimates the stripped request.
func TestSendReestimatesStrippedRequest(t *testing.T) {
	req := Request{
		Messages:     []Message{{Role: RoleUser, Content: "look", Images: []string{"/nonexistent/shot.png"}}},
		PromptTokens: 5000,
	}
	want := fmt.Sprint(EstimateRequestTokens(stripImages(req)))
	if got := sendEstimate(t, req); got != want {
		t.Fatalf("Send checked estimate %s, want the stripped request's %s", got, want)
	}
}
