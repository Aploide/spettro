package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/workflow"
)

// Workflows: deterministic multi-agent orchestration. Where Ultra fans one
// prompt template over a list of items, a workflow is a script that decides in
// ordinary control flow — loops, conditionals, staged pipelines — which
// sub-agents run and how their results combine. The model writes the script;
// Spettro executes it exactly as written.

const (
	workflowToolID = "workflow"
	// workflowKeyword opts a single turn into workflows — ultracode, the
	// standing-mode guidance included. It is a one-shot switch by default:
	// injecting the tool and its guidance changes the system prompt, and
	// paying that on every turn is only right for a user who asked for it,
	// which is what the host's session toggle (LLMAgent.Ultracode) is for.
	workflowKeyword = "ultracode"
	// workflowMaxItems caps one parallel()/pipeline() call.
	workflowMaxItems = 4096
	// workflowMaxAgents is the runaway-loop backstop for a whole run.
	workflowMaxAgents = 1000
	// workflowRetryBase is the first backoff for a transient provider failure
	// inside a workflow agent; attempts double it.
	workflowRetryBase   = 3 * time.Second
	workflowMaxAttempts = 3
)

// WorkflowKeyword is the shorthand a user writes to opt a turn into
// workflows. Asking for one in plain English works too — see
// workflowActivationRes.
const WorkflowKeyword = workflowKeyword

// workflowActivationRes are the ways a user turns workflows on for a turn.
//
// The keyword is the shorthand, not the only door: "use a workflow to
// modernise these handlers" is an unmistakable request for orchestration, and
// making people learn a magic word to get it would be a worse tool. Every
// pattern here has to be a phrase that only makes sense as a request for this
// feature — "our deploy workflow" and ".github/workflows" must stay quiet.
//
// A false positive costs a couple of kilobytes of system prompt and nothing
// else: the tool is offered, not invoked, and the model is told to ignore it
// when the work does not need it. A false negative costs the user the
// feature. The patterns lean accordingly.
var workflowActivationRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b` + workflowKeyword + `\b`),
	// "use a workflow", "write a multi-agent workflow", "set up a workflow".
	// An indefinite article only: "run the workflow" almost always means a CI
	// job or an already-named saved script, and /workflows run covers the
	// latter by rewriting the prompt with the keyword in it.
	regexp.MustCompile(`(?i)\b(?:use|using|run|write|author|make|create|build|set ?up|start|launch|kick off|do this as|do it as)\s+(?:a|an|another)\s+(?:new\s+)?(?:multi[- ]?agent\s+|orchestration\s+)?workflow\b`),
	// "with workflows", "via workflows"
	regexp.MustCompile(`(?i)\b(?:use|using|run|with|via)\s+workflows\b`),
	// an explicit reference to the tool itself
	regexp.MustCompile(`(?i)\bworkflow tool\b`),
	// "fan this out across sub-agents"
	regexp.MustCompile(`(?i)\bfan\s+(?:this|that|it|them|the \w+)?\s*out\s+(?:across|over|to|into)\s+(?:\w+\s+){0,2}(?:sub-?)?agents?\b`),
	// "orchestrate this with subagents"
	regexp.MustCompile(`(?i)\borchestrate\s+(?:\w+\s+){0,3}(?:with|using|across|over)\s+(?:\w+\s+){0,2}(?:sub-?)?agents?\b`),
	// "multi-agent orchestration"
	regexp.MustCompile(`(?i)\bmulti[- ]?agent\s+(?:orchestration|workflow|pipeline|run)\b`),
}

// WorkflowPreapproved reports whether the user granted the keyword itself,
// which counts as consent to spend on a run.
//
// Asking for "a workflow" in plain English is a request; typing the keyword is
// a standing yes. The difference decides whether the tool confirms before it
// starts spawning agents, so it is kept distinct from WorkflowRequested rather
// than folded into it.
func WorkflowPreapproved(task string) bool {
	return workflowActivationRes[0].MatchString(task)
}

// WorkflowRequested reports whether a user message opts this turn into
// workflows. Detection lives here rather than in each host so the TUI, ACP,
// goal runs, Telegram and headless mode all honour it identically.
func WorkflowRequested(task string) bool {
	return len(WorkflowActivationSpans(task)) > 0
}

// WorkflowActivationSpans returns the [start, end) rune ranges of every phrase
// in task that turns workflows on, ordered and non-overlapping.
//
// It exists so the TUI can light those words up as they are typed and be
// right: the highlight is driven by the same match that decides whether the
// tool gets injected, so the input can never promise a mode the run will not
// enter.
func WorkflowActivationSpans(task string) [][2]int {
	var byteSpans [][2]int
	for _, re := range workflowActivationRes {
		for _, m := range re.FindAllStringIndex(task, -1) {
			byteSpans = append(byteSpans, [2]int{m[0], m[1]})
		}
	}
	if len(byteSpans) == 0 {
		return nil
	}
	sort.Slice(byteSpans, func(i, j int) bool {
		if byteSpans[i][0] != byteSpans[j][0] {
			return byteSpans[i][0] < byteSpans[j][0]
		}
		return byteSpans[i][1] > byteSpans[j][1]
	})
	// Patterns overlap by design ("use a workflow" and "workflow tool" both
	// match "use a workflow tool"), and a renderer handed overlapping ranges
	// would style the same cell twice.
	merged := byteSpans[:1]
	for _, sp := range byteSpans[1:] {
		last := &merged[len(merged)-1]
		if sp[0] <= last[1] {
			if sp[1] > last[1] {
				last[1] = sp[1]
			}
			continue
		}
		merged = append(merged, sp)
	}

	// Byte offsets are useless to a renderer that works in cells; convert
	// them once here rather than making every caller redo it. The merged
	// spans are sorted and disjoint, so one forward walk over task counts
	// every rune at most once — no map of the whole input to size.
	spans := make([][2]int, 0, len(merged))
	byteIdx, runeIdx := 0, 0
	for _, m := range merged {
		if m[0] < byteIdx || m[1] > len(task) {
			continue
		}
		runeIdx += utf8.RuneCountInString(task[byteIdx:m[0]])
		start := runeIdx
		runeIdx += utf8.RuneCountInString(task[m[0]:m[1]])
		byteIdx = m[1]
		spans = append(spans, [2]int{start, runeIdx})
	}
	return spans
}

// The workflow guidance is appended to the system prompt when workflows are
// active. Like the Ultra section it is fixed for the whole run, which keeps
// the prompt-cache prefix byte-stable: workflowGuidance.prompt composes it
// once, in Run, from the consts below and the run's size tier, budget, paused
// runs and the kind of agent it is for.
//
// It comes in two variants that share one core. A plain-English request ("use
// a workflow") gets the judge-it variant: the tool is offered, the model
// decides whether the task earns it, the user confirms. Ultracode — the
// keyword or the host's session toggle — gets the standing-mode variant: a
// workflow is the default for every substantive task. The two policies
// contradict each other, which is why neither lives in the core.

// workflowToolIntro opens both variants.
const workflowToolIntro = `You have the workflow tool, which runs a JavaScript orchestration script you write, so the control flow around your sub-agents is deterministic instead of re-decided by you every step.`

// The judge-it variant opens by saying why the tool is there this turn. The
// usual reason is the user asking for a workflow; the other is a run paused
// at a checkpoint in an earlier turn, which must stay continuable on a turn
// that never mentions workflows ("yes, fix 1 and 3") — there the opener must
// not claim the user asked for anything.
const (
	workflowJudgeRequested = `

WORKFLOWS are available this turn: the user asked for one in their own words. `
	workflowJudgePausedOnly = `

