package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPhaseEventsCarryDetailAndDynamic(t *testing.T) {
	events := &eventLog{}
	script := "export const meta = {name: 'p', description: 'phases', phases: [{title: 'Scan', detail: 'declared detail'}]}\n" + `
		phase('Scan')
		phase('Scan again', {detail: 'added at runtime'})
		phase('Third', 'string detail')
		phase('Scan', {detail: 'override'})
	`
	if _, err := Run(context.Background(), script, Options{Runner: echoRunner(0), Observer: events.observe}); err != nil {
		t.Fatal(err)
	}
	got := events.ofKind(EventPhase)
	want := []struct {
		title, detail string
		dynamic       bool
	}{
		{"Scan", "declared detail", false},
		{"Scan again", "added at runtime", true},
		{"Third", "string detail", true},
		{"Scan", "override", false},
	}
	if len(got) != len(want) {
		t.Fatalf("phase events = %+v", got)
	}
	for i, w := range want {
		if got[i].Phase != w.title || got[i].Detail != w.detail || got[i].Dynamic != w.dynamic {
			t.Fatalf("phase event %d = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestInlineSubWorkflow(t *testing.T) {
	res := run(t, `
		const source = "export const meta = {name: 'generated', description: 'made at runtime'}\n" +
			"return 'gen:' + (await agent(args.task))"
		const a = await workflow({script: source, args: {task: 'inline'}})
		const b = await workflow({script: source}, {task: 'second-arg wins'})
		return [a, b]
	`, Options{})
	if fmt.Sprint(res.Value) != "[gen:done:inline gen:done:second-arg wins]" {
		t.Fatalf("value = %#v", res.Value)
	}
	if res.Agents != 2 {
		t.Fatalf("agents = %d", res.Agents)
	}
}

// A generated stage that does not validate rejects with the reason, so the
// script can catch it and ask for a fixed one.
func TestInlineSubWorkflowValidationRejects(t *testing.T) {
	events := &eventLog{}
	res := run(t, `
		const out = []
		try {
			await workflow({script: 'return 1'})
		} catch (e) { out.push('no header: ' + e.message) }
		out.push(await workflow({script: "export const meta = {name: 'x', description: 'y'}\nreturn ((("})
			.catch(e => 'syntax: ' + e.message))
		out.push(await workflow({script: "export const meta = {name: String('x'), description: 'y'}\nreturn 1"})
			.catch(e => 'computed meta: ' + e.message))
		return out
	`, Options{Observer: events.observe})
	list, _ := res.Value.([]any)
	if len(list) != 3 {
		t.Fatalf("value = %#v", res.Value)
	}
	for _, v := range list {
		if !strings.Contains(v.(string), "the generated script is invalid") {
			t.Fatalf("rejection = %q", v)
		}
	}
	for _, ev := range events.ofKind(EventStart) {
		if ev.Nested {
			t.Fatal("an invalid generated script must not start")
		}
	}
}

func TestSubWorkflowReferenceForms(t *testing.T) {
	res := run(t, `return await workflow({name: 'child', args: {x: 'named'}})`, Options{
		Resolve: func(name string) (string, error) {
			return "export const meta = {name: 'child', description: 'c'}\nreturn args.x", nil
		},
	})
	if res.Value != "named" {
		t.Fatalf("value = %#v", res.Value)
	}
	_, err := Run(context.Background(), header(`await workflow({nothing: 1})`), Options{Runner: echoRunner(0)})
	if err == nil || !strings.Contains(err.Error(), "needs a script, a scriptPath or a name") {
		t.Fatalf("err = %v", err)
	}
}

func planRunner(n int) *fakeRunner {
	return &fakeRunner{fn: func(req Request) (Response, error) {
		if strings.Contains(req.Prompt, "work-list") {
			var tasks []map[string]any
			for i := range n {
				tasks = append(tasks, map[string]any{"label": fmt.Sprintf("t%d", i), "prompt": fmt.Sprintf("do %d", i), "phase": "Do"})
			}
			tasks = append(tasks, map[string]any{"label": "broken"}) // no prompt: filtered out
			out, _ := json.Marshal(map[string]any{"tasks": tasks})
			return Response{Text: string(out)}, nil
		}
		return Response{Text: "done:" + req.Prompt}, nil
	}}
}

func TestPlanCapsAtTheFanoutAndSaysSo(t *testing.T) {
	runner := planRunner(10)
	res := run(t, `
		const tasks = await plan('find the modules', {label: 'scout', agentType: 'explore'})
		return tasks.map(t => t.label + ':' + t.prompt + ':' + t.phase)
	`, Options{Runner: runner, SizeTier: SizeSmall})
	if fmt.Sprint(res.Value) != "[t0:do 0:Do t1:do 1:Do t2:do 2:Do]" {
		t.Fatalf("value = %#v — small's fanout is 3", res.Value)
	}
	if len(res.Logs) != 1 || !strings.Contains(res.Logs[0], "kept 3 of 10 tasks (7 dropped") {
		t.Fatalf("logs = %v — a cap that bites must be logged", res.Logs)
	}
	call := runner.calls[0]
	if call.Label != "scout" || call.AgentType != "explore" || call.Schema == nil {
		t.Fatalf("planner request = %+v", call)
	}

	res = run(t, `return (await plan('x', {max: 5})).length`, Options{Runner: planRunner(10)})
	if res.Value != int64(5) {
		t.Fatalf("explicit max: %#v", res.Value)
	}
	res = run(t, `return (await plan('x')).length`, Options{Runner: planRunner(4)})
	if res.Value != int64(4) || len(res.Logs) != 0 {
		t.Fatalf("under the cap: %#v, logs %v", res.Value, res.Logs)
	}
}

func TestPlanReturnsEmptyWhenThePlannerFails(t *testing.T) {
	runner := &fakeRunner{fn: func(Request) (Response, error) { return Response{}, fmt.Errorf("down") }}
	res := run(t, `return (await plan('x')).length`, Options{Runner: runner})
	if res.Value != int64(0) {
		t.Fatalf("value = %#v", res.Value)
	}
}

func TestUntilDryStopsWhenNothingNewTurnsUp(t *testing.T) {
	res := run(t, `
		const rounds = [['a', 'b'], ['b', 'c', null], ['a'], ['c'], ['d']]
		const seenSizes = []
		const found = await untilDry((i, seen) => {
			seenSizes.push(seen.length)
			return Promise.resolve(rounds[i])
		})
		return {found, seenSizes}
	`, Options{})
	out := res.Value.(map[string]any)
	if fmt.Sprint(out["found"]) != "[a b c]" {
		t.Fatalf("found = %v — two dry rounds must stop the loop before 'd'", out["found"])
	}
	if fmt.Sprint(out["seenSizes"]) != "[0 2 3 3]" {
		t.Fatalf("seen sizes = %v", out["seenSizes"])
	}
	if len(res.Logs) != 5 || !strings.Contains(res.Logs[4], "dry after 4 rounds") {
		t.Fatalf("logs = %v", res.Logs)
	}
}

func TestUntilDryOptions(t *testing.T) {
	// A custom key dedupes objects by identity field; dry: 1 stops on the
	// first round with nothing fresh.
	res := run(t, `
		const rounds = [[{id: 1, v: 'x'}], [{id: 1, v: 'other text'}], [{id: 2}]]
		return (await untilDry(i => rounds[i], {key: f => String(f.id), dry: 1})).map(f => f.id)
	`, Options{})
	if fmt.Sprint(res.Value) != "[1]" {
		t.Fatalf("value = %v", res.Value)
	}
	// maxRounds caps a loop that never dries up, and says so.
	res = run(t, `return (await untilDry(i => ['item' + i], {maxRounds: 3})).length`, Options{})
	if res.Value != int64(3) || !strings.Contains(res.Logs[len(res.Logs)-1], "cap of 3 rounds") {
		t.Fatalf("value = %#v, logs = %v", res.Value, res.Logs)
	}
	// Agents in the rounds, with the budget stopping the loop.
	res = run(t, `
		return (await untilDry(async i => [await agent('find ' + i)], {maxRounds: 50})).length
	`, Options{BudgetTokens: 30})
	if res.Value != int64(3) || !strings.Contains(strings.Join(res.Logs, "\n"), "token budget is spent") {
		t.Fatalf("value = %#v, logs = %v", res.Value, res.Logs)
	}
	_, err := Run(context.Background(), header(`await untilDry(() => 'nope')`), Options{Runner: echoRunner(0)})
	if err == nil || !strings.Contains(err.Error(), "not an array") {
		t.Fatalf("err = %v", err)
	}
}

func TestSizeGlobalPerTier(t *testing.T) {
	cases := []struct {
		tier string
		want string
	}{
		{SizeSmall, "small 5 3 1 4"},
		{SizeMedium, "medium 10 6 1 9"},
		{"", "medium 10 6 1 9"},
		{"bogus", "medium 10 6 1 9"},
		{SizeLarge, "large 30 16 1 29"},
		{SizeUnbounded, "unbounded Infinity 64 1 Infinity"},
	}
	for _, c := range cases {
		t.Run(c.tier, func(t *testing.T) {
			res := run(t, `
				await agent('one')
				return [size.tier, size.agents, size.fanout, size.spawned(), size.remaining()].join(' ')
			`, Options{SizeTier: c.tier})
			if res.Value != c.want {
				t.Fatalf("size = %q, want %q", res.Value, c.want)
			}
		})
	}
	res := run(t, `return size.agents + ' ' + size.remaining()`, Options{SizeTier: SizeSmall, SizeAgents: 7})
	if res.Value != "7 7" {
		t.Fatalf("SizeAgents override: %q", res.Value)
	}
}

func TestSizeGuidelineIsLoggedOnceAndNeverEnforced(t *testing.T) {
	events := &eventLog{}
	res := run(t, `
		for (let i = 0; i < 8; i++) await agent('w' + i)
		return size.remaining()
	`, Options{SizeTier: SizeSmall, Observer: events.observe})
	if res.Agents != 8 || res.Value != int64(0) {
		t.Fatalf("agents = %d, remaining = %#v — the guideline must not stop the run", res.Agents, res.Value)
	}
	want := "size guideline (small: ~5 agents) exceeded"
	if len(res.Logs) != 1 || res.Logs[0] != want {
		t.Fatalf("logs = %v, want exactly one %q", res.Logs, want)
	}
	if logs := events.ofKind(EventLog); len(logs) != 1 {
		t.Fatalf("log events = %+v", logs)
	}
	// Unbounded never logs.
	res = run(t, `for (let i = 0; i < 70; i++) await agent('w' + i)`, Options{SizeTier: SizeUnbounded})
	if len(res.Logs) != 0 {
		t.Fatalf("unbounded logged %v", res.Logs)
	}
}

func TestSmallTierLowersDefaultConcurrency(t *testing.T) {
	if got := (Options{SizeTier: SizeSmall}).withDefaults().MaxConcurrency; got != 4 {
		t.Fatalf("small concurrency = %d, want 4", got)
	}
	if got := (Options{SizeTier: SizeSmall, MaxConcurrency: 9}).withDefaults().MaxConcurrency; got != 9 {
		t.Fatalf("an explicit max_concurrency must win, got %d", got)
	}
	if ResolveSize(" LARGE ").Tier != SizeLarge || ResolveSize("").Tier != SizeMedium {
		t.Fatal("tier names must resolve case-insensitively, empty to medium")
	}
	for _, name := range SizeTierNames {
		if SizeTiers[name].Tier != name {
			t.Fatalf("SizeTiers[%q] = %+v", name, SizeTiers[name])
		}
	}
}

const paramsHeader = `export const meta = {
  name: 'tmpl',
  description: 'a template',
  phases: [{title: 'Work'}],
  params: {
    target: {type: 'string', description: 'what to review', required: true},
    base: {type: 'string', default: 'main'},
    depth: {type: 'number', default: 2},
    focus: 'optional focus',
    tags: {type: 'array'},
  },
}
`

func TestParamsParseInDeclarationOrder(t *testing.T) {
	m, err := ParseMeta(paramsHeader)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range m.Params {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "target,base,depth,focus,tags" {
		t.Fatalf("order = %v", names)
	}
	if p := m.Params[0]; p.Type != "string" || !p.Required || p.Description != "what to review" {
		t.Fatalf("target = %+v", p)
	}
	if p := m.Params[2]; p.Type != "number" || p.Default != int64(2) {
		t.Fatalf("depth = %+v", p)
	}
	if p := m.Params[3]; p.Type != "string" || p.Required || p.Description != "optional focus" {
		t.Fatalf("shorthand = %+v", p)
	}
	if encoded, _ := json.Marshal(m); !strings.Contains(string(encoded), `"params":[{"name":"target"`) {
		t.Fatalf("meta.json form = %s", encoded)
	}
}

func TestParamsDefaultsAndValidation(t *testing.T) {
	body := `return [args.target, args.base, args.depth, args.focus === undefined, args.extra].join('|')`
	runner := echoRunner(0)
	cases := []struct {
		name    string
		args    any
		want    string
		wantErr string
	}{
		{name: "defaults fill in", args: map[string]any{"target": "auth", "extra": "kept"}, want: "auth|main|2|true|kept"},
		{name: "explicit values win", args: map[string]any{"target": "auth", "base": "dev", "depth": float64(5)}, want: "auth|dev|5|true|"},
		{name: "bare value binds to the sole required param", args: "auth", want: "auth|main|2|true|"},
		{name: "missing required", args: nil, wantErr: `workflow "tmpl": missing required param(s): target — pass them in the tool call's args`},
		{name: "null required", args: map[string]any{"target": nil}, wantErr: "missing required param(s): target"},
		{name: "wrong type", args: map[string]any{"target": float64(3), "tags": "x"}, wantErr: "param(s) of the wrong type: target (want string, got number), tags (want array, got string)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := Run(context.Background(), paramsHeader+body, Options{Runner: runner, Args: c.args})
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				if res.Meta.Name != "tmpl" {
					t.Fatalf("a param error must still report the meta, got %+v", res.Meta)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Value != c.want {
				t.Fatalf("value = %#v, want %q", res.Value, c.want)
			}
		})
	}
	if runner.count() != 0 {
		t.Fatal("no agent should have run")
	}
}

// A param error fails the run before a single agent is paid for.
func TestParamErrorsFailBeforeAnyAgent(t *testing.T) {
	runner := echoRunner(0)
	_, err := Run(context.Background(), paramsHeader+`await agent('expensive')`, Options{Runner: runner})
	if err == nil || runner.count() != 0 {
		t.Fatalf("err = %v, calls = %d", err, runner.count())
	}
}

func TestParamsAmbiguousBareValue(t *testing.T) {
	script := `export const meta = {name: 'two', description: 'd', params: {a: {required: true}, b: {required: true}}}
return 1`
	_, err := Run(context.Background(), script, Options{Runner: echoRunner(0), Args: "bare"})
	if err == nil || !strings.Contains(err.Error(), "args must be an object with the declared params (a, b)") {
		t.Fatalf("err = %v", err)
	}
	// One optional param and nothing required: the bare value is unambiguous.
	one := "export const meta = {name: 'one', description: 'd', params: {subject: 'what to explain'}}\nreturn args.subject"
	res, err := Run(context.Background(), one, Options{Runner: echoRunner(0), Args: "the engine"})
	if err != nil || res.Value != "the engine" {
		t.Fatalf("value = %#v, err = %v", res.Value, err)
	}
}

func TestNestedParamErrorsNameTheWorkflowCall(t *testing.T) {
	child := `export const meta = {name: 'child', description: 'c', params: {x: {type: 'string', required: true}}}
return args.x`
	res := run(t, `return await workflow({script: `+"`"+child+"`"+`}).catch(e => e.message)`, Options{})
	if s, _ := res.Value.(string); !strings.Contains(s, "missing required param(s): x — pass them in the workflow() call's args") {
		t.Fatalf("value = %#v", res.Value)
	}
}

func TestScriptsWithoutParamsGetArgsUntouched(t *testing.T) {
	res := run(t, `return typeof args + ':' + args`, Options{Args: "plain string"})
	if res.Value != "string:plain string" {
		t.Fatalf("value = %#v", res.Value)
	}
	res = run(t, `return typeof args`, Options{})
	if res.Value != "undefined" {
		t.Fatalf("value = %#v", res.Value)
	}
}

func TestParseMetaRejectsBadParams(t *testing.T) {
	cases := map[string]string{
		"array":            `params: ['a']`,
		"unknown type":     `params: {a: {type: 'date'}}`,
		"bad required":     `params: {a: {required: 'yes'}}`,
		"default mismatch": `params: {a: {type: 'number', default: 'two'}}`,
		"number shorthand": `params: {a: 3}`,
	}
	for label, field := range cases {
		if _, err := ParseMeta("export const meta = {name: 'x', description: 'y', " + field + "}"); err == nil {
			t.Fatalf("%s: expected rejection", label)
		}
	}
}

// The header runs in a runtime with no globals at all — the builtins are
// non-enumerable, so deleting only enumerable keys used to leave every one of
// them reachable.
func TestParseMetaHasNoBuiltins(t *testing.T) {
	cases := map[string]string{
		"String":     `export const meta = {name: String('x'), description: 'y'}`,
		"JSON.parse": `export const meta = {name: JSON.parse('"x"'), description: 'y'}`,
		"Math":       `export const meta = {name: 'x', description: 'y', n: Math.max(1, 2)}`,
		"globalThis": `export const meta = {name: 'x', description: 'y', g: globalThis.Object}`,
	}
	for label, script := range cases {
		if _, err := ParseMeta(script); err == nil || !strings.Contains(err.Error(), "pure object literal") {
			t.Fatalf("%s: err = %v, want a pure-literal rejection", label, err)
		}
	}
}

// Headers that would loop, or backtrack for ever inside a regular expression
// (which the watchdog's interrupt cannot stop: regexp2 runs in native Go), are
// rejected by the literal check before anything is evaluated.
func TestParseMetaStopsLoopingHeaders(t *testing.T) {
	cases := map[string]string{
		"loop in an IIFE":    `export const meta = {name: (()=>{for(;;);})(), description: 'y'}`,
		"loop in a getter":   `export const meta = {name: 'x', description: 'y', get trap() { for(;;); }}`,
		"getter in params":   `export const meta = {name: 'x', description: 'y', params: {get p() { for(;;); }}}`,
		"backtracking regex": `export const meta = {name: /^(a+)+(?=b)/.test('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaac') ? 'x' : 'y', description: 'y'}`,
		"regex in an array":  `export const meta = {name: 'x', description: 'y', p: [/^(a+)+(?=b)/]}`,
	}
	for label, script := range cases {
		t.Run(label, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := ParseMeta(script)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "pure object literal") {
					t.Fatalf("err = %v", err)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("ParseMeta evaluated a header that is not a literal")
			}
		})
	}
}

// Every form of code is turned away by the literal check, before evaluation.
func TestParseMetaRejectsCode(t *testing.T) {
	cases := map[string]string{
		"shorthand":         `{name: 'x', description: 'y', z}`,
		"spread":            `{name: 'x', description: 'y', ...{a: 1}}`,
		"computed key":      `{name: 'x', description: 'y', ['k']: 1}`,
		"method":            `{name: 'x', description: 'y', m() {}}`,
		"setter":            `{name: 'x', description: 'y', set s(v) {}}`,
		"member access":     `{name: 'x'.constructor.name, description: 'y'}`,
		"conditional":       `{name: true ? 'x' : 'y', description: 'y'}`,
		"tagged template":   "{name: String.raw`x`, description: 'y'}",
		"other operator":    `{name: 'x', description: 'y', n: 2 * 3}`,
		"typeof":            `{name: 'x', description: 'y', n: typeof 1}`,
		"negated string":    `{name: 'x', description: 'y', n: -'1'}`,
		"new":               `{name: 'x', description: 'y', n: new Array(3)}`,
		"sequence":          `{name: ('a', 'x'), description: 'y'}`,
		"assignment":        `{name: x = 'x', description: 'y'}`,
		"function value":    `{name: 'x', description: 'y', f: function () {}}`,
		"concat with a var": `{name: 'x' + suffix, description: 'y'}`,
	}
	for label, literal := range cases {
		_, err := ParseMeta("export const meta = " + literal)
		if err == nil || !strings.Contains(err.Error(), "pure object literal") {
			t.Fatalf("%s: err = %v, want a pure-literal rejection", label, err)
		}
	}
}

// Literal-only headers that existing scripts use keep parsing.
func TestParseMetaStillAcceptsLiterals(t *testing.T) {
	m, err := ParseMeta("export const meta = {name: 'x', description: 'multi ' + 'line', n: -1, t: `plain`, u: undefined, f: NaN}")
	if err != nil || m.Description != "multi line" {
		t.Fatalf("meta = %+v, err = %v", m, err)
	}
	m, err = ParseMeta("export const meta = {'name': \"x\", 3: +2.5, description: `a` + 'b' + \"c\", i: -Infinity, z: null, b: [true, false, , {}], " +
		"params: {n: {type: 'number', default: -1}, s: 'a string'}}")
	if err != nil || m.Name != "x" || m.Description != "abc" || len(m.Params) != 2 || m.Params[0].Default != int64(-1) {
		t.Fatalf("meta = %+v, err = %v", m, err)
	}
}

// TestDroppedBranchesAreLogged: parallel() and pipeline() keep turning a
// throwing branch into null, but say so — with the error and the script line —
// instead of leaving the orchestrator to guess why a result is missing.
func TestDroppedBranchesAreLogged(t *testing.T) {
	runner := &fakeRunner{fn: func(req Request) (Response, error) { return Response{Text: "ok"}, nil }}
	script := "export const meta = {name:'d', description:'d'}\n" + `
const a = await parallel([() => agent('x'), () => { throw new Error('boom') }])
const b = await pipeline([1, 2], v => v, (v) => { if (v === 2) { return undefined.findings } return v })
return [a[1], b[0], b[1]]
`
	res, err := Run(context.Background(), script, Options{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	logs := strings.Join(res.Logs, "\n")
	for _, want := range []string{"parallel(): item 1 dropped to null: Error: boom", "pipeline(): item 1 at stage 2 dropped to null: TypeError", "d.workflow.js:"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
	got, _ := res.Value.([]any)
	if len(got) != 3 || got[0] != nil || got[1] != int64(1) || got[2] != nil {
		t.Errorf("value = %#v, want [nil 1 nil]", res.Value)
	}
}

// TestSyntaxErrorQuotesTheLine: a script that does not parse reports the
// offending line with a marker, and the nested-backtick hint when the line
// has the tell-tale extra backticks.
func TestSyntaxErrorQuotesTheLine(t *testing.T) {
	script := "export const meta = {name:'s', description:'d'}\n" +
		"const x = 1\n" +
		"const p = await agent(`run ` + \"`go run`\" + ` now`)\n" +
		"const q = agent(`then `go vet` please`)\n"
	_, err := Validate(script)
	if err == nil {
		t.Fatal("expected a syntax error")
	}
	msg := err.Error()
	for _, want := range []string{"line 4:", "go vet", "^", "hint: this line has more than two backticks"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q:\n%s", want, msg)
		}
	}
}
