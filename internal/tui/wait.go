package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// ErrCanceled is returned by Wait when the user presses ctrl+c.
var ErrCanceled = errors.New("canceled")

type doneMsg struct{ err error }

// WaitModel shows a spinner and elapsed time while work runs.
type WaitModel struct {
	Label   string
	spinner spinner.Model
	start   time.Time
	Err     error
	Done    bool
}

func newWait(label string) WaitModel {
	s := spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(AccentStyle))
	return WaitModel{Label: label, spinner: s, start: time.Now()}
}

func (m WaitModel) Init() tea.Cmd { return m.spinner.Tick }

func (m WaitModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case doneMsg:
		m.Err, m.Done = msg.err, true
		return m, tea.Quit
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.Err, m.Done = ErrCanceled, true
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return m, cmd
}

func (m WaitModel) View() tea.View { return tea.NewView(m.render()) }

func (m WaitModel) render() string {
	if m.Done {
		return "" // the caller prints the result
	}
	return fmt.Sprintf("%s %s %s\n", m.spinner.View(), m.Label,
		SubtleStyle.Render(time.Since(m.start).Round(100*time.Millisecond).String()))
}

// Wait runs fn while showing a spinner on out (stderr, so stdout stays
// clean for the result). ctrl+c cancels fn's context and returns
// ErrCanceled.
func Wait(ctx context.Context, out io.Writer, in io.Reader, label string, fn func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := tea.NewProgram(newWait(label), tea.WithOutput(out), tea.WithInput(in), tea.WithContext(ctx))
	go func() { p.Send(doneMsg{err: fn(ctx)}) }()
	final, err := p.Run()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return err
	}
	m, _ := final.(WaitModel)
	if !m.Done {
		return ctx.Err()
	}
	return m.Err
}