WORKFLOWS are available this turn because a workflow run you started earlier is paused at a checkpoint, waiting for you (listed below). If this turn answers its question, continue the run; if the user moved on, stop it. Starting a new run is a separate decision, made as below. `
)

// workflowJudgePolicy is the policy when the user asked for a workflow in
// their own words (or a paused run brought the tool back): availability, not
// an instruction.
const workflowJudgePolicy = `

Availability is not an instruction to use it. Judge the task: if it is a single edit, a question, a quick fix, or anything you would finish in a few tool calls, just do the work and do not mention the tool. A workflow multiplies token usage — every agent() call is a full agent run — so it has to earn that.

Use it when the work has structure worth encoding — fan out and verify, several independent attempts judged against each other, a sweep that loops until it stops finding things, a migration over a discovered work-list. For a single delegation use the agent tool; for a flat fan-out of one template over N items, ultra is still the simpler choice.

If the user explicitly asked for a workflow and the task genuinely does not warrant one, do the work directly and say in one line why a script would not have helped. Do not manufacture phases to look busy.

Spettro asks the user to confirm before the run starts, and they may choose to keep the script without running it. That is theirs to decide: if they decline, do not run it anyway and do not re-propose it — do the work directly.`

// ultracodeOpener opens the standing-mode variant: ultracode is on, the
// opt-in is standing, and a workflow is the default for substantive work.
const ultracodeOpener = `

WORKFLOWS are available and ULTRACODE is on: the user's opt-in is standing, not a one-off request. ` + workflowToolIntro

// The standing-mode policy depends on what the agent is for. The session
// toggle applies to whichever agent the user is talking to, and telling a
// planner or a read-only Q&A agent to run "understand → design → implement →
// review" workflows would hand it instructions that contradict its role — and,
// since workflow members default to an agent that can write, the means to
// carry them out. An agent that cannot change files itself is told to use
// workflows for understanding, research and review instead.
const (
	ultracodeImplementPolicy = `

Author and run a workflow for every substantive task by default. The goal is the most exhaustive, correct answer — token cost is not the constraint here (respect the size guideline below, though). Work solo only on conversational turns (a question, a clarification) or trivial mechanical edits (a rename, a typo, a one-line fix).

For multi-phase work — understand → design → implement → review — run several workflows in sequence, one per phase, and read each result before deciding the next; never fold the whole job into one script whose later phases were decided before you saw the earlier ones.`

	ultracodeResearchPolicy = `

You do not change files in this role, and neither do your workflows: use them for understanding, research and review — mapping the code a question touches, investigating it from several independent angles, auditing a design or a diff, verifying claims — never to implement. Author and run a workflow for every substantive question or analysis by default; the goal is the most exhaustive, correct answer, and token cost is not the constraint here (respect the size guideline below, though). Answer solo only on conversational turns (a quick question, a clarification).

Workflow members default to an agent that can edit files: tell every member, in its prompt, that it must not modify anything, and give it a read-only agentType (such as explore) when the manifest has one. For multi-phase work — understand → analyse → verify — run several workflows in sequence, one per phase, and read each result before deciding the next; never fold the whole job into one script whose later phases were decided before you saw the earlier ones.`
)

// ultracodeShared closes both standing-mode policies.
const ultracodeShared = ` Lean toward adversarially verifying what a workflow finds: independent skeptics that try to refute each finding, diverse lenses over the same code, loop-until-dry sweeps (untilDry), and a final completeness critic asking what was missed. Use checkpoint() inside a script when its next stage depends on your judgement of interim results.

Runs start without a confirmation prompt: the user already said yes.`

// How the work-list is found depends on whether the agent can look at the
// code itself: a planner with no read tools told to "scout inline first"
// would be told to do something the runtime forbids it.
const (
	workflowScoutInline = `

Scout inline first (list the files, scope the diff, find the call sites), then hand the discovered work-list to a script. Anything the script still has to discover it discovers at runtime — plan(), an agent with a schema — never from a list hardcoded from memory.`

	workflowScoutDelegated = `

You have no file tools of your own, so do not scout inline: let the script discover its work-list at runtime — plan(), an agent with a schema, or a first discovery workflow — never from a list hardcoded from memory.`
)

// workflowPromptCore is the shared rest: how to work with the tool and the
// script API.
const workflowPromptCore = ` You stay in the loop between workflows: read each result and decide the next phase yourself.

Generate a fresh script for the task in front of you; that is the default. Saved workflows (.spettro/workflows, ~/.spettro/workflows) are TEMPLATES, not finished answers: when one plausibly fits, read it — call the workflow tool with {"name": "<name>", "show": true}, or script_path with show, which returns its source and params without running anything — adapt it to this task (discover work-lists at runtime, never replay a hardcoded file list) and run the adapted script inline. Run one by name, with args for its meta.params, only when it fits as-is. When a script is worth keeping — the user will plainly want it again, or asked for a reusable one — set save_as and write it as a template: declare meta.params for everything task-specific and discover the work at runtime. Do not save one-off scripts; a folder of near-duplicates is worse than none.

The script must begin with a pure object literal header and then use the provided globals:

export const meta = {
  name: 'review-changes',
  description: 'Review the diff, then adversarially verify each finding',
  phases: [{title: 'Review'}, {title: 'Verify'}],
  params: {base: {type: 'string', description: 'branch to diff against', default: 'main'}},
}
phase('Review')
const results = await pipeline(DIMENSIONS,
  d => agent(` + "`Review the diff against ${args.base} for ${d}`" + `, {label: 'review:' + d, phase: 'Review', schema: FINDINGS}),
  review => parallel(review.findings.map(f => () =>
    agent('Try to refute: ' + f.title, {phase: 'Verify', schema: VERDICT}))))
return results.flat().filter(Boolean)

Use opts.schema whenever a stage produces data the next stage consumes. Spettro appends the contract to the prompt, parses the answer back, and retries the agent with the parse error if it does not fit — hand-rolling JSON.parse over the text in the script gets none of that, and one malformed answer silently drops a result.

Globals: agent(prompt, opts) → the sub-agent's final text, or the parsed object when opts.schema is a JSON Schema, or null if it failed; parallel(thunks) → runs all concurrently and waits for every one (a barrier); pipeline(items, ...stages) → pushes each item through every stage independently with NO barrier between stages; phase(title, {detail}) → starts a progress group (phases not declared in meta.phases are fine: they show as added at runtime); log(message); args (the tool call's args, checked against meta.params); budget.remaining(); size → {tier, agents, fanout, spawned(), remaining()}, the size guideline; plan(prompt, {max}) → one agent turns a goal into a work-list [{label, prompt, phase?, data?}], capped at size.fanout; untilDry(round, {key, dry, maxRounds}) → calls round(i, seen) until rounds stop finding new items, returning every fresh item; workflow(name, args) or workflow({script, args}) → runs a saved or freshly generated script as a sub-step; checkpoint(message, data) → pauses the run and resolves to your reply.

meta.params declares a script's inputs — params: {base: {type, description, required, default}, focus: 'a description'} — with type one of string, number, boolean, array, object, any. A missing required param or a wrong type fails the run before any agent starts.

The orchestrator stays in the loop through checkpoints. await checkpoint('Fix these?', findings) makes the tool call return with the checkpoint: read it, then call the workflow tool with {"continue_run_id": "<run id>", "reply": <any JSON>} — the script receives reply as checkpoint()'s value — or {"continue_run_id": "<run id>", "stop": true} to end the run. Every in-flight agent finishes before the pause and nothing runs while it waits. auto_checkpoint: true on the first call also pauses at every phase boundary. On a run resumed with resume_from_run_id, a checkpoint you already answered replays your earlier reply without pausing; a new one pauses as usual. A continue without a reply resolves checkpoint() to null, so treat null as "carry on with the default".

Prefer pipeline over parallel-then-parallel: a barrier is only right when a stage genuinely needs every previous result at once (dedup across the whole set, an early exit on zero findings). Give agent() a label and a phase so the user can follow the run.

Every agent is a fresh sub-agent that cannot see your context or the other agents: each prompt must be self-contained, with paths, constraints, and the expected output. Never give two concurrently running agents work that touches the same file — set isolation:"worktree" on the ones that edit.

With isolation:"worktree" the agent runs inside its own checkout, so give it REPOSITORY-RELATIVE paths ("internal/budget/budget.go"). An absolute path built from the main checkout points outside its worktree: the edit lands in the shared tree, the worktree merges back empty, and the isolation you asked for silently did nothing.

Date.now(), Math.random() and argless new Date() are unavailable (they would break resume); pass timestamps in through args and vary work by index.`

