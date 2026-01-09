package peermsg

import "time"

type Config struct {
	// ManifestResendInterval, if set to a positive duration, periodically
	// re-sends `lcp_manifest` to connected peers.
	//
	// Unset (or <= 0) disables periodic re-sends.
	ManifestResendInterval *time.Duration
}
