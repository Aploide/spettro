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
