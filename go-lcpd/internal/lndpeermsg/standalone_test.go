//nolint:testpackage // Test-only constructor for building PeerMessaging with a fake client without exporting it.
package lndpeermsg

import (
	"errors"
	"fmt"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/peerdirectory"
	"go.uber.org/zap"
)

type Option func(*PeerMessaging)

func WithInboundHandler(handler InboundMessageHandler) Option {
	return func(pm *PeerMessaging) {
		pm.inboundHandler = handler
	}
}

func NewStandalone(
	peerDirectory *peerdirectory.Directory,
	client PeerMessagingClient,
	logger *zap.SugaredLogger,
	localManifest *lcpwire.Manifest,
	opts ...Option,
) (*PeerMessaging, error) {
	if peerDirectory == nil {
		return nil, errors.New("PeerDirectory is required")
	}
	if client == nil {
		return nil, errors.New("client is required")
	}

	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	logger = logger.With("component", "lndpeermsg")

	maxPayloadBytes := defaultMaxPayloadBytes
	manifest := lcpwire.Manifest{
		ProtocolVersion: defaultProtocolVersion,
		MaxPayloadBytes: &maxPayloadBytes,
	}
	if localManifest != nil {
		manifest = *localManifest
	}

	payload, err := lcpwire.EncodeManifest(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode local lcp_manifest: %w", err)
	}

	pm := &PeerMessaging{
		peerDirectory:        peerDirectory,
		client:               client,
		logger:               logger,
		router:               nil,
		inboundHandler:       nil,
		localManifestPayload: payload,
		peerAddr:             make(map[string]string),
		manifestLastSent:     make(map[string]time.Time),
		peerEventsReady:      make(chan struct{}),
		customMessagesReady:  make(chan struct{}),
	}

	for _, opt := range opts {
		if opt != nil {
			opt(pm)
		}
	}
	pm.setDefaults()

	return pm, nil
}
