package provider

import "github.com/bruwbird/lcp/go-lcpd/internal/llm"

// ModelConfig configures a single OpenAI model used with
// `openai.chat_completions.v1`.
type ModelConfig struct {
	// MaxOutputTokens overrides the Provider-wide max output tokens for this model.
	// If nil, the Provider-wide default is used.
	MaxOutputTokens *uint32

	// Price defines msat pricing for this model (msat per 1M tokens).
	Price llm.PriceTableEntry
}

// InFlightSurgeConfig configures load-based surge pricing based on the current
// number of in-flight jobs on this Provider.
//
// Multipliers are expressed in basis points (bps): 10_000 = 1.0x, 12_500 = 1.25x.
// A zero-value config disables surge pricing.
type InFlightSurgeConfig struct {
	// Threshold is the number of in-flight jobs before surge applies.
	Threshold uint32

	// PerJobBps is the additive multiplier (in bps) per job above Threshold.
	PerJobBps uint32

	// MaxMultiplierBps caps the total multiplier (in bps).
	//
	// If zero and PerJobBps > 0, a safe default cap is applied by the Provider runtime.
	MaxMultiplierBps uint32
}

// PricingConfig controls Provider-side pricing behavior at quote time.
type PricingConfig struct {
	InFlightSurge InFlightSurgeConfig
}

type Config struct {
	// Enabled controls whether this node acts as an LCP Provider (i.e. handles
	// inbound lcp_quote_request / lcp_cancel messages).
	//
	// When disabled, the handler MUST NOT create invoices or start execution.
	Enabled bool

	QuoteTTLSeconds uint64

	Pricing PricingConfig

	// Models restrict accepted/advertised `openai.chat_completions.v1` model IDs.
	//
	// If empty, any model is accepted and will be passed through to the compute
	// backend as the upstream model ID. In this case the local manifest MUST NOT
	// advertise `supported_tasks`.
	Models map[string]ModelConfig
}
