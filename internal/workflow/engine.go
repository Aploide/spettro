package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
)

// Request is one agent() call, fully resolved: the engine has already applied
// the schema contract and named the instance, so a Runner only has to execute
// the prompt and hand back the final message.
type Request struct {
	Prompt    string
	Label     string
	Phase     string
	AgentType string
	Model     string
	Effort    string
	Isolation string
	Schema    json.RawMessage
	// Index is the 1-based global agent counter across the whole run,
	// including nested workflows.
	Index int
	// Instance is the display identity of this call ("review#3").
	Instance string
}

// Response is what a Runner returns for one agent call.
type Response struct {
	Text   string
	Tokens int
}

// Runner executes a single sub-agent. Implementations must be safe to call
// from many goroutines at once: the engine dispatches concurrent agent() calls
// directly against it, bounded only by MaxConcurrency.
type Runner interface {
	RunAgent(ctx context.Context, req Request) (Response, error)
}

// CallScoper lets a Runner hold state for the whole of one agent() call rather
// than for one attempt at it.
//
// The engine may call RunAgent several times for a single agent() — a
// schema-carrying call is retried until its answer parses — and a Runner that
// sets up per-call resources must not redo that work per attempt. Spettro's
// worktree isolation is the case that forced this: creating a fresh worktree
// on every attempt left two branches carrying overlapping edits for one call,
// and merging both back collided.
//
// BeginCall runs once before the first attempt; EndCall runs once after the
// last, with the error the call ended on (nil on success).
type CallScoper interface {
	BeginCall(ctx context.Context, req Request) error
	EndCall(ctx context.Context, req Request, runErr error)
}

// Options configures one workflow run.
type Options struct {
	Runner   Runner
	Observer Observer
	// MaxConcurrency bounds simultaneous agents across the run (including
	// nested workflows). 0 → min(16, NumCPU-2).
	MaxConcurrency int
	// MaxAgents is a runaway-loop backstop on the total number of agents a run
	// may ever start. 0 → 1000.
	MaxAgents int
	// MaxItems caps a single parallel()/pipeline() call. 0 → 4096.
	MaxItems int
	// BudgetTokens is the run's token target, exposed to the script as
	// budget.total. 0 → no target (budget.total is null, remaining() is
	// Infinity). Once reached, further agent() calls throw.
	BudgetTokens int
	// SchemaAttempts is how many times a schema-carrying call is retried when
	// the agent's answer does not parse. 0 → 3.
	SchemaAttempts int
	// Journal, when set, records every call for resume and stores artifacts.
	Journal *Journal
	// Resolve maps a saved workflow name to its script source, backing
	// workflow('name') calls. nil → named sub-workflows are unavailable.
	Resolve func(name string) (string, error)
	// Args is the value exposed to the script as the `args` global.
	Args any
	// DefaultAgentType names the agent an agent() call runs as when the script
	// does not pick one. It only affects display: instance names read
	// "general-purpose#7" rather than "agent#7", which matters because those
	// names are how a user tells concurrent members apart.
	DefaultAgentType string
	// Checkpoints makes checkpoint() pause the run for the orchestrator: Next
	// returns the checkpoint and Resume answers it. A host without an
	// orchestrator in the loop leaves it false, and checkpoint() then resolves
	// immediately to null with a "checkpoint skipped" log line. Run always
	// runs with it off.
	Checkpoints bool
	// AutoCheckpoint (with Checkpoints) also pauses at every phase boundary:
	// a phase() call entering a phase not seen before, once at least one agent
	// has run, raises a checkpoint with Auto set and data {finished_phase,
	// next_phase, agents, failed}. The script does not see it; the reply is
	// the host's to interpret (a {stop:true} reply means the host stops the
	// run).
	AutoCheckpoint bool
	// SizeTier names the run's size guideline (see SizeTiers); empty or
	// unknown means medium. It is exposed to the script as the size global.
	SizeTier string
	// SizeAgents overrides the tier's agent guideline when positive.
	SizeAgents int
}

func (o Options) withDefaults() Options {
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = ResolveSize(o.SizeTier).Concurrency
	}
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = min(16, max(1, runtime.NumCPU()-2))
	}
	if o.MaxAgents <= 0 {
		o.MaxAgents = 1000
	}
	if o.MaxItems <= 0 {
		o.MaxItems = 4096
	}
	if o.SchemaAttempts <= 0 {
		o.SchemaAttempts = 3
	}
	return o
}

