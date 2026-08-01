package agent

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"spettro/internal/jobs"
)

var spoolFooterRe = regexp.MustCompile(`\[truncated: ([\d,]+) of ([\d,]+) lines omitted; use job-output \{"job_id":"(spool:\d+)","offset":(\d+)\} to read more\]`)

func TestSpoolIfLargeSmallPassThrough(t *testing.T) {
	out := "just a few lines\nof output\n"
	if got := spoolIfLarge(out, 8000, false); got != out {
		t.Fatalf("small output modified: %q", got)
	}
}

func TestSpoolIfLargeTruncatesWithFooterAndPages(t *testing.T) {
	t.Cleanup(jobs.Spool().Cleanup)
	var b strings.Builder
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&b, "match line %04d: some grep content here\n", i)
	}
	out := b.String()
	budget := 4000

	got := spoolIfLarge(out, budget, false)
	if len(got) > budget {
		t.Fatalf("truncated output exceeds budget: %d > %d", len(got), budget)
	}
	m := spoolFooterRe.FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("missing spool footer in %q", got[len(got)-300:])
	}
	if !strings.HasPrefix(got, "match line 0001:") {
		t.Fatalf("head not preserved: %q", got[:80])
	}
	// Deterministic: same output truncates to the same head/cut points.
	again := spoolIfLarge(out, budget, false)
	if spoolFooterRe.ReplaceAllString(again, "") != spoolFooterRe.ReplaceAllString(got, "") {
		t.Fatal("truncation not deterministic for identical output")
	}

	// Page through the spool from the footer's offset: the omitted middle
	// must come back, in order, until the end of the full output.
	spoolID := m[3]
	var offset int
	fmt.Sscanf(m[4], "%d", &offset)
	startOffset := offset
	var paged strings.Builder
	for {
		chunk, next, size, err := jobs.Spool().Read(spoolID, offset, 10000)
		if err != nil {
			t.Fatalf("spool read: %v", err)
		}
		paged.WriteString(chunk)
		if size != len(out) {
			t.Fatalf("spool size = %d, want %d", size, len(out))
		}
		if next >= size {
			break
		}
		offset = next
	}
	if paged.String() != out[startOffset:] {
		t.Fatal("paged content does not match the omitted remainder")
	}
	if !strings.HasSuffix(paged.String(), "match line 2000: some grep content here\n") {
		t.Fatal("paging did not reach the end of the spooled output")
	}
}

func TestSpoolShellOutputKeepsTail(t *testing.T) {
	t.Cleanup(jobs.Spool().Cleanup)
	var b strings.Builder
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&b, "build log line %04d\n", i)
	}
	out := b.String()
	got := spoolIfLarge(out, 4000, true)
	if len(got) > 4000+1 { // +1 for the newline joining footer and tail
		t.Fatalf("truncated output exceeds budget: %d", len(got))
	}
	if !strings.HasPrefix(got, "build log line 0001") {
		t.Fatalf("head not preserved: %q", got[:60])
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), "build log line 3000") {
		t.Fatalf("tail not preserved: %q", got[len(got)-80:])
	}
	if !spoolFooterRe.MatchString(got) {
		t.Fatal("missing spool footer between head and tail")
	}
}

func TestJobOutputReadsSpool(t *testing.T) {
	t.Cleanup(jobs.Spool().Cleanup)
	content := strings.Repeat("spooled output line\n", 500)
	id, err := jobs.Spool().Add(content)
	if err != nil {
		t.Fatal(err)
	}
	r := &toolRuntime{}
	out, err := r.runJobOutput(fmt.Appendf(nil, `{"job_id":%q,"offset":0}`, id))
	if err != nil {
		t.Fatalf("job-output on spool ID: %v", err)
	}
	if !strings.Contains(out, "spool="+id) || !strings.Contains(out, "next_offset=") {
		t.Fatalf("missing spool header: %q", out[:120])
	}
	if !strings.Contains(out, "spooled output line") {
		t.Fatal("spool content missing from job-output result")
	}
}

