package provider

import (
	"errors"
	"math/big"

	"github.com/bruwbird/lcp/go-lcpd/internal/llm"
)

const (
	multiplierBpsOne = uint64(10_000)

	// Default cap for pricing.in_flight_surge.max_multiplier_bps when omitted.
	defaultMaxInFlightSurgeMultiplierBps = uint32(30_000) // 3.0x
)

func inFlightMultiplierBps(cfg InFlightSurgeConfig, inFlightJobs uint64) uint64 {
	if cfg.PerJobBps == 0 {
		return multiplierBpsOne
	}

	inFlight := inFlightJobs

	threshold := uint64(cfg.Threshold)
	over := uint64(0)
	if inFlight > threshold {
		over = inFlight - threshold
	}

	bps := multiplierBpsOne + uint64(cfg.PerJobBps)*over

	maxBps := cfg.MaxMultiplierBps
	if maxBps == 0 {
		maxBps = defaultMaxInFlightSurgeMultiplierBps
	}
	if maxBps != 0 && bps > uint64(maxBps) {
		bps = uint64(maxBps)
	}
	if bps < multiplierBpsOne {
		bps = multiplierBpsOne
	}

	return bps
}

func applyMultiplierCeil(priceMsat uint64, multiplierBps uint64) (uint64, error) {
	if multiplierBps == 0 {
		return 0, errors.New("multiplier_bps must be > 0")
	}
	if multiplierBps == multiplierBpsOne {
		return priceMsat, nil
	}

	total := new(big.Int).SetUint64(priceMsat)
	total.Mul(total, new(big.Int).SetUint64(multiplierBps))

	q, r := new(big.Int).QuoRem(total, new(big.Int).SetUint64(multiplierBpsOne), new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsUint64() {
		return 0, llm.ErrPriceOverflow
	}
	return q.Uint64(), nil
}
