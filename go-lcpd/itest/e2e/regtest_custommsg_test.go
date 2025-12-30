package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/lnrpc"
	"github.com/bruwbird/lcp/go-lcpd/itest/harness/lcpd"
	"github.com/bruwbird/lcp/go-lcpd/itest/harness/regtest"
	"github.com/google/go-cmp/cmp"
)

func TestE2E_Regtest_LNDCustomMessages_LCPManifest(t *testing.T) {
	env := regtest.Require(t)

	ctx, cancel := context.WithTimeout(context.Background(), regtest.Timeout)
	t.Cleanup(cancel)

	dataDir := regtest.MkdirTemp(t, "lcp-itest-regtest-custommsg-*")
	t.Cleanup(func() {
		if regtest.KeepData(env, t) {
			t.Logf("keeping regtest data dir: %s", dataDir)
			return
		}
		_ = regtest.RemoveAll(dataDir)
	})

	ports := regtest.ReservePorts(t, regtest.PortCount)
	bitcoind := regtest.StartBitcoind(ctx, t, dataDir, regtest.BitcoindPorts{
		RPCPort: ports[0],
	})

	alice := regtest.StartLndNode(ctx, t, dataDir, regtest.LndNodeConfig{
		Name:    "alice",
		RPCPort: ports[1],
		P2PPort: ports[2],
	}, bitcoind)
	bob := regtest.StartLndNode(ctx, t, dataDir, regtest.LndNodeConfig{
		Name:    "bob",
		RPCPort: ports[3],
		P2PPort: ports[4],
	}, bitcoind)

	regtest.InitWallet(ctx, t, alice)
	regtest.InitWallet(ctx, t, bob)

	aliceConn := regtest.DialLND(ctx, t, alice.RPCAddr, alice.TLSCertPath)
	t.Cleanup(func() { _ = aliceConn.Close() })
	aliceRPC := lnrpc.NewLightningClient(aliceConn)

	bobConn := regtest.DialLND(ctx, t, bob.RPCAddr, bob.TLSCertPath)
	t.Cleanup(func() { _ = bobConn.Close() })
	bobRPC := lnrpc.NewLightningClient(bobConn)

	regtest.WaitForLNDReady(ctx, t, aliceRPC)
	regtest.WaitForLNDReady(ctx, t, bobRPC)

	aliceInfo, err := aliceRPC.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		t.Fatalf("alice GetInfo: %v", err)
	}
	alicePubKey := aliceInfo.GetIdentityPubkey()
	if alicePubKey == "" {
		t.Fatalf("alice GetInfo: identity_pubkey is empty")
	}

	bobInfo, err := bobRPC.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		t.Fatalf("bob GetInfo: %v", err)
	}
	bobPubKey := bobInfo.GetIdentityPubkey()
	if bobPubKey == "" {
		t.Fatalf("bob GetInfo: identity_pubkey is empty")
	}

	// Establish the underlying LN peer connection first (no channel needed).
	regtest.ConnectPeer(ctx, t, aliceRPC, bobPubKey, bob.P2PAddr)

	lcpdPorts := regtest.ReservePorts(t, 2)
	aliceLcpdPort := lcpdPorts[0]
	bobLcpdPort := lcpdPorts[1]

	aliceLcpd := lcpd.Start(ctx, t, lcpd.RunConfig{
		GRPCAddr: netAddr(aliceLcpdPort),
		LND: &lcpd.LNDConfig{
			RPCAddr:     alice.RPCAddr,
			TLSCertPath: alice.TLSCertPath,
		},
	})
	t.Cleanup(func() { _ = aliceLcpd.Process.Stop(3 * time.Second) })

	bobLcpd := lcpd.Start(ctx, t, lcpd.RunConfig{
		GRPCAddr: netAddr(bobLcpdPort),
		LND: &lcpd.LNDConfig{
			RPCAddr:     bob.RPCAddr,
			TLSCertPath: bob.TLSCertPath,
		},
	})
	t.Cleanup(func() { _ = bobLcpd.Process.Stop(3 * time.Second) })

	aliceClient := newGRPCClient(t, aliceLcpd.Addr)
	bobClient := newGRPCClient(t, bobLcpd.Addr)

	peersAlice := regtest.WaitForLCPPeers(ctx, t, aliceClient)
	peersBob := regtest.WaitForLCPPeers(ctx, t, bobClient)

	if got, want := peersAlice[0].GetPeerId(), bobPubKey; got != want {
		t.Fatalf("alice peer_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if peersAlice[0].GetRemoteManifest() == nil {
		t.Fatalf("alice remote_manifest is nil")
	}
	if got, want := peersAlice[0].GetRemoteManifest().GetProtocolVersion(), uint32(lcpwire.ProtocolVersionV01); got != want {
		t.Fatalf(
			"alice remote_manifest.protocol_version mismatch (-want +got):\n%s",
			cmp.Diff(want, got),
		)
	}
	if got, want := peersAlice[0].GetRemoteManifest().GetMaxPayloadBytes(), uint32(16384); got != want {
		t.Fatalf(
			"alice remote_manifest.max_payload_bytes mismatch (-want +got):\n%s",
			cmp.Diff(want, got),
		)
	}

	if got, want := peersBob[0].GetPeerId(), alicePubKey; got != want {
		t.Fatalf("bob peer_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if peersBob[0].GetRemoteManifest() == nil {
		t.Fatalf("bob remote_manifest is nil")
	}
	if got, want := peersBob[0].GetRemoteManifest().GetProtocolVersion(), uint32(lcpwire.ProtocolVersionV01); got != want {
		t.Fatalf(
			"bob remote_manifest.protocol_version mismatch (-want +got):\n%s",
			cmp.Diff(want, got),
		)
	}
	if got, want := peersBob[0].GetRemoteManifest().GetMaxPayloadBytes(), uint32(16384); got != want {
		t.Fatalf(
			"bob remote_manifest.max_payload_bytes mismatch (-want +got):\n%s",
			cmp.Diff(want, got),
		)
	}
}