func TestSpoolCleanupRemovesSpools(t *testing.T) {
	id, err := jobs.Spool().Add("some content")
	if err != nil {
		t.Fatal(err)
	}
	jobs.Spool().Cleanup()
	if _, _, _, err := jobs.Spool().Read(id, 0, 100); err == nil {
		t.Fatal("expected error reading spool after cleanup")
	}
}

func TestGroupDigits(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1000: "1,000", 12400: "12,400", 1234567: "1,234,567"}
	for n, want := range cases {
		if got := groupDigits(n); got != want {
			t.Errorf("groupDigits(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestEnsureSpooledSmallOutputNotSpooled(t *testing.T) {
	if id := ensureSpooled("short output"); id != "" {
		t.Fatalf("small output was spooled: %q", id)
	}
}

func TestEnsureSpooledPersistsFullOutputAtExecTime(t *testing.T) {
	t.Cleanup(jobs.Spool().Cleanup)
	out := strings.Repeat("line of build output\n", 300) // > offloadFloor, < history budget
	id := ensureSpooled(out)
	if id == "" {
		t.Fatal("oversized output not spooled")
	}
	got, _, size, err := jobs.Spool().Read(id, 0, 0)
	if err != nil || got != out || size != len(out) {
		t.Fatalf("spool round-trip failed: err=%v size=%d", err, size)
	}
}

func TestEnsureSpooledReusesTruncationFooterID(t *testing.T) {
	t.Cleanup(jobs.Spool().Cleanup)
	full := strings.Repeat("some very long shell output line\n", 1000)
	truncated := spoolIfLarge(full, 4000, true)
	m := spoolFooterRe.FindStringSubmatch(truncated)
	if m == nil {
		t.Fatal("expected truncation footer")
	}
	if id := ensureSpooled(truncated); id != m[3] {
		t.Fatalf("footer ID not reused: got %q want %q", id, m[3])
	}
	// The spool must hold the FULL output, not the truncated view.
	got, _, _, err := jobs.Spool().Read(m[3], 0, 0)
	if err != nil || got != full {
		t.Fatalf("spool does not hold full output: err=%v", err)
	}
}

func TestRunToolOutputReadsBackWithOffsetAndLimit(t *testing.T) {
	t.Cleanup(jobs.Spool().Cleanup)
	id, err := jobs.Spool().Add("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	r := &toolRuntime{}
	out, err := r.runToolOutput(fmt.Appendf(nil, `{"id":%q,"offset":4,"limit":6}`, id))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "456789") || strings.Contains(out, "abc") {
		t.Fatalf("offset/limit not honored: %q", out)
	}
	if !strings.Contains(out, "next_offset=10") || !strings.Contains(out, "size=16") {
		t.Fatalf("missing paging header: %q", out)
	}
	// Bare numeric ID resolves to spool:<n>.
	n := strings.TrimPrefix(id, "spool:")
	if out, err = r.runToolOutput(fmt.Appendf(nil, `{"id":%q}`, n)); err != nil || !strings.Contains(out, "0123456789abcdef") {
		t.Fatalf("bare ID read failed: err=%v out=%q", err, out)
	}
	if _, err := r.runToolOutput([]byte(`{"id":"spool:999"}`)); err == nil {
		t.Fatal("unknown spool ID must error")
	}
}

func TestSpoolSurvivesRunEndUntilCleanup(t *testing.T) {
	t.Cleanup(jobs.Spool().Cleanup)
	id := ensureSpooled(strings.Repeat("z", 3000))
	if id == "" {
		t.Fatal("not spooled")
	}
	// Nothing at run end touches the spool; only Cleanup (process exit or
	// /clear) removes it.
	if _, _, _, err := jobs.Spool().Read(id, 0, 0); err != nil {
		t.Fatalf("spool unreadable after run: %v", err)
	}
	jobs.Spool().Cleanup()
	if _, _, _, err := jobs.Spool().Read(id, 0, 0); err == nil {
		t.Fatal("spool must be gone after Cleanup (/clear)")
	}
}
