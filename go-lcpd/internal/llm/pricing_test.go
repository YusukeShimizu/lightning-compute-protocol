//nolint:testpackage // These tests intentionally use the internal package to access unexported pricing helpers.
package llm

import (
	"errors"
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/computebackend"
	"github.com/google/go-cmp/cmp"
)

func TestQuotePrice_UsesDefaultTable(t *testing.T) {
	t.Parallel()

	table := DefaultPriceTable()

	estimate := UsageEstimate{
		InputTokens:     2,
		MaxOutputTokens: 10,
		TotalTokens:     12,
	}

	got, err := QuotePrice("gpt-5.2", estimate, 0, table)
	if err != nil {
		t.Fatalf("QuotePrice: %v", err)
	}

	want := PriceBreakdown{
		InputTokens:       2,
		CachedInputTokens: 0,
		OutputTokens:      10,
		PriceMsat:         144, // ceil((2*1.75e6 + 10*14e6) / 1e6) = 144 msat
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("price mismatch (-want +got):\n%s", diff)
	}
}

func TestQuotePrice_UsesCachedPriceWhenDefined(t *testing.T) {
	t.Parallel()

	table := DefaultPriceTable()

	estimate := UsageEstimate{
		InputTokens:     0,
		MaxOutputTokens: 0,
	}

	got, err := QuotePrice("gpt-5.2", estimate, 1_000_000, table)
	if err != nil {
		t.Fatalf("QuotePrice: %v", err)
	}

	want := PriceBreakdown{
		InputTokens:       0,
		CachedInputTokens: 1_000_000,
		OutputTokens:      0,
		PriceMsat:         175_000, // 0.175 msat per token * 1M tokens
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("price mismatch (-want +got):\n%s", diff)
	}
}

func TestQuotePrice_FallsBackToInputWhenCachedMissing(t *testing.T) {
	t.Parallel()

	table := PriceTable{
		"gpt-5.2": {
			Model:             "gpt-5.2",
			InputMsatPerMTok:  21_000_000,
			OutputMsatPerMTok: 168_000_000,
		},
	}

	estimate := UsageEstimate{
		InputTokens:     0,
		MaxOutputTokens: 0,
	}

	got, err := QuotePrice("gpt-5.2", estimate, 100, table)
	if err != nil {
		t.Fatalf("QuotePrice: %v", err)
	}

	want := PriceBreakdown{
		InputTokens:       0,
		CachedInputTokens: 100,
		OutputTokens:      0,
		PriceMsat:         2_100, // cached charged at input price (21,000,000 msat/MTok)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("price mismatch (-want +got):\n%s", diff)
	}
}

func TestSettlePrice_UsesActualUsage(t *testing.T) {
	t.Parallel()

	table := PriceTable{
		"gpt-5.2": {
			Model:             "gpt-5.2",
			InputMsatPerMTok:  250_000,
			OutputMsatPerMTok: 2_000_000,
		},
	}

	usage := computebackend.Usage{
		InputUnits:  3,
		OutputUnits: 5,
		TotalUnits:  0,
	}

	got, err := SettlePrice("gpt-5.2", usage, 0, table)
	if err != nil {
		t.Fatalf("SettlePrice: %v", err)
	}

	want := PriceBreakdown{
		InputTokens:       3,
		CachedInputTokens: 0,
		OutputTokens:      5,
		PriceMsat:         11, // ceil((3*0.25e6 + 5*2.0e6)/1e6) = 10.75 -> 11 msat
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("price mismatch (-want +got):\n%s", diff)
	}
}

func TestQuotePrice_UnknownModel(t *testing.T) {
	t.Parallel()

	_, err := QuotePrice("unknown", UsageEstimate{}, 0, DefaultPriceTable())
	if !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("expected ErrUnknownModel, got %v", err)
	}
}
