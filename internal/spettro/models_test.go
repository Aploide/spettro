package spettro

import (
	"encoding/json"
	"testing"
)

// The plan's reasoning flag has three states: true marks the model
// reasoning, false marks it non-reasoning (no thinking parameter is sent),
// and a list that leaves it out decides neither.
func TestProviderModelsReasoningFlag(t *testing.T) {
	var infos []ModelInfo
	if err := json.Unmarshal([]byte(`[{"id":"thinker","reasoning":true},{"id":"chat","reasoning":false},{"id":"flash"}]`), &infos); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]bool{"thinker": {true, false}, "chat": {false, true}, "flash": {false, false}}
	for _, m := range ProviderModels(infos) {
		if m.Provider != ProviderID {
			t.Errorf("%s: provider %q, want %q", m.Name, m.Provider, ProviderID)
		}
		if got := [2]bool{m.Reasoning, m.NoReasoning}; got != want[m.Name] {
			t.Errorf("%s: Reasoning, NoReasoning = %v, want %v", m.Name, got, want[m.Name])
		}
	}
}