// workflowGuidance is what a run knows about workflows when its prompt is
// built: whether the tool is granted, which variant of the guidance applies,
// and the run's sizing. It is decided once, in Run, so the prompt it renders
// stays byte-stable for the whole run.
type workflowGuidance struct {
	// Enabled grants the workflow tool.
	Enabled bool
	// Requested says something in the turn asked for workflows — the
	// keyword, the toggle, a plain-English request — as opposed to the tool
	// being back only for the runs in Paused.
	Requested bool
	// Ultracode selects the standing-mode guidance over the judge-it one.
	Ultracode bool
	// Research marks an agent that cannot change files (a planner, a
	// read-only Q&A agent): the standing-mode guidance points its workflows
	// at understanding and review rather than implementation.
	Research bool
	// NoRead marks an agent with no tools to read the code itself, which
	// cannot scout a work-list inline.
	NoRead bool
	// Paused lists the runs waiting at a checkpoint when the run started.
	// They alone grant the tool when nothing else this turn does, so a run
	// paused in an earlier turn can be continued on a turn that never
	// mentions workflows.
	Paused []PausedWorkflow
	// SizeTier is the configured size tier ("" means medium).
	SizeTier string
	// BudgetTokens is the turn's "+500k"-style budget directive; 0 when the
	// user set none.
	BudgetTokens int
}

// section composes the guidance's policy and core text.
func (g workflowGuidance) section() string {
	var b strings.Builder
	switch {
	case g.Ultracode:
		b.WriteString(ultracodeOpener)
		if g.Research {
			b.WriteString(ultracodeResearchPolicy)
		} else {
			b.WriteString(ultracodeImplementPolicy)
		}
		b.WriteString(ultracodeShared)
	default:
		if g.pausedOnly() {
			b.WriteString(workflowJudgePausedOnly)
		} else {
			b.WriteString(workflowJudgeRequested)
		}
		b.WriteString(workflowToolIntro)
		b.WriteString(workflowJudgePolicy)
	}
	if g.NoRead {
		b.WriteString(workflowScoutDelegated)
	} else {
		b.WriteString(workflowScoutInline)
	}
	b.WriteString(workflowPromptCore)
	return b.String()
}

// pausedOnly reports whether the tool is granted only because runs are
// paused: nothing in the turn asked for workflows.
func (g workflowGuidance) pausedOnly() bool {
	return len(g.Paused) > 0 && !g.Requested
}

// prompt renders the guidance appended to the system prompt.
func (g workflowGuidance) prompt() string {
	if !g.Enabled {
		return ""
	}
	section := g.section()
	section += "\n\n" + workflowSizeLine(g.SizeTier)
	if g.BudgetTokens > 0 {
		section += "\n\n" + fmt.Sprintf("The user set a token budget of %d tokens for this turn: one pool shared by every workflow you start this turn without budget_tokens. A new run gets what the earlier ones left, which scripts see as budget.total and budget.remaining(). Once the pool is spent no further agent starts in any of those runs — agent() resolves to null — and a new run is refused. Agents already running when it runs out still finish, so a wide fan-out can overshoot: size each wave to what is left.", g.BudgetTokens)
	}
	// Only when the paused runs are the reason the tool is here at all. On a
	// turn that asked for workflows anyway, the checkpoint result with the
	// same ids is already in the history, and a line that changes with every
	// pause and continue would cost a prompt-cache miss on the whole
	// conversation each time.
	if g.pausedOnly() {
		section += "\n\n" + workflowPausedLine(g.Paused)
	}
	return section
}

// workflowPausedLine lists the runs waiting on the orchestrator when the run
// started, with the exact ids a continue needs. Each checkpoint message is
// flattened and cut short (compact): enough to recognise the question, not a
// copy of its data, which the model already read in the checkpoint result.
func workflowPausedLine(paused []PausedWorkflow) string {
	var b strings.Builder
	b.WriteString(`Workflow runs paused at a checkpoint, waiting for you — continue one with {"continue_run_id": "<run id>", "reply": <any JSON>}, or end it with {"continue_run_id": "<run id>", "stop": true}:`)
	for _, p := range paused {
		fmt.Fprintf(&b, "\n- run_id %s (%q) at checkpoint_id %s: %s", p.RunID, p.Name, p.CheckpointID, compact(p.Message))
	}
	return b.String()
}

// workflowSizeLine states the run's size guideline, read from the same tier
// table the engine exposes to scripts as the size global.
func workflowSizeLine(tier string) string {
	size := workflow.ResolveSize(tier)
	if size.Agents <= 0 {
		return fmt.Sprintf("Workflow size guideline: %s — no agent guideline; size each workflow to the task (plan() still caps one work-list at %d unless you pass max). The hard runaway cap is %d agents per run.", size.Tier, size.Fanout, workflowMaxAgents)
	}
	return fmt.Sprintf("Workflow size guideline: %s — keep each workflow under ~%d agents and each fan-out under ~%d (scripts can read size.agents and size.fanout). This is a guideline, not a hard limit: go past it only when the task plainly needs to.", size.Tier, size.Agents, size.Fanout)
}

type workflowArgs struct {
	Script          string          `json:"script"`
	ScriptPath      string          `json:"script_path"`
	Name            string          `json:"name"`
	Args            json.RawMessage `json:"args"`
	ResumeFromRunID string          `json:"resume_from_run_id"`
	MaxConcurrency  int             `json:"max_concurrency"`
	BudgetTokens    int             `json:"budget_tokens"`
	SaveAs          string          `json:"save_as"`
	SaveScope       string          `json:"save_scope"`
	// ContinueRunID continues a run paused at a checkpoint; Reply is what the
	// script's checkpoint() resolves to, Stop ends the run instead, and
	// CheckpointID (optional) must name the pending checkpoint.
	ContinueRunID string          `json:"continue_run_id"`
	Reply         json.RawMessage `json:"reply"`
	Stop          bool            `json:"stop"`
	CheckpointID  string          `json:"checkpoint_id"`
	// AutoCheckpoint pauses the run at every phase boundary as well.
	AutoCheckpoint bool `json:"auto_checkpoint"`
	// Size overrides the configured size tier for this run.
	Size string `json:"size"`
	// Show returns a saved script's source and params instead of running it.
	Show bool `json:"show"`
}