// Result is the outcome of a completed run.
type Result struct {
	Meta Meta
	// Value is whatever the script returned, exported to Go types.
	Value any
	// Agents, Failed and Cached count agent() calls started, that failed, and
	// that were replayed from a previous run's journal.
	Agents int
	Failed int
	Cached int
	Tokens int
	// Logs are the script's log() lines, in order.
	Logs []string
	// Phases are the phase() titles in the order they were entered.
	Phases []string
}

// Validate parses a script's header and compiles its body without running
// anything. Hosts use it to reject a broken saved workflow at listing time
// rather than after the first agent has already been paid for.
func Validate(script string) (Meta, error) {
	meta, err := ParseMeta(script)
	if err != nil {
		return Meta{}, err
	}
	if _, err := compileScript(meta.Name, script); err != nil {
		return meta, err
	}
	return meta, nil
}

// compileScript wraps the body in an async IIFE so scripts get top-level
// await and a top-level return. The opening brace shares line 1 with the
// script's first line, so reported error line numbers match what the author
// wrote.
func compileScript(name, script string) (*goja.Program, error) {
	prog, err := goja.Compile(name+".workflow.js", "(async function(){"+stripMetaExport(script)+"\n})()", true)
	if err != nil {
		return nil, fmt.Errorf("%w%s", err, syntaxContext(script, err))
	}
	return prog, nil
}

// syntaxErrLineRe pulls the position out of goja's "Line 99:215 …" message.
var syntaxErrLineRe = regexp.MustCompile(`Line (\d+):(\d+)`)

// syntaxContext quotes the line a syntax error points at, and names the
// commonest cause when it fits.
//
// A position alone is not enough for the model that wrote the script: a live
// run spent several turns counting columns on a 300-character prompt line to
// find that a backtick quoted inside a template literal had closed it. Seeing
// the line, with a marker at the column, makes that a one-step fix.
func syntaxContext(script string, err error) string {
	m := syntaxErrLineRe.FindStringSubmatch(err.Error())
	if m == nil {
		return ""
	}
	lineNo, _ := strconv.Atoi(m[1])
	col, _ := strconv.Atoi(m[2])
	lines := strings.Split(script, "\n")
	if lineNo < 1 || lineNo > len(lines) {
		return ""
	}
	line := lines[lineNo-1]
	if lineNo == 1 {
		col -= len("(async function(){")
	}
	// Show a window around the column so a long prompt line stays readable.
	start := max(0, min(col-1, len(line))-60)
	end := min(len(line), start+120)
	snippet := line[start:end]
	caret := max(0, min(col-1, len(line))-start)
	out := fmt.Sprintf("\n  line %d: %s\n  %s^", lineNo, snippet, strings.Repeat(" ", len(fmt.Sprintf("line %d: ", lineNo))+caret))
	if strings.Count(line, "`") > 2 {
		out += "\n  hint: this line has more than two backticks — a backtick inside a template literal ends it; escape it as \\` or quote with ' instead"
	}
	return out
}

// Run executes a workflow script to completion.
//
// It is the non-interactive host: checkpoints are off (checkpoint() resolves
// to null and logs that it was skipped), so the run never pauses and Run
// returns only once the script settles.
func Run(ctx context.Context, script string, opts Options) (Result, error) {
	opts.Checkpoints = false
	opts.AutoCheckpoint = false
	h, err := Start(ctx, script, opts)
	if err != nil {
		return Result{}, err
	}
	for {
		// Next cannot fail on a background context, and with checkpoints off
		// it never returns one; answering null keeps Run total regardless.
		step, err := h.Next(context.Background())
		if err != nil {
			return h.Snapshot(), err
		}
		if step.Result != nil {
			return *step.Result, step.Err
		}
		_ = h.Resume(step.Checkpoint.ID, nil)
	}
}

// shared is the state every script in a run — the top-level one and any
// workflow() children — contends on: the concurrency slot pool, the agent
// counter, the token budget, the journal, and the checkpoint queue.
type shared struct {
	opts   Options
	size   Size
	sem    chan struct{}
	agents atomic.Int64
	failed atomic.Int64
	tokens atomic.Int64
	cpSeq  atomic.Int64
	// sizeWarned makes the size-guideline note a one-off: the first agent
	// past the guideline is news, the fortieth is noise.
	sizeWarned atomic.Bool

	mu     sync.Mutex
	logs   []string
	phases []string
	meta   Meta

	// Checkpoint state; see checkpoint.go.
	queue    []*pendingCheckpoint
	surfaced *pendingCheckpoint
	active   int
	wake     chan struct{}
}

func newShared(opts Options) *shared {
	size := ResolveSize(opts.SizeTier)
	if opts.SizeAgents > 0 {
		size.Agents = opts.SizeAgents
	}
	return &shared{
		opts: opts,
		size: size,
		sem:  make(chan struct{}, opts.MaxConcurrency),
		wake: make(chan struct{}),
	}
}

