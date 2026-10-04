package tui

import (
	"embed"
	"strings"

	"charm.land/lipgloss/v2"
)

//go:embed eyes.txt
var eyeFramesFile embed.FS

const eyeArtWidth = 60
const eyeIntroFrames = 24

var (
	eyesClosed, eyesAggressive, eyesNormal []string
	brailleDotBits                         = [4][2]uint8{{1, 8}, {2, 16}, {4, 32}, {64, 128}}
)

func init() {
	data, err := eyeFramesFile.ReadFile("eyes.txt")
	if err != nil {
		panic(err)
	}
	eyesClosed, eyesAggressive, eyesNormal = parseEyeFrames(string(data))
}

// parseEyeFrames splits the frame file into its three sections. A Windows
// checkout with autocrlf gives the file CRLF endings, so each line is
// stripped of its carriage return before it is matched or kept — otherwise
// no section marker matches and the art slices stay empty.
func parseEyeFrames(data string) (closed, aggressive, normal []string) {
	section := ""
	for _, line := range strings.Split(strings.TrimSuffix(data, "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		switch line {
		case "CLOSED", "AGGRESSIVE", "NORMAL":
			section = line
		default:
			switch section {
			case "CLOSED":
				closed = append(closed, line)
			case "AGGRESSIVE":
				aggressive = append(aggressive, line)
			case "NORMAL":
				normal = append(normal, line)
			}
		}
	}
	return closed, aggressive, normal
}

// eyeFrameArt opens the eyes into the aggressive frame, holds it briefly,
// then interpolates its Braille dots into the normal frame.
func eyeArtFor(mode string) []string { return eyesNormal }

func isPlanningEyeMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "plan", "ask":
		return true
	default:
		return false
	}
}

func eyeFrameArt(frame int) []string {
	if frame > eyeIntroFrames {
		return idleEyeArt(frame - eyeIntroFrames)
	}
	if frame <= 2 {
		return eyesClosed
	}
	if frame <= 10 {
		return openEyes(frame-2, 8)
	}
	if frame <= 15 {
		return eyesAggressive
	}
	if frame >= eyeIntroFrames {
		return eyesNormal
	}
	return interpolateEyes(eyesAggressive, eyesNormal, frame-15, 9)
}

func interpolateEyes(from, to []string, step, total int) []string {
	out := make([]string, len(from))
	for y := range from {
		a, b := []rune(from[y]), []rune(to[y])
		row := make([]rune, eyeArtWidth)
		for x := range row {
			am, bm := uint8(a[x]-0x2800), uint8(b[x]-0x2800)
			mask := uint8(0)
			for dy := range brailleDotBits {
				for dx, bit := range brailleDotBits[dy] {
					if am&bit != 0 && bm&bit != 0 {
						mask |= bit
						continue
					}
					threshold := brailleTransitionThreshold(x, y, dx, dy)
					progress := step * 64 / total
					if am&bit != 0 && (bm&bit != 0 || progress < threshold) ||
						bm&bit != 0 && am&bit == 0 && progress >= threshold {
						mask |= bit
					}
				}
			}
			row[x] = rune(0x2800 + int(mask))
		}
		out[y] = string(row)
	}
	return out
}

var brailleBayer8 = [8][8]uint8{
	{0, 32, 8, 40, 2, 34, 10, 42},
	{48, 16, 56, 24, 50, 18, 58, 26},
	{12, 44, 4, 36, 14, 46, 6, 38},
	{60, 28, 52, 20, 62, 30, 54, 22},
	{3, 35, 11, 43, 1, 33, 9, 41},
	{51, 19, 59, 27, 49, 17, 57, 25},
	{15, 47, 7, 39, 13, 45, 5, 37},
	{63, 31, 55, 23, 61, 29, 53, 21},
}

func brailleTransitionThreshold(x, y, dx, dy int) int {
	return int(brailleBayer8[(y*4+dy)%8][(x*2+dx)%8])
}

// openEyes vertically expands the supplied aggressive eyes from a shut line
// into their full frame, so the opening reads as motion instead of a fade.
func openEyes(step, total int) []string {
	return scaleEyesVertically(eyesAggressive, step, total)
}

