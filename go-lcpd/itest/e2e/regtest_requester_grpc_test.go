package e2e_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcptasks"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/lnrpc"
	"github.com/bruwbird/lcp/go-lcpd/internal/protocolcompat"
	"github.com/bruwbird/lcp/go-lcpd/itest/harness/lcpd"
	"github.com/bruwbird/lcp/go-lcpd/itest/harness/regtest"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

//nolint:gocognit // setup is intentionally linear for readability.
func TestE2E_Regtest_RequesterGRPC(t *testing.T) {
	env := regtest.Require(t)

	ctx, cancel := context.WithTimeout(context.Background(), regtest.Timeout)
	t.Cleanup(cancel)

	dataDir := regtest.MkdirTemp(t, "lcp-itest-regtest-grpc-*")
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

	regtest.FundWallet(ctx, t, bitcoind, aliceRPC)

	regtest.OpenChannel(ctx, t, bitcoind, aliceRPC, bobRPC, bob.P2PAddr)

	aliceProviderConfigPath := filepath.Join(dataDir, "provider-alice.yaml")
	writeProviderConfig(t, aliceProviderConfigPath, false, nil)
	bobProviderConfigPath := filepath.Join(dataDir, "provider-bob.yaml")
	writeProviderConfig(t, bobProviderConfigPath, true, []string{"gpt-5.2"})

	// start lcpd (deterministic backend) for alice/bob
	aliceAddr := regtest.ReservePorts(t, 1)[0]
	bobAddr := regtest.ReservePorts(t, 1)[0]

	aliceLcpd := lcpd.Start(ctx, t, lcpd.RunConfig{
		GRPCAddr: netAddr(aliceAddr),
		LND: &lcpd.LNDConfig{
			RPCAddr:     alice.RPCAddr,
			TLSCertPath: alice.TLSCertPath,
		},
		ExtraEnv: []string{
			"LCPD_PROVIDER_CONFIG_PATH=" + aliceProviderConfigPath,
		},
	})
	t.Cleanup(func() { _ = aliceLcpd.Process.Stop(3 * time.Second) })

	bobLcpd := lcpd.Start(ctx, t, lcpd.RunConfig{
		GRPCAddr: netAddr(bobAddr),
		LND: &lcpd.LNDConfig{
			RPCAddr:     bob.RPCAddr,
			TLSCertPath: bob.TLSCertPath,
		},
		ExtraEnv: []string{
			"LCPD_PROVIDER_CONFIG_PATH=" + bobProviderConfigPath,
		},
	})
	t.Cleanup(func() { _ = bobLcpd.Process.Stop(3 * time.Second) })

	aliceClient := newGRPCClient(t, netAddr(aliceAddr))
	bobClient := newGRPCClient(t, netAddr(bobAddr))

	regtest.ConnectPeer(ctx, t, aliceRPC, bobPubKey, bob.P2PAddr)

	peersAlice := regtest.WaitForLCPPeers(ctx, t, aliceClient)
	peersBob := regtest.WaitForLCPPeers(ctx, t, bobClient)

	if got, want := peersAlice[0].GetPeerId(), bobPubKey; got != want {
		t.Fatalf("alice peer_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if peersAlice[0].GetRemoteManifest() == nil {
		t.Fatalf("alice remote_manifest is nil")
	}
	if got, want := peersBob[0].GetPeerId(), alicePubKey; got != want {
		t.Fatalf("bob peer_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if peersBob[0].GetRemoteManifest() == nil {
		t.Fatalf("bob remote_manifest is nil")
	}

	quoteReq := &lcpdv1.RequestQuoteRequest{
		PeerId: bobPubKey,
		Task: &lcpdv1.Task{
			Spec: &lcpdv1.Task_OpenaiChatCompletionsV1{
				OpenaiChatCompletionsV1: &lcpdv1.OpenAIChatCompletionsV1TaskSpec{
					RequestJson: []byte(
						`{"model":"gpt-5.2","messages":[{"role":"user","content":"hello from grpc requester"}]}`,
					),
					Params: &lcpdv1.OpenAIChatCompletionsV1Params{
						Model: "gpt-5.2",
					},
				},
			},
		},
	}
	quoteResp, err := aliceClient.RequestQuote(ctx, quoteReq)
	if err != nil {
		t.Fatalf("RequestQuote: %v", err)
	}
	if got, want := quoteResp.GetPeerId(), bobPubKey; got != want {
		t.Fatalf("quote peer_id mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	terms := quoteResp.GetTerms()
	if terms == nil {
		t.Fatalf("quote terms is nil")
	}
	if got, want := len(terms.GetJobId()), lcp.Hash32Len; got != want {
		t.Fatalf("quote terms job_id length mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if got, want := len(terms.GetTermsHash()), lcp.Hash32Len; got != want {
		t.Fatalf("quote terms terms_hash length mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}
	if terms.GetPaymentRequest() == "" {
		t.Fatalf("quote terms payment_request is empty")
	}
	if terms.GetQuoteExpiry() == nil {
		t.Fatalf("quote terms quote_expiry is nil")
	}
	if got, want := terms.GetQuoteExpiry().GetNanos(), int32(0); got != want {
		t.Fatalf("quote terms quote_expiry nanos mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	var jobID lcp.JobID
	copy(jobID[:], terms.GetJobId())

	wireTask, err := lcptasks.ToWireQuoteRequestTask(quoteReq.GetTask())
	if err != nil {
		t.Fatalf("ToWireQuoteRequestTask: %v", err)
	}
	inputStream, err := lcptasks.ToWireInputStream(quoteReq.GetTask())
	if err != nil {
		t.Fatalf("ToWireInputStream: %v", err)
	}

	wantTermsHash, err := protocolcompat.ComputeTermsHash(lcp.Terms{
		ProtocolVersion: uint16(terms.GetProtocolVersion()),
		JobID:           jobID,
		PriceMsat:       terms.GetPriceMsat(),
		QuoteExpiry:     uint64(terms.GetQuoteExpiry().GetSeconds()),
	}, protocolcompat.TermsCommit{
		TaskKind:             wireTask.TaskKind,
		Input:                inputStream.DecodedBytes,
		InputContentType:     inputStream.ContentType,
		InputContentEncoding: inputStream.ContentEncoding,
		Params:               wireTask.ParamsBytes,
	})
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}
	if diff := cmp.Diff(wantTermsHash[:], terms.GetTermsHash()); diff != "" {
		t.Fatalf("terms_hash mismatch (-want +got):\n%s", diff)
	}

	// invoice binding check
	_ = regtest.AssertPaymentRequestBinding(
		ctx,
		t,
		aliceRPC,
		terms.GetPaymentRequest(),
		bobPubKey,
		terms.GetTermsHash(),
		terms.GetPriceMsat(),
		uint64(terms.GetQuoteExpiry().GetSeconds()),
	)

	amtSats := int64((terms.GetPriceMsat() + 999) / 1000)
	if amtSats <= 0 {
		amtSats = 1
	}
	regtest.WaitForLocalBalance(ctx, t, aliceRPC, bobPubKey, amtSats)
	regtest.WaitForRoute(ctx, t, aliceRPC, bobPubKey, amtSats)

	execResp, err := aliceClient.AcceptAndExecute(ctx, &lcpdv1.AcceptAndExecuteRequest{
		PeerId:     bobPubKey,
		JobId:      terms.GetJobId(),
		PayInvoice: true,
	})
	if err != nil {
		t.Fatalf("AcceptAndExecute: %v", err)
	}
	if execResp.GetResult() == nil {
		t.Fatalf("AcceptAndExecute: result is nil")
	}
	if diff := cmp.Diff("application/json; charset=utf-8", execResp.GetResult().GetContentType()); diff != "" {
		t.Fatalf("result content_type mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]byte("deterministic-output"), execResp.GetResult().GetResult()); diff != "" {
		t.Fatalf("result bytes mismatch (-want +got):\n%s", diff)
	}
}

func netAddr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

func newGRPCClient(t *testing.T, addr string) lcpdv1.LCPDServiceClient {
	t.Helper()

	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc new client (%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return lcpdv1.NewLCPDServiceClient(conn)
}

func writeProviderConfig(
	t *testing.T,
	path string,
	enabled bool,
	supportedModels []string,
) {
	t.Helper()

	var builder strings.Builder
	builder.Grow(64)

	if enabled {
		builder.WriteString("enabled: true\n")
	} else {
		builder.WriteString("enabled: false\n")
	}
	if len(supportedModels) > 0 {
		builder.WriteString("llm:\n")
		builder.WriteString("  models:\n")
		for _, model := range supportedModels {
			builder.WriteString("    ")
			builder.WriteString(model)
			builder.WriteString(": {}\n")
		}
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatalf("write provider config %s: %v", path, err)
	}
}
