package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Post-edit diagnostics: the block file-write / file-edit / multi-edit append
// to their result, so the model sees the compile errors it just introduced
// and fixes them in its next step instead of discovering them at build time.

const (
	// defaultSettle is how long the server must stay quiet after its first
	// publish before the set counts as final. Most servers publish once per
	// change; the window catches the ones that follow up quickly.
	defaultSettle = 300 * time.Millisecond
	// MaxPostEditErrors caps the errors listed for the edited file.
	MaxPostEditErrors = 20
	// maxOtherFiles caps the other files named in the summary line.
	maxOtherFiles = 5
	// maxDiagMessage caps one diagnostic's message; TypeScript in particular
	// produces multi-paragraph messages whose first lines say it all.
	maxDiagMessage = 300
	// postEditGrace is how long past its deadline PostEditDiagnostics waits
	// for the report of work that honoured the deadline itself.
	postEditGrace = 150 * time.Millisecond
)

// settleFor returns the quiet window for a server key.
func settleFor(key string) time.Duration {
	if b, ok := builtinServers[key]; ok && b.settle > 0 {
		return b.settle
	}
	return defaultSettle
}

// fileErrors is the error count of one file other than the edited one.
type fileErrors struct {
	rel   string
	count int
}

// postEditReport is what one post-edit diagnostics pass found.
type postEditReport struct {
	server string // "gopls"; empty when no server handles the file
	rel    string // edited file, workspace-relative

	skip     bool // nothing worth saying: no server for the file, or it failed
	starting bool // the server was still starting when the budget ran out
	noAnswer bool // the server published nothing for the file within the budget

	errors []Diagnostic // the edited file's errors
	others []fileErrors // other files with errors, sorted by path
}

// PostEditDiagnostics syncs the just-written file to its language server and
// returns a short report of the errors in it, plus a count of errors in other
// files, for appending to an edit tool's result. also names other files the
// same tool call wrote (the rest of a rename), which are synced first. It
// returns "" when there is nothing to report: a clean file, or no server for
// the file type. It never takes meaningfully longer than ctx allows, even
// when the server is wedged.
func (m *Manager) PostEditDiagnostics(ctx context.Context, absPath string, also ...string) string {
	ch := make(chan string, 1)
	go func() { ch <- m.postEdit(ctx, absPath, also).format() }()
	select {
	case out := <-ch:
		return out
	case <-ctx.Done():
	}
	// The pass honours ctx itself and reports what it has at the deadline
	// (a write to a server that stopped reading its stdin gives up then and
	// abandons that server, see Client.write). The grace is a backstop in
	// case some step still overruns: the edit never waits on it for long.
	select {
	case out := <-ch:
		return out
	case <-time.After(postEditGrace):
		return ""
	}
}

func (m *Manager) postEdit(ctx context.Context, absPath string, also []string) postEditReport {
	absPath = realPath(absPath)
	rep := postEditReport{rel: m.relPath(absPath)}
	c, key, err := m.clientFor(ctx, absPath)
	if key != "" {
		rep.server = m.serverName(key)
	}
	if err != nil {
		// A missing or crashed server is not the model's problem to solve;
		// only a start in progress is worth a word, so it knows the silence
		// does not mean the file is clean.
		rep.starting = errors.Is(err, ErrServerStarting)
		rep.skip = !rep.starting
		return rep
	}
	d, content, err := m.syncFromDisk(ctx, c, key, absPath, also...)
	if err == nil {
		err = c.didSave(ctx, d, content)
	}
	if err != nil {
		rep.skip = true
		return rep
	}
	ds, fresh := c.waitSettled(ctx, d, settleFor(key))
	if !fresh {
		rep.noAnswer = true
		return rep
	}
	rep.errors = errorsOnly(ds)
	rep.others = m.otherFileErrors(c, d.key)
	return rep
}

// isError reports whether a diagnostic is an error. A missing severity is
// left to the client to interpret by the spec; editors show it as an error.
func isError(d Diagnostic) bool { return d.Severity == 1 || d.Severity == 0 }

func errorsOnly(ds []Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, d := range ds {
		if isError(d) {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Range.Start, out[j].Range.Start
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Character < b.Character
	})
	return out
}

// otherFileErrors counts the errors the server currently reports for files
// other than the edited one, limited to files that still exist inside the
// workspace: a deleted file's last publish is stale, and errors in
// dependencies outside the root are not the model's to fix.
func (m *Manager) otherFileErrors(c *Client, editedKey string) []fileErrors {
	var out []fileErrors
	for path, ds := range c.allDiagnostics() {
		if foldPath(path) == editedKey {
			continue
		}
		n := 0
		for _, d := range ds {
			if isError(d) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		rel, ok := relTo(m.root, path)
		if !ok {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		out = append(out, fileErrors{rel: rel, count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

func (r postEditReport) format() string {
	switch {
	case r.skip:
		return ""
	case r.starting:
		return fmt.Sprintf("(%s is still starting, so %s was not checked for errors yet; later edits will be.)", r.server, r.rel)
	case r.noAnswer:
		return fmt.Sprintf("(%s reported no diagnostics for %s in time; it was not checked for errors.)", r.server, r.rel)
	case len(r.errors) == 0 && len(r.others) == 0:
		return ""
	}
	var sb strings.Builder
	if len(r.errors) > 0 {
		fmt.Fprintf(&sb, "Diagnostics (errors) in %s:\n", r.rel)
		for i, d := range r.errors {
			if i == MaxPostEditErrors {
				fmt.Fprintf(&sb, "... and %d more errors in %s\n", len(r.errors)-MaxPostEditErrors, r.rel)
				break
			}
			sb.WriteString(formatError(r.rel, d))
			sb.WriteByte('\n')
		}
	}
	if len(r.others) > 0 {
		total := 0
		names := make([]string, 0, maxOtherFiles)
		for i, f := range r.others {
			total += f.count
			if i < maxOtherFiles {
				names = append(names, fmt.Sprintf("%s (%d)", f.rel, f.count))
			}
		}
		if extra := len(r.others) - len(names); extra > 0 {
			names = append(names, fmt.Sprintf("%d more", extra))
		}
		lead := "Also"
		if len(r.errors) == 0 {
			lead = fmt.Sprintf("No errors in %s;", r.rel)
		}
		fmt.Fprintf(&sb, "%s %s in %s: %s — use the lsp tool (op: diagnostics) to list them.\n",
			lead, plural(total, "error"), plural(len(r.others), "other file"), strings.Join(names, ", "))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// formatError renders one error as "file:line:col: message (source)", the
// shape compilers print and models already know how to act on.
func formatError(rel string, d Diagnostic) string {
	msg := strings.Join(strings.Fields(d.Message), " ")
	if len(msg) > maxDiagMessage {
		cut := maxDiagMessage
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = strings.TrimSpace(msg[:cut]) + "…"
	}
	src := ""
	if d.Source != "" {
		src = " (" + d.Source + ")"
	}
	return fmt.Sprintf("%s:%d:%d: %s%s", rel, d.Range.Start.Line+1, d.Range.Start.Character+1, msg, src)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
