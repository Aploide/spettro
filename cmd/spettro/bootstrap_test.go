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
	d := startModelDiscovery(context.Background(), cfg, pm, false, nil)
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
	d := startModelDiscovery(context.Background(), cfg, provider.NewManager(), false, nil)
	start := time.Now()
	if d.Wait(50 * time.Millisecond) {
		t.Fatal("Wait reported a hung probe as finished")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("Wait took %v", waited)
	}
}

// A probe that answers after the user removed its endpoint does not bring
// the endpoint back, neither when it answers nor in the config-order pass.
func TestModelDiscoveryDoesNotUndoRemoval(t *testing.T) {
	fast := fakeModelsServer(t, "removed-by-user", 0)
	slow := fakeModelsServer(t, "slow", 300*time.Millisecond)
	slower := fakeModelsServer(t, "removed-while-probing", 300*time.Millisecond)
	cfg := config.UserConfig{LocalEndpoints: []string{fast, slow, slower}, APIKeys: map[string]string{}}
	pm := provider.NewManager()

	d := startModelDiscovery(context.Background(), cfg, pm, false, nil)
	deadline := time.Now().Add(2 * time.Second)
	for len(localModelNames(pm)) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// As /connect does: the fast endpoint after it answered, the slower one
	// while its probe is still running.
	pm.RemoveLocalModels(provider.LocalProviderID(fast))
	pm.RemoveLocalModels(provider.LocalProviderID(slower))
	if !d.Wait(5 * time.Second) {
		t.Fatal("discovery did not finish")
	}
	if got := localModelNames(pm); len(got) != 1 || got[0] != "slow" {
		t.Fatalf("local models = %v, want only the endpoint nobody removed", got)
	}
}

// Each probe that answers signals the front-end, and by the time the signal
// arrives its models are in the provider manager.
func TestModelDiscoverySignalsAfterApplying(t *testing.T) {
	endpoint := fakeModelsServer(t, "arrived", 0)
	cfg := config.UserConfig{LocalEndpoints: []string{endpoint}, APIKeys: map[string]string{}}
	pm := provider.NewManager()
	signal := newModelsChangedSignal()

	d := startModelDiscovery(context.Background(), cfg, pm, false, signal.notify)
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("no signal after the probe answered")
	}
	if got := localModelNames(pm); len(got) != 1 || got[0] != "arrived" {
		t.Fatalf("local models at the signal = %v, want the probe's", got)
	}
	if !d.Wait(5 * time.Second) {
		t.Fatal("discovery did not finish")
	}
	// One endpoint: the config-order pass has nothing to reorder.
	select {
	case <-signal:
		t.Fatal("a second signal for a single endpoint")
	default:
	}
}

// A probe that fails changes nothing and signals nothing.
func TestModelDiscoveryFailedProbeDoesNotSignal(t *testing.T) {
	cfg := config.UserConfig{LocalEndpoints: []string{"http://127.0.0.1:1/v1"}, APIKeys: map[string]string{}}
	signal := newModelsChangedSignal()
	d := startModelDiscovery(context.Background(), cfg, provider.NewManager(), false, signal.notify)
	if !d.Wait(10 * time.Second) {
		t.Fatal("discovery did not finish")
	}
	select {
	case <-signal:
		t.Fatal("a failed probe signalled a change")
	default:
	}
}

// notify never blocks, and signals raised while one is pending collapse
// into it.
func TestModelsChangedSignalCoalesces(t *testing.T) {
	signal := newModelsChangedSignal()
	for range 3 {
		signal.notify()
	}
	<-signal
	select {
	case <-signal:
		t.Fatal("three notifications left more than one pending signal")
	default:
	}
	signal.notify()
	signal.discard()
	select {
	case <-signal:
		t.Fatal("discard left the signal pending")
	default:
	}
}
