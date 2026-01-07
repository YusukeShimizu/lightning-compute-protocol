package peermsg

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpmsgrouter"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lightningnode"
	"github.com/bruwbird/lcp/go-lcpd/internal/peerdirectory"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type InboundCustomMessage struct {
	PeerPubKey string
	MsgType    uint16
	Payload    []byte
}

type InboundMessageHandler interface {
	HandleInboundCustomMessage(context.Context, InboundCustomMessage)
}

type InboundMessageHandlerFunc func(context.Context, InboundCustomMessage)

func (f InboundMessageHandlerFunc) HandleInboundCustomMessage(
	ctx context.Context,
	msg InboundCustomMessage,
) {
	if f == nil {
		return
	}
	f(ctx, msg)
}

type Params struct {
	fx.In

	Lifecycle fx.Lifecycle

	Subscriber lightningnode.PeerSubscriber `optional:"true"`
	Messenger  lightningnode.PeerMessenger  `optional:"true"`

	Config *Config `optional:"true"`

	PeerDirectory  *peerdirectory.Directory
	LocalManifest  *lcpwire.Manifest   `optional:"true"`
	Router         lcpmsgrouter.Router `optional:"true"`
	InboundHandler InboundMessageHandler
	Logger         *zap.SugaredLogger `optional:"true"`
}

type PeerMessaging struct {
	subscriber lightningnode.PeerSubscriber
	messenger  lightningnode.PeerMessenger
	cfg        *Config

	peerDirectory  *peerdirectory.Directory
	logger         *zap.SugaredLogger
	router         lcpmsgrouter.Router
	inboundHandler InboundMessageHandler

	localManifestPayload []byte

	now func() time.Time

	runCancel context.CancelFunc
	wg        sync.WaitGroup

	peerEventsReadyOnce     sync.Once
	peerEventsReady         chan struct{}
	customMessagesReadyOnce sync.Once
	customMessagesReady     chan struct{}

	peerAddrMu sync.RWMutex
	peerAddr   map[string]string

	manifestSendMu sync.Mutex
}

const (
	defaultProtocolVersion = lcpwire.ProtocolVersionV02
	defaultMaxPayloadBytes = uint32(16384)
	defaultMaxStreamBytes  = uint64(4_194_304)
	defaultMaxJobBytes     = uint64(8_388_608)

	defaultReconnectBackoff = 1 * time.Second

	subscriptionLoops = 2
)

func New(p Params) (*PeerMessaging, error) {
	if p.PeerDirectory == nil {
		return nil, errors.New("PeerDirectory is required")
	}

	logger := p.Logger
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	logger = logger.With("component", "peermsg")

	maxPayloadBytes := defaultMaxPayloadBytes
	localManifest := lcpwire.Manifest{
		ProtocolVersion: defaultProtocolVersion,
		MaxPayloadBytes: maxPayloadBytes,
		MaxStreamBytes:  defaultMaxStreamBytes,
		MaxJobBytes:     defaultMaxJobBytes,
	}
	if p.LocalManifest != nil {
		localManifest = *p.LocalManifest
	}

	payload, err := lcpwire.EncodeManifest(localManifest)
	if err != nil {
		return nil, fmt.Errorf("encode local lcp_manifest: %w", err)
	}

	pm := &PeerMessaging{
		subscriber:           p.Subscriber,
		messenger:            p.Messenger,
		cfg:                  p.Config,
		peerDirectory:        p.PeerDirectory,
		logger:               logger,
		router:               p.Router,
		inboundHandler:       p.InboundHandler,
		localManifestPayload: payload,
		peerAddr:             make(map[string]string),
		peerEventsReady:      make(chan struct{}),
		customMessagesReady:  make(chan struct{}),
	}

	pm.setDefaults()

	p.Lifecycle.Append(fx.Hook{
		OnStart: pm.start,
		OnStop:  pm.stop,
	})

	return pm, nil
}