// runWorkflow is the workflow tool: resolve the script, run it, and hand the
// script's return value back to the model together with what the run did —
// or, when the script pauses at a checkpoint, hand the checkpoint over and
// keep the run alive for a later call to continue.
func (r *toolRuntime) runWorkflow(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	var args workflowArgs
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("workflow args: %w", err)
	}
	if r.delegationDepth > 0 {
		return "", fmt.Errorf("workflow: only the top-level agent can run a workflow")
	}
	if args.Show {
		// Reading a template spends nothing and runs nothing, so it needs
		// neither sub-agent execution nor a permission that allows a run.
		return r.showWorkflow(args)
	}
	if r.manifest == nil || r.providerMgr == nil {
		return "", fmt.Errorf("workflow: sub-agent execution not configured")
	}
	// Same rule as Ultra: a workflow runs many sub-agents concurrently, and
	// ask-first would turn that into a wall of approval prompts.
	if r.perm() == config.PermissionAskFirst {
		id := strings.TrimSpace(args.ContinueRunID)
		if id != "" && args.Stop {
			// Stopping starts nothing. Refusing it left a run paused on a
			// turn whose agent was ask-first (the user switched to plan or
			// ask to discuss the findings) stranded until the idle reaper.
			return r.continueWorkflow(ctx, id, args)
		}
		if id != "" {
			return "", fmt.Errorf("workflow: continuing run %s starts sub-agents again, which needs restricted or yolo permission (current: ask-first); stop it with {\"continue_run_id\": %q, \"stop\": true}, or ask the user to switch permission", id, id)
		}
		return "", fmt.Errorf("workflow: requires restricted or yolo permission (current: ask-first)")
	}
	if id := strings.TrimSpace(args.ContinueRunID); id != "" {
		return r.continueWorkflow(ctx, id, args)
	}
	if args.Stop || workflowReplyGiven(args.Reply) || strings.TrimSpace(args.CheckpointID) != "" {
		return "", fmt.Errorf("workflow: reply, stop and checkpoint_id only apply with continue_run_id (the run id from the checkpoint result)")
	}
	tier, err := workflowSizeTier(args.Size, r.workflowSize)
	if err != nil {
		return "", err
	}

	script, origin, err := r.resolveWorkflowScript(args)
	if err != nil {
		return "", err
	}
	meta, err := workflow.ParseMeta(script)
	if err != nil {
		return "", fmt.Errorf("workflow: %w", err)
	}

	var scriptArgs any
	if len(args.Args) > 0 {
		if err := json.Unmarshal(args.Args, &scriptArgs); err != nil {
			return "", fmt.Errorf("workflow: args is not valid JSON: %w", err)
		}
	}

	// A run that sets no budget_tokens draws on the turn's "+500k" pool, if
	// there is one. A spent pool refuses the run outright: handing it a
	// token budget of 1 instead would let its first fan-out wave dispatch in
	// full before any spend was recorded, and 0 means "no budget" to the
	// engine — the opposite of a pool that is spent.
	budget := args.BudgetTokens
	var pool *workflowPool
	if budget <= 0 && r.workflowPool != nil {
		left := r.workflowPool.left()
		if left <= 0 {
			return "", fmt.Errorf("workflow: the turn's token budget of %d is spent (%d used); work with what the earlier runs found, or pass budget_tokens to run anyway", r.workflowPool.limit, r.workflowPool.spent())
		}
		budget, pool = left, r.workflowPool
	}

	// Consent comes before anything is written or spent. Typing the keyword is
	// a standing yes; anything else — a plain English "use a workflow", or the
	// model reaching for one on its own — has to clear a run this expensive
	// with the person paying for it.
	decision := r.confirmWorkflow(ctx, meta, args)
	if decision != workflowRun && decision != workflowSaveOnly {
		return "", fmt.Errorf("workflow: the user declined to run %q", meta.Name)
	}
	saveName := strings.TrimSpace(args.SaveAs)
	if saveName == "" && decision == workflowSaveOnly {
		saveName = meta.Name
	}
	savedAt := ""
	if saveName != "" {
		path, err := workflow.Save(r.cwd, saveName, args.SaveScope, script)
		if err != nil {
			return "", fmt.Errorf("workflow: %w", err)
		}
		savedAt = path
	}
	if decision == workflowSaveOnly {
		return renderWorkflowSaved(meta, savedAt) + workflowSaveLint(meta, script), nil
	}

	runID := newWorkflowRunID()
	journal, jerr := workflow.OpenJournal(r.workflowRunDir(runID))
	if jerr != nil {
		// Persistence is a convenience; a run that cannot journal still runs.
		journal = nil
	}
	if journal != nil {
		_ = journal.WriteFile("script.js", script)
		if encoded, err := json.MarshalIndent(meta, "", "  "); err == nil {
			_ = journal.WriteFile("meta.json", string(encoded))
		}
		if args.ResumeFromRunID != "" {
			prior, err := r.findWorkflowRunDir(args.ResumeFromRunID, origin)
			if err != nil {
				_ = journal.Close()
				return "", fmt.Errorf("workflow: %w", err)
			}
			if err := journal.LoadCache(prior); err != nil {
				_ = journal.Close()
				return "", fmt.Errorf("workflow: resume from %s: %w", args.ResumeFromRunID, err)
			}
		}
	}

	size := workflow.ResolveSize(tier)
	maxConcurrency := args.MaxConcurrency
	if maxConcurrency <= 0 {
		// The tier's default (small runs four at a time); 0 leaves the
		// engine's own.
		maxConcurrency = size.Concurrency
	}

	obs := &workflowObserver{rt: r, runID: runID, meta: meta, origin: origin,
		sizeTier: size.Tier, sizeAgents: size.Agents, budget: budget}
	obs.start()
	runner := &workflowRunner{rt: r, runID: runID, pool: pool}
	// The run lives on a context of its own: a run paused at a checkpoint
	// must survive this tool call returning (its context is cancelled the
	// moment it does) and the turn ending. The tool call's context still
	// bounds the wait, and cancelling it while the run is running stops the
	// run (driveWorkflow), so Esc stops a workflow as it always did.
	handle, err := workflow.Start(context.WithoutCancel(ctx), script, workflow.Options{
		Runner:           runner,
		Observer:         obs.handle,
		MaxConcurrency:   maxConcurrency,
		MaxAgents:        workflowMaxAgents,
		MaxItems:         workflowMaxItems,
		BudgetTokens:     budget,
		Journal:          journal,
		DefaultAgentType: defaultWorkflowAgentType(r.manifest),
		Resolve: func(name string) (string, error) {
			src, _, err := workflow.Load(r.cwd, name)
			return src, err
		},
		Args:           scriptArgs,
		Checkpoints:    true,
		AutoCheckpoint: args.AutoCheckpoint,
		SizeTier:       size.Tier,
	})
	if err != nil {
		obs.finish(workflow.Result{}, err)
		_ = journal.Close()
		return "", fmt.Errorf("workflow: %w", err)
	}
	live := &liveWorkflow{
		runID: runID, name: meta.Name,
		handle: handle, journal: journal, observer: obs, runner: runner,
		meta: meta, script: script, origin: origin, dir: r.workflowRunDir(runID),
		savedAt: savedAt, saveName: saveName,
		registry:  r.workflowRegistry(),
		cancel:    handle.Stop,
		finalized: make(chan struct{}),
		busy:      true,
	}
	live.registry.put(live)
	live.watch()
	r.workflowPool.add(live)
	return r.driveWorkflow(ctx, live)
}

