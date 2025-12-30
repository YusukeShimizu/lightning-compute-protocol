package regtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	lcpdv1 "github.com/bruwbird/lcp/go-lcpd/gen/go/lcpd/v1"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/lnrpc"
	"github.com/bruwbird/lcp/go-lcpd/internal/lnd/routerrpc"
	"github.com/google/go-cmp/cmp"
	"github.com/sethvargo/go-envconfig"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// Timeouts and polling intervals for regtest harness.
const (
	Timeout                = 3 * time.Minute
	WalletPassword         = "test-password"
	PortCount              = 5
	ProcessStopTimeout     = 5 * time.Second
	GRPCDialTimeout        = 15 * time.Second
	BitcoinRPCTimeout      = 15 * time.Second
	FilePollInterval       = 100 * time.Millisecond
	BitcoinPollInterval    = 200 * time.Millisecond
	LCPPeerPollInterval    = 50 * time.Millisecond
	LNDReadyPollInterval   = 200 * time.Millisecond
	WalletBalancePoll      = 250 * time.Millisecond
	ChannelPollInterval    = 300 * time.Millisecond
	InvoicePollInterval    = 200 * time.Millisecond
	BlocksToMatureCoinbase = 101
	ChannelConfirmBlocks   = 3
	MinWalletBalanceSats   = 1
	ChannelFundingSats     = 200_000
	InvoiceValueSats       = 1_000
	defaultJobID           = "lcp-itest"
	defaultPaymentTimeout  = 10 * time.Second
	defaultRouteTimeout    = 5 * time.Second
	defaultFeeLimitSat     = int64(10_000)
)

// Env captures regtest flags.
type Env struct {
	Regtest  string `env:"LCP_ITEST_REGTEST"`
	KeepData string `env:"LCP_ITEST_KEEP_DATA"`
}

// KeepData returns true when env requests to retain regtest data or the test failed.
func KeepData(env Env, t *testing.T) bool {
	t.Helper()
	return isTruthy(env.KeepData) || t.Failed()
}

// RemoveAll removes a directory tree (fatal on error).
func RemoveAll(path string) error {
	return os.RemoveAll(path)
}

// Require skips the test when regtest is not opted-in or dependencies are missing.
func Require(t *testing.T) Env {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping regtest integration test in -short mode")
	}

	env := MustLoadEnv(t)
	if !isTruthy(env.Regtest) {
		t.Skip("set LCP_ITEST_REGTEST=1 to run regtest integration tests")
	}
	if _, err := exec.LookPath("bitcoind"); err != nil {
		t.Skipf("bitcoind not found in PATH: %v", err)
	}
	if _, err := exec.LookPath("lnd"); err != nil {
		t.Skipf("lnd not found in PATH: %v", err)
	}
	return env
}

// MustLoadEnv loads regtest env vars or fails the test.
func MustLoadEnv(t *testing.T) Env {
	t.Helper()

	var env Env
	if err := envconfig.Process(context.Background(), &env); err != nil {
		t.Fatalf("process env: %v", err)
	}
	return env
}

func isTruthy(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// MkdirTemp is like os.MkdirTemp but fatal on error.
func MkdirTemp(t *testing.T, pattern string) string {
	t.Helper()

	prefix := strings.TrimSpace(pattern)
	prefix = strings.TrimSuffix(prefix, "*")
	prefix = strings.Trim(prefix, "-_")
	if prefix == "" {
		prefix = "lcp-itest"
	}

	repoRoot := findRepoRoot(t)
	baseDir := filepath.Join(repoRoot, ".data", "itest")
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		t.Fatalf("mkdir itest data dir %s: %v", baseDir, err)
	}

	// Use t.TempDir for uniqueness, but keep artifacts under repoRoot/.data/itest
	// so logs are easy to locate and optionally persist.
	tmp := t.TempDir()
	tmpParent := filepath.Base(filepath.Dir(tmp))
	tmpBase := filepath.Base(tmp)
	if tmpParent == "" || tmpParent == "." || tmpParent == string(filepath.Separator) {
		tmpParent = "temp"
	}
	if tmpBase == "" || tmpBase == "." || tmpBase == string(filepath.Separator) {
		tmpBase = "dir"
	}

	dir := filepath.Join(baseDir, prefix+"-"+tmpParent+"-"+tmpBase)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir tempdir %s: %v", dir, err)
	}
	return dir
}

