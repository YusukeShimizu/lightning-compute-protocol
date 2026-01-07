package lndgrpc

import (
	"encoding/hex"
	"strings"

	"github.com/bruwbird/lcp/go-lcpd/internal/lightningnode"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/lnrpc"
)

type peerEventStream struct {
	stream lnrpc.Lightning_SubscribePeerEventsClient
}

func (s peerEventStream) Recv() (*lightningnode.PeerEvent, error) {
	ev, err := s.stream.Recv()
	if err != nil {
		return nil, err
	}
	if ev == nil {
		return &lightningnode.PeerEvent{}, nil
	}

	out := &lightningnode.PeerEvent{
		PubKey: strings.TrimSpace(ev.GetPubKey()),
		Type:   lightningnode.PeerEventTypeUnspecified,
	}
	switch ev.GetType() {
	case lnrpc.PeerEvent_PEER_ONLINE:
		out.Type = lightningnode.PeerEventTypeOnline
	case lnrpc.PeerEvent_PEER_OFFLINE:
		out.Type = lightningnode.PeerEventTypeOffline
	}
	return out, nil
}

type customMessageStream struct {
	stream lnrpc.Lightning_SubscribeCustomMessagesClient
}

func (s customMessageStream) Recv() (*lightningnode.CustomMessage, error) {
	msg, err := s.stream.Recv()
	if err != nil {
		return nil, err
	}
	if msg == nil {
		return &lightningnode.CustomMessage{}, nil
	}

	peerBytes := msg.GetPeer()
	var peerPubKey string
	if len(peerBytes) > 0 {
		peerPubKey = hex.EncodeToString(peerBytes)
	}

	return &lightningnode.CustomMessage{
		PeerPubKey: peerPubKey,
		MsgType:    msg.GetType(),
		Payload:    append([]byte(nil), msg.GetData()...),
	}, nil
}
