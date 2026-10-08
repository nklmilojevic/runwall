// Package joblog cleans up GitHub Actions job logs for display.
package joblog

import (
	"regexp"
	"strings"
)

// Tail is how many lines of a job log the dashboard shows; the full log is a click away on GitHub.
const Tail = 1000

type Line struct {
	Kind string // "", group, error, warning, notice, command, debug
	Text string
}

type View struct {
	Lines      []Line
	Total      int
	FirstError int // index into Lines, -1 if none
}

var (
	ansiRe      = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	timestampRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z ?`)
)

var markers = []struct{ prefix, kind string }{
	{"##[group]", "group"},
	{"##[error]", "error"},
	{"##[warning]", "warning"},
	{"##[notice]", "notice"},
	{"##[command]", "command"},
	{"##[debug]", "debug"},
	{"[command]", "command"},
}

// Parse strips timestamps and terminal colours from a GitHub Actions job log and
// classifies its workflow-command markers, keeping the last tail lines (Tail when tail <= 0).
func Parse(raw string, tail int) View {
	if tail <= 0 {
		tail = Tail
	}
	raw = strings.TrimPrefix(raw, "\ufeff")
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	all := strings.Split(strings.TrimRight(raw, "\n"), "\n")

	v := View{FirstError: -1}
	var lines []Line
	for _, l := range all {
		l = timestampRe.ReplaceAllString(l, "")
		l = ansiRe.ReplaceAllString(l, "")
		if strings.HasPrefix(l, "##[endgroup]") {
			continue
		}
		line := Line{Text: l}
		for _, m := range markers {
			if strings.HasPrefix(l, m.prefix) {
				line = Line{Kind: m.kind, Text: strings.TrimPrefix(l, m.prefix)}
				break
			}
		}
		lines = append(lines, line)
	}
	v.Total = len(lines)
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	v.Lines = lines
	for i, l := range lines {
		if l.Kind == "error" {
			v.FirstError = i
			break
		}
	}
	return v
}

// Text renders lines as plain text, marking errors and warnings.
func (v View) Text() string {
	var b strings.Builder
	for _, l := range v.Lines {
		switch l.Kind {
		case "group":
			b.WriteString("== ")
		case "error":
			b.WriteString("ERROR: ")
		case "warning":
			b.WriteString("WARNING: ")
		}
		b.WriteString(l.Text)
		b.WriteByte('\n')
	}
	return b.String()
}
