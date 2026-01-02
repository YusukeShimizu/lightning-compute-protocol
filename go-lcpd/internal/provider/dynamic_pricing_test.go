package provider

import (
	"math/big"
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/jobstore"
	"github.com/bruwbird/lcp/go-lcpd/internal/llm"
	"github.com/bruwbird/lcp/go-lcpd/internal/peerdirectory"
	"github.com/bruwbird/lcp/go-lcpd/internal/replaystore"
	"github.com/google/go-cmp/cmp"
)

func TestInFlightMultiplierBps_DefaultMaxCap(t *testing.T) {
	t.Parallel()

	cfg := InFlightSurgeConfig{
		Threshold: 0,
		PerJobBps: 5_000, // +50% per job
		// MaxMultiplierBps omitted -> default cap should apply.
	}
	if got, want := inFlightMultiplierBps(cfg, 10), uint64(30_000); got != want {
		t.Fatalf("multiplier_bps mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

func TestHandler_quotePrice_AppliesInFlightSurge(t *testing.T) {
	t.Parallel()

	policy := llm.MustFixedExecutionPolicy(llm.DefaultMaxOutputTokens)
	estimator := llm.NewApproxUsageEstimator()

	handler := NewHandler(
		Config{
			Enabled: true,
			Pricing: PricingConfig{
				InFlightSurge: InFlightSurgeConfig{
					Threshold:        1,
					PerJobBps:        500,    // +5%
					MaxMultiplierBps: 12_000, // 1.2x cap
				},
			},
		},
		DefaultValidator(),
		nil, // messenger not used by quotePrice
		nil, // invoices not used by quotePrice
		&fakeBackend{},
		policy,
		estimator,
		jobstore.New(),
		replaystore.New(0),
		peerdirectory.New(),
		nil,
	)

	// Simulate existing in-flight jobs.
	handler.jobMu.Lock()
	for i := 0; i < 5; i++ {
		var jobID lcp.JobID
		jobID[0] = byte(i + 1)
		handler.jobCancels[jobstore.Key{PeerPubKey: "peer", JobID: jobID}] = func() {}
	}
	handler.jobMu.Unlock()

	req := newLLMChatQuoteRequest("gpt-5.2")

	base := mustQuotePriceForPrompt(t, policy, estimator, "gpt-5.2", req.Input).PriceMsat
	got, err := handler.quotePrice(req)
	if err != nil {
		t.Fatalf("quotePrice: %v", err)
	}

	// in_flight_jobs=5, threshold=1 => over=4 => multiplier=10000+4*500=12000 (capped at 12000)
	multiplierBps := uint64(12_000)

	total := new(big.Int).SetUint64(base)
	total.Mul(total, new(big.Int).SetUint64(multiplierBps))
	q, r := new(big.Int).QuoRem(total, new(big.Int).SetUint64(10_000), new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsUint64() {
		t.Fatalf("expected price to fit in uint64")
	}
	wantPrice := q.Uint64()

	if diff := cmp.Diff(wantPrice, got.PriceMsat); diff != "" {
		t.Fatalf("surge price_msat mismatch (-want +got):\n%s", diff)
	}
}