// showWorkflow returns a saved script for the model to read and adapt.
//
// Saved workflows are templates the model is told to adapt rather than replay,
// and that needs their source. The file tools cannot always reach it: a global
// template lives under ~/.spettro/workflows, outside the workspace every file
// tool is confined to, and an agent without a shell (ask, plan) has no other
// way in. script_path is accepted from the same roots a run accepts it from.
func (r *toolRuntime) showWorkflow(args workflowArgs) (string, error) {
	if strings.TrimSpace(args.Name) == "" && strings.TrimSpace(args.ScriptPath) == "" {
		return "", fmt.Errorf("workflow: show needs name or script_path (the saved workflow to read)")
	}
	if strings.TrimSpace(args.ScriptPath) != "" {
		if err := r.showablePath(args.ScriptPath); err != nil {
			return "", err
		}
	}
	script, origin, err := r.resolveWorkflowScript(workflowArgs{Name: args.Name, ScriptPath: args.ScriptPath})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	meta, metaErr := workflow.ParseMeta(script)
	if metaErr != nil {
		// Still worth showing: the model may be about to fix it.
		fmt.Fprintf(&b, "<workflow_template path=%q>\n<error>%s</error>\n", origin, metaErr)
	} else {
		fmt.Fprintf(&b, "<workflow_template name=%q path=%q>\n", meta.Name, origin)
		if meta.Description != "" {
			fmt.Fprintf(&b, "<description>%s</description>\n", meta.Description)
		}
		if params := describeWorkflowParams(meta.Params); params != "" {
			fmt.Fprintf(&b, "<params>%s</params>\n", params)
		}
	}
	b.WriteString("<source>\n")
	b.WriteString(truncate(script, 60000))
	b.WriteString("\n</source>\n</workflow_template>\n")
	b.WriteString("Nothing was run. Adapt the script to the task in front of you and run the adapted version inline (script), or run it by name with args when it fits as-is.")
	return b.String(), nil
}

// showablePath limits what show will echo back to workflow scripts proper: a
// file in a saved-workflow folder, or a run's script.js.
//
// show runs before the permission check — reading a template spends nothing —
// and script_path otherwise accepts any file in the workspace or under the
// sessions root, so without this a {"script_path": ".env", "show": true} call
// printed the file verbatim to an agent with no file tools, or one whose
// permission rules deny reading it. Workspace scripts are the file tools' to
// read, under the policy those tools enforce.
func (r *toolRuntime) showablePath(p string) error {
	path, err := r.workflowScriptPath(p)
	if err != nil {
		return err
	}
	if filepath.Ext(path) != ".js" {
		return fmt.Errorf("workflow: show reads workflow scripts only (.js), not %s", p)
	}
	for _, root := range workflow.SearchPaths(r.cwd) {
		if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
	}
	if filepath.Base(path) == "script.js" {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), "journal.jsonl")); err == nil {
			return nil
		}
	}
	return fmt.Errorf("workflow: show reads saved workflows (pass name) and run transcripts (<run>/script.js); read %s with the file tools instead", p)
}

// workflowSizeTier picks a run's size tier: the call's size argument, else
// the configured tier, else medium. A bad size argument is an error the model
// can correct; it is not silently read as medium the way a config typo is.
func workflowSizeTier(arg, configured string) (string, error) {
	if arg = strings.ToLower(strings.TrimSpace(arg)); arg != "" {
		if _, ok := workflow.SizeTiers[arg]; !ok {
			return "", fmt.Errorf("workflow: size %q is not a size tier (use one of %s)", arg, strings.Join(workflow.SizeTierNames, ", "))
		}
		return arg, nil
	}
	return workflow.ResolveSize(configured).Tier, nil
}

// workflowRegistry is where this runtime's live runs are kept: the host's,
// or the turn-local one runToolLoop made. A runtime built without either (a
// test, a direct caller) gets one of its own on first use.
func (r *toolRuntime) workflowRegistry() *WorkflowRuns {
	r.workflowMu.Lock()
	defer r.workflowMu.Unlock()
	if r.workflowRuns == nil {
		r.workflowRuns = NewWorkflowRuns()
	}
	return r.workflowRuns
}

// driveWorkflow waits for the run to pause or settle, on behalf of a tool
// call that has claimed it, and renders what the model reads.
func (r *toolRuntime) driveWorkflow(ctx context.Context, live *liveWorkflow) (string, error) {
	step, err := live.handle.Next(ctx)
	if err != nil {
		// The wait was cut short — Esc, or the tool's deadline — while the
		// run was still running. Only a run paused at a checkpoint may
		// outlive its tool call, so this one is stopped, as before.
		live.handle.Stop()
		live.finalize()
		live.release()
		return "", fmt.Errorf("%w (run %s; transcript at %s)", err, live.runID, live.dir)
	}
	if step.Result != nil {
		live.finalize()
		live.release()
		if step.Err != nil {
			if reason := live.stoppedBecause(); reason != "" {
				// Stopped under the call — the host ended the session —
				// not failed: say so rather than hand back a bare
				// "context canceled".
				return "", fmt.Errorf("workflow: run %s was stopped (%s) before the script finished; its journal at %s stays resumable with resume_from_run_id", live.runID, reason, live.dir)
			}
			return "", fmt.Errorf("%w (run %s; transcript at %s)", step.Err, live.runID, live.dir)
		}
		out := renderWorkflowResult(live.runID, live.dir, live.origin, live.meta, *step.Result, live.runner.mergeNotes())
		out += workflowSizeNote(live.observer.sizeTier, live.observer.sizeAgents, step.Result.Agents-step.Result.Cached)
		if live.savedAt != "" {
			out += fmt.Sprintf("\nSaved as a reusable workflow at %s — it can be re-run with /workflows run %s.", live.savedAt, live.saveName)
			out += workflowSaveLint(live.meta, live.script)
		}
		return out, nil
	}

	cp := *step.Checkpoint
	live.markPaused(cp)
	live.observer.paused(cp)
	// Detach before returning: this turn's callbacks must not be reached
	// through the run once the call is over, and nothing runs while paused.
	live.bind(nil)
	live.release()
	snap := live.handle.Snapshot()
	return renderWorkflowCheckpoint(live.runID, live.meta, cp, snap, live.registry.isTurnLocal()) +
		workflowSizeNote(live.observer.sizeTier, live.observer.sizeAgents, snap.Agents-snap.Cached), nil
}

// workflowSizeNote tells the model, in the result it is about to read, that a
// run went past the size guideline and by how much.
//
// The guideline is in the system prompt, but a prompt line read once before
// the script is written is easy to lose: a live run on the small tier (~5
// agents) started fifteen, and the engine's one-off log line about it sat
// among a dozen others. Feedback on the run itself is what changes the next
// script. Replayed agents cost nothing, so only the ones that ran count.
func workflowSizeNote(tier string, guideline, ran int) string {
	if guideline <= 0 || ran <= guideline {
		return ""
	}
	return fmt.Sprintf("\nSize: this run started %d agents against the %s guideline of ~%d. Size the next workflow within it — fewer, broader agents, or plan(…, {max}) — unless the task plainly needs more.", ran, tier, guideline)
}

// continueWorkflow answers (or stops) a run paused at a checkpoint. There is
// no consent prompt: the run was approved when it started, and the
// orchestrator answering its question is not a new spend decision.
func (r *toolRuntime) continueWorkflow(ctx context.Context, runID string, args workflowArgs) (string, error) {
	live := r.workflowRegistry().get(runID)
	if live == nil {
		return "", r.unknownWorkflowRunError(runID)
	}
	if err := live.claim(); err != nil {
		return "", err
	}
	cp, paused := live.handle.Pending()
	if want := strings.TrimSpace(args.CheckpointID); paused && want != "" && want != cp.ID {
		live.release()
		return "", fmt.Errorf("workflow: run %s is paused at %s, not %s — continue with checkpoint_id %q, or omit checkpoint_id", runID, cp.ID, want, cp.ID)
	}

	// The run now belongs to this turn: its traces, approvals and questions
	// go to the live host, not to the turn that started it.
	live.bind(r)
	live.observer.resumed()

	if args.Stop || (paused && cp.Auto && workflowReplyStops(args.Reply)) {
		live.stopWith(workflowStopOrchestrator)
		live.finalize()
		live.release()
		return renderWorkflowStopped(live), nil
	}
	if paused {
		var reply any
		if workflowReplyGiven(args.Reply) {
			reply = args.Reply
		}
		if err := live.handle.Resume(cp.ID, reply); err != nil {
			// Still paused (or settled under us): hand it back as it was.
			live.observer.paused(cp)
			live.bind(nil)
			live.release()
			return "", fmt.Errorf("workflow: %w", err)
		}
		live.markRunning()
	}
	return r.driveWorkflow(ctx, live)
}