func findRepoRoot(t *testing.T) string {
	t.Helper()

	const modTarget = "module github.com/bruwbird/lcp/go-lcpd"

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	start := dir

	for {
		modPath := filepath.Join(dir, "go.mod")
		data, readErr := os.ReadFile(modPath)
		if readErr == nil && bytes.Contains(data, []byte(modTarget)) {
			return dir
		}

		next := filepath.Dir(dir)
		if next == dir {
			t.Fatalf("go.mod (%s) not found upwards from %s", modTarget, start)
		}
		dir = next
	}
}

// ReservePorts reserves n unique TCP ports on localhost and returns them.
func ReservePorts(t *testing.T, n int) []int {
	t.Helper()

	ports := make([]int, 0, n)
	seen := make(map[int]struct{}, n)
	for len(ports) < n {
		listenCfg := &net.ListenConfig{}
		ln, err := listenCfg.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen :0: %v", err)
		}

		addr, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			_ = ln.Close()
			t.Fatalf("unexpected addr type: %T", ln.Addr())
		}

		port := addr.Port
		_ = ln.Close()

		if _, exists := seen[port]; exists {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	return ports
}

type BitcoindPorts struct {
	RPCPort int
}

// BitcoindHandle tracks a running bitcoind instance.
type BitcoindHandle struct {
	Cookie  string
	RPCAddr string
	RPC     *bitcoinRPC
}

// StartBitcoind launches bitcoind (regtest) and waits for RPC readiness.
func StartBitcoind(
	ctx context.Context,
	t *testing.T,
	dataDir string,
	ports BitcoindPorts,
) *BitcoindHandle {
	t.Helper()

	bitcoindDir := filepath.Join(dataDir, "bitcoind")
	if err := os.MkdirAll(bitcoindDir, 0o700); err != nil {
		t.Fatalf("mkdir bitcoind dir: %v", err)
	}

	logPath := filepath.Join(dataDir, "bitcoind.log")
	logFile := mustCreate(t, logPath)
	t.Cleanup(func() { _ = logFile.Close() })

	rpcAddr := fmt.Sprintf("127.0.0.1:%d", ports.RPCPort)

	args := []string{
		"-regtest",
		"-server=1",
		"-listen=0",
		"-txindex=1",
		"-fallbackfee=0.0002",
		fmt.Sprintf("-datadir=%s", bitcoindDir),
		"-rpcbind=127.0.0.1",
		"-rpcallowip=127.0.0.1",
		fmt.Sprintf("-rpcport=%d", ports.RPCPort),
		"-printtoconsole",
	}

	cmd := exec.CommandContext(ctx, "bitcoind", args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bitcoind: %v", err)
	}
	t.Cleanup(func() { _ = stopProcess(cmd, ProcessStopTimeout) })

	cookiePath := filepath.Join(bitcoindDir, "regtest", ".cookie")
	waitForFile(ctx, t, cookiePath)

	rpc := newBitcoinRPC(t, rpcAddr, cookiePath)
	waitForBitcoinRPC(ctx, t, rpc)
	ensureBitcoindWallet(ctx, t, rpc)
	primeBitcoindForLND(ctx, t, rpc)

	return &BitcoindHandle{
		Cookie:  cookiePath,
		RPCAddr: rpcAddr,
		RPC:     rpc,
	}
}

type LndNodeConfig struct {
	Name    string
	RPCPort int
	P2PPort int
}

// LndNodeHandle tracks a running lnd instance.
type LndNodeHandle struct {
	Name        string
	RPCAddr     string
	P2PAddr     string
	TLSCertPath string
}

