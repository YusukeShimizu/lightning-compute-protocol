package lndpeermsg

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpmsgrouter"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/lnrpc"
	"github.com/bruwbird/lcp/go-lcpd/internal/peerdirectory"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type PeerMessagingClient interface {
	ListPeers(
		ctx context.Context,
		in *lnrpc.ListPeersRequest,
		opts ...grpc.CallOption,
	) (*lnrpc.ListPeersResponse, error)
	SubscribePeerEvents(
		ctx context.Context,
		in *lnrpc.PeerEventSubscription,
		opts ...grpc.CallOption,
	) (lnrpc.Lightning_SubscribePeerEventsClient, error)
	SubscribeCustomMessages(
		ctx context.Context,
		in *lnrpc.SubscribeCustomMessagesRequest,
		opts ...grpc.CallOption,
	) (lnrpc.Lightning_SubscribeCustomMessagesClient, error)
	SendCustomMessage(
		ctx context.Context,
		in *lnrpc.SendCustomMessageRequest,
		opts ...grpc.CallOption,
	) (*lnrpc.SendCustomMessageResponse, error)
	DisconnectPeer(
		ctx context.Context,
		in *lnrpc.DisconnectPeerRequest,
		opts ...grpc.CallOption,
	) (*lnrpc.DisconnectPeerResponse, error)
}

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

	Config *Config `optional:"true"`

	PeerDirectory  *peerdirectory.Directory
	LocalManifest  *lcpwire.Manifest   `optional:"true"`
	Router         lcpmsgrouter.Router `optional:"true"`
	InboundHandler InboundMessageHandler
	Logger         *zap.SugaredLogger `optional:"true"`
}

type PeerMessaging struct {
	cfg            *Config
	peerDirectory  *peerdirectory.Directory
	logger         *zap.SugaredLogger
	router         lcpmsgrouter.Router
	inboundHandler InboundMessageHandler

	localManifestPayload []byte

	conn   *grpc.ClientConn
	client PeerMessagingClient

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
	defaultProtocolVersion = lcpwire.ProtocolVersionV03
	defaultMaxPayloadBytes = uint32(16384)
	defaultMaxStreamBytes  = uint64(4_194_304)
	defaultMaxCallBytes    = uint64(8_388_608)

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
	logger = logger.With("component", "lndpeermsg")

	maxPayloadBytes := defaultMaxPayloadBytes
	localManifest := lcpwire.Manifest{
		ProtocolVersion: defaultProtocolVersion,
		MaxPayloadBytes: maxPayloadBytes,
		MaxStreamBytes:  defaultMaxStreamBytes,
		MaxCallBytes:    defaultMaxCallBytes,
	}
	if p.LocalManifest != nil {
		localManifest = *p.LocalManifest
	}

	payload, err := lcpwire.EncodeManifest(localManifest)
	if err != nil {
		return nil, fmt.Errorf("encode local lcp_manifest: %w", err)
	}

	pm := &PeerMessaging{
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
		p.logger = zap.NewNop().Sugar().With("component", "lndpeermsg")
	}
	if p.router == nil {
		p.router = lcpmsgrouter.New()
	}
	if p.now == nil {
		p.now = time.Now
	}
}

func (p *PeerMessaging) start(ctx context.Context) error {
	if p.cfg == nil {
		p.logger.Infow(
			"lnd peer messaging disabled",
			"reason", "lnd connection not configured",
			"hint", "set LCPD_LND_RPC_ADDR (and LCPD_LND_TLS_CERT_PATH if required)",
		)
		return nil
	}

	rpcAddr := stringsTrim(p.cfg.RPCAddr)
	if rpcAddr == "" {
		p.logger.Infow(
			"lnd peer messaging disabled",
			"reason", "missing LCPD_LND_RPC_ADDR",
			"hint", "set LCPD_LND_RPC_ADDR (and LCPD_LND_TLS_CERT_PATH if required)",
		)
		return nil
	}

	manifestResendInterval := time.Duration(0)
	if p.cfg.ManifestResendInterval != nil {
		manifestResendInterval = *p.cfg.ManifestResendInterval
		if manifestResendInterval < 0 {
			p.logger.Warnw(
				"invalid LCPD_LND_MANIFEST_RESEND_INTERVAL; disabling periodic resend",
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

	conn, dialErr := dial(ctx, *p.cfg)
	if dialErr != nil {
		return dialErr
	}

	runCtx, cancel := context.WithCancel(context.Background())
	p.runCancel = cancel
	p.conn = conn
	p.client = lnrpc.NewLightningClient(conn)

	refreshErr := p.refreshPeers(runCtx)
	if refreshErr != nil {
		p.cleanupConn(cancel, conn)
		return refreshErr
	}
	p.sendManifestToConnectedPeersOnce(runCtx, "startup")

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

	peerEventsReadyErr := waitReady(ctx, p.peerEventsReady, "SubscribePeerEvents")
	if peerEventsReadyErr != nil {
		p.cleanupConn(cancel, conn)
		return peerEventsReadyErr
	}
	customMessagesReadyErr := waitReady(ctx, p.customMessagesReady, "SubscribeCustomMessages")
	if customMessagesReadyErr != nil {
		p.cleanupConn(cancel, conn)
		return customMessagesReadyErr
	}

	p.logger.Infow("lnd peer messaging started", "rpc_addr", p.cfg.RPCAddr)
	return nil
}

func (p *PeerMessaging) cleanupConn(cancel context.CancelFunc, conn *grpc.ClientConn) {
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
	p.conn = nil
	p.client = nil
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

	if p.conn != nil {
		_ = p.conn.Close()
	}

	p.conn = nil
	p.client = nil
	p.runCancel = nil
	return nil
}

func (p *PeerMessaging) peerEventsLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		stream, err := p.client.SubscribePeerEvents(ctx, &lnrpc.PeerEventSubscription{})
		if err != nil {
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
			p.HandlePeerEvent(ctx, ev)
		}
	}
}

func (p *PeerMessaging) customMessagesLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		stream, err := p.client.SubscribeCustomMessages(
			ctx,
			&lnrpc.SubscribeCustomMessagesRequest{},
		)
		if err != nil {
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
			p.HandleCustomMessage(ctx, msg)
		}
	}
}

