package agent

import "testing"

func TestToolOutputHistoryLimit(t *testing.T) {
	if got := toolOutputHistoryLimit("file-read"); got != 40000 {
		t.Errorf("file-read limit = %d, want 40000", got)
	}
	if got := toolOutputHistoryLimit("comment"); got != 2000 {
		t.Errorf("default limit = %d, want 2000", got)
	}
	if toolOutputHistoryLimit("file-read") <= toolOutputHistoryLimit("comment") {
		t.Error("read tools should get a larger budget than the default")
	}
}