// StartLndNode launches lnd against the provided bitcoind handle.
func StartLndNode(
	ctx context.Context,
	t *testing.T,
	dataDir string,
	cfg LndNodeConfig,
	bitcoind *BitcoindHandle,
) *LndNodeHandle {
	t.Helper()

	lnddir := filepath.Join(dataDir, "lnd", cfg.Name)
	if err := os.MkdirAll(lnddir, 0o700); err != nil {
		t.Fatalf("mkdir lnddir: %v", err)
	}

	logPath := filepath.Join(dataDir, fmt.Sprintf("lnd-%s.log", cfg.Name))
	logFile := mustCreate(t, logPath)
	t.Cleanup(func() { _ = logFile.Close() })

	rpcAddr := fmt.Sprintf("127.0.0.1:%d", cfg.RPCPort)
	p2pAddr := fmt.Sprintf("127.0.0.1:%d", cfg.P2PPort)
	tlsCertPath := filepath.Join(lnddir, "tls.cert")

	args := []string{
		fmt.Sprintf("--lnddir=%s", lnddir),
		fmt.Sprintf("--logdir=%s", filepath.Join(lnddir, "logs")),
		fmt.Sprintf("--listen=%s", p2pAddr),
		fmt.Sprintf("--rpclisten=%s", rpcAddr),
		"--norest",
		"--nobootstrap",
		"--no-macaroons",
		"--tlsdisableautofill",
		"--tlsextradomain=localhost",
		"--tlsextraip=127.0.0.1",
		"--bitcoin.active",
		"--bitcoin.regtest",
		"--bitcoin.node=bitcoind",
		"--bitcoin.defaultchanconfs=1",
		fmt.Sprintf("--bitcoind.rpchost=%s", bitcoind.RPCAddr),
		fmt.Sprintf("--bitcoind.rpccookie=%s", bitcoind.Cookie),
		"--bitcoind.rpcpolling",
		"--bitcoind.blockpollinginterval=200ms",
		"--bitcoind.txpollinginterval=200ms",
		"--debuglevel=info",
	}

	cmd := exec.CommandContext(ctx, "lnd", args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lnd (%s): %v", cfg.Name, err)
	}
	t.Cleanup(func() { _ = stopProcess(cmd, ProcessStopTimeout) })

	waitForFile(ctx, t, tlsCertPath)

	return &LndNodeHandle{
		Name:        cfg.Name,
		RPCAddr:     rpcAddr,
		P2PAddr:     p2pAddr,
		TLSCertPath: tlsCertPath,
	}
}

// ConnectPeer connects alice to bob (idempotent).
func ConnectPeer(
	ctx context.Context,
	t *testing.T,
	aliceRPC lnrpc.LightningClient,
	bobPubKey string,
	bobP2PAddr string,
) {
	t.Helper()

	connectReq := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{Pubkey: bobPubKey, Host: bobP2PAddr},
	}
	waitFor(ctx, t, LNDReadyPollInterval, func() (bool, error) {
		_, err := aliceRPC.ConnectPeer(ctx, connectReq)
		switch {
		case err == nil:
			return true, nil
		case strings.Contains(err.Error(), "already connected"):
			return true, nil
		default:
			return false, err
		}
	})
}

// WaitForLCPPeers waits until ListLCPPeers returns exactly one LCP-ready peer.
func WaitForLCPPeers(
	ctx context.Context,
	t *testing.T,
	client lcpdv1.LCPDServiceClient,
) []*lcpdv1.LCPPeer {
	t.Helper()

	const want = 1

	var peers []*lcpdv1.LCPPeer
	waitFor(ctx, t, LCPPeerPollInterval, func() (bool, error) {
		resp, err := client.ListLCPPeers(ctx, &lcpdv1.ListLCPPeersRequest{})
		if err != nil {
			if st, ok := status.FromError(err); ok && st.Code() == codes.Unavailable {
				return false, retryable(err)
			}
			return false, err
		}
		peers = resp.GetPeers()
		if len(peers) != want {
			return false, nil
		}
		if peers[0].GetRemoteManifest() == nil {
			return false, nil
		}
		return true, nil
	})
	return peers
}