// snapshot is the run's Result so far, without a Value.
func (s *shared) snapshot() Result {
	s.mu.Lock()
	meta := s.meta
	s.mu.Unlock()
	return Result{
		Meta:   meta,
		Agents: int(s.agents.Load()),
		Failed: int(s.failed.Load()),
		Cached: s.opts.Journal.Hits(),
		Tokens: int(s.tokens.Load()),
		Logs:   s.snapshotLogs(),
		Phases: s.snapshotPhases(),
	}
}

func (s *shared) snapshotLogs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.logs...)
}

func (s *shared) snapshotPhases() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.phases...)
}

func (s *shared) addLog(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.logs) < 500 {
		s.logs = append(s.logs, line)
	}
}

// addPhase records a phase title and reports whether it is new to the run.
func (s *shared) addPhase(title string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.phases {
		if p == title {
			return false
		}
	}
	s.phases = append(s.phases, title)
	return true
}

func (s *shared) emit(ev Event) {
	if s.opts.Observer == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	s.opts.Observer(ev)
}

func (s *shared) remaining() int64 {
	if s.opts.BudgetTokens <= 0 {
		return -1 // unlimited
	}
	return max(int64(s.opts.BudgetTokens)-s.tokens.Load(), 0)
}

// vmRun is one script instance: a goja runtime, its job queue, and the phase
// the script is currently in.
type vmRun struct {
	sh     *shared
	vm     *goja.Runtime
	jobs   chan func()
	done   chan struct{}
	depth  int
	nested bool
	// syncStart is the nanotime at which the current stretch of synchronous
	// JavaScript began, or 0 when the engine is not inside the runtime. The
	// watchdog reads it to tell "waiting on agents" from "looping".
	syncStart atomic.Int64

	// meta is this script's own header: phase() reads its declared phases.
	meta Meta
	// jsonParse is the runtime's JSON.parse, captured before the script runs
	// so a script reassigning JSON cannot break checkpoint replies.
	jsonParse goja.Callable

	mu       sync.Mutex
	phase    string
	inflight atomic.Int64
}

// post hands a closure to the script goroutine. Everything that touches the
// goja runtime — resolving a promise, most of all — must go through here:
// a goja.Runtime is not safe to touch from two goroutines, and the loop below
// is the single one allowed to.
func (r *vmRun) post(ctx context.Context, fn func()) {
	select {
	case r.jobs <- fn:
	case <-r.done:
	case <-ctx.Done():
	}
}

func (r *vmRun) currentPhase() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.phase
}

func (r *vmRun) setPhase(p string) {
	r.mu.Lock()
	r.phase = p
	r.mu.Unlock()
}

// execute compiles and runs one script, pumping its event loop until the
// script's promise settles.
func (s *shared) execute(ctx context.Context, script string, args any, depth int) (any, Meta, error) {
	meta, err := ParseMeta(script)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("workflow: %w", err)
	}
	if depth == 0 {
		s.mu.Lock()
		s.meta = meta
		s.mu.Unlock()
	}
	// Declared params are checked before anything runs, so a template missing
	// an input fails at the door with the names of what is missing.
	args, err = applyParams(meta, args, depth)
	if err != nil {
		return nil, meta, err
	}
	r := &vmRun{
		sh:     s,
		vm:     goja.New(),
		jobs:   make(chan func(), 512),
		done:   make(chan struct{}),
		depth:  depth,
		nested: depth > 0,
		meta:   meta,
	}
	defer close(r.done)

	if err := r.bindGlobals(ctx, args); err != nil {
		return nil, meta, fmt.Errorf("workflow %q: %w", meta.Name, err)
	}
	// A script is written by a model, so it can contain a loop that never
	// yields. goja runs it on this goroutine and checks nothing, so without
	// this the loop ignores Esc, ignores the tool timeout, and burns a core
	// for the life of the process. Interrupt is the only thing that stops a
	// running script from outside it.
	stopWatchdog := make(chan struct{})
	defer close(stopWatchdog)
	go r.watchdog(ctx, stopWatchdog)
	s.emit(Event{Kind: EventStart, Message: meta.Name, Nested: r.nested})

	program, err := compileScript(meta.Name, script)
	if err != nil {
		return nil, meta, fmt.Errorf("workflow %q: script does not parse: %w", meta.Name, err)
	}
	// A workflow() child can be scheduled while the run is paused; its first
	// synchronous stretch waits for the answer like any other script code.
	if err := s.waitThawed(ctx); err != nil {
		return nil, meta, fmt.Errorf("workflow %q: %w", meta.Name, err)
	}
	leave := r.enterJS()
	val, err := r.vm.RunProgram(program)
	leave()
	if err != nil {
		return nil, meta, fmt.Errorf("workflow %q: %w", meta.Name, describeRunError(err, ctx))
	}
	promise, ok := val.Export().(*goja.Promise)
	if !ok {
		return nil, meta, fmt.Errorf("workflow %q: script did not produce a result", meta.Name)
	}

	value, err := r.pump(ctx, promise)
	if err != nil {
		s.emit(Event{Kind: EventFinish, Message: err.Error(), Nested: r.nested})
		return nil, meta, fmt.Errorf("workflow %q: %w%s", meta.Name, err, missingArgsHint(script, args, err))
	}
	s.emit(Event{Kind: EventFinish, Message: meta.Name, Nested: r.nested})
	return value, meta, nil
}

