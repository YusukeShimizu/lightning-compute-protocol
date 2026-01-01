package openai

import (
	"errors"
	"fmt"
	"strings"
)

func BuildPrompt(messages []ChatMessage) (string, error) {
	if len(messages) == 0 {
		return "", errors.New("messages is required")
	}

	var systemParts []string
	var convoLines []string

	for i, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		content := strings.TrimSpace(string(m.Content))
		if role == "" {
			return "", fmt.Errorf("messages[%d].role is required", i)
		}
		if content == "" {
			return "", fmt.Errorf("messages[%d].content is required", i)
		}

		switch role {
		case "system":
			systemParts = append(systemParts, content)
		case "user":
			convoLines = append(convoLines, fmt.Sprintf("User: %s", content))
		case "assistant":
			convoLines = append(convoLines, fmt.Sprintf("Assistant: %s", content))
		default:
			return "", fmt.Errorf("unsupported messages[%d].role: %q", i, m.Role)
		}
	}

	var b strings.Builder
	if len(systemParts) > 0 {
		b.WriteString("<SYSTEM>\n")
		b.WriteString(strings.Join(systemParts, "\n\n"))
		b.WriteString("\n</SYSTEM>\n")
	}
	if len(convoLines) > 0 {
		b.WriteString("<CONVERSATION>\n")
		for _, line := range convoLines {
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteString("</CONVERSATION>\n")
	}
	b.WriteString("Assistant:")
	return b.String(), nil
}

func ApproxTokensFromBytes(n int) int {
	if n <= 0 {
		return 0
	}
	const approxBytesPerToken = 4
	const approxRoundUp = approxBytesPerToken - 1
	return (n + approxRoundUp) / approxBytesPerToken
}
