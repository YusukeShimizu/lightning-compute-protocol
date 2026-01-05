package openai

func ApproxTokensFromBytes(n int) int {
	if n <= 0 {
		return 0
	}
	const approxBytesPerToken = 4
	const approxRoundUp = approxBytesPerToken - 1
	return (n + approxRoundUp) / approxBytesPerToken
}