// workflowReplyGiven reports whether the call carries a reply. An explicit
// null counts as none: it is what a client serialising an unset field sends.
func workflowReplyGiven(reply json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(reply))
	return trimmed != "" && trimmed != "null"
}

// workflowReplyStops reports whether an automatic checkpoint's reply asks to
// stop the run: {"stop": true}. Only automatic checkpoints read it — the
// script never sees their reply — while a script's own checkpoint() gets
// whatever the orchestrator sent, stop key or not.
func workflowReplyStops(reply json.RawMessage) bool {
	var v struct {
		Stop bool `json:"stop"`
	}
	return workflowReplyGiven(reply) && json.Unmarshal(reply, &v) == nil && v.Stop
}

// renderWorkflowStopped is the result of a run the orchestrator stopped: what
// it did up to the stop, and how to pick it up again.
func renderWorkflowStopped(live *liveWorkflow) string {
	res := live.handle.Snapshot()
	res.Value = nil
	return fmt.Sprintf("The %q workflow (run %s) was stopped at your request before the script finished; the counts below are what it did up to the stop.\n", live.meta.Name, live.runID) +
		renderWorkflowResult(live.runID, live.dir, live.origin, live.meta, res, live.runner.mergeNotes())
}

// unknownWorkflowRunError explains a continue_run_id that names no live run.
// The run may have finished, been reaped, or died with the process — in which
// case its journal is still on disk and resume_from_run_id is the way back.
func (r *toolRuntime) unknownWorkflowRunError(runID string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "workflow: run %q is not paused at a checkpoint in this session", runID)
	if reason, ok := r.workflowRegistry().endedReason(runID); ok {
		fmt.Fprintf(&b, " (it was %s)", reason)
	}
	if ids := r.workflowRegistry().ids(); len(ids) > 0 {
		fmt.Fprintf(&b, "; live runs: %s", strings.Join(ids, ", "))
	}
	b.WriteString(".")
	if dir, err := r.findWorkflowRunDir(runID, ""); err == nil {
		fmt.Fprintf(&b, " Its journal is kept at %s: re-run its script with script_path=%q and resume_from_run_id=%q, and the finished agents and answered checkpoints replay instead of re-running.",
			dir, filepath.Join(dir, "script.js"), runID)
	}
	return errors.New(b.String())
}

// workflowArgsRe spots a script reading its args global. It is a lint, not a
// parser: a mention in a comment counts, which only ever spares a script the
// note below.
var workflowArgsRe = regexp.MustCompile(`\bargs\b`)

// workflowSaveLint is the note a saved script gets when nothing in it can
// vary between runs. Saved workflows are templates; one with no params that
// never reads args replays the same hardcoded work every time, which is
// almost never what saving it was for. A note, not a refusal: the script is
// already saved, and some workflows really are fixed.
func workflowSaveLint(meta workflow.Meta, script string) string {
	if len(meta.Params) > 0 || workflowArgsRe.MatchString(script) {
		return ""
	}
	return "\nSaved without meta.params — it will replay the same hardcoded work every time; consider declaring params so it adapts."
}

// workflowDecision is what the user said about starting a run.
type workflowDecision int

const (
	workflowRun workflowDecision = iota
	workflowSaveOnly
	workflowDeclined
)

// confirmWorkflow asks before spending on a run that was not pre-authorised.
//
// When nobody can be asked — headless, a goal loop, a relay — the request that
// turned workflows on this turn is taken as the answer. The alternative is
// hanging on a prompt no one will see, or silently refusing work the user
// explicitly asked for; neither is better than trusting the request that got
// us here.
func (r *toolRuntime) confirmWorkflow(ctx context.Context, meta workflow.Meta, args workflowArgs) workflowDecision {
	if r.workflowPreapproved || r.askUser == nil {
		return workflowRun
	}
	phases := make([]string, 0, len(meta.Phases))
	for _, p := range meta.Phases {
		phases = append(phases, p.Title)
	}
	detail := meta.Description
	if len(phases) > 0 {
		detail += "\nPhases: " + strings.Join(phases, " → ")
	}
	if params := describeWorkflowParams(meta.Params); params != "" {
		detail += "\nParams: " + params
	}
	saveLabel := "Save it, don't run"
	saveName := strings.TrimSpace(args.SaveAs)
	if saveName == "" {
		saveName = meta.Name
	}
	form := AskUserForm{
		Context: detail,
		Questions: []AskUserQuestion{{
			Header:   "Workflow",
			Question: fmt.Sprintf("Run the %q workflow? It spawns sub-agents, so it costs real tokens.", meta.Name),
			Options: []AskUserOption{
				{Label: "Run it", Description: "Start the workflow now.", IsRecommended: true},
				{Label: saveLabel, Description: "Write it to .spettro/workflows/" + saveName + ".js for later, and do the work another way."},
				{Label: "Don't run it", Description: "Skip the workflow; handle the task directly."},
			},
		}},
	}
	answers, err := r.askUser(ctx, form)
	if err != nil || len(answers) == 0 {
		// A transport that cannot ask must not become a silent refusal.
		if errors.Is(err, ErrAskUserReplyInChat) {
			return workflowDeclined
		}
		return workflowRun
	}
	a := answers[0]
	if a.Skipped {
		return workflowDeclined
	}
	choice := strings.ToLower(strings.TrimSpace(strings.Join(a.Selected, " ") + " " + a.Custom))
	switch {
	case strings.Contains(choice, "save"):
		return workflowSaveOnly
	case strings.Contains(choice, "don't") || strings.Contains(choice, "dont") ||
		strings.Contains(choice, "no") || strings.Contains(choice, "skip") || strings.Contains(choice, "cancel"):
		return workflowDeclined
	}
	return workflowRun
}

