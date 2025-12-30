package llm

import (
	"errors"
	"fmt"
	"math/big"
)

const (
	tokensPerMillion = 1_000_000

	gpt52InputMsatPerMTok       = 1_750_000
	gpt52CachedInputMsatPerMTok = 175_000
	gpt52OutputMsatPerMTok      = 14_000_000
)

var (
	ErrUnknownModel  = errors.New("unknown model")
	ErrPriceOverflow = errors.New("price overflow")
)

// PriceTableEntry holds msat prices (per 1M tokens) for a model.
type PriceTableEntry struct {
	Model                  string
	InputMsatPerMTok       uint64
	CachedInputMsatPerMTok *uint64
	OutputMsatPerMTok      uint64
}

// PriceTable maps model identifiers to their pricing entry.
type PriceTable map[string]PriceTableEntry

// DefaultPriceTable returns the built-in price table for supported models.
func DefaultPriceTable() PriceTable {
	return PriceTable{
		"gpt-5.2": {
			Model:                  "gpt-5.2",
			InputMsatPerMTok:       gpt52InputMsatPerMTok,
			CachedInputMsatPerMTok: ptrUint64(gpt52CachedInputMsatPerMTok),
			OutputMsatPerMTok:      gpt52OutputMsatPerMTok,
		},
	}
}

// PriceTokens contains token counts for pricing.
type PriceTokens struct {
	InputTokens       uint64
	CachedInputTokens uint64
	OutputTokens      uint64
}

// PriceBreakdown reports the computed amounts for a quote/settlement.
type PriceBreakdown struct {
	InputTokens       uint64
	CachedInputTokens uint64
	OutputTokens      uint64
	PriceMsat         uint64
}

// QuotePrice computes the msat quote from a UsageEstimate and optional cached tokens.
func QuotePrice(
	model string,
	estimate UsageEstimate,
	cachedInputTokens uint64,
	table PriceTable,
) (PriceBreakdown, error) {
	tokens := PriceTokens{
		InputTokens:       estimate.InputTokens,
		CachedInputTokens: cachedInputTokens,
		OutputTokens:      estimate.MaxOutputTokens,
	}
	return priceFromTokens(model, tokens, table)
}

func priceFromTokens(
	model string,
	tokens PriceTokens,
	table PriceTable,
) (PriceBreakdown, error) {
	entry, ok := table[model]
	if !ok {
		return PriceBreakdown{}, fmt.Errorf("%w: %s", ErrUnknownModel, model)
	}

	priceMsat, err := entry.totalMsat(tokens)
	if err != nil {
		return PriceBreakdown{}, err
	}

	return PriceBreakdown{
		InputTokens:       tokens.InputTokens,
		CachedInputTokens: tokens.CachedInputTokens,
		OutputTokens:      tokens.OutputTokens,
		PriceMsat:         priceMsat,
	}, nil
}

func (e PriceTableEntry) totalMsat(tokens PriceTokens) (uint64, error) {
	cachedPrice := e.InputMsatPerMTok
	if e.CachedInputMsatPerMTok != nil {
		cachedPrice = *e.CachedInputMsatPerMTok
	}

	total := big.NewInt(0)
	addMul(total, tokens.InputTokens, e.InputMsatPerMTok)
	addMul(total, tokens.CachedInputTokens, cachedPrice)
	addMul(total, tokens.OutputTokens, e.OutputMsatPerMTok)

	msat := divCeilBig(total, big.NewInt(tokensPerMillion))
	if !msat.IsUint64() {
		return 0, ErrPriceOverflow
	}
	return msat.Uint64(), nil
}

func addMul(sum *big.Int, tokens uint64, pricePerMTokMsat uint64) {
	if tokens == 0 || pricePerMTokMsat == 0 {
		return
	}
	product := new(big.Int).SetUint64(tokens)
	product.Mul(product, new(big.Int).SetUint64(pricePerMTokMsat))
	sum.Add(sum, product)
}

func divCeilBig(n, d *big.Int) *big.Int {
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func ptrUint64(v uint64) *uint64 {
	return &v
}