// maxSyncExecution bounds one uninterrupted stretch of JavaScript — the time
// between two awaits.
//
// A workflow legitimately runs for hours, but it spends that time waiting on
// agents, not executing script: the synchronous segments are milliseconds. A
// stretch this long therefore means a runaway loop, and bounding it is the
// only defence against one that allocates, since goja exposes no memory limit
// and the tool's own two-hour deadline is far too late to stop an OOM.
const maxSyncExecution = 30 * time.Second

// errRunawayScript distinguishes the watchdog's interrupt from a cancellation.
var errRunawayScript = errors.New("script ran for more than 30s without awaiting anything — it is looping")

// idlePoll bounds how long the loop waits before checking whether the script
// is deadlocked. It only fires when nothing else is happening, so it costs
// nothing on a busy run.
const idlePoll = 50 * time.Millisecond

// pump drives the script's event loop: run queued resolutions until the
// script's promise settles. Nothing else touches the runtime while this runs.
func (r *vmRun) pump(ctx context.Context, promise *goja.Promise) (any, error) {
	timer := time.NewTimer(idlePoll)
	defer timer.Stop()
	for promise.State() == goja.PromiseStatePending {
		// Paused at a checkpoint the host has surfaced: run nothing until it
		// is answered, so the paused run cannot emit progress into a turn
		// that has ended, nor start work the orchestrator has not seen.
		if wait := r.sh.frozen(); wait != nil {
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(idlePoll)
		select {
		case job := <-r.jobs:
			if err := r.runJob(job); err != nil {
				return nil, err
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			// No work arrived. If nothing is in flight and no resolution is
			// queued, the script is awaiting something that can never settle
			// (a bare `new Promise(() => {})`, say) — report that instead of
			// hanging until the tool timeout kills the run.
			if r.inflight.Load() == 0 && len(r.jobs) == 0 && promise.State() == goja.PromiseStatePending {
				return nil, fmt.Errorf("script awaited a promise that never settles (no agents in flight)")
			}
		}
	}
	if promise.State() == goja.PromiseStateRejected {
		return nil, fmt.Errorf("script threw: %s", describeRejection(promise.Result()))
	}
	result := promise.Result()
	if result == nil || goja.IsUndefined(result) || goja.IsNull(result) {
		return nil, nil
	}
	return result.Export(), nil
}

// describeRunError turns goja's interrupt into the cancellation it actually
// is, so a script killed by Esc or by the tool timeout does not read as a
// mysterious engine fault.
func describeRunError(err error, ctx context.Context) error {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		if v, ok := interrupted.Value().(error); ok && errors.Is(v, errRunawayScript) {
			return fmt.Errorf("script stopped: %w", errRunawayScript)
		}
		if cause := ctx.Err(); cause != nil {
			return fmt.Errorf("script stopped: %w", cause)
		}
		return fmt.Errorf("script stopped: %v", interrupted.Value())
	}
	return err
}

// missingArgsHint explains the commonest way a first run dies: the script
// reads args, but the tool call passed none, so every property access on it is
// a TypeError. The raw message names the property and not the cause, and a
// live run burned three attempts rediscovering it.
func missingArgsHint(script string, args any, err error) string {
	if args != nil || !strings.Contains(err.Error(), "of undefined") || !strings.Contains(script, "args") {
		return ""
	}
	return " — the tool call passed no args, so the `args` global is undefined; pass args in the workflow tool call, or stop reading it in the script"
}

// watchdog is the only thing that can stop a script from outside it: it
// interrupts on cancellation, and on a stretch of synchronous execution long
// enough to mean a runaway loop.
func (r *vmRun) watchdog(ctx context.Context, stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.vm.Interrupt(ctx.Err())
			return
		case <-stop:
			return
		case now := <-ticker.C:
			if start := r.syncStart.Load(); start != 0 && now.Sub(time.Unix(0, start)) > maxSyncExecution {
				r.vm.Interrupt(errRunawayScript)
				return
			}
		}
	}
}