// AssertPaymentRequestBinding validates invoice binding against LCP terms.
func AssertPaymentRequestBinding(
	ctx context.Context,
	t *testing.T,
	client lnrpc.LightningClient,
	paymentRequest string,
	peerPubKey string,
	termsHash []byte,
	priceMsat uint64,
	quoteExpiryUnix uint64,
) *lnrpc.PayReq {
	t.Helper()

	payReq, err := client.DecodePayReq(ctx, &lnrpc.PayReqString{PayReq: paymentRequest})
	if err != nil {
		t.Fatalf("DecodePayReq: %v", err)
	}
	if payReq.GetDescriptionHash() == "" {
		t.Fatalf("DecodePayReq: description_hash is empty")
	}
	descHash, err := hex.DecodeString(payReq.GetDescriptionHash())
	if err != nil {
		t.Fatalf("DecodePayReq: decode description_hash: %v", err)
	}
	if diff := cmp.Diff(termsHash, descHash); diff != "" {
		t.Fatalf("invoice description_hash mismatch (-want +got):\n%s", diff)
	}
	if got, want := payReq.GetDestination(), peerPubKey; got != want {
		t.Fatalf("invoice destination mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	if priceMsat == 0 {
		t.Fatalf("price_msat must be > 0")
	}

	amtMsat := payReq.GetNumMsat()
	if amtMsat == 0 && payReq.GetNumSatoshis() > 0 {
		const msatPerSat = int64(1000)
		amtMsat = payReq.GetNumSatoshis() * msatPerSat
	}
	if amtMsat <= 0 {
		t.Fatalf(
			"invoice amount is missing or invalid: num_msat=%d num_satoshis=%d",
			payReq.GetNumMsat(),
			payReq.GetNumSatoshis(),
		)
	}
	if got, want := uint64(amtMsat), priceMsat; got != want {
		t.Fatalf("invoice amount mismatch (-want +got):\n%s", cmp.Diff(want, got))
	}

	invoiceExpiryUnix := payReq.GetTimestamp() + payReq.GetExpiry()
	if invoiceExpiryUnix < 0 {
		t.Fatalf("invoice expiry is negative: %d", invoiceExpiryUnix)
	}
	const allowedClockSkewSeconds = int64(5)
	if got, want := uint64(invoiceExpiryUnix), quoteExpiryUnix; got > want+uint64(allowedClockSkewSeconds) {
		t.Fatalf("invoice expiry exceeds quote_expiry: invoice_expiry_unix=%d quote_expiry=%d", got, want)
	}

	return payReq
}

// SendCustomMessage sends a BOLT custom message.
func SendCustomMessage(
	ctx context.Context,
	t *testing.T,
	client lnrpc.LightningClient,
	peerPubKey string,
	msgType lcpwire.MessageType,
	payload []byte,
) {
	t.Helper()

	peerBytes, err := hex.DecodeString(peerPubKey)
	if err != nil {
		t.Fatalf("decode peer pubkey: %v", err)
	}
	_, err = client.SendCustomMessage(ctx, &lnrpc.SendCustomMessageRequest{
		Peer: peerBytes,
		Type: uint32(msgType),
		Data: payload,
	})
	if err != nil {
		t.Fatalf("SendCustomMessage(type=%d): %v", msgType, err)
	}
}

// WaitForCustomMessage waits for a specific custom message type, failing on lcp_error.
func WaitForCustomMessage(
	ctx context.Context,
	t *testing.T,
	stream lnrpc.Lightning_SubscribeCustomMessagesClient,
	wantType lcpwire.MessageType,
) []byte {
	t.Helper()

	for {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("SubscribeCustomMessages recv: %v", err)
		}
		if msg.GetType() == uint32(lcpwire.MessageTypeError) {
			errMsg, decodeErr := lcpwire.DecodeError(msg.GetData())
			if decodeErr != nil {
				t.Fatalf("decode lcp_error: %v", decodeErr)
			}
			t.Fatalf(
				"received lcp_error while waiting for msg_type=%d: code=%d message=%v",
				wantType,
				errMsg.Code,
				errMsg.Message,
			)
		}

		if msg.GetType() != uint32(wantType) {
			select {
			case <-ctx.Done():
				t.Fatalf("context canceled while waiting for msg_type=%d: %v", wantType, ctx.Err())
			default:
			}
			continue
		}
		return msg.GetData()
	}
}

// NewJobMsgID returns a random lcpwire.MsgID.
func NewJobMsgID() (lcpwire.MsgID, error) {
	var id lcpwire.MsgID
	_, err := rand.Read(id[:])
	return id, err
}

