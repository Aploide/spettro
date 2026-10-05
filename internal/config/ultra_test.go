package config

import (
	"encoding/json"
	"testing"
)

// /ultra switches on ultracode, whose workflow tool refuses to run under
// ask-first: the toggle is suspended there, not cleared.
func TestUltraActive_RequiresRestrictedOrYolo(t *testing.T) {
	cfg := UserConfig{Ultra: true, Permission: PermissionAskFirst}
	if cfg.UltraActive() {
		t.Fatal("ultra must be suspended under ask-first")
	}
	for _, p := range []PermissionLevel{PermissionRestricted, PermissionYOLO} {
		cfg.Permission = p
		if !cfg.UltraActive() {
			t.Fatalf("ultra should be active under %s", p)
		}
	}
	cfg.Ultra = false
	if cfg.UltraActive() {
		t.Fatal("ultra off must stay off")
	}
}

// A config saved while /ultra still meant the swarm keeps its setting: the
// same key now switches on ultracode.
func TestUltraKeepsItsJSONKey(t *testing.T) {
	var cfg UserConfig
	if err := json.Unmarshal([]byte(`{"ultra": true}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Ultra {
		t.Fatal(`an existing "ultra": true must still load as on`)
	}
	out, _ := json.Marshal(UserConfig{Ultra: true})
	var back map[string]any
	_ = json.Unmarshal(out, &back)
	if back["ultra"] != true {
		t.Fatalf(`the toggle must still save under "ultra": %s`, out)
	}
}