// enterJS marks the start of a synchronous stretch for the watchdog; the
// returned function marks its end.
func (r *vmRun) enterJS() func() {
	r.syncStart.Store(time.Now().UnixNano())
	return func() { r.syncStart.Store(0) }
}

// runJob executes one queued promise resolution.
//
// Resolving a promise runs JS — the continuation of whatever awaited it — so
// an interrupt lands here as a panic rather than as a returned error, and a
// bug in a binding would too. Neither may take the process down: this is a
// script a model wrote, executing inside the user's editor session.
func (r *vmRun) runJob(job func()) (err error) {
	leave := r.enterJS()
	defer func() {
		leave()
		rec := recover()
		if rec == nil {
			return
		}
		if interrupted, ok := rec.(*goja.InterruptedError); ok {
			err = fmt.Errorf("script stopped: %v", interrupted.Value())
			return
		}
		err = fmt.Errorf("workflow engine panic: %v", rec)
	}()
	job()
	return nil
}

func describeRejection(v goja.Value) string {
	if v == nil {
		return "unknown error"
	}
	if obj, ok := v.(*goja.Object); ok {
		if stack := obj.Get("stack"); stack != nil && !goja.IsUndefined(stack) {
			return strings.TrimSpace(stack.String())
		}
	}
	return strings.TrimSpace(v.String())
}

func (r *vmRun) bindGlobals(ctx context.Context, args any) error {
	vm := r.vm
	if err := vm.Set("__wfMaxItems", r.sh.opts.MaxItems); err != nil {
		return err
	}
	if _, err := vm.RunString(prelude); err != nil {
		return fmt.Errorf("workflow prelude: %w", err)
	}
	if parse, ok := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("parse")); ok {
		r.jsonParse = parse
	}
	if args == nil {
		if err := vm.Set("args", goja.Undefined()); err != nil {
			return err
		}
	} else if err := vm.Set("args", args); err != nil {
		return err
	}
	if err := vm.Set("log", func(call goja.FunctionCall) goja.Value {
		msg := strings.TrimSpace(call.Argument(0).String())
		if msg != "" {
			r.sh.addLog(msg)
			r.sh.emit(Event{Kind: EventLog, Phase: r.currentPhase(), Message: msg, Nested: r.nested})
		}
		return goja.Undefined()
	}); err != nil {
		return err
	}
	if err := vm.Set("phase", r.jsPhase(ctx)); err != nil {
		return err
	}
	if err := vm.Set("agent", r.jsAgent(ctx)); err != nil {
		return err
	}
	if err := vm.Set("workflow", r.jsWorkflow(ctx)); err != nil {
		return err
	}
	if err := vm.Set("checkpoint", r.jsCheckpoint(ctx)); err != nil {
		return err
	}
	if err := vm.Set("size", r.sizeObject()); err != nil {
		return err
	}
	return vm.Set("budget", r.budgetObject())
}

// jsPhase implements phase(title, {detail}?): start a progress group.
//
// A title need not be declared in meta.phases — a script that discovers its
// work at runtime discovers its phases too — but the event says whether it
// was, so a host that drew the declared plan up front can mark the newcomer.
func (r *vmRun) jsPhase(ctx context.Context) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		arg := call.Argument(0)
		if goja.IsUndefined(arg) || goja.IsNull(arg) {
			return goja.Undefined()
		}
		title := strings.TrimSpace(arg.String())
		if title == "" {
			return goja.Undefined()
		}
		detail := ""
		switch opts := call.Argument(1).Export().(type) {
		case string:
			detail = strings.TrimSpace(opts)
		case map[string]any:
			detail = stringField(opts, "detail")
		}
		dynamic := true
		for _, p := range r.meta.Phases {
			if p.Title == title {
				dynamic = false
				if detail == "" {
					detail = p.Detail
				}
				break
			}
		}
		prev := r.currentPhase()
		r.setPhase(title)
		isNew := r.sh.addPhase(title)
		r.sh.emit(Event{Kind: EventPhase, Phase: title, Detail: detail, Dynamic: dynamic, Nested: r.nested})
		opts := r.sh.opts
		if isNew && opts.Checkpoints && opts.AutoCheckpoint && r.sh.agents.Load() > 0 {
			r.autoCheckpoint(ctx, prev, title)
		}
		return goja.Undefined()
	}
}

