package tui

// Side panel cost against the size of the activity feed. Corresponds to
// BenchmarkSidePanelScale in the performance plan's TUI harness (unit B),
// measured on the panel alone so the frame around it does not dilute it.
// The feed is filled directly, past the maxActivityItems cap upsertActivity
// enforces, to show the cost does not grow with it.

import (
	"fmt"
	"testing"
)

func BenchmarkSidePanel(b *testing.B) {
	for _, n := range []int{200, 2000, 10000} {
		m := footerModel(180, 50)
		m.showSidePanel = true
		for i := 0; i < n; i++ {
			m.activityFeed = append(m.activityFeed, activityItem{
				Key: fmt.Sprint(i), Kind: "tool", ID: "bash", AgentID: fmt.Sprint("agent", i/50),
				Title: "bash go test", Detail: `{"command":"x"}`, Body: "line1\nline2\nline3", Status: "done",
			})
		}
		width := m.sidePanelWidth()
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				benchSink = m.viewSidePanel(width)
			}
		})
	}
}

var benchSink string
