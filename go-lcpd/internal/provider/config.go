package provider

import "github.com/bruwbird/lcp/go-lcpd/internal/llm"

// OpenAIChatParams configures OpenAI-compatible Chat Completions parameters.
// These parameters are Provider-side defaults and are not sourced from request params.
type OpenAIChatParams struct {
	Temperature      *float64
	TopP             *float64
	Stop             []string
	PresencePenalty  *float64
	FrequencyPenalty *float64
	Seed             *int64
}

// LLMChatProfile configures a single `llm.chat` profile.
type LLMChatProfile struct {
	// BackendModel is the upstream model identifier passed to the compute backend.
	// If empty, the profile name is used.
	BackendModel string

	// MaxOutputTokens overrides the Provider-wide max output tokens for this profile.
	// If nil, the Provider-wide default is used.
	MaxOutputTokens *uint32

	// Price defines msat pricing for this profile (msat per 1M tokens).
	Price llm.PriceTableEntry

	// OpenAI defines per-profile OpenAI-compatible Chat Completions parameters.
	OpenAI OpenAIChatParams
}

type Config struct {
	// Enabled controls whether this node acts as an LCP Provider (i.e. handles
	// inbound lcp_quote_request / lcp_cancel messages).
	//
	// When disabled, the handler MUST NOT create invoices or start execution.
	Enabled bool

	QuoteTTLSeconds uint64

	// LLMChatProfiles restricts accepted/advertised `llm.chat` profile values.
	//
	// If empty, any profile is accepted and will be passed through to the compute
	// backend as the upstream model ID. In this case the local manifest MUST NOT
	// advertise `supported_tasks`.
	LLMChatProfiles map[string]LLMChatProfile
}