// sizeObject builds the size global: the run's size guideline, which the
// prelude's plan() also reads for its default cap.
func (r *vmRun) sizeObject() *goja.Object {
	vm := r.vm
	sz := r.sh.size
	infinity := vm.Get("Infinity")
	obj := vm.NewObject()
	_ = obj.Set("tier", sz.Tier)
	if sz.Agents > 0 {
		_ = obj.Set("agents", sz.Agents)
	} else {
		_ = obj.Set("agents", infinity)
	}
	_ = obj.Set("fanout", sz.Fanout)
	_ = obj.Set("spawned", func() int64 { return r.sh.agents.Load() })
	_ = obj.Set("remaining", func() goja.Value {
		if sz.Agents <= 0 {
			return infinity
		}
		return vm.ToValue(max(int64(sz.Agents)-r.sh.agents.Load(), 0))
	})
	return obj
}

func (r *vmRun) budgetObject() *goja.Object {
	vm := r.vm
	obj := vm.NewObject()
	if r.sh.opts.BudgetTokens > 0 {
		_ = obj.Set("total", r.sh.opts.BudgetTokens)
	} else {
		_ = obj.Set("total", goja.Null())
	}
	_ = obj.Set("spent", func() int64 { return r.sh.tokens.Load() })
	_ = obj.Set("remaining", func() goja.Value {
		rem := r.sh.remaining()
		if rem < 0 {
			return vm.ToValue(vm.Get("Infinity"))
		}
		return vm.ToValue(rem)
	})
	return obj
}

type agentOpts struct {
	Label     string
	Phase     string
	AgentType string
	Model     string
	Effort    string
	Isolation string
	Schema    json.RawMessage
}

func decodeAgentOpts(vm *goja.Runtime, v goja.Value) (agentOpts, error) {
	var o agentOpts
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return o, nil
	}
	raw, ok := v.Export().(map[string]any)
	if !ok {
		return o, fmt.Errorf("agent(): the second argument must be an options object")
	}
	str := func(key string) string {
		s, _ := raw[key].(string)
		return strings.TrimSpace(s)
	}
	o.Label = str("label")
	o.Phase = str("phase")
	o.AgentType = str("agentType")
	o.Model = str("model")
	o.Effort = str("effort")
	o.Isolation = str("isolation")
	if schema, ok := raw["schema"]; ok && schema != nil {
		encoded, err := json.Marshal(schema)
		if err != nil {
			return o, fmt.Errorf("agent(): schema is not JSON-encodable: %w", err)
		}
		o.Schema = encoded
	}
	if o.Isolation != "" && o.Isolation != "worktree" {
		return o, fmt.Errorf("agent(): unsupported isolation %q (only \"worktree\")", o.Isolation)
	}
	return o, nil
}

// jsAgent implements the agent() global: dispatch one sub-agent and resolve
// with its answer.
//
// A failed agent resolves to null rather than rejecting. A fan-out is worth
// running precisely when individual members may die, and a rejection would
// take the whole script down with the first one; the failure is still recorded
// as an event, counted in the result, and visible to the caller as a null.
func (r *vmRun) jsAgent(ctx context.Context) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		vm := r.vm
		prompt := strings.TrimSpace(call.Argument(0).String())
		if prompt == "" || goja.IsUndefined(call.Argument(0)) {
			panic(vm.NewTypeError("agent(): a prompt string is required"))
		}
		opts, err := decodeAgentOpts(vm, call.Argument(1))
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		if rem := r.sh.remaining(); rem == 0 {
			panic(vm.NewGoError(fmt.Errorf("agent(): token budget of %d exhausted — guard your loop with budget.remaining()", r.sh.opts.BudgetTokens)))
		}
		index := int(r.sh.agents.Add(1))
		if index > r.sh.opts.MaxAgents {
			panic(vm.NewGoError(fmt.Errorf("agent(): this run already started %d agents, the cap is %d — the script is probably looping", index, r.sh.opts.MaxAgents)))
		}
		if sz := r.sh.size; sz.Agents > 0 && index > sz.Agents && r.sh.sizeWarned.CompareAndSwap(false, true) {
			// A guideline, not a cap: the run continues, but the overrun is
			// on the record for the orchestrator reading the result.
			line := fmt.Sprintf("size guideline (%s: ~%d agents) exceeded", sz.Tier, sz.Agents)
			r.sh.addLog(line)
			r.sh.emit(Event{Kind: EventLog, Phase: r.currentPhase(), Message: line, Nested: r.nested})
		}

		phase := opts.Phase
		if phase == "" {
			phase = r.currentPhase()
		}
		agentType := opts.AgentType
		if agentType == "" {
			agentType = r.sh.opts.DefaultAgentType
		}
		req := Request{
			Prompt:    prompt,
			Label:     labelFor(opts.Label, prompt),
			Phase:     phase,
			AgentType: agentType,
			Model:     opts.Model,
			Effort:    opts.Effort,
			Isolation: opts.Isolation,
			Schema:    opts.Schema,
			Index:     index,
		}

		promise, resolve, _ := vm.NewPromise()
		r.inflight.Add(1)
		admitted := r.sh.admit()
		go func() {
			defer r.inflight.Add(-1)
			text, value, err := r.sh.dispatch(ctx, req, r.nested, admitted)
			r.post(ctx, func() {
				switch {
				case err != nil:
					resolve(goja.Null())
				case req.Schema != nil:
					resolve(vm.ToValue(value))
				default:
					resolve(vm.ToValue(text))
				}
			})
		}()
		return vm.ToValue(promise)
	}
}