func (p *PeerMessaging) setDefaults() {
	if p.logger == nil {
		p.logger = zap.NewNop().Sugar().With("component", "peermsg")
	}
	if p.router == nil {
		p.router = lcpmsgrouter.New()
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.subscriber == nil || p.messenger == nil {
		disabled := lightningnode.NewDisabled()
		p.subscriber = disabled
		p.messenger = disabled
	}
}

func (p *PeerMessaging) start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(context.Background())
	p.runCancel = cancel

	refreshErr := p.refreshPeers(runCtx)
	if refreshErr != nil {
		if errors.Is(refreshErr, lightningnode.ErrNotConfigured) {
			p.logger.Infow(
				"peer messaging disabled",
				"reason", "lightning node not configured",
			)
			p.cleanup(cancel)
			return nil
		}
		p.cleanup(cancel)
		return refreshErr
	}
	p.sendManifestToConnectedPeersOnce(runCtx, "startup")

	manifestResendInterval := time.Duration(0)
	if p.cfg != nil && p.cfg.ManifestResendInterval != nil {
		manifestResendInterval = *p.cfg.ManifestResendInterval
		if manifestResendInterval < 0 {
			p.logger.Warnw(
				"invalid manifest resend interval; disabling periodic resend",
				"value", manifestResendInterval,
			)
			manifestResendInterval = 0
		} else if manifestResendInterval > 0 {
			p.logger.Infow(
				"periodic lcp_manifest resend enabled",
				"interval", manifestResendInterval,
			)
		}
	}

	if manifestResendInterval > 0 {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.manifestResendLoop(runCtx, manifestResendInterval)
		}()
	}

	p.wg.Add(subscriptionLoops)
	go func() {
		defer p.wg.Done()
		p.peerEventsLoop(runCtx)
	}()
	go func() {
		defer p.wg.Done()
		p.customMessagesLoop(runCtx)
	}()

	if err := waitReady(ctx, p.peerEventsReady, "SubscribePeerEvents"); err != nil {
		p.cleanup(cancel)
		return err
	}
	if err := waitReady(ctx, p.customMessagesReady, "SubscribeCustomMessages"); err != nil {
		p.cleanup(cancel)
		return err
	}

	p.logger.Infow("peer messaging started")
	return nil
}

func (p *PeerMessaging) cleanup(cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	p.runCancel = nil
}

func (p *PeerMessaging) stop(ctx context.Context) error {
	if p.runCancel != nil {
		p.runCancel()
	}

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}

	p.runCancel = nil
	return nil
}

func (p *PeerMessaging) peerEventsLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		stream, err := p.subscriber.SubscribePeerEvents(ctx)
		if err != nil {
			if errors.Is(err, lightningnode.ErrNotConfigured) {
				return
			}
			p.logger.Warnw("subscribe peer events failed", "err", err)
			sleepContext(ctx, defaultReconnectBackoff)
			continue
		}
		p.peerEventsReadyOnce.Do(func() { close(p.peerEventsReady) })

		for {
			ev, recvErr := stream.Recv()
			if recvErr != nil {
				if isStreamTerminalError(recvErr) || ctx.Err() != nil {
					return
				}
				p.logger.Warnw("peer events recv failed", "err", recvErr)
				break
			}
			p.handlePeerEvent(ctx, ev)
		}
	}
}

func (p *PeerMessaging) customMessagesLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		stream, err := p.subscriber.SubscribeCustomMessages(ctx)
		if err != nil {
			if errors.Is(err, lightningnode.ErrNotConfigured) {
				return
			}
			p.logger.Warnw("subscribe custom messages failed", "err", err)
			sleepContext(ctx, defaultReconnectBackoff)
			continue
		}
		p.customMessagesReadyOnce.Do(func() { close(p.customMessagesReady) })

		for {
			msg, recvErr := stream.Recv()
			if recvErr != nil {
				if isStreamTerminalError(recvErr) || ctx.Err() != nil {
					return
				}
				p.logger.Warnw("custom messages recv failed", "err", recvErr)
				break
			}
			p.handleCustomMessage(ctx, msg)
		}
	}
}

func (p *PeerMessaging) handlePeerEvent(ctx context.Context, ev *lightningnode.PeerEvent) {
	if ev == nil {
		return
	}

	peerPubKey := stringsTrim(ev.PubKey)
	if peerPubKey == "" {
		return
	}

	switch ev.Type {
	case lightningnode.PeerEventTypeOnline:
		if err := p.refreshPeers(ctx); err != nil {
			p.logger.Warnw(
				"refresh peers on online failed",
				"peer_pub_key", peerPubKey,
				"err", err,
			)
		}
		p.markPeerConnected(peerPubKey)
		if err := p.sendManifestIfNotSent(ctx, peerPubKey, "peer_online"); err != nil {
			p.logger.Warnw(
				"send lcp_manifest on peer online failed",
				"peer_pub_key", peerPubKey,
				"err", err,
			)
		}
	case lightningnode.PeerEventTypeOffline:
		p.peerDirectory.MarkDisconnected(peerPubKey)
	case lightningnode.PeerEventTypeUnspecified:
		return
	default:
	}
}

