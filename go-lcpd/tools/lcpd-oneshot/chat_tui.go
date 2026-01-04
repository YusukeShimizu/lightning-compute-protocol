package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	tuiMinInputWidth     = 10
	tuiInputWidthPadding = 2

	tuiLabelSeparator = ": "
)

type chatTUITurnResultMsg struct {
	reply     string
	priceMsat uint64
	totalMsat uint64
	trimmed   bool
	err       error
}

type chatTUIModel struct {
	ctx     context.Context
	session *chatSession
	client  lcpdClient

	input  textinput.Model
	styles chatTUIStyles

	width   int
	height  int
	sending bool

	totalMsat uint64
	lines     []string
}

type chatTUIStyles struct {
	header    lipgloss.Style
	user      lipgloss.Style
	assistant lipgloss.Style
	meta      lipgloss.Style
	err       lipgloss.Style
}

func runChatTUI(
	ctx context.Context,
	session *chatSession,
	client lcpdClient,
	in *os.File,
	out *os.File,
) error {
	m := newChatTUIModel(ctx, session, client)
	p := tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out))
	_, err := p.Run()
	return err
}

func newChatTUIModel(ctx context.Context, session *chatSession, client lcpdClient) *chatTUIModel {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.Focus()
	ti.CharLimit = 0

	styles := chatTUIStyles{
		header:    lipgloss.NewStyle().Bold(true),
		user:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")),
		assistant: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4")),
		meta:      lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		err:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")),
	}

	return &chatTUIModel{
		ctx:     ctx,
		session: session,
		client:  client,
		input:   ti,
		styles:  styles,
		lines:   nil,
	}
}

func (m *chatTUIModel) Init() tea.Cmd {
	initial := strings.TrimSpace(m.session.baseOpts.Prompt)
	if initial == "" {
		return nil
	}

	m.appendUser(initial)
	m.sending = true
	m.input.Blur()
	return m.turnCmd(initial)
}

func (m *chatTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.updateWindowSize(msg)
		return m, nil

	case tea.KeyMsg:
		return m, m.updateKey(msg)

	case chatTUITurnResultMsg:
		m.updateTurnResult(msg)
		return m, nil
	}

	return m, nil
}

func (m *chatTUIModel) updateWindowSize(msg tea.WindowSizeMsg) {
	m.width = msg.Width
	m.height = msg.Height
	if m.width <= 0 {
		return
	}
	m.input.Width = max(tuiMinInputWidth, m.width-tuiInputWidthPadding)
}

func (m *chatTUIModel) updateKey(msg tea.KeyMsg) tea.Cmd {
	if msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyCtrlD || msg.Type == tea.KeyEsc {
		return tea.Quit
	}
	if m.sending {
		return nil
	}
	if msg.Type != tea.KeyEnter {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return cmd
	}

	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return nil
	}
	m.input.SetValue("")
	if text == "/exit" || text == "/quit" {
		return tea.Quit
	}

	m.appendUser(text)
	m.sending = true
	m.input.Blur()
	return m.turnCmd(text)
}

func (m *chatTUIModel) updateTurnResult(msg chatTUITurnResultMsg) {
	m.sending = false
	m.input.Focus()

	if msg.trimmed {
		m.appendMeta("(history truncated to fit max-prompt-bytes)")
	}

	if msg.err != nil {
		m.appendError(msg.err)
		return
	}

	m.appendAssistant(msg.reply)
	m.appendMeta(fmt.Sprintf(
		"paid=%s sat total=%s sat",
		formatMsatAsSat(msg.priceMsat),
		formatMsatAsSat(msg.totalMsat),
	))
	m.totalMsat = msg.totalMsat
}

func (m *chatTUIModel) View() string {
	header := m.styles.header.Render("lcpd-oneshot chat") + " " +
		m.styles.meta.Render(fmt.Sprintf("model=%s", m.session.baseOpts.Model))
	peer := m.styles.meta.Render(fmt.Sprintf("peer_id=%s", m.session.baseOpts.PeerID))
	help := m.styles.meta.Render("Enter: send • Ctrl-D/Ctrl-C/Esc: quit • /exit")

	status := m.styles.meta.Render(fmt.Sprintf("total=%s sat", formatMsatAsSat(m.totalMsat)))
	if m.sending {
		status = m.styles.meta.Render("sending…")
	}

	bodyLines := m.visibleBodyLines()

	var b strings.Builder
	b.WriteString(header)
	b.WriteByte('\n')
	b.WriteString(peer)
	b.WriteByte('\n')
	b.WriteString(help)
	b.WriteString("\n\n")
	for _, line := range bodyLines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(status)
	b.WriteByte('\n')
	b.WriteString(m.input.View())
	return b.String()
}

func (m *chatTUIModel) visibleBodyLines() []string {
	if m.height <= 0 {
		return m.lines
	}

	const headerLines = 3
	const spacerLines = 3
	const footerLines = 2

	available := m.height - headerLines - spacerLines - footerLines
	if available <= 0 {
		return nil
	}
	if len(m.lines) <= available {
		return m.lines
	}
	return m.lines[len(m.lines)-available:]
}

func (m *chatTUIModel) turnCmd(userText string) tea.Cmd {
	userText = strings.TrimSpace(userText)
	return func() tea.Msg {
		out, err := m.session.Turn(m.ctx, m.client, userText)
		return chatTUITurnResultMsg{
			reply:     out.Reply,
			priceMsat: out.PriceMsat,
			totalMsat: out.TotalMsat,
			trimmed:   out.Trimmed,
			err:       err,
		}
	}
}

func (m *chatTUIModel) appendUser(text string) {
	m.appendMessage("You", m.styles.user, text)
}

func (m *chatTUIModel) appendAssistant(text string) {
	m.appendMessage("Assistant", m.styles.assistant, text)
}

func (m *chatTUIModel) appendMeta(text string) {
	m.appendLines(m.styles.meta.Render(text))
}

func (m *chatTUIModel) appendError(err error) {
	m.appendLines(m.styles.err.Render("error: " + err.Error()))
}

func (m *chatTUIModel) appendMessage(label string, labelStyle lipgloss.Style, text string) {
	text = strings.TrimRight(text, "\r\n")
	parts := strings.Split(text, "\n")

	m.appendBlankLine()

	prefix := labelStyle.Render(label + ":")
	if len(parts) == 0 {
		m.lines = append(m.lines, prefix)
		return
	}
	m.lines = append(m.lines, prefix+" "+parts[0])
	indent := strings.Repeat(" ", len(label)+len(tuiLabelSeparator))
	for _, line := range parts[1:] {
		m.lines = append(m.lines, indent+line)
	}
}

func (m *chatTUIModel) appendLines(lines ...string) {
	m.appendBlankLine()
	m.lines = append(m.lines, lines...)
}

func (m *chatTUIModel) appendBlankLine() {
	if len(m.lines) == 0 {
		return
	}
	if m.lines[len(m.lines)-1] == "" {
		return
	}
	m.lines = append(m.lines, "")
}