// describeWorkflowParams lists declared params for the consent prompt:
// "base (string, default "main") — branch to diff against; focus (any,
// required)".
func describeWorkflowParams(params []workflow.ParamMeta) string {
	parts := make([]string, 0, len(params))
	for _, p := range params {
		typ := p.Type
		if typ == "" {
			typ = "any"
		}
		attrs := []string{typ}
		if p.Required {
			attrs = append(attrs, "required")
		}
		if p.Default != nil {
			if encoded, err := json.Marshal(p.Default); err == nil {
				attrs = append(attrs, "default "+truncate(string(encoded), 60))
			}
		}
		part := fmt.Sprintf("%s (%s)", p.Name, strings.Join(attrs, ", "))
		if d := strings.TrimSpace(p.Description); d != "" {
			part += " — " + d
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// renderWorkflowSaved is what the model reads when the user chose to keep the
// script but not run it.
func renderWorkflowSaved(meta workflow.Meta, path string) string {
	return fmt.Sprintf("The user chose not to run the %q workflow. It is saved at %s and can be run later with /workflows run %s.\n"+
		"Do not run it anyway. Continue with the task directly, or ask what they would prefer.",
		meta.Name, path, meta.Name)
}

// resolveWorkflowScript picks the script to run: inline source, a file path
// (how an edited script is re-run), or a saved workflow by name.
func (r *toolRuntime) resolveWorkflowScript(args workflowArgs) (script, origin string, err error) {
	switch {
	case strings.TrimSpace(args.ScriptPath) != "":
		path, err := r.workflowScriptPath(args.ScriptPath)
		if err != nil {
			return "", "", err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("workflow: read script_path: %w", err)
		}
		return string(data), path, nil
	case strings.TrimSpace(args.Name) != "":
		src, path, err := workflow.Load(r.cwd, args.Name)
		if err != nil {
			return "", "", fmt.Errorf("workflow: %w", err)
		}
		return src, path, nil
	case strings.TrimSpace(args.Script) != "":
		return args.Script, "inline", nil
	}
	return "", "", fmt.Errorf("workflow: pass script (inline source), script_path, or name")
}

// workflowRunDir is where a run's script, journal and result live. It sits
// under the session directory so /storage accounts for it and prunes it with
// the conversation it belongs to.
func (r *toolRuntime) workflowRunDir(runID string) string {
	if base := strings.TrimSpace(r.sessionDir); base != "" {
		return filepath.Join(base, "workflows", runID)
	}
	// Without a session (headless one-shots, tests) transcripts fall back into
	// the project. They must not land in .spettro/workflows: that folder holds
	// the user's reusable scripts, and filling it with run directories would
	// bury them.
	return filepath.Join(r.cwd, ".spettro", "workflow-runs", runID)
}

// workflowScriptPath resolves a script_path argument to an absolute path.
//
// A relative path is relative to the agent's workspace, like every other
// tool path; it is never resolved against spettro's process directory,
// which is not the workspace under ACP (each session has its own cwd) or for
// a sub-agent in a worktree. The result must lie in the workspace, in one of
// the session run directories (where every run keeps its script.js, the
// documented way to resume an edited script, see findWorkflowRunDir), or in
// a saved-workflow folder; anything else is refused, as the file tools
// refuse paths outside the workspace.
func (r *toolRuntime) workflowScriptPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.cwd, p)
	}
	p = filepath.Clean(p)
	if _, _, err := r.resolvePath(p); err == nil {
		return p, nil
	}
	roots := workflow.SearchPaths(r.cwd)
	if r.sessionDir != "" {
		// The parent of this session's directory holds every session,
		// since a run being resumed is often from another one.
		roots = append(roots, filepath.Dir(filepath.Clean(r.sessionDir)))
	}
	for _, root := range roots {
		if rel, err := filepath.Rel(root, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return p, nil
		}
	}
	return "", fmt.Errorf("workflow: script_path %q is outside the workspace, the session's workflow runs and the saved-workflow folders", p)
}

// findWorkflowRunDir locates a previous run by id.
//
// Run directories are session-scoped, but the run being resumed very often is
// not from this session — an editor opens a fresh one per prompt, and a
// crashed run is exactly the case you want to resume from a new session. So
// the current session is only the first place to look: the script's own
// directory is checked next (re-running <run>/script.js is the documented way
// to resume an edited script), then the sibling sessions, since run ids are
// unique. Not finding it is an error rather than an empty cache: silently
// replaying nothing would re-run every agent at full price under a flag that
// promised the opposite.
func (r *toolRuntime) findWorkflowRunDir(runID, scriptPath string) (string, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" || strings.ContainsAny(runID, `/\`) || strings.Contains(runID, "..") {
		return "", fmt.Errorf("invalid resume_from_run_id %q", runID)
	}
	var candidates []string
	if dir := r.workflowRunDir(runID); dir != "" {
		candidates = append(candidates, dir)
	}
	if scriptPath != "" {
		if parent := filepath.Dir(scriptPath); filepath.Base(parent) == runID {
			candidates = append(candidates, parent)
		}
	}
	if sessions := filepath.Dir(strings.TrimRight(r.sessionDir, string(filepath.Separator))); sessions != "" && r.sessionDir != "" {
		if entries, err := os.ReadDir(sessions); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					candidates = append(candidates, filepath.Join(sessions, e.Name(), "workflows", runID))
				}
			}
		}
	}
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "journal.jsonl")); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("resume_from_run_id %q: no journal found for that run (looked under this session and its siblings)", runID)
}

// newWorkflowRunID mints a run id that sorts by start time and never collides.
// The random tail is not decoration: the timestamp has one-second resolution,
// so concurrent runs — or two spettro processes in the same project — would
// otherwise share a run directory and interleave their journals.
func newWorkflowRunID() string {
	return fmt.Sprintf("wf_%s_%s", time.Now().UTC().Format("20060102150405"), uuid.NewString()[:8])
}

// workflowRunner executes one agent() call as a Spettro sub-agent.
type workflowRunner struct {
	// rt is the runtime of the turn driving the run; every sub-agent takes
	// its callbacks (traces, approvals, questions, checkpoints) from it. A run
	// continued in a later turn is rebound to that turn's runtime, and while
	// it sits paused it is bound to nothing (see liveWorkflow.bind).
	bindMu sync.RWMutex
	rt     *toolRuntime
	runID  string
	// pool is the turn budget the run draws on (nil: the run has its own
	// budget_tokens, or the turn set no directive). It is checked before
	// every agent starts, so a pool spent by another run — or by this one
	// before it paused — stops this run's dispatches too; see workflowPool.
	pool *workflowPool

	// spaces holds the isolated worktree for each in-flight call, keyed by the
	// engine's call index. It is keyed per call rather than per attempt
	// because the engine retries a schema call until its answer parses, and a
	// worktree per attempt means two branches of overlapping edits for one
	// agent — which is how a live run lost a merge.
	mu     sync.Mutex
	spaces map[int]*agentWorkspace
	// notes collects workspaces that did not merge cleanly, so the run's
	// result can tell the model where the stranded work is. They cannot ride
	// on the agent's answer: by the time EndCall runs that answer has already
	// been parsed against the call's schema.
	notes []string
}

// errWorkflowDetached is what a call reaching a run nobody is driving gets.
// Quiescence keeps it theoretical — no agent starts while a run is paused —
// but a sub-agent must fail cleanly rather than call into a finished turn.
var errWorkflowDetached = errors.New("workflow: the run is paused and no turn is attached to it")

func (w *workflowRunner) runtime() *toolRuntime {
	w.bindMu.RLock()
	defer w.bindMu.RUnlock()
	return w.rt
}

func (w *workflowRunner) rebind(rt *toolRuntime) {
	w.bindMu.Lock()
	w.rt = rt
	w.bindMu.Unlock()
}

func (w *workflowRunner) mergeNotes() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.notes...)
}

// BeginCall creates the call's isolated workspace, once, before the first
// attempt.
func (w *workflowRunner) BeginCall(ctx context.Context, req workflow.Request) error {
	if w.pool != nil && w.pool.left() <= 0 {
		// Refused before the worktree is made, not just before the agent.
		return w.pool.errSpent()
	}
	if req.Isolation != "worktree" {
		return nil
	}
	rt := w.runtime()
	if rt == nil {
		return errWorkflowDetached
	}
	ws, err := rt.newSubagentWorkspace(ctx, req.Instance)
	if err != nil {
		return fmt.Errorf("workflow: %w", err)
	}
	w.mu.Lock()
	if w.spaces == nil {
		w.spaces = map[int]*agentWorkspace{}
	}
	w.spaces[req.Index] = ws
	w.mu.Unlock()
	return nil
}