// InitWallet initializes a brand-new lnd wallet.
func InitWallet(ctx context.Context, t *testing.T, node *LndNodeHandle) {
	t.Helper()

	conn := DialLND(ctx, t, node.RPCAddr, node.TLSCertPath)
	defer func() { _ = conn.Close() }()

	client := lnrpc.NewWalletUnlockerClient(conn)

	seedResp, err := client.GenSeed(ctx, &lnrpc.GenSeedRequest{})
	if err != nil {
		t.Fatalf("%s GenSeed: %v", node.Name, err)
	}
	if len(seedResp.GetCipherSeedMnemonic()) == 0 {
		t.Fatalf("%s GenSeed: cipher_seed_mnemonic is empty", node.Name)
	}

	_, err = client.InitWallet(ctx, &lnrpc.InitWalletRequest{
		WalletPassword:     []byte(WalletPassword),
		CipherSeedMnemonic: seedResp.GetCipherSeedMnemonic(),
	})
	if err != nil {
		t.Fatalf("%s InitWallet: %v", node.Name, err)
	}
}

// DialLND establishes a TLS gRPC connection to lnd and waits for Ready state.
func DialLND(ctx context.Context, t *testing.T, rpcAddr, tlsCertPath string) *grpc.ClientConn {
	t.Helper()

	pem, err := os.ReadFile(tlsCertPath)
	if err != nil {
		t.Fatalf("read tls cert: %v", err)
	}

	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM(pem) {
		t.Fatalf("append tls cert to pool: %s", tlsCertPath)
	}

	creds := credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    cp,
		ServerName: "localhost",
	})

	conn, err := grpc.NewClient(
		rpcAddr,
		grpc.WithTransportCredentials(creds),
	)
	if err != nil {
		t.Fatalf("grpc new client (%s): %v", rpcAddr, err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, GRPCDialTimeout)
	defer cancel()

	conn.Connect()
	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			return conn
		}
		if !conn.WaitForStateChange(dialCtx, state) {
			_ = conn.Close()
			t.Fatalf("grpc connect lnd (%s): %v", rpcAddr, dialCtx.Err())
		}
	}
}

// WaitForLNDReady waits until GetInfo returns identity and synced_to_chain.
func WaitForLNDReady(ctx context.Context, t *testing.T, client lnrpc.LightningClient) {
	t.Helper()

	waitFor(ctx, t, LNDReadyPollInterval, func() (bool, error) {
		resp, err := client.GetInfo(ctx, &lnrpc.GetInfoRequest{})
		if err != nil {
			return false, retryable(err)
		}
		if resp.GetIdentityPubkey() == "" {
			return false, retryable(errors.New("identity_pubkey is empty"))
		}
		if !resp.GetSyncedToChain() {
			return false, retryable(fmt.Errorf(
				"not synced to chain yet (block_height=%d)",
				resp.GetBlockHeight(),
			))
		}
		return true, nil
	})
}

// FundWallet mines and pays an invoice to the target node to reach a usable balance.
func FundWallet(
	ctx context.Context,
	t *testing.T,
	bitcoind *BitcoindHandle,
	lightning lnrpc.LightningClient,
) string {
	t.Helper()

	resp, err := lightning.NewAddress(
		ctx,
		&lnrpc.NewAddressRequest{Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH},
	)
	if err != nil {
		t.Fatalf("NewAddress: %v", err)
	}
	addr := resp.GetAddress()
	if addr == "" {
		t.Fatalf("NewAddress: got empty address")
	}

	// fund address and mature coinbase
	mineBlocksWithBitcoind(ctx, t, bitcoind.RPC, BlocksToMatureCoinbase)
	if err = bitcoind.RPC.call(ctx, "sendtoaddress", []any{addr, 1.0}, nil); err != nil {
		t.Fatalf("sendtoaddress: %v", err)
	}
	mineBlocksWithBitcoind(ctx, t, bitcoind.RPC, ChannelConfirmBlocks)

	waitForConfirmedBalance(ctx, t, lightning, MinWalletBalanceSats)
	return addr
}

