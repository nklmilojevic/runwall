package notify

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
)

type Notification struct {
	Title    string
	Subtitle string
	Message  string
	URL      string
	Group    string
}

type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// New returns the notifier named by kind: "macos", "log" or "none".
func New(kind string, log *slog.Logger) (Notifier, error) {
	switch kind {
	case "macos":
		return newMacOS(log), nil
	case "log":
		return Log{log}, nil
	case "none":
		return None{}, nil
	}
	return nil, fmt.Errorf("unknown notifier %q (want macos, log or none)", kind)
}

type None struct{}

func (None) Notify(context.Context, Notification) error { return nil }

type Log struct{ L *slog.Logger }

func (l Log) Notify(_ context.Context, n Notification) error {
	l.L.Warn("notification", "title", n.Title, "subtitle", n.Subtitle, "message", n.Message, "url", n.URL)
	return nil
}

// MacOS uses terminal-notifier when installed (clicking opens the run), and osascript otherwise.
type MacOS struct {
	terminalNotifier string
	log              *slog.Logger
}

func newMacOS(log *slog.Logger) *MacOS {
	path, err := exec.LookPath("terminal-notifier")
	if err != nil {
		log.Info("terminal-notifier not found; notifications will not open the run when clicked")
	}
	return &MacOS{terminalNotifier: path, log: log}
}

func (m *MacOS) Notify(ctx context.Context, n Notification) error {
	if m.terminalNotifier != "" {
		args := []string{"-title", n.Title, "-subtitle", n.Subtitle, "-message", n.Message, "-sound", "Basso"}
		if n.URL != "" {
			args = append(args, "-open", n.URL)
		}
		if n.Group != "" {
			args = append(args, "-group", n.Group)
		}
		return exec.CommandContext(ctx, m.terminalNotifier, args...).Run()
	}
	script := fmt.Sprintf("display notification %s with title %s subtitle %s sound name \"Basso\"",
		strconv.Quote(n.Message), strconv.Quote(n.Title), strconv.Quote(n.Subtitle))
	return exec.CommandContext(ctx, "osascript", "-e", script).Run()
}
