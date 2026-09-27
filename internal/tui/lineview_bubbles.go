//go:build spettro_bubblesviewport

package tui

import (
	"strings"

	"charm.land/bubbles/v2/viewport"
)

// lineView, in builds tagged spettro_bubblesviewport, is the transcript
// viewport as it was before lineview.go: bubbles/viewport fed one joined
// string. It exists for one release so the two can be A/B'd
// (go build -tags spettro_bubblesviewport); see lineview.go for the
// contract both implement.
type lineView struct {
	vp viewport.Model
}

func newLineView(width, height int) lineView {
	return lineView{vp: viewport.New(viewport.WithWidth(width), viewport.WithHeight(height))}
}

func (v lineView) Width() int           { return v.vp.Width() }
func (v lineView) Height() int          { return v.vp.Height() }
func (v *lineView) SetWidth(w int)      { v.vp.SetWidth(w) }
func (v *lineView) SetHeight(h int)     { v.vp.SetHeight(h) }
func (v *lineView) SetContent(s string) { v.vp.SetContent(s) }
func (v lineView) GetContent() string   { return v.vp.GetContent() }
func (v lineView) TotalLineCount() int  { return v.vp.TotalLineCount() }
func (v lineView) YOffset() int         { return v.vp.YOffset() }
func (v *lineView) SetYOffset(n int)    { v.vp.SetYOffset(n) }
func (v lineView) AtTop() bool          { return v.vp.AtTop() }
func (v lineView) AtBottom() bool       { return v.vp.AtBottom() }
func (v *lineView) GotoTop()            { v.vp.GotoTop() }
func (v *lineView) GotoBottom()         { v.vp.GotoBottom() }
func (v *lineView) ScrollUp(n int)      { v.vp.ScrollUp(n) }
func (v *lineView) ScrollDown(n int)    { v.vp.ScrollDown(n) }
func (v *lineView) PageUp()             { v.vp.PageUp() }
func (v *lineView) PageDown()           { v.vp.PageDown() }
func (v lineView) View() string         { return v.vp.View() }

// SetBlocks joins the blocks with a blank row between them, as the
// transcript was built before lineView existed.
func (v *lineView) SetBlocks(blocks [][]string) {
	parts := make([]string, len(blocks))
	for i, b := range blocks {
		parts[i] = strings.Join(b, "\n")
	}
	v.vp.SetContent(strings.Join(parts, "\n\n"))
}
