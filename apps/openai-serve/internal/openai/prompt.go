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
		roleRaw := strings.TrimSpace(m.Role)
		role := strings.ToLower(roleRaw)
		content := strings.TrimSpace(string(m.Content))
		if role == "" {
			return "", fmt.Errorf("messages[%d].role is required", i)
		}
		if content == "" {
			continue
		}

		switch role {
		case "system", "developer":
			systemParts = append(systemParts, content)
		case "user":
			convoLines = append(convoLines, fmt.Sprintf("User: %s", content))
		case "assistant":
			convoLines = append(convoLines, fmt.Sprintf("Assistant: %s", content))
		case "tool":
			if name := strings.TrimSpace(m.Name); name != "" {
				convoLines = append(convoLines, fmt.Sprintf("Tool(%s): %s", name, content))
			} else {
				convoLines = append(convoLines, fmt.Sprintf("Tool: %s", content))
			}
		case "function":
			if name := strings.TrimSpace(m.Name); name != "" {
				convoLines = append(convoLines, fmt.Sprintf("Function(%s): %s", name, content))
			} else {
				convoLines = append(convoLines, fmt.Sprintf("Function: %s", content))
			}
		default:
			convoLines = append(convoLines, fmt.Sprintf("%s: %s", roleRaw, content))
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