func labelFor(label, prompt string) string {
	if label != "" {
		return label
	}
	line := strings.TrimSpace(strings.SplitN(prompt, "\n", 2)[0])
	if len(line) > 72 {
		line = strings.TrimSpace(line[:72]) + "…"
	}
	return line
}

// dispatch runs one request against the Runner: journal replay first, then a
// concurrency slot, then the call itself with schema retries.
//
// admitted says whether the call already passed the pause gate when agent()
// was called (see shared.admit); if not, it waits at the gate first.
func (s *shared) dispatch(ctx context.Context, req Request, nested, admitted bool) (string, any, error) {
	if !admitted {
		if err := s.enterDispatch(ctx); err != nil {
			s.failed.Add(1)
			return "", nil, err
		}
	}
	defer s.leaveDispatch()
	key := callKey(req)
	if entry, ok := s.opts.Journal.Take(key); ok {
		req.Instance = entry.Instance
		if req.Instance == "" {
			req.Instance = instanceName(req)
		}
		s.emit(Event{Kind: EventAgentStart, Phase: req.Phase, Label: req.Label, Instance: req.Instance, AgentType: req.AgentType, Index: req.Index, Cached: true, Nested: nested})
		value, err := decodeCached(entry.Output, req.Schema)
		if err == nil {
			s.emit(Event{Kind: EventAgentDone, Phase: req.Phase, Label: req.Label, Instance: req.Instance, AgentType: req.AgentType, Index: req.Index, Output: entry.Output, Cached: true, Nested: nested})
			return entry.Output, value, nil
		}
		// A cached answer that no longer parses is treated as absent rather
		// than as a failure: re-running the agent is always the better fix.
	}

	req.Instance = instanceName(req)
	s.emit(Event{Kind: EventAgentStart, Phase: req.Phase, Label: req.Label, Instance: req.Instance, AgentType: req.AgentType, Index: req.Index, Nested: nested})

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		s.failed.Add(1)
		return "", nil, ctx.Err()
	}

	text, value, err := s.callScoped(ctx, req)
	entry := JournalEntry{
		Kind: JournalKindAgent, Key: key, Index: req.Index, Label: req.Label, Phase: req.Phase,
		Instance: req.Instance, AgentType: req.AgentType, Output: text,
	}
	if err != nil {
		s.failed.Add(1)
		entry.Error = err.Error()
		_ = s.opts.Journal.Append(entry)
		s.emit(Event{Kind: EventAgentError, Phase: req.Phase, Label: req.Label, Instance: req.Instance, AgentType: req.AgentType, Index: req.Index, Message: err.Error(), Nested: nested})
		return "", nil, err
	}
	_ = s.opts.Journal.Append(entry)
	s.emit(Event{Kind: EventAgentDone, Phase: req.Phase, Label: req.Label, Instance: req.Instance, AgentType: req.AgentType, Index: req.Index, Output: text, Nested: nested})
	return text, value, nil
}

func decodeCached(output string, schema json.RawMessage) (any, error) {
	if schema == nil {
		return nil, nil
	}
	return parseStructured(output, schema)
}

func instanceName(req Request) string {
	name := req.AgentType
	if name == "" {
		name = "agent"
	}
	return fmt.Sprintf("%s#%d", name, req.Index)
}

// callScoped brackets the attempt loop with the Runner's per-call setup and
// teardown, so resources scoped to one agent() call are created once however
// many attempts it takes.
func (s *shared) callScoped(ctx context.Context, req Request) (text string, value any, err error) {
	scoper, ok := s.opts.Runner.(CallScoper)
	if !ok {
		return s.callWithSchema(ctx, req)
	}
	if err := scoper.BeginCall(ctx, req); err != nil {
		return "", nil, err
	}
	defer func() { scoper.EndCall(ctx, req, err) }()
	return s.callWithSchema(ctx, req)
}