// EndCall folds the workspace back once the call is finished, however many
// attempts it took.
//
// A failed call's work is preserved rather than merged; a successful one is
// merged back. Both run on an uncancellable context so a cancelled workflow
// still cleans its worktrees up instead of leaking them.
func (w *workflowRunner) EndCall(ctx context.Context, req workflow.Request, runErr error) {
	ws := w.takeWorkspace(req.Index)
	if ws == nil {
		return
	}
	mergeCtx := context.WithoutCancel(ctx)
	if runErr != nil || ctx.Err() != nil {
		ws.abandon(mergeCtx)
		return
	}
	rt := w.runtime()
	if rt == nil {
		// Nobody to snapshot for or report to: keep the work on its branch
		// rather than merging it into a checkout no turn is watching.
		ws.abandon(mergeCtx)
		return
	}
	// The merge writes into the main checkout, which the call's own
	// snapshots (taken in its worktree) never covered.
	rt.checkpointStep(workflowToolID)
	if m := ws.finalize(mergeCtx); m.Status != "merged" && m.Status != "no_changes" {
		// Anything that is not a clean merge has to reach both the user and
		// the model: silently dropping it leaves work on a branch nobody
		// knows about.
		note := fmt.Sprintf("%s: workspace merge %s — branch %q kept at %s%s",
			req.Instance, m.Status, m.Branch, m.Path, mergeDetail(m))
		w.mu.Lock()
		w.notes = append(w.notes, note)
		w.mu.Unlock()
		rt.emitWorkflowMergeNote(req, note)
	}
}

func (w *workflowRunner) takeWorkspace(index int) *agentWorkspace {
	w.mu.Lock()
	defer w.mu.Unlock()
	ws := w.spaces[index]
	delete(w.spaces, index)
	return ws
}

func (w *workflowRunner) workspaceFor(index int) *agentWorkspace {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.spaces[index]
}

func (w *workflowRunner) RunAgent(ctx context.Context, req workflow.Request) (workflow.Response, error) {
	r := w.runtime()
	if r == nil {
		return workflow.Response{}, errWorkflowDetached
	}
	// Checked per attempt as well as per call (BeginCall): a schema retry is
	// another full agent run, and the pool may have run out in between.
	if w.pool != nil && w.pool.left() <= 0 {
		return workflow.Response{}, w.pool.errSpent()
	}
	spec, err := resolveWorkflowTarget(r.manifest, req.AgentType)
	if err != nil {
		return workflow.Response{}, err
	}
	if p := r.perm(); p != "" {
		spec.Permission = p
	}
	cwd := r.cwd
	if ws := w.workspaceFor(req.Index); ws != nil {
		cwd = ws.subCWD
	}

	sub := LLMAgent{
		Spec:            spec,
		InstanceID:      req.Instance,
		PermissionFn:    r.permissionFn,
		ProviderManager: r.providerMgr,
		ProviderName:    r.providerName,
		ModelName:       r.modelName,
		CWD:             cwd,
		MaxTokens:       r.maxTokens,
		MaxOutputTokens: r.maxOutputTokens,
		Thinking:        r.subAgentThinking(),
		Compact:         r.compactCfg,
		parentSnapshot:  r.sessionCtx,
		parentCWD:       r.cwd,
		ToolCallback:    r.toolCallback,
		Checkpoint:      r.subagentCheckpoint(cwd),
		ShellApproval:   r.shellApproval,
		AskUser:         r.askUser,
		Manifest:        r.manifest,
		SandboxState:    r.sandboxState,
		SessionDir:      r.sessionDir,
		DelegationDepth: r.delegationDepth + 1,
		ParentAgentID:   r.agentID,
	}
	// A script may pin a call to a cheaper or stronger model than the session
	// is on ("scan with the fast one, judge with the strong one"), and to a
	// different reasoning effort. Both are per-call overrides; unset means
	// inherit, which is almost always what a script wants.
	if req.Model != "" {
		model := req.Model
		sub.ModelName = func() string { return model }
	}
	if level, ok := workflowEffort(req.Effort); ok {
		sub.Thinking = level
	}

	return w.runWithRetries(ctx, sub, req.Prompt)
}

// emitWorkflowMergeNote surfaces a workspace that did not merge cleanly. It
// goes out as a trace rather than being appended to the agent's answer,
// because by the time EndCall runs the answer has already been parsed against
// the call's schema and appending prose to it would break that contract.
func mergeDetail(m workspaceMerge) string {
	if strings.TrimSpace(m.Detail) == "" {
		return ""
	}
	return " — " + truncate(m.Detail, 400)
}

func (r *toolRuntime) emitWorkflowMergeNote(req workflow.Request, note string) {
	if r.toolCallback == nil {
		return
	}
	args, _ := json.Marshal(map[string]any{"kind": "log", "phase": req.Phase})
	r.toolCallback(ToolTrace{
		AgentID: req.Instance,
		Name:    workflowProgressTraceName,
		Status:  "error",
		Args:    string(args),
		Output:  note,
	})
}

// runWithRetries retries transient provider failures per agent, matching the
// Ultra backoff so a rate-limited swarm behaves the same either way.
func (w *workflowRunner) runWithRetries(ctx context.Context, sub LLMAgent, prompt string) (workflow.Response, error) {
	var lastErr error
	for attempt := range workflowMaxAttempts {
		if attempt > 0 {
			if !ultraSleep(ctx, workflowRetryBase<<(attempt-1)) {
				return workflow.Response{}, ctx.Err()
			}
		}
		result, err := sub.Run(ctx, prompt)
		if err == nil {
			return workflow.Response{Text: strings.TrimSpace(result.Content), Tokens: result.TokensUsed}, nil
		}
		lastErr = err
		if ctx.Err() != nil || !rerunSubagentAfter(err) {
			break
		}
	}
	return workflow.Response{}, lastErr
}

// resolveWorkflowTarget picks the manifest agent an agent() call runs as. It
// mirrors the ultra rule — workers and subagents only, never an orchestrator —
// so a script cannot smuggle a nested swarm in through a phase.
//
// The default is the general-purpose subagent rather than the code worker:
// workflow stages are usually "read this and judge it", and a specialist would
// be the wrong shape for most of them. Manifests predating that agent fall
// back to whatever ultra would have picked.
func resolveWorkflowTarget(manifest *config.AgentManifest, agentType string) (config.AgentSpec, error) {
	target := strings.TrimSpace(agentType)
	if target == "" {
		target = "general-purpose"
		if _, ok := manifest.AgentByID(target); !ok {
			target = ""
		}
	}
	spec, err := resolveUltraTarget(manifest, target)
	if err != nil {
		return spec, fmt.Errorf("workflow: %s", strings.TrimPrefix(err.Error(), "ultra: "))
	}
	return spec, nil
}

// defaultWorkflowAgentType is the agent an unqualified agent() call resolves
// to, resolved once so instance names in the panel read "general-purpose#7"
// instead of a generic "agent#7".
func defaultWorkflowAgentType(manifest *config.AgentManifest) string {
	spec, err := resolveWorkflowTarget(manifest, "")
	if err != nil {
		return ""
	}
	return spec.ID
}

func workflowEffort(effort string) (provider.ThinkingLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "off", "none":
		return provider.ThinkingOff, true
	case "low":
		return provider.ThinkingLow, true
	case "medium":
		return provider.ThinkingMedium, true
	case "high":
		return provider.ThinkingHigh, true
	case "xhigh", "x-high":
		return provider.ThinkingXHigh, true
	case "max":
		return provider.ThinkingMax, true
	}
	return "", false
}