func (p *PeerMessaging) HandlePeerEvent(ctx context.Context, ev *lnrpc.PeerEvent) {
	if ev == nil {
		return
	}

	peerPubKey := stringsTrim(ev.GetPubKey())
	if peerPubKey == "" {
		return
	}

	switch ev.GetType() {
	case lnrpc.PeerEvent_PEER_ONLINE:
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
	case lnrpc.PeerEvent_PEER_OFFLINE:
		p.peerDirectory.MarkDisconnected(peerPubKey)
	default:
	}
}

func (p *PeerMessaging) HandleCustomMessage(ctx context.Context, msg *lnrpc.CustomMessage) {
	if msg == nil {
		return
	}

	peerBytes := msg.GetPeer()
	if len(peerBytes) == 0 {
		return
	}
	peerPubKey := hex.EncodeToString(peerBytes)

	msgType32 := msg.GetType()
	if msgType32 > uint32(^uint16(0)) {
		p.logger.Warnw(
			"custom message type exceeds uint16; disconnecting",
			"peer_pub_key", peerPubKey,
			"msg_type", msgType32,
		)
		p.disconnectPeer(ctx, peerPubKey)
		return
	}
	msgType := uint16(msgType32)

	payload := append([]byte(nil), msg.GetData()...)

	p.markPeerConnected(peerPubKey)

	customMsg := lcpmsgrouter.CustomMessage{
		PeerPubKey: peerPubKey,
		MsgType:    msgType,
		Payload:    payload,
	}
	decision := p.router.Route(customMsg)

	switch decision.Action {
	case lcpmsgrouter.RouteActionDispatchManifest:
		// The remote may not have been ready to receive our startup/online
		// `lcp_manifest` (for example, if their SubscribeCustomMessages stream was
		// not yet established). To avoid ending up in a one-sided "LCP-ready"
		// state, reply with our manifest once when we observe the peer's first
		// manifest, even if we already marked our manifest as sent.
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
	case lcpmsgrouter.RouteActionDispatchCall,
		lcpmsgrouter.RouteActionDispatchQuote,
		lcpmsgrouter.RouteActionDispatchStreamBegin,
		lcpmsgrouter.RouteActionDispatchStreamChunk,
		lcpmsgrouter.RouteActionDispatchStreamEnd,
		lcpmsgrouter.RouteActionDispatchComplete,
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
	resp, listErr := p.client.ListPeers(ctx, &lnrpc.ListPeersRequest{})
	if listErr != nil {
		return fmt.Errorf("ListPeers: %w", listErr)
	}

	peers := resp.GetPeers()
	addrMap := make(map[string]string, len(peers))
	for _, peer := range peers {
		if peer == nil {
			continue
		}
		pubKey := stringsTrim(peer.GetPubKey())
		if pubKey == "" {
			continue
		}
		addrMap[pubKey] = stringsTrim(peer.GetAddress())
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

func (p *PeerMessaging) sendManifest(ctx context.Context, peerPubKey string) error {
	peerBytes, err := hex.DecodeString(peerPubKey)
	if err != nil {
		return fmt.Errorf("decode peer pubkey: %w", err)
	}

	_, err = p.client.SendCustomMessage(ctx, &lnrpc.SendCustomMessageRequest{
		Peer: peerBytes,
		Type: uint32(lcpwire.MessageTypeManifest),
		Data: p.localManifestPayload,
	})
	if err != nil {
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
	_, err := p.client.DisconnectPeer(ctx, &lnrpc.DisconnectPeerRequest{PubKey: peerPubKey})
	if err != nil {
		p.logger.Warnw("disconnect peer failed", "peer_pub_key", peerPubKey, "err", err)
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
