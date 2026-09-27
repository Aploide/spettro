package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// fakeModelsServer answers /v1/models with one model named name after delay.
func fakeModelsServer(t *testing.T, name string, delay time.Duration) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		fmt.Fprintf(w, `{"data":[{"id":%q}]}`, name)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func localModelNames(pm *provider.Manager) []string {
	var names []string
	for _, m := range pm.Models() {
		if m.Local {
			names = append(names, m.Name)
		}
	}
	return names
}

// Probes run concurrently, but once discovery is done the local models are
// in config order whatever order the servers answered in.
func TestModelDiscoveryKeepsConfigOrder(t *testing.T) {
	slow := fakeModelsServer(t, "first-in-config", 150*time.Millisecond)
	fast := fakeModelsServer(t, "second-in-config", 0)
	cfg := config.UserConfig{LocalEndpoints: []string{slow, fast}, APIKeys: map[string]string{}}
	pm := provider.NewManager()

	start := time.Now()
	d := startModelDiscovery(context.Background(), cfg, pm, false)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("startModelDiscovery blocked on the network")
	}
	if !d.Wait(5 * time.Second) {
		t.Fatal("discovery did not finish")
	}
	got := localModelNames(pm)
	if len(got) != 2 || got[0] != "first-in-config" || got[1] != "second-in-config" {
		t.Fatalf("local models = %v, want config order", got)
	}
}

// Wait gives up after its timeout while a probe is still running.
func TestModelDiscoveryWaitIsBounded(t *testing.T) {
	hung := fakeModelsServer(t, "never", 500*time.Millisecond)
	cfg := config.UserConfig{LocalEndpoints: []string{hung}, APIKeys: map[string]string{}}
	d := startModelDiscovery(context.Background(), cfg, provider.NewManager(), false)
	start := time.Now()
	if d.Wait(50 * time.Millisecond) {
		t.Fatal("Wait reported a hung probe as finished")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("Wait took %v", waited)
	}
}