func (p *PeerMessaging) handleCustomMessage(ctx context.Context, msg *lightningnode.CustomMessage) {
	if msg == nil {
		return
	}

	peerPubKey := stringsTrim(msg.PeerPubKey)
	if peerPubKey == "" {
		return
	}

	if msg.MsgType > uint32(^uint16(0)) {
		p.logger.Warnw(
			"custom message type exceeds uint16; disconnecting",
			"peer_pub_key", peerPubKey,
			"msg_type", msg.MsgType,
		)
		p.disconnectPeer(ctx, peerPubKey)
		return
	}
	msgType := uint16(msg.MsgType)

	payload := append([]byte(nil), msg.Payload...)

	p.markPeerConnected(peerPubKey)

	customMsg := lcpmsgrouter.CustomMessage{
		PeerPubKey: peerPubKey,
		MsgType:    msgType,
		Payload:    payload,
	}
	decision := p.router.Route(customMsg)

	switch decision.Action {
	case lcpmsgrouter.RouteActionDispatchManifest:
		peerBefore, _ := p.peerDirectory.GetPeer(peerPubKey)
		hadRemoteManifest := peerBefore.RemoteManifest != nil

		manifest, decodeErr := lcpwire.DecodeManifest(payload)
		if decodeErr != nil {
			p.logger.Warnw(
				"decode lcp_manifest failed",
				"peer_pub_key", peerPubKey,
				"err", decodeErr,
			)
			return
		}
		p.peerDirectory.MarkLCPReady(peerPubKey, manifest)
		if !hadRemoteManifest {
			if err := p.sendManifestAndMarkSent(ctx, peerPubKey, "reply_to_inbound_manifest"); err != nil {
				p.logger.Warnw(
					"send lcp_manifest reply failed",
					"peer_pub_key", peerPubKey,
					"err", err,
				)
			}
			return
		}
		if err := p.sendManifestIfNotSent(ctx, peerPubKey, "reply_to_inbound_manifest"); err != nil {
			p.logger.Warnw(
				"send lcp_manifest reply failed",
				"peer_pub_key", peerPubKey,
				"err", err,
			)
		}
	case lcpmsgrouter.RouteActionDispatchQuoteRequest,
		lcpmsgrouter.RouteActionDispatchQuoteResponse,
		lcpmsgrouter.RouteActionDispatchStreamBegin,
		lcpmsgrouter.RouteActionDispatchStreamChunk,
		lcpmsgrouter.RouteActionDispatchStreamEnd,
		lcpmsgrouter.RouteActionDispatchResult,
		lcpmsgrouter.RouteActionDispatchCancel,
		lcpmsgrouter.RouteActionDispatchError:
		p.dispatchInbound(ctx, customMsg, decision)
	case lcpmsgrouter.RouteActionDisconnect:
		p.logger.Warnw(
			"custom message routed to disconnect",
			"peer_pub_key", peerPubKey,
			"msg_type", msgType,
			"reason", decision.Reason,
		)
		p.disconnectPeer(ctx, peerPubKey)
	case lcpmsgrouter.RouteActionIgnore:
		p.logger.Debugw(
			"custom message ignored",
			"peer_pub_key", peerPubKey,
			"msg_type", msgType,
			"reason", decision.Reason,
		)
	}
}

func (p *PeerMessaging) markPeerConnected(peerPubKey string) {
	if addr := p.peerAddrFor(peerPubKey); addr != "" {
		p.peerDirectory.UpsertPeer(peerPubKey, addr)
	}
	p.peerDirectory.MarkConnected(peerPubKey)
	p.peerDirectory.MarkCustomMsgEnabled(peerPubKey, true)
}

func (p *PeerMessaging) peerAddrFor(peerPubKey string) string {
	p.peerAddrMu.RLock()
	defer p.peerAddrMu.RUnlock()
	return p.peerAddr[peerPubKey]
}

func (p *PeerMessaging) refreshPeers(ctx context.Context) error {
	peers, err := p.subscriber.ListPeers(ctx)
	if err != nil {
		return err
	}

	addrMap := make(map[string]string, len(peers))
	for _, peer := range peers {
		pubKey := stringsTrim(peer.PubKey)
		if pubKey == "" {
			continue
		}
		addrMap[pubKey] = stringsTrim(peer.Address)
	}

	p.peerAddrMu.Lock()
	for k, v := range addrMap {
		if v == "" {
			continue
		}
		p.peerAddr[k] = v
	}
	p.peerAddrMu.Unlock()

	for peerPubKey, addr := range addrMap {
		if addr != "" {
			p.peerDirectory.UpsertPeer(peerPubKey, addr)
		}
		p.peerDirectory.MarkConnected(peerPubKey)
		p.peerDirectory.MarkCustomMsgEnabled(peerPubKey, true)
	}

	return nil
}

