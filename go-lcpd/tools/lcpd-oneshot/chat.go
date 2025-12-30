package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	lcpdv1 "github.com/bruwbird/lcp/go-lcpd/gen/go/lcpd/v1"
)

type chatRole string

const (
	chatRoleUser      chatRole = "user"
	chatRoleAssistant chatRole = "assistant"
)

type chatMessage struct {
	Role    chatRole
	Content string
}

type chatSession struct {
	baseOpts runOptions

	history   []chatMessage
	totalMsat uint64
}

type chatTurnOutput struct {
	Reply     string
	PriceMsat uint64
	TotalMsat uint64
	Trimmed   bool
}

func newChatSession(opts runOptions) *chatSession {
	return &chatSession{
		baseOpts: opts,
		history:  nil,
	}
}

func (s *chatSession) Turn(
	ctx context.Context,
	client lcpdClient,
	userText string,
) (chatTurnOutput, error) {
	userText = strings.TrimSpace(userText)
	if userText == "" {
		return chatTurnOutput{}, nil
	}

	s.history = append(s.history, chatMessage{Role: chatRoleUser, Content: userText})

	prompt, keepHistory, trimmed, err := buildChatPrompt(
		s.baseOpts.SystemPrompt,
		s.history,
		s.baseOpts.MaxPromptBytes,
	)
	if err != nil {
		s.history = s.history[:len(s.history)-1]
		return chatTurnOutput{}, err
	}
	if trimmed {
		s.history = keepHistory
	}

	turnCtx, cancel := context.WithTimeout(ctx, s.baseOpts.Timeout)
	defer cancel()

	quoteResp, err := client.RequestQuote(turnCtx, &lcpdv1.RequestQuoteRequest{
		PeerId: s.baseOpts.PeerID,
		Task: &lcpdv1.Task{
			Spec: &lcpdv1.Task_LlmChat{
				LlmChat: &lcpdv1.LLMChatTaskSpec{
					Prompt: prompt,
					Params: &lcpdv1.LLMChatParams{
						Profile:          s.baseOpts.Profile,
						TemperatureMilli: s.baseOpts.TemperatureMilli,
						MaxOutputTokens:  s.baseOpts.MaxOutputTokens,
					},
				},
			},
		},
	})
	if err != nil {
		s.history = s.history[:len(s.history)-1]
		return chatTurnOutput{Trimmed: trimmed}, fmt.Errorf("request quote: %w", err)
	}

	terms := quoteResp.GetTerms()
	if terms == nil {
		s.history = s.history[:len(s.history)-1]
		return chatTurnOutput{Trimmed: trimmed}, errors.New("request quote: response terms is nil")
	}

	execResp, err := client.AcceptAndExecute(turnCtx, &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     s.baseOpts.PeerID,
		JobId:      terms.GetJobId(),
		PayInvoice: true,
	})
	if err != nil {
		s.history = s.history[:len(s.history)-1]
		return chatTurnOutput{Trimmed: trimmed}, fmt.Errorf("accept and execute: %w", err)
	}
	if execResp == nil {
		s.history = s.history[:len(s.history)-1]
		return chatTurnOutput{Trimmed: trimmed}, errors.New("accept and execute: response is nil")
	}
	if execResp.GetResult() == nil {
		s.history = s.history[:len(s.history)-1]
		return chatTurnOutput{Trimmed: trimmed}, errors.New("accept and execute: result is nil")
	}

	reply := normalizeAssistantReply(string(execResp.GetResult().GetResult()))
	reply = strings.TrimRight(reply, "\r\n")

	s.totalMsat += terms.GetPriceMsat()
	s.history = append(s.history, chatMessage{Role: chatRoleAssistant, Content: reply})

	return chatTurnOutput{
		Reply:     reply,
		PriceMsat: terms.GetPriceMsat(),
		TotalMsat: s.totalMsat,
		Trimmed:   trimmed,
	}, nil
}