// OpenChannel opens a channel from alice to bob.
func OpenChannel(
	ctx context.Context,
	t *testing.T,
	bitcoind *BitcoindHandle,
	alice lnrpc.LightningClient,
	bob lnrpc.LightningClient,
	bobP2PAddr string,
) string {
	t.Helper()

	pubResp, err := bob.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		t.Fatalf("bob GetInfo: %v", err)
	}
	bobPubkey := pubResp.GetIdentityPubkey()
	if bobPubkey == "" {
		t.Fatalf("bob GetInfo: identity_pubkey is empty")
	}

	connectReq := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{Pubkey: bobPubkey, Host: bobP2PAddr},
	}
	waitFor(ctx, t, LNDReadyPollInterval, func() (bool, error) {
		_, connectErr := alice.ConnectPeer(ctx, connectReq)
		switch {
		case connectErr == nil:
			return true, nil
		case strings.Contains(connectErr.Error(), "already connected"):
			return true, nil
		default:
			return false, connectErr
		}
	})

	chanResp, err := alice.OpenChannelSync(ctx, &lnrpc.OpenChannelRequest{
		NodePubkeyString: bobPubkey,
		LocalFundingAmount: func() int64 {
			return ChannelFundingSats
		}(),
	})
	if err != nil {
		t.Fatalf("OpenChannelSync: %v", err)
	}
	fundingTxID := chanResp.GetFundingTxidStr()
	if fundingTxID == "" {
		txidBytes := chanResp.GetFundingTxidBytes()
		if len(txidBytes) == 0 {
			t.Fatalf("OpenChannelSync: empty funding_txid")
		}

		// Match the semantics of funding_txid_str (byte-reversed txid).
		reversed := slices.Clone(txidBytes)
		slices.Reverse(reversed)
		fundingTxID = hex.EncodeToString(reversed)
	}

	mineBlocksWithBitcoind(ctx, t, bitcoind.RPC, ChannelConfirmBlocks)
	waitForActiveChannel(ctx, t, alice, bobPubkey)
	waitForActiveChannel(ctx, t, bob, "")

	return fundingTxID
}

// AddInvoice creates an invoice with the given memo/amount/terms hash binding.
func AddInvoice(
	ctx context.Context,
	t *testing.T,
	lightning lnrpc.LightningClient,
	memo string,
	amtSats int64,
	descriptionHash []byte,
) *lnrpc.AddInvoiceResponse {
	t.Helper()

	resp, err := lightning.AddInvoice(ctx, &lnrpc.Invoice{
		Memo:            memo,
		Value:           amtSats,
		DescriptionHash: descriptionHash,
		Expiry:          int64(time.Hour.Seconds()),
	})
	if err != nil {
		t.Fatalf("AddInvoice: %v", err)
	}
	if resp.GetPaymentRequest() == "" {
		t.Fatalf("AddInvoice: payment_request is empty")
	}
	return resp
}

// SendPayment pays a BOLT11 invoice via lnd's router.
func SendPayment(
	ctx context.Context,
	t *testing.T,
	router routerrpc.RouterClient,
	paymentRequest string,
) *lnrpc.Payment {
	t.Helper()

	stream, err := router.SendPaymentV2(
		ctx,
		&routerrpc.SendPaymentRequest{
			PaymentRequest:    paymentRequest,
			TimeoutSeconds:    int32(defaultPaymentTimeout.Seconds()),
			FeeLimitSat:       defaultFeeLimitSat,
			NoInflightUpdates: true,
		},
	)
	if err != nil {
		t.Fatalf("SendPaymentV2: %v", err)
	}

	var payment *lnrpc.Payment
	for {
		pmt, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatalf("SendPaymentV2 recv: %v", recvErr)
		}
		payment = pmt
		if payment.GetStatus() == lnrpc.Payment_SUCCEEDED {
			break
		}
	}

	if payment == nil {
		t.Fatalf("payment failed: no payment updates received")
	}
	if payment.GetStatus() != lnrpc.Payment_SUCCEEDED {
		t.Fatalf("payment failed: status=%s", payment.GetStatus())
	}
	return payment
}

