package agent

import "testing"

func TestToolOutputHistoryLimit(t *testing.T) {
	if got := toolOutputHistoryLimit("file-read"); got != defaultMaxOutputBytes {
		t.Errorf("file-read limit = %d, want %d", got, defaultMaxOutputBytes)
	}
	if got := toolOutputHistoryLimit("shell-exec"); got != defaultMaxOutputBytes {
		t.Errorf("shell-exec limit = %d, want %d", got, defaultMaxOutputBytes)
	}
	if got := toolOutputHistoryLimit("comment"); got != 2000 {
		t.Errorf("default limit = %d, want 2000", got)
	}
	if toolOutputHistoryLimit("file-read") <= toolOutputHistoryLimit("comment") {
		t.Error("read tools should get a larger budget than the default")
	}
}
