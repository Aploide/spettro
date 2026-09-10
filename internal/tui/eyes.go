package tui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"spettro/internal/theme"
)

// Eye art from eyes.txt – two states: acting (coding) and planning.
// Each slice is the raw braille art lines.
var eyesActing = []string{
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⢠⣄⣀⣀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⣀⣀⣤⡄⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠻⣿⣿⡻⠿⢶⣦⣤⣀⡀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⢀⣠⣤⣴⡶⠿⢟⣿⣿⠏⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠙⢿⣷⡀⠀⠀⠉⠙⠛⠿⣶⣤⣀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⢀⣀⣤⣶⠿⠛⠋⠉⠀⠀⢀⣾⡿⠃⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠈⠻⣿⣄⠀⠀⠀⠀⠀⠀⠈⣿⣿⣷⣦⣀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⣠⣴⣾⣿⣿⠁⠀⠀⠀⠀⠀⠀⣠⣿⠟⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠈⠻⢧⣀⠀⠀⠀⠀⠀⢿⣿⡇⠈⠙⠿⣶⣄⡀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⢀⣠⣶⠿⠋⠁⢼⣿⡿⠀⠀⠀⠀⠀⣀⡾⠟⠁⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠙⠳⣤⡀⠀⠀⠘⢿⡇⠀⠀⠀⠀⠙⠻⣶⣄⡀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⢀⣠⣶⠟⠋⠀⠀⠀⠀⢸⡿⠃⠀⠀⢀⣤⠞⠋⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠈⠐⠋⠷⠶⣤⣄⣁⣀⣀⣀⣀⣀⣀⣀⣹⣿⣦⣄⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⣠⣴⣿⣋⣀⣀⣀⡀⣀⣀⣀⣈⣠⣤⠶⠾⠙⠂⠁⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠈⠉⠉⠉⠉⠉⠉⠉⠉⠉⠀⠁⠁⠀⠀⠀⠀⠀⠀⠀⠀⠈⠁⠀⠉⠉⠉⠉⠉⠉⠉⠉⠉⠁⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
}

var eyesPlanning = []string{
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠹⣿⣿⡿⠿⠷⠶⠶⢶⣶⣦⣤⣤⣄⣀⡀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⢀⣀⣠⣤⣤⣴⣶⡶⠶⠶⠿⠿⢿⣿⣿⠏⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠘⢿⣷⡄⠀⠀⠀⠀⠀⠀⠀⠉⣽⣿⣿⣿⣶⣦⣤⣀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⣀⣤⣴⣾⣿⣿⣿⣯⠉⠀⠀⠀⠀⠀⠀⠀⢠⣾⡿⠁⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠙⢿⣆⠀⠀⠀⠀⠀⠀⢸⣿⣿⣿⡇⠀⢹⣿⣿⣿⣶⣤⣀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⣀⣤⣶⣿⣿⣿⡏⠀⢹⣿⣿⣿⡇⠀⠀⠀⠀⠀⠀⣰⡿⠋⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠙⢷⣄⠀⠀⠀⠀⠸⣿⣿⣿⣿⣿⣿⣿⣿⣿⡇⠉⠛⢷⣦⣀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⣀⣴⡾⠛⠁⢸⣿⣿⣿⣿⣿⣿⣿⣿⣿⠇⠀⠀⠀⢀⣠⠾⠋⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠨⢛⡦⣄⣀⠀⠙⢿⣿⣿⣿⣿⣿⡿⠟⠀⠀⠀⠀⣈⣿⠷⡄⠀⠀⠀⠀⠀⠀⡠⠾⣟⣁⠀⠀⠀⠀⠻⣿⣿⣿⣿⣿⣿⡿⠋⠀⣀⡤⢶⡋⠅⠂⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠉⠉⠛⠓⠶⠾⠭⠭⠭⠥⠴⠶⠖⠒⠛⠉⠁⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠉⠉⠛⠒⠶⠶⠤⠭⠭⠭⠭⠶⠖⠚⠛⠉⠉⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
}

// eyeArtFor returns the correct art for the given mode.
func eyeArtFor(mode string) []string {
	if isPlanningEyeMode(mode) {
		return eyesPlanning
	}
	return eyesActing
}

func isPlanningEyeMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "plan", "ask":
		return true
	default:
		return false
	}
}

// renderEyes renders the eye art with animation based on frame counter.
// frame drives blinking and thinking scan-line effects.
func renderEyes(mode string, frame int, thinking bool, termWidth int) string {
	art := eyeArtFor(mode)
	n := len(art)

	var lines []string

	if thinking {
		// Scan-line animation: one row is bright, others fade by distance
		scanPos := (frame / 2) % (n*2 - 2)
		if scanPos >= n {
			scanPos = (n*2 - 2) - scanPos
		}
		// The falloff runs toward the terminal's own ground, so on a light
		// theme "further from the scan line" means lighter, not darker; the
		// anchors come from the palette rather than a fade toward black.
		scan := theme.Current().EyesScan
		lines = make([]string, n)
		for i, raw := range art {
			dist := i - scanPos
			if dist < 0 {
				dist = -dist
			}
			var s lipgloss.Style
			switch dist {
			case 0:
				s = lipgloss.NewStyle().Foreground(modeColor(mode)).Bold(true)
			case 1:
				s = lipgloss.NewStyle().Foreground(scan[0])
			case 2:
				s = lipgloss.NewStyle().Foreground(scan[1])
			default:
				s = lipgloss.NewStyle().Foreground(scan[2])
			}
			lines[i] = s.Render(raw)
		}
	} else {
		// Normal mode: blink cycle every ~8 seconds (at 20fps = 160 frames)
		cycle := frame % 160
		blink := theme.Current().EyesBlink
		var eyeStyle lipgloss.Style
		switch {
		case cycle >= 156: // closing
			eyeStyle = lipgloss.NewStyle().Foreground(blink[2])
		case cycle >= 152: // half-closed
			eyeStyle = lipgloss.NewStyle().Foreground(blink[1])
		case cycle >= 148: // squinting
			eyeStyle = lipgloss.NewStyle().Foreground(blink[0])
		default:
			eyeStyle = lipgloss.NewStyle().Foreground(modeColor(mode))
		}
		lines = make([]string, n)
		for i, raw := range art {
			lines[i] = eyeStyle.Render(raw)
		}
	}

	content := strings.Join(lines, "\n")

	// Center horizontally if terminal is wide enough
	artWidth := 99 // approximate width of the braille art
	if termWidth > artWidth+4 {
		pad := (termWidth - artWidth) / 2
		if pad > 0 {
			prefix := strings.Repeat(" ", pad)
			centered := make([]string, len(lines))
			for i, l := range lines {
				centered[i] = prefix + l
			}
			content = strings.Join(centered, "\n")
		}
	}

	return content
}