// WaitForLocalBalance waits until a local balance against a peer is at least sats.
func WaitForLocalBalance(
	ctx context.Context,
	t *testing.T,
	client lnrpc.LightningClient,
	peerPubkey string,
	wantSats int64,
) {
	t.Helper()

	peerBytes, err := hex.DecodeString(peerPubkey)
	if err != nil {
		t.Fatalf("decode peer pubkey: %v", err)
	}

	waitFor(ctx, t, WalletBalancePoll, func() (bool, error) {
		resp, listErr := client.ListChannels(ctx, &lnrpc.ListChannelsRequest{
			Peer: peerBytes,
		})
		if listErr != nil {
			return false, listErr
		}
		for _, ch := range resp.GetChannels() {
			if ch.GetRemotePubkey() == peerPubkey && ch.GetLocalBalance() >= wantSats {
				return true, nil
			}
		}
		return false, nil
	})
}

// WaitForRoute waits until a route to the peer is found for a given amount.
func WaitForRoute(
	ctx context.Context,
	t *testing.T,
	client lnrpc.LightningClient,
	peerPubkey string,
	amtSats int64,
) {
	t.Helper()

	waitFor(ctx, t, defaultRouteTimeout, func() (bool, error) {
		_, err := client.QueryRoutes(ctx, &lnrpc.QueryRoutesRequest{
			PubKey: peerPubkey,
			Amt:    amtSats,
		})
		switch {
		case err == nil:
			return true, nil
		case strings.Contains(err.Error(), "insufficient local balance"):
			return false, nil
		default:
			return false, err
		}
	})
}

// WaitForInvoiceSettled waits until the provided payment hash is settled.
func WaitForInvoiceSettled(
	ctx context.Context,
	t *testing.T,
	client lnrpc.LightningClient,
	paymentHash []byte,
) {
	t.Helper()

	waitFor(ctx, t, InvoicePollInterval, func() (bool, error) {
		resp, err := client.LookupInvoice(ctx, &lnrpc.PaymentHash{RHash: paymentHash})
		if err != nil {
			return false, err
		}
		return resp.GetState() == lnrpc.Invoice_SETTLED, nil
	})
}

func waitForConfirmedBalance(
	ctx context.Context,
	t *testing.T,
	client lnrpc.LightningClient,
	minSats int64,
) {
	t.Helper()

	waitFor(ctx, t, WalletBalancePoll, func() (bool, error) {
		resp, err := client.WalletBalance(ctx, &lnrpc.WalletBalanceRequest{})
		if err != nil {
			return false, err
		}
		return resp.GetConfirmedBalance() >= minSats, nil
	})
}

func waitForActiveChannel(
	ctx context.Context,
	t *testing.T,
	client lnrpc.LightningClient,
	peerPubkey string,
) {
	t.Helper()

	waitFor(ctx, t, ChannelPollInterval, func() (bool, error) {
		resp, err := client.ListChannels(ctx, &lnrpc.ListChannelsRequest{})
		if err != nil {
			return false, err
		}
		for _, ch := range resp.GetChannels() {
			if peerPubkey != "" && ch.GetRemotePubkey() != peerPubkey {
				continue
			}
			if ch.GetActive() {
				return true, nil
			}
		}
		return false, nil
	})
}

func waitForFile(ctx context.Context, t *testing.T, path string) {
	t.Helper()

	waitFor(ctx, t, FilePollInterval, func() (bool, error) {
		_, err := os.Stat(path)
		switch {
		case errors.Is(err, nil):
			return true, nil
		case errors.Is(err, os.ErrNotExist):
			return false, nil
		default:
			return false, err
		}
	})
}

