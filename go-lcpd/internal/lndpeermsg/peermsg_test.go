//nolint:testpackage // These tests need access to unexported fields to inject a fake Lightning client.
package lndpeermsg

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/lnrpc"
	"github.com/bruwbird/lcp/go-lcpd/internal/peerdirectory"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type fakeLightningClient struct {
	mu sync.Mutex

	listPeersResp *lnrpc.ListPeersResponse

	sendReqs       []*lnrpc.SendCustomMessageRequest
	disconnectReqs []*lnrpc.DisconnectPeerRequest
}

func (f *fakeLightningClient) ListPeers(
	context.Context,
	*lnrpc.ListPeersRequest,
	...grpc.CallOption,
) (*lnrpc.ListPeersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listPeersResp == nil {
		return &lnrpc.ListPeersResponse{}, nil
	}
	return f.listPeersResp, nil
}

func (*fakeLightningClient) SubscribePeerEvents(
	context.Context,
	*lnrpc.PeerEventSubscription,
	...grpc.CallOption,
) (lnrpc.Lightning_SubscribePeerEventsClient, error) {
	return nil, errors.New("SubscribePeerEvents not implemented in fake")
}

func (*fakeLightningClient) SubscribeCustomMessages(
	context.Context,
	*lnrpc.SubscribeCustomMessagesRequest,
	...grpc.CallOption,
) (lnrpc.Lightning_SubscribeCustomMessagesClient, error) {
	return nil, errors.New("SubscribeCustomMessages not implemented in fake")
}

func (f *fakeLightningClient) SendCustomMessage(
	_ context.Context,
	req *lnrpc.SendCustomMessageRequest,
	_ ...grpc.CallOption,
) (*lnrpc.SendCustomMessageResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendReqs = append(f.sendReqs, req)
	return &lnrpc.SendCustomMessageResponse{}, nil
}

func (f *fakeLightningClient) DisconnectPeer(
	_ context.Context,
	req *lnrpc.DisconnectPeerRequest,
	_ ...grpc.CallOption,
) (*lnrpc.DisconnectPeerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnectReqs = append(f.disconnectReqs, req)
	return &lnrpc.DisconnectPeerResponse{}, nil
}

func newDiscardLogger() *zap.SugaredLogger {
	return zap.NewNop().Sugar()
}

func newPeerPubKey() ([]byte, string) {
	peerBytes := make([]byte, 33)
	for i := range peerBytes {
		peerBytes[i] = byte(i + 1)
	}
	return peerBytes, hex.EncodeToString(peerBytes)
}

