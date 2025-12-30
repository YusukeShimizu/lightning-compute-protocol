//nolint:testpackage // Test-only helper to exercise settlement pricing without exporting it in production builds.
package llm

import "github.com/bruwbird/lcp/go-lcpd/internal/computebackend"

func SettlePrice(
	model string,
	usage computebackend.Usage,
	cachedInputTokens uint64,
	table PriceTable,
) (PriceBreakdown, error) {
	tokens := PriceTokens{
		InputTokens:       usage.InputUnits,
		CachedInputTokens: cachedInputTokens,
		OutputTokens:      usage.OutputUnits,
	}
	return priceFromTokens(model, tokens, table)
}
