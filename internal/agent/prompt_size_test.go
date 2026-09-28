package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"testing"

	"spettro/internal/provider"
)

// sizerTestHistory builds a conversation of about n messages shaped like a
// tool loop: the task, then assistant tool-call / tool-result pairs whose
// outputs are resultSize characters (with some non-ASCII text).
func sizerTestHistory(n, resultSize int) []provider.Message {
	msgs := []provider.Message{{Role: provider.RoleUser, Content: "Task:\nrefactor the handlers"}}
	for i := 0; len(msgs) < n; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			provider.Message{
				Role:      provider.RoleAssistant,
				Content:   "Reading the next handler.",
				Reasoning: []provider.ReasoningBlock{{Text: "the wiring is in h" + fmt.Sprint(i)}},
				ToolCalls: []provider.NativeTool{{ID: id, Name: "file-read", Args: json.RawMessage(fmt.Sprintf(`{"path":"pkg/h%d.go"}`, i))}},
			},
			provider.Message{
				Role:        provider.RoleUser,
				ToolResults: []provider.ToolResult{{ID: id, Name: "file-read", Output: strings.Repeat("func ünïcode() {}\n", resultSize/19+1)[:resultSize]}},
			},
		)
	}
	return msgs
}

var sizerTestTools = []provider.ToolSpec{
	{Name: "file-read", Description: "Read a file.", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
	{Name: "bash", Description: "Run a shell command — carefully.", Schema: json.RawMessage(`{"type":"object"}`)},
}

func fullEstimate(system string, msgs []provider.Message, tools []provider.ToolSpec) int {
	return provider.EstimateRequestTokens(provider.Request{System: system, Messages: msgs, Tools: tools})
}

// The sizer must always equal the full count it replaces: the compaction
// trigger, the input budget and the usage calibration all depend on the
// exact value. The history is mutated every way the run loop (and
// compaction) can: appends, replaced and truncated messages, in-place edits
// of a tool output, replaced tool-call arguments (never edited in place; see
// promptSizer), new tools, a new system prompt.
func TestPromptSizerMatchesFullEstimate(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var s promptSizer
	system := "You are a coding agent. ✓"
	tools := sizerTestTools
	msgs := sizerTestHistory(40, 300)
	check := func(step int, op string) {
		t.Helper()
		if got, want := s.requestTokens(system, msgs, tools), fullEstimate(system, msgs, tools); got != want {
			t.Fatalf("step %d after %s: sizer = %d, full estimate = %d", step, op, got, want)
		}
	}
	check(0, "initial")
	for step := 1; step <= 3000; step++ {
		op := ""
		switch rng.Intn(10) {
		case 0, 1, 2:
			op = "append"
			msgs = append(msgs, sizerTestHistory(3, rng.Intn(4000))[1:]...)
		case 3:
			op = "replace message"
			i := rng.Intn(len(msgs))
			msgs[i] = provider.Message{Role: provider.RoleUser, Content: strings.Repeat("é", rng.Intn(50))}
		case 4:
			op = "truncate"
			msgs = msgs[:1+rng.Intn(len(msgs))]
		case 5:
			op = "edit tool output in place"
			for i := range msgs {
				if len(msgs[i].ToolResults) > 0 && rng.Intn(3) == 0 {
					msgs[i].ToolResults[0].Output = "[elided]"
				}
			}
		case 6:
			op = "replace argument bytes"
			for i := range msgs {
				if len(msgs[i].ToolCalls) > 0 && len(msgs[i].ToolCalls[0].Args) > 3 {
					// Same length, one rune fewer: "pa" <-> "é". The
					// message keeps its position; only the slice changes.
					args := bytes.Clone(msgs[i].ToolCalls[0].Args)
					if args[2] == 'p' {
						args[2], args[3] = 0xC3, 0xA9
					} else {
						args[2], args[3] = 'p', 'a'
					}
					calls := slices.Clone(msgs[i].ToolCalls)
					calls[0].Args = args
					msgs[i].ToolCalls = calls
					break
				}
			}
		case 7:
			op = "new tools"
			tools = append(tools[:len(tools):len(tools)], provider.ToolSpec{Name: fmt.Sprint("t", step), Description: "d", Schema: json.RawMessage(`{}`)})
		case 8:
			op = "new system prompt"
			system = fmt.Sprintf("system %d ✓", step)
		case 9:
			op = "compaction-shaped rebuild"
			keep := min(len(msgs), 1+rng.Intn(6))
			rebuilt := append([]provider.Message{msgs[0], {Role: provider.RoleUser, Content: "summary"}}, msgs[len(msgs)-keep:]...)
			msgs = rebuilt
		}
		check(step, op)
		check(step, op+" (unchanged re-measure)")
	}
	if got, want := s.requestTokens("", nil, nil), fullEstimate("", nil, nil); got != want {
		t.Fatalf("empty request: sizer = %d, full estimate = %d", got, want)
	}
}

// Re-measuring an unchanged 500-message history allocates nothing: the run
// loop does it several times per step.
func TestPromptSizerSteadyStateAllocs(t *testing.T) {
	msgs := sizerTestHistory(500, 1500)
	var s promptSizer
	s.requestTokens("system", msgs, sizerTestTools)
	if allocs := testing.AllocsPerRun(20, func() { s.requestTokens("system", msgs, sizerTestTools) }); allocs != 0 {
		t.Fatalf("steady-state requestTokens allocates %.0f times, want 0", allocs)
	}
}

// writeHeavyHistory is n file-write calls, each carrying size bytes of
// content in its arguments, with short results.
func writeHeavyHistory(n, size int) []provider.Message {
	msgs := []provider.Message{{Role: provider.RoleUser, Content: "Task:\nwrite the files"}}
	body := strings.Repeat("x", size)
	for i := range n {
		id := fmt.Sprintf("call_%d", i)
		args, _ := json.Marshal(map[string]string{"path": fmt.Sprintf("f%d.go", i), "content": body})
		msgs = append(msgs,
			provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.NativeTool{{ID: id, Name: "file-write", Args: args}}},
			provider.Message{Role: provider.RoleUser, ToolResults: []provider.ToolResult{{ID: id, Name: "file-write", Output: "wrote f.go"}}},
		)
	}
	return msgs
}

// The sizer keeps no copy of the tool-call arguments it counted: a history
// heavy with file-write content is held once, by the conversation.
func TestPromptSizerRetainsNoArgumentCopies(t *testing.T) {
	msgs := writeHeavyHistory(50, 20<<10) // 1 MB of arguments
	var s promptSizer
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s.requestTokens("system", msgs, sizerTestTools)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<10 {
		t.Fatalf("first measurement of 1 MB of arguments allocated %d bytes, want no copies", allocated)
	}
}