func TestPeerMessaging_HandleCustomMessage_DispatchesJobMessages(t *testing.T) {
	t.Parallel()

	fake := &fakeLightningClient{}
	dir := peerdirectory.New()

	var (
		mu        sync.Mutex
		received  []InboundCustomMessage
		localCopy []byte
	)
	handler := InboundMessageHandlerFunc(func(
		_ context.Context,
		msg InboundCustomMessage,
	) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, msg)
		localCopy = append([]byte(nil), msg.Payload...)
	})

	pm, err := NewStandalone(
		dir,
		fake,
		newDiscardLogger(),
		nil,
		WithInboundHandler(handler),
	)
	if err != nil {
		t.Fatalf("NewStandalone: %v", err)
	}

	peerBytes, peerPubKey := newPeerPubKey()
	payload := []byte{0x01, 0x02}

	pm.HandleCustomMessage(context.Background(), &lnrpc.CustomMessage{
		Peer: peerBytes,
		Type: uint32(lcpwire.MessageTypeCall),
		Data: payload,
	})

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(received), 1; got != want {
		t.Fatalf("handler call count mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	want := InboundCustomMessage{
		PeerPubKey: peerPubKey,
		MsgType:    uint16(lcpwire.MessageTypeCall),
		Payload:    payload,
	}
	if diff := cmp.Diff(want, received[0]); diff != "" {
		t.Fatalf("handler message mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(payload, localCopy); diff != "" {
		t.Fatalf("handler payload copy mismatch (-want +got):\n%s", diff)
	}
}

func TestPeerMessaging_start_NoConfig_DisablesWithoutError(t *testing.T) {
	t.Parallel()

	fake := &fakeLightningClient{}
	dir := peerdirectory.New()

	pm, err := NewStandalone(dir, fake, newDiscardLogger(), nil)
	if err != nil {
		t.Fatalf("NewStandalone: %v", err)
	}

	// NewStandalone does not set lnd config.
	if pm.cfg != nil {
		t.Fatalf("cfg expected nil (-want +got):\n%s", cmp.Diff((*Config)(nil), pm.cfg))
	}

	startErr := pm.start(context.Background())
	if startErr != nil {
		t.Fatalf("start returned error: %v", startErr)
	}
}

func TestPeerMessaging_start_MissingRPCAddr_DisablesWithoutError(t *testing.T) {
	t.Parallel()

	fake := &fakeLightningClient{}
	dir := peerdirectory.New()

	pm, err := NewStandalone(dir, fake, newDiscardLogger(), nil)
	if err != nil {
		t.Fatalf("NewStandalone: %v", err)
	}

	pm.cfg = &Config{
		RPCAddr:           "",
		TLSCertPath:       "",
		AdminMacaroonPath: "",
	}

	startErr := pm.start(context.Background())
	if startErr != nil {
		t.Fatalf("start returned error: %v", startErr)
	}
}

func TestPeerMessaging_manifestResendLoopWithTicks_ResendsManifestToConnectedPeers(t *testing.T) {
	t.Parallel()

	fake := &fakeLightningClient{}
	dir := peerdirectory.New()

	pm, err := NewStandalone(dir, fake, newDiscardLogger(), nil)
	if err != nil {
		t.Fatalf("NewStandalone: %v", err)
	}

	peerBytes, peerPubKey := newPeerPubKey()
	dir.MarkConnected(peerPubKey)
	dir.MarkManifestSent(peerPubKey) // resend should still send again

	ticks := make(chan time.Time, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pm.manifestResendLoopWithTicks(ctx, ticks)
		close(done)
	}()

	ticks <- time.Now()
	ticks <- time.Now()

	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	for {
		fake.mu.Lock()
		got := len(fake.sendReqs)
		fake.mu.Unlock()
		if got >= 2 {
			break
		}

		select {
		case <-deadline.C:
			t.Fatalf("timeout waiting for SendCustomMessage calls; got=%d want>=2", got)
		default:
			time.Sleep(1 * time.Millisecond)
		}
	}

	cancel()
	<-done

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if got, want := len(fake.sendReqs), 2; got != want {
		t.Fatalf("SendCustomMessage calls mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	for i, req := range fake.sendReqs {
		if got, want := req.GetType(), uint32(lcpwire.MessageTypeManifest); got != want {
			t.Fatalf("send[%d] type mismatch (-want +got):\n%s", i, cmp.Diff(want, got))
		}
		if diff := cmp.Diff(peerBytes, req.GetPeer()); diff != "" {
			t.Fatalf("send[%d] peer mismatch (-want +got):\n%s", i, diff)
		}
		if diff := cmp.Diff(pm.localManifestPayload, req.GetData()); diff != "" {
			t.Fatalf("send[%d] manifest payload mismatch (-want +got):\n%s", i, diff)
		}
	}
}

func TestPeerMessaging_HandleCustomMessage_UnknownEvenDisconnects(t *testing.T) {
	t.Parallel()

	fake := &fakeLightningClient{}
	dir := peerdirectory.New()

	pm, err := NewStandalone(dir, fake, newDiscardLogger(), nil)
	if err != nil {
		t.Fatalf("NewStandalone: %v", err)
	}

	peerBytes, peerPubKey := newPeerPubKey()

	pm.HandleCustomMessage(context.Background(), &lnrpc.CustomMessage{
		Peer: peerBytes,
		Type: uint32(lcpwire.MessageTypeManifest + 1), // even
		Data: []byte("payload"),
	})

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got, want := len(fake.disconnectReqs), 1; got != want {
		t.Fatalf("disconnect calls mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := fake.disconnectReqs[0].GetPubKey(), peerPubKey; got != want {
		t.Fatalf("disconnect pubkey mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

func TestPeerMessaging_HandleCustomMessage_LCPManifestMarksReadyAndRepliesOnce(t *testing.T) {
	t.Parallel()

	fake := &fakeLightningClient{}
	dir := peerdirectory.New()

	pm, err := NewStandalone(dir, fake, newDiscardLogger(), nil)
	if err != nil {
		t.Fatalf("NewStandalone: %v", err)
	}

	peerBytes, peerPubKey := newPeerPubKey()

	remoteManifestPayload, err := lcpwire.EncodeManifest(lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV02,
		MaxPayloadBytes: 123,
		MaxStreamBytes:  1024,
		MaxCallBytes:    2048,
	})
	if err != nil {
		t.Fatalf("encode remote manifest: %v", err)
	}

	msg := &lnrpc.CustomMessage{
		Peer: peerBytes,
		Type: uint32(lcpwire.MessageTypeManifest),
		Data: remoteManifestPayload,
	}

	pm.HandleCustomMessage(context.Background(), msg)
	pm.HandleCustomMessage(context.Background(), msg)

	peers := dir.ListLCPPeers()
	if got, want := len(peers), 1; got != want {
		t.Fatalf("ListLCPPeers count mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := peers[0].PeerID, peerPubKey; got != want {
		t.Fatalf("peer_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := peers[0].RemoteManifest.ProtocolVersion, lcpwire.ProtocolVersionV02; got != want {
		t.Fatalf("remote_manifest.protocol_version mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got, want := len(fake.sendReqs), 1; got != want {
		t.Fatalf("SendCustomMessage calls mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

func TestPeerMessaging_HandleCustomMessage_LCPManifestRepliesOnceEvenIfAlreadySentOnOnline(t *testing.T) {
	t.Parallel()

	peerBytes, peerPubKey := newPeerPubKey()

	fake := &fakeLightningClient{
		listPeersResp: &lnrpc.ListPeersResponse{
			Peers: []*lnrpc.Peer{
				{PubKey: peerPubKey, Address: "127.0.0.1:9735"},
			},
		},
	}
	dir := peerdirectory.New()

	pm, err := NewStandalone(dir, fake, newDiscardLogger(), nil)
	if err != nil {
		t.Fatalf("NewStandalone: %v", err)
	}

	remoteManifestPayload, err := lcpwire.EncodeManifest(
		lcpwire.Manifest{
			ProtocolVersion: lcpwire.ProtocolVersionV02,
			MaxPayloadBytes: 123,
			MaxStreamBytes:  1024,
			MaxCallBytes:    2048,
		},
	)
	if err != nil {
		t.Fatalf("encode remote manifest: %v", err)
	}

	pm.HandlePeerEvent(context.Background(), &lnrpc.PeerEvent{
		PubKey: peerPubKey,
		Type:   lnrpc.PeerEvent_PEER_ONLINE,
	})

	msg := &lnrpc.CustomMessage{
		Peer: peerBytes,
		Type: uint32(lcpwire.MessageTypeManifest),
		Data: remoteManifestPayload,
	}
	pm.HandleCustomMessage(context.Background(), msg)
	pm.HandleCustomMessage(context.Background(), msg)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got, want := len(fake.sendReqs), 2; got != want {
		t.Fatalf("SendCustomMessage calls mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}

func TestPeerMessaging_HandlePeerEvent_OfflineResetsState(t *testing.T) {
	t.Parallel()

	fake := &fakeLightningClient{}
	dir := peerdirectory.New()

	pm, err := NewStandalone(dir, fake, newDiscardLogger(), nil)
	if err != nil {
		t.Fatalf("NewStandalone: %v", err)
	}

	peerBytes, peerPubKey := newPeerPubKey()
	remoteManifestPayload, err := lcpwire.EncodeManifest(
		lcpwire.Manifest{
			ProtocolVersion: lcpwire.ProtocolVersionV02,
			MaxPayloadBytes: 123,
			MaxStreamBytes:  1024,
			MaxCallBytes:    2048,
		},
	)
	if err != nil {
		t.Fatalf("encode remote manifest: %v", err)
	}

	msg := &lnrpc.CustomMessage{
		Peer: peerBytes,
		Type: uint32(lcpwire.MessageTypeManifest),
		Data: remoteManifestPayload,
	}

	pm.HandleCustomMessage(context.Background(), msg)
	pm.HandlePeerEvent(context.Background(), &lnrpc.PeerEvent{
		PubKey: peerPubKey,
		Type:   lnrpc.PeerEvent_PEER_OFFLINE,
	})

	if got, want := len(dir.ListLCPPeers()), 0; got != want {
		t.Fatalf("ListLCPPeers mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	pm.HandleCustomMessage(context.Background(), msg)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got, want := len(fake.sendReqs), 2; got != want {
		t.Fatalf(
			"SendCustomMessage calls mismatch after offline reset (-want +got):\n%s",
			cmp.Diff(want, got),
		)
	}
}

func TestPeerMessaging_HandlePeerEvent_OnlineSendsManifestOnce(t *testing.T) {
	t.Parallel()

	peerPubKey := "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	fake := &fakeLightningClient{
		listPeersResp: &lnrpc.ListPeersResponse{
			Peers: []*lnrpc.Peer{
				{PubKey: peerPubKey, Address: "127.0.0.1:9735"},
			},
		},
	}
	dir := peerdirectory.New()

	pm, err := NewStandalone(dir, fake, newDiscardLogger(), nil)
	if err != nil {
		t.Fatalf("NewStandalone: %v", err)
	}

	pm.HandlePeerEvent(context.Background(), &lnrpc.PeerEvent{
		PubKey: peerPubKey,
		Type:   lnrpc.PeerEvent_PEER_ONLINE,
	})

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got, want := len(fake.sendReqs), 1; got != want {
		t.Fatalf("SendCustomMessage calls mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
}