func waitFor(ctx context.Context, t *testing.T, interval time.Duration, fn func() (bool, error)) {
	t.Helper()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error
	for {
		done, err := fn()
		if done {
			return
		}
		if err != nil {
			if isRetryable(err) {
				lastErr = err
			} else {
				t.Fatalf("wait: %v", err)
			}
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				t.Fatalf("wait: %v (last error: %v)", ctx.Err(), lastErr)
			}
			t.Fatalf("wait: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

type retryableError struct{ cause error }

func (e retryableError) Error() string { return e.cause.Error() }

func (e retryableError) Unwrap() error { return e.cause }

func retryable(err error) error {
	if err == nil {
		return nil
	}
	return retryableError{cause: err}
}

func isRetryable(err error) bool {
	var retryErr retryableError
	return errors.As(err, &retryErr)
}

func mustCreate(t *testing.T, path string) *os.File {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	return f
}

func stopProcess(cmd *exec.Cmd, timeout time.Duration) error {
	_ = cmd.Process.Signal(os.Interrupt)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return errors.New("process kill after timeout")
	}
}

type bitcoinRPC struct {
	client *http.Client
	url    string
	auth   string
}

func newBitcoinRPC(t *testing.T, rpcAddr, cookiePath string) *bitcoinRPC {
	t.Helper()

	cookieBytes, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatalf("read cookie: %v", err)
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString(bytes.TrimSpace(cookieBytes))

	return &bitcoinRPC{
		client: &http.Client{Timeout: BitcoinRPCTimeout},
		url:    fmt.Sprintf("http://%s", rpcAddr),
		auth:   auth,
	}
}

type bitcoinRPCRequest struct {
	Method string `json:"method"`
	Params []any  `json:"params"`
	ID     string `json:"id"`
}

type bitcoinRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type bitcoinRPCResponse struct {
	Result json.RawMessage  `json:"result"`
	Error  *bitcoinRPCError `json:"error"`
	ID     string           `json:"id"`
}

func (c *bitcoinRPC) call(ctx context.Context, method string, params []any, out any) error {
	reqBytes, err := json.Marshal(bitcoinRPCRequest{
		Method: method,
		Params: params,
		ID:     defaultJobID,
	})
	if err != nil {
		return fmt.Errorf("marshal rpc request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(reqBytes))
	if err != nil {
		return fmt.Errorf("new http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.auth)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("rpc http: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read resp: %w", err)
	}

	var rpcResp bitcoinRPCResponse
	unmarshalErr := json.Unmarshal(respBytes, &rpcResp)
	if unmarshalErr != nil {
		return fmt.Errorf("unmarshal resp: %w", unmarshalErr)
	}
	if rpcResp.Error != nil {
		return fmt.Errorf("bitcoin rpc error: %d %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if out != nil {
		resultErr := json.Unmarshal(rpcResp.Result, out)
		if resultErr != nil {
			return fmt.Errorf("unmarshal result: %w", resultErr)
		}
	}
	return nil
}

func waitForBitcoinRPC(ctx context.Context, t *testing.T, rpc *bitcoinRPC) {
	t.Helper()

	waitFor(ctx, t, BitcoinPollInterval, func() (bool, error) {
		var result any
		if err := rpc.call(ctx, "getblockcount", nil, &result); err != nil {
			return false, retryable(err)
		}
		return true, nil
	})
}

func ensureBitcoindWallet(ctx context.Context, t *testing.T, rpc *bitcoinRPC) {
	t.Helper()

	var wallets []string
	if err := rpc.call(ctx, "listwallets", nil, &wallets); err == nil && len(wallets) > 0 {
		return
	}

	const walletName = "lcp-itest"
	if err := rpc.call(ctx, "createwallet", []any{walletName}, nil); err != nil {
		t.Fatalf("createwallet: %v", err)
	}
}

func primeBitcoindForLND(ctx context.Context, t *testing.T, rpc *bitcoinRPC) {
	t.Helper()
	mineBlocksWithBitcoind(ctx, t, rpc, BlocksToMatureCoinbase)
}

func mineBlocksWithBitcoind(ctx context.Context, t *testing.T, rpc *bitcoinRPC, blocks int) {
	t.Helper()

	var addrs []string
	if err := rpc.call(ctx, "getnewaddress", nil, &addrs); err != nil || len(addrs) == 0 {
		// try alternative return form
		var addr string
		callErr := rpc.call(ctx, "getnewaddress", nil, &addr)
		if callErr != nil || addr == "" {
			t.Fatalf("getnewaddress: %v addr=%s addrs=%v", callErr, addr, addrs)
		}
		addrs = []string{addr}
	}
	target := addrs[0]

	var hashes []string
	if err := rpc.call(ctx, "generatetoaddress", []any{blocks, target}, &hashes); err != nil {
		t.Fatalf("generatetoaddress: %v", err)
	}
	if len(hashes) != blocks {
		t.Fatalf("generatetoaddress: want %d blocks, got %d", blocks, len(hashes))
	}
}
