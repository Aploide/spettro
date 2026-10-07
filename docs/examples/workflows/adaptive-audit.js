export const meta = {
  name: 'adaptive-audit',
  description: 'Audit part of the repo for one class of defect: plan the sweep at runtime, refute every finding, sweep until dry, and ask the orchestrator before fixing anything',
  whenToUse: 'when the work-list is unknown up front ("find every X in Y") and findings must survive verification before anyone acts on them',
  params: {
    concern: { type: 'string', required: true, description: 'what to look for, e.g. "errors that are checked but then dropped"' },
    scope: { type: 'string', default: '.', description: 'directory or package to audit' },
    fix: { type: 'boolean', default: false, description: 'offer to fix confirmed findings; the orchestrator still decides at a checkpoint' },
  },
  // Only the stages every run has are declared. Sweep rounds and the Fix
  // stage are added with phase() when, and only if, the run reaches them.
  phases: [
    { title: 'Plan', detail: 'one agent splits the scope into slices' },
    { title: 'Audit', detail: 'find, then refute, per slice' },
  ],
}

const { concern, scope, fix } = args

const FINDINGS = {
  type: 'object',
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          title: { type: 'string' },
          file: { type: 'string' },
          line: { type: 'integer' },
          why: { type: 'string' },
        },
        required: ['title', 'file', 'why'],
      },
    },
  },
  required: ['findings'],
}

const VERDICT = {
  type: 'object',
  properties: { refuted: { type: 'boolean' }, reason: { type: 'string' } },
  required: ['refuted'],
}

// Two findings are the same finding when they name the same place for the
// same reason; untilDry dedupes every round against everything seen so far
// (refuted findings included), which is what lets the loop converge.
const key = f => `${f.file}:${f.line || ''}:${f.title}`.toLowerCase()

// A small run gets one skeptic per finding; anything bigger gets two, and a
// finding survives only if neither can refute it.
const SKEPTICS = size.tier === 'small' ? 1 : 2

const find = (slice, where) => agent(
  `${slice.prompt}\n\nAudit only for: ${concern}\n` +
  `Read the code before claiming anything. Every finding must name a file, a line when there is one, ` +
  `and the concrete way the code goes wrong. Report nothing rather than something speculative.`,
  { label: `find: ${slice.label}`, phase: where, schema: FINDINGS })

const verify = (finding, where) => parallel(Array.from({ length: SKEPTICS }, (_, i) => () =>
  agent(
    `An auditor looking for "${concern}" claims this about the repository:\n` +
    `${JSON.stringify(finding, null, 2)}\n\n` +
    `Your job is to REFUTE it. Read ${finding.file} and everything it calls. Set refuted=true if the ` +
    `claim is wrong, already handled elsewhere, or cannot happen. Default to refuted=true when unsure.`,
    { label: `refute${SKEPTICS > 1 ? ' #' + (i + 1) : ''}: ${finding.title}`, phase: where, schema: VERDICT })))
  .then(verdicts => ({ ...finding, verdicts, real: verdicts.every(v => v && v.refuted === false) }))

phase('Plan')
const slices = await plan(
  `Split ${scope} in this repository into independent slices to audit for: ${concern}.\n` +
  `List the directories and files first, then group them so each slice is a sensible unit of work for one ` +
  `agent. Each task's prompt must say exactly which paths that agent reads.`,
  { label: 'plan the audit', phase: 'Plan' })

if (slices.length === 0) {
  log('planning produced no slices; nothing to audit')
  return { concern, scope, confirmed: [], raised: 0 }
}
log(`${slices.length} slices planned`)

// The first round audits the planned slices. Every later round plans fresh
// angles from what has been seen, and untilDry stops after two rounds that
// turn up nothing new, after eight rounds, or when the token budget runs out.
//
// A finding already seen is not verified again: the skeptics are the
// expensive part of a round. "Seen" has to include this round too. The
// slices' find and verify stages run concurrently, so two slices reporting
// the same finding (or one slice reporting it twice) would each send it to
// its own skeptics, and untilDry would only drop the duplicate after both
// were paid for. Stage callbacks run one at a time on the script's single
// thread, so checking and recording a key in the same synchronous filter is
// enough to claim it: the first copy is verified, every later one skipped.
//
// The Audit phase is entered here, not merely named in each agent's opts:
// only phase() moves the run into a phase, so without it round one's agents
// and log lines would belong to Plan, the result's phase list would skip
// Audit, and an auto_checkpoint run would never pause between the two.
phase('Audit')
const known = new Set()
let sweeps = 0
const raised = await untilDry(async (_round, seen) => {
  let where = 'Audit'
  let work = slices
  if (sweeps++ > 0) {
    where = `Sweep ${sweeps - 1}`
    phase(where, { detail: `new angles on what ${seen.length} earlier findings missed` })
    work = await plan(
      `An audit of ${scope} for "${concern}" has already raised these findings:\n` +
      seen.map(f => `- ${f.file}:${f.line || '?'} ${f.title}`).join('\n') +
      `\n\nName slices or search strategies that the earlier passes probably missed: other packages, ` +
      `other call paths, other spellings of the same mistake. Return no tasks if you think coverage is complete.`,
      { label: `plan ${where}`, phase: where })
  }
  const out = await pipeline(
    work,
    slice => find(slice, where),
    found => parallel(((found && found.findings) || [])
      .filter(f => {
        const k = key(f)
        if (known.has(k)) return false
        known.add(k)
        return true
      })
      .map(f => () => verify(f, where))),
  )
  return out.flat().filter(Boolean)
}, { key })

const confirmed = raised.filter(f => f.real)
log(`${raised.length} findings raised, ${confirmed.length} survived refutation`)

const report = confirmed.map((f, id) => ({ id, title: f.title, file: f.file, line: f.line, why: f.why }))
if (!fix || confirmed.length === 0) {
  return { concern, scope, confirmed: report, raised: raised.length }
}

// Nothing is changed without the orchestrator reading the verified list
// first. The reply is whatever the model passed as `reply` when it
// continued the run: {fix: [ids] | "all", notes?}. A host with no
// orchestrator resolves this to null, and null fixes nothing.
const reply = await checkpoint(
  `${confirmed.length} findings survived refutation. Which should be fixed? ` +
  `Reply {"fix": [ids]} or {"fix": "all"}, optionally with "notes" for the fixers; anything else stops here.`,
  { confirmed: report })

const chosen = reply && reply.fix === 'all' ? report
  : (reply && Array.isArray(reply.fix) ? reply.fix.map(id => report[id]).filter(Boolean) : [])
if (chosen.length === 0) {
  log('no fixes approved')
  return { concern, scope, confirmed: report, raised: raised.length, fixed: [] }
}

phase('Fix', { detail: `${chosen.length} findings approved by the orchestrator` })
const fixed = await parallel(chosen.map(f => () =>
  agent(
    `Fix this verified defect in the repository:\n${JSON.stringify(f, null, 2)}\n\n` +
    (reply.notes ? `Guidance from the orchestrator: ${reply.notes}\n\n` : '') +
    `Make the smallest change that removes the defect, add or update a test that would have caught it, ` +
    `and run that test. Reply with what you changed and the test result.`,
    { label: `fix: ${f.title}`, phase: 'Fix', isolation: 'worktree' })
    .then(summary => summary && { id: f.id, title: f.title, summary })))

return { concern, scope, confirmed: report, raised: raised.length, fixed: fixed.filter(Boolean) }