func scaleEyesVertically(source []string, step, total int) []string {
	if step <= 0 {
		return eyesClosed
	}
	if step >= total {
		return source
	}
	height, width := len(source)*4, eyeArtWidth*2
	pixels := make([][]bool, height)
	for y := range pixels {
		pixels[y] = make([]bool, width)
	}
	center := height / 2
	for cellY, line := range source {
		for cellX, cell := range []rune(line) {
			mask := uint8(cell - 0x2800)
			for dy := range brailleDotBits {
				for dx, bit := range brailleDotBits[dy] {
					if mask&bit == 0 {
						continue
					}
					delta := cellY*4 + dy - center
					var scaled int
					if delta < 0 {
						scaled = center - ((-delta*step + total/2) / total)
					} else {
						scaled = center + ((delta*step + total/2) / total)
					}
					pixels[scaled][cellX*2+dx] = true
				}
			}
		}
	}
	out := make([]string, len(source))
	for cellY := range out {
		row := make([]rune, eyeArtWidth)
		for cellX := range row {
			mask := uint8(0)
			for dy := range brailleDotBits {
				for dx, bit := range brailleDotBits[dy] {
					if pixels[cellY*4+dy][cellX*2+dx] {
						mask |= bit
					}
				}
			}
			row[cellX] = rune(0x2800 + int(mask))
		}
		out[cellY] = string(row)
	}
	return out
}

// Idle blinks are deliberately brief and gentle, with most of the time spent
// resting on the open frame.
func idleEyeArt(frame int) []string {
	switch frame {
	case 1:
		return scaleEyesVertically(eyesNormal, 3, 4)
	case 2, 6:
		return scaleEyesVertically(eyesNormal, 2, 4)
	case 3, 5:
		return scaleEyesVertically(eyesNormal, 1, 4)
	case 4:
		return eyesClosed
	default:
		return eyesNormal
	}
}

func (m Model) eyeBannerFrame() int {
	if m.eyeIntroFrame < eyeIntroFrames {
		return m.eyeIntroFrame
	}
	if m.idleEyesFrame > 0 && !m.hasUserMessage() {
		return eyeIntroFrames + m.idleEyesFrame
	}
	return eyeIntroFrames
}

func (m Model) hasUserMessage() bool {
	for _, message := range m.messages {
		if message.Role == RoleUser {
			return true
		}
	}
	return false
}

func renderEyesStatic(mode string, width int) string {
	return renderEyesAt(mode, width, eyeIntroFrames)
}

func renderEyesAt(mode string, width, frame int) string {
	art := eyeFrameArt(frame)
	style := lipgloss.NewStyle().Foreground(modeColor(mode))
	lead, trail := 0, eyeArtWidth-1
	if width <= eyeArtWidth {
		lead, trail = eyeArtInk(art)
	}
	lines := make([]string, len(art))
	for i, raw := range art {
		runes := []rune(raw)
		hi := min(trail+1, len(runes))
		lo := min(lead, hi)
		cropped := runes[lo:hi]
		if len(cropped) > width {
			cropped = cropped[:width]
		}
		lines[i] = style.Render(string(cropped))
	}
	if pad := (width - (trail - lead + 1)) / 2; pad > 0 {
		prefix := strings.Repeat(" ", pad)
		for i, line := range lines {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

// eyeArtInk returns the column range the art actually paints in, ignoring the
// blank braille gutter both slices are padded with. Centring on the ink rather
// than on the full grid is what keeps the logo looking centred once the pane
// is too narrow to show the whole thing.
func eyeArtInk(art []string) (lead, trail int) {
	lead, trail = eyeArtWidth, 0
	for _, raw := range art {
		runes := []rune(raw)
		for i, r := range runes {
			if r == '\u2800' || r == ' ' {
				continue
			}
			lead = min(lead, i)
			trail = max(trail, i)
		}
	}
	if trail < lead {
		return 0, eyeArtWidth - 1
	}
	return lead, trail
}
