package audio

import (
	"fmt"
	"strings"
)

// Cue is one timed piece of text, in seconds.
type Cue struct {
	Start, End float64
	Text       string
}

func stamp(sec float64, sep byte) string {
	if sec < 0 {
		sec = 0
	}
	ms := int64(sec*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d%c%03d", ms/3600000, ms/60000%60, ms/1000%60, sep, ms%1000)
}

// SRT renders cues as SubRip.
func SRT(cues []Cue) string {
	var b strings.Builder
	for i, c := range cues {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, stamp(c.Start, ','), stamp(c.End, ','), strings.TrimSpace(c.Text))
	}
	return b.String()
}

// VTT renders cues as WebVTT.
func VTT(cues []Cue) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, c := range cues {
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", stamp(c.Start, '.'), stamp(c.End, '.'), strings.TrimSpace(c.Text))
	}
	return b.String()
}