func (p *PeerMessaging) sendManifestToConnectedPeersOnce(ctx context.Context, reason string) {
	peerIDs := p.peerDirectory.ListConnectedPeerIDs()
	for _, peerID := range peerIDs {
		if ctx.Err() != nil {
			return
		}
		if err := p.sendManifestIfNotSent(ctx, peerID, reason); err != nil {
			p.logger.Debugw(
				"send lcp_manifest failed",
				"peer_pub_key", peerID,
				"reason", reason,
				"err", err,
			)
		}
	}
}

func (p *PeerMessaging) sendManifestToConnectedPeers(ctx context.Context, reason string) {
	peerIDs := p.peerDirectory.ListConnectedPeerIDs()
	for _, peerID := range peerIDs {
		if ctx.Err() != nil {
			return
		}
		if err := p.sendManifestAndMarkSent(ctx, peerID, reason); err != nil {
			p.logger.Debugw(
				"send lcp_manifest failed",
				"peer_pub_key", peerID,
				"reason", reason,
				"err", err,
			)
		}
	}
}

func (p *PeerMessaging) sendManifestAndMarkSent(
	ctx context.Context,
	peerPubKey string,
	reason string,
) error {
	if peerPubKey == "" {
		return nil
	}

	p.manifestSendMu.Lock()
	defer p.manifestSendMu.Unlock()

	if err := p.sendManifest(ctx, peerPubKey); err != nil {
		return err
	}
	p.peerDirectory.MarkManifestSent(peerPubKey)

	p.logger.Debugw(
		"sent lcp_manifest",
		"peer_pub_key", peerPubKey,
		"reason", reason,
	)
	return nil
}

func (p *PeerMessaging) sendManifestIfNotSent(
	ctx context.Context,
	peerPubKey string,
	reason string,
) error {
	if peerPubKey == "" {
		return nil
	}

	p.manifestSendMu.Lock()
	defer p.manifestSendMu.Unlock()

	if peer, ok := p.peerDirectory.GetPeer(peerPubKey); ok && peer.ManifestSent {
		return nil
	}

	if err := p.sendManifest(ctx, peerPubKey); err != nil {
		return err
	}
	p.peerDirectory.MarkManifestSent(peerPubKey)

	p.logger.Debugw(
		"sent lcp_manifest",
		"peer_pub_key", peerPubKey,
		"reason", reason,
	)
	return nil
}

func (p *PeerMessaging) sendManifest(ctx context.Context, peerPubKey string) error {
	peerPubKey = stringsTrim(peerPubKey)
	if peerPubKey == "" {
		return fmt.Errorf("%w: peer_pub_key is required", lightningnode.ErrInvalidRequest)
	}
	if _, err := hex.DecodeString(peerPubKey); err != nil {
		return fmt.Errorf(
			"%w: decode peer_pub_key: %s",
			lightningnode.ErrInvalidRequest,
			err.Error(),
		)
	}

	if err := p.messenger.SendCustomMessage(
		ctx,
		peerPubKey,
		lcpwire.MessageTypeManifest,
		p.localManifestPayload,
	); err != nil {
		return fmt.Errorf("SendCustomMessage: %w", err)
	}
	return nil
}

func (p *PeerMessaging) dispatchInbound(
	ctx context.Context,
	msg lcpmsgrouter.CustomMessage,
	decision lcpmsgrouter.RouteDecision,
) {
	if p.inboundHandler == nil {
		p.logger.Debugw(
			"drop inbound custom message; no handler",
			"peer_pub_key", msg.PeerPubKey,
			"msg_type", msg.MsgType,
			"reason", decision.Reason,
		)
		return
	}

	p.inboundHandler.HandleInboundCustomMessage(ctx, InboundCustomMessage{
		PeerPubKey: msg.PeerPubKey,
		MsgType:    msg.MsgType,
		Payload:    msg.Payload,
	})
}

func (p *PeerMessaging) disconnectPeer(ctx context.Context, peerPubKey string) {
	if err := p.subscriber.DisconnectPeer(ctx, peerPubKey); err != nil {
		p.logger.Warnw("disconnect peer failed", "peer_pub_key", peerPubKey, "err", err)
	}
}

func (p *PeerMessaging) manifestResendLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	p.manifestResendLoopWithTicks(ctx, ticker.C)
}

func (p *PeerMessaging) manifestResendLoopWithTicks(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			p.sendManifestToConnectedPeers(ctx, "periodic_resend")
		}
	}
}

func isStreamTerminalError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, io.EOF)
}

func sleepContext(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func waitReady(ctx context.Context, ch <-chan struct{}, name string) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("%s not ready: %w", name, ctx.Err())
	case <-ch:
		return nil
	}
}

func stringsTrim(s string) string {
	return strings.TrimSpace(s)
}
