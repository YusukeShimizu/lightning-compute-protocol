package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/lnrpc"
	"github.com/bruwbird/lcp/go-lcpd/itest/harness/lcpd"
	"github.com/bruwbird/lcp/go-lcpd/itest/harness/openaiserve"
	"github.com/bruwbird/lcp/go-lcpd/itest/harness/regtest"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"github.com/google/go-cmp/cmp"
)

//nolint:gocognit // setup is intentionally linear for readability.
func TestE2E_Regtest_OpenAIServe_StreamAndResponses(t *testing.T) {
	env := regtest.Require(t)

	ctx, cancel := context.WithTimeout(context.Background(), regtest.Timeout)
	t.Cleanup(cancel)

	dataDir := regtest.MkdirTemp(t, "lcp-itest-regtest-openai-serve-*")
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

	streamBytes := bytes.Repeat([]byte("data: hello\n\n"), 2048)
	streamBytesB64 := base64.StdEncoding.EncodeToString(streamBytes)

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
		DeterministicB64: streamBytesB64,
		ExtraEnv: []string{
			"LCPD_PROVIDER_CONFIG_PATH=" + bobProviderConfigPath,
		},
	})
	t.Cleanup(func() { _ = bobLcpd.Process.Stop(3 * time.Second) })

	aliceClient := newGRPCClient(t, netAddr(aliceAddr))
	bobClient := newGRPCClient(t, netAddr(bobAddr))

	regtest.ConnectPeer(ctx, t, aliceRPC, bobPubKey, bob.P2PAddr)
	_ = regtest.WaitForLCPPeers(ctx, t, aliceClient)
	_ = regtest.WaitForLCPPeers(ctx, t, bobClient)

	maxPriceMsat := uint64(0)
	chatParams, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(lcpwire.OpenAIChatCompletionsV1Params{Model: "gpt-5.2"})
	if err != nil {
		t.Fatalf("EncodeOpenAIChatCompletionsV1Params: %v", err)
	}
	respParams, err := lcpwire.EncodeOpenAIResponsesV1Params(lcpwire.OpenAIResponsesV1Params{Model: "gpt-5.2"})
	if err != nil {
		t.Fatalf("EncodeOpenAIResponsesV1Params: %v", err)
	}

	for _, call := range []*lcpdv1.CallSpec{
		{
			Method:                 "openai.chat_completions.v1",
			Params:                 chatParams,
			RequestBytes:           []byte(`{"model":"gpt-5.2","messages":[{"role":"user","content":"preflight"}]}`),
			RequestContentType:     "application/json; charset=utf-8",
			RequestContentEncoding: "identity",
		},
		{
			Method:                 "openai.responses.v1",
			Params:                 respParams,
			RequestBytes:           []byte(`{"model":"gpt-5.2","input":"preflight"}`),
			RequestContentType:     "application/json; charset=utf-8",
			RequestContentEncoding: "identity",
		},
	} {
		quoteResp, quoteErr := aliceClient.RequestQuote(ctx, &lcpdv1.RequestQuoteRequest{
			PeerId: bobPubKey,
			Call:   call,
		})
		if quoteErr != nil {
			t.Fatalf("RequestQuote (preflight): %v", quoteErr)
		}
		if quoteResp.GetQuote() == nil {
			t.Fatalf("RequestQuote (preflight): quote is nil")
		}
		price := quoteResp.GetQuote().GetPriceMsat()
		if price > maxPriceMsat {
			maxPriceMsat = price
		}
	}

	amtSats := int64((maxPriceMsat + 999) / 1000)
	if amtSats <= 0 {
		amtSats = 1
	}
	regtest.WaitForLocalBalance(ctx, t, aliceRPC, bobPubKey, amtSats)
	regtest.WaitForRoute(ctx, t, aliceRPC, bobPubKey, amtSats)

	httpAddr := netAddr(regtest.ReservePorts(t, 1)[0])
	openaiServe := openaiserve.Start(ctx, t, openaiserve.RunConfig{
		HTTPAddr:     httpAddr,
		LCPDGRPCAddr: netAddr(aliceAddr),
	})
	t.Cleanup(func() { _ = openaiServe.Process.Stop(3 * time.Second) })

	httpClient := &http.Client{Timeout: 60 * time.Second}

	t.Run("chat completions stream", func(t *testing.T) {
		resp, body := mustPostJSON(
			t,
			httpClient,
			fmt.Sprintf("http://%s/v1/chat/completions", httpAddr),
			[]byte(`{"model":"gpt-5.2","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
		)

		if diff := cmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
			t.Fatalf("status mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff("text/event-stream; charset=utf-8", resp.Header.Get("Content-Type")); diff != "" {
			t.Fatalf("content-type mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff("no-cache", resp.Header.Get("Cache-Control")); diff != "" {
			t.Fatalf("cache-control mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff("no", resp.Header.Get("X-Accel-Buffering")); diff != "" {
			t.Fatalf("x-accel-buffering mismatch (-want +got):\n%s", diff)
		}

		if diff := cmp.Diff(bobPubKey, resp.Header.Get("X-Lcp-Peer-Id")); diff != "" {
			t.Fatalf("peer-id header mismatch (-want +got):\n%s", diff)
		}

		if diff := cmp.Diff(lcp.Hash32Len*2, len(resp.Header.Get("X-Lcp-Call-Id"))); diff != "" {
			t.Fatalf("call-id header length mismatch (-want +got):\n%s", diff)
		}
		if _, decodeErr := hex.DecodeString(resp.Header.Get("X-Lcp-Call-Id")); decodeErr != nil {
			t.Fatalf("call-id header is not hex: %v", decodeErr)
		}
		if diff := cmp.Diff(lcp.Hash32Len*2, len(resp.Header.Get("X-Lcp-Terms-Hash"))); diff != "" {
			t.Fatalf("terms-hash header length mismatch (-want +got):\n%s", diff)
		}
		if _, decodeErr := hex.DecodeString(resp.Header.Get("X-Lcp-Terms-Hash")); decodeErr != nil {
			t.Fatalf("terms-hash header is not hex: %v", decodeErr)
		}
		if strings.TrimSpace(resp.Header.Get("X-Lcp-Price-Msat")) == "" {
			t.Fatalf("price-msat header is empty")
		}

		assertBodySHA256(t, streamBytes, body)
	})

	t.Run("responses non-streaming", func(t *testing.T) {
		resp, body := mustPostJSON(
			t,
			httpClient,
			fmt.Sprintf("http://%s/v1/responses", httpAddr),
			[]byte(`{"model":"gpt-5.2","input":"hello"}`),
		)

		if diff := cmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
			t.Fatalf("status mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff("application/json; charset=utf-8", resp.Header.Get("Content-Type")); diff != "" {
			t.Fatalf("content-type mismatch (-want +got):\n%s", diff)
		}
		if got := strings.TrimSpace(resp.Header.Get("X-Accel-Buffering")); got != "" {
			t.Fatalf("unexpected x-accel-buffering header: %q", got)
		}

		if diff := cmp.Diff(bobPubKey, resp.Header.Get("X-Lcp-Peer-Id")); diff != "" {
			t.Fatalf("peer-id header mismatch (-want +got):\n%s", diff)
		}

		assertBodySHA256(t, streamBytes, body)
	})

	t.Run("responses stream", func(t *testing.T) {
		resp, body := mustPostJSON(
			t,
			httpClient,
			fmt.Sprintf("http://%s/v1/responses", httpAddr),
			[]byte(`{"model":"gpt-5.2","stream":true,"input":"hello"}`),
		)

		if diff := cmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
			t.Fatalf("status mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff("text/event-stream; charset=utf-8", resp.Header.Get("Content-Type")); diff != "" {
			t.Fatalf("content-type mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff("no-cache", resp.Header.Get("Cache-Control")); diff != "" {
			t.Fatalf("cache-control mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff("no", resp.Header.Get("X-Accel-Buffering")); diff != "" {
			t.Fatalf("x-accel-buffering mismatch (-want +got):\n%s", diff)
		}

		if diff := cmp.Diff(bobPubKey, resp.Header.Get("X-Lcp-Peer-Id")); diff != "" {
			t.Fatalf("peer-id header mismatch (-want +got):\n%s", diff)
		}

		assertBodySHA256(t, streamBytes, body)
	})
}

func mustPostJSON(t *testing.T, client *http.Client, url string, body []byte) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("http request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, respBody
}

func assertBodySHA256(t *testing.T, want, got []byte) {
	t.Helper()

	if diff := cmp.Diff(len(want), len(got)); diff != "" {
		t.Fatalf("body length mismatch (-want +got):\n%s", diff)
	}

	wantSum := sha256.Sum256(want)
	gotSum := sha256.Sum256(got)
	if diff := cmp.Diff(wantSum, gotSum); diff != "" {
		t.Fatalf("body sha256 mismatch (-want +got):\n%s", diff)
	}
}