func (s *chatSession) Run(
	ctx context.Context,
	client lcpdClient,
	in io.Reader,
	out io.Writer,
	errOut io.Writer,
) error {
	fmt.Fprintf(
		out,
		"chat peer_id=%s profile=%s (Ctrl-D or /exit)\n",
		s.baseOpts.PeerID,
		s.baseOpts.Profile,
	)

	if initial := strings.TrimSpace(s.baseOpts.Prompt); initial != "" {
		if err := s.runTurn(ctx, client, initial, out, errOut); err != nil {
			return err
		}
	}

	reader := newReadLine(in)
	for {
		fmt.Fprint(out, "> ")
		line, ok, err := reader.Scan()
		if err != nil {
			return fmt.Errorf("read input: %w", err)
		}
		if !ok {
			return nil
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "/exit" || line == "/quit" {
			return nil
		}

		if turnErr := s.runTurn(ctx, client, line, out, errOut); turnErr != nil {
			fmt.Fprintln(errOut, turnErr)
		}
	}
}

func (s *chatSession) runTurn(
	ctx context.Context,
	client lcpdClient,
	userText string,
	out io.Writer,
	errOut io.Writer,
) error {
	turn, err := s.Turn(ctx, client, userText)
	if turn.Trimmed {
		fmt.Fprintln(errOut, "(history truncated to fit max-prompt-bytes)")
	}
	if err != nil {
		return err
	}

	fmt.Fprintln(out, turn.Reply)
	fmt.Fprintf(
		out,
		"paid=%s sat total=%s sat\n",
		formatMsatAsSat(turn.PriceMsat),
		formatMsatAsSat(turn.TotalMsat),
	)
	return nil
}

func buildChatPrompt(
	systemPrompt string,
	history []chatMessage,
	maxBytes int,
) (string, []chatMessage, bool, error) {
	systemPrompt = strings.TrimSpace(systemPrompt)

	var systemBlock string
	if systemPrompt != "" {
		systemBlock = "System: " + systemPrompt + "\n\n"
	}

	const assistantCue = "Assistant: "

	if maxBytes == 0 {
		var b strings.Builder
		b.WriteString(systemBlock)
		for _, msg := range history {
			b.WriteString(formatChatMessage(msg))
		}
		b.WriteString(assistantCue)
		return b.String(), history, false, nil
	}

	if maxBytes < 0 {
		return "", nil, false, errors.New("maxBytes must be >= 0")
	}

	baseLen := len(systemBlock) + len(assistantCue)
	if baseLen > maxBytes {
		return "", nil, false, fmt.Errorf(
			"max-prompt-bytes too small (%d): cannot fit system prompt + framing",
			maxBytes,
		)
	}

	used := baseLen
	var keptRev []chatMessage
	for i := len(history) - 1; i >= 0; i-- {
		block := formatChatMessage(history[i])
		if used+len(block) > maxBytes {
			return buildChatPromptFromRev(systemBlock, assistantCue, keptRev, true, maxBytes)
		}
		keptRev = append(keptRev, history[i])
		used += len(block)
	}

	return buildChatPromptFromRev(systemBlock, assistantCue, keptRev, false, maxBytes)
}

func formatChatMessage(msg chatMessage) string {
	label := "User"
	if msg.Role == chatRoleAssistant {
		label = "Assistant"
	}
	return fmt.Sprintf("%s: %s\n", label, msg.Content)
}

func normalizeAssistantReply(reply string) string {
	reply = strings.TrimLeft(reply, "\r\n")
	if len(reply) < len("assistant:") {
		return reply
	}

	lower := strings.ToLower(reply)
	if strings.HasPrefix(lower, "assistant:") {
		return strings.TrimSpace(reply[len("assistant:"):])
	}

	return reply
}

func buildChatPromptFromRev(
	systemBlock string,
	assistantCue string,
	keptRev []chatMessage,
	trimmed bool,
	maxBytes int,
) (string, []chatMessage, bool, error) {
	if len(keptRev) == 0 {
		return "", nil, false, fmt.Errorf(
			"prompt too large to fit max-prompt-bytes=%d (shorten your message or increase the limit)",
			maxBytes,
		)
	}

	keepHistory := make([]chatMessage, 0, len(keptRev))
	for i := len(keptRev) - 1; i >= 0; i-- {
		keepHistory = append(keepHistory, keptRev[i])
	}

	var b strings.Builder
	b.WriteString(systemBlock)
	for _, msg := range keepHistory {
		b.WriteString(formatChatMessage(msg))
	}
	b.WriteString(assistantCue)
	return b.String(), keepHistory, trimmed, nil
}