// callWithSchema runs the agent, and when the call carries a schema, retries
// with the parse error appended until the answer is usable.
func (s *shared) callWithSchema(ctx context.Context, req Request) (string, any, error) {
	attempts := 1
	if req.Schema != nil {
		attempts = s.opts.SchemaAttempts
	}
	base := req.Prompt
	var lastErr error
	for attempt := range attempts {
		call := req
		if req.Schema != nil {
			call.Prompt = base + schemaInstruction(req.Schema)
			if attempt > 0 {
				call.Prompt += fmt.Sprintf("\n\nYour previous answer was rejected: %v. Return only the JSON value.", lastErr)
			}
		}
		resp, err := s.opts.Runner.RunAgent(ctx, call)
		s.tokens.Add(int64(resp.Tokens))
		if err != nil {
			return "", nil, err
		}
		if req.Schema == nil {
			return strings.TrimSpace(resp.Text), nil, nil
		}
		value, perr := parseStructured(resp.Text, req.Schema)
		if perr == nil {
			return strings.TrimSpace(resp.Text), value, nil
		}
		lastErr = perr
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
	}
	return "", nil, fmt.Errorf("structured output never parsed after %d attempts: %v", attempts, lastErr)
}

// jsWorkflow implements the workflow() global: run a saved workflow, a script
// file, or a script generated at runtime, as a sub-step of this one.
func (r *vmRun) jsWorkflow(ctx context.Context) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		vm := r.vm
		if r.depth > 0 {
			panic(vm.NewGoError(fmt.Errorf("workflow(): sub-workflows cannot nest further — inline the work instead")))
		}
		sub, err := r.resolveSubWorkflow(call.Argument(0))
		if err != nil {
			panic(vm.NewGoError(err))
		}
		subArgs := sub.args
		if a := call.Argument(1); !goja.IsUndefined(a) && !goja.IsNull(a) {
			subArgs = a.Export()
		}

		promise, resolve, reject := vm.NewPromise()
		if sub.inline {
			// A generated stage is usually written by an agent earlier in the
			// run, so a broken one is an expected failure the script should be
			// able to catch and retry — a rejection, with the reason, rather
			// than a throw that only a try around the call itself would see.
			if _, err := Validate(sub.script); err != nil {
				reject(vm.NewGoError(fmt.Errorf("workflow(): the generated script is invalid: %w", err)))
				return vm.ToValue(promise)
			}
		}
		r.inflight.Add(1)
		go func() {
			defer r.inflight.Add(-1)
			value, _, err := r.sh.execute(ctx, sub.script, subArgs, r.depth+1)
			r.post(ctx, func() {
				if err != nil {
					reject(vm.NewGoError(err))
					return
				}
				resolve(vm.ToValue(value))
			})
		}()
		return vm.ToValue(promise)
	}
}

// subWorkflow is a resolved workflow() reference.
type subWorkflow struct {
	script string
	// args are the reference object's own args ({script, args}), used when
	// the call passes none as its second argument.
	args   any
	inline bool
}

func (r *vmRun) resolveSubWorkflow(arg goja.Value) (subWorkflow, error) {
	const usage = "workflow(): a saved workflow name, {scriptPath}, or {script} is required"
	if goja.IsUndefined(arg) || goja.IsNull(arg) {
		return subWorkflow{}, fmt.Errorf(usage)
	}
	if ref, ok := arg.Export().(map[string]any); ok {
		sub := subWorkflow{args: ref["args"]}
		if source, ok := ref["script"].(string); ok && strings.TrimSpace(source) != "" {
			sub.script, sub.inline = source, true
			return sub, nil
		}
		if path, _ := ref["scriptPath"].(string); strings.TrimSpace(path) != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return subWorkflow{}, fmt.Errorf("workflow(): read %s: %w", path, err)
			}
			sub.script = string(data)
			return sub, nil
		}
		if name, _ := ref["name"].(string); strings.TrimSpace(name) != "" {
			script, err := r.loadSaved(name)
			sub.script = script
			return sub, err
		}
		return subWorkflow{}, fmt.Errorf("workflow(): the reference object needs a script, a scriptPath or a name")
	}
	name := strings.TrimSpace(arg.String())
	if name == "" {
		return subWorkflow{}, fmt.Errorf(usage)
	}
	script, err := r.loadSaved(name)
	return subWorkflow{script: script}, err
}

func (r *vmRun) loadSaved(name string) (string, error) {
	if r.sh.opts.Resolve == nil {
		return "", fmt.Errorf("workflow(): saved workflows are not available in this run")
	}
	return r.sh.opts.Resolve(strings.TrimSpace(name))
}
