package lncli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/model"
)

type WalletState string

const (
	WalletUninitialized WalletState = "uninitialized"
	WalletLocked        WalletState = "locked"
	WalletUnlocked      WalletState = "unlocked"
	WalletUnknown       WalletState = "unknown"
)

type Client struct {
	Path string
}

func DefaultClient() Client {
	return Client{Path: "lncli"}
}

func (c Client) GetInfo(ctx context.Context, network model.Network) ([]byte, error) {
	return c.run(ctx, network, "getinfo")
}

func (c Client) IsPeerConnected(ctx context.Context, network model.Network, peerID model.ProviderPeerID) (bool, error) {
	if strings.TrimSpace(string(peerID)) == "" {
		return false, errors.New("peer_id is empty")
	}
	out, err := c.run(ctx, network, "listpeers")
	if err != nil {
		return false, err
	}
	type peer struct {
		PubKey string `json:"pub_key"`
	}
	type list struct {
		Peers []peer `json:"peers"`
	}
	var lp list
	if err := json.Unmarshal(out, &lp); err != nil {
		return false, fmt.Errorf("parse listpeers json: %w", err)
	}
	for _, p := range lp.Peers {
		if strings.TrimSpace(p.PubKey) == string(peerID) {
			return true, nil
		}
	}
	return false, nil
}

func (c Client) ValidateNetwork(network model.Network, getInfoJSON []byte) error {
	type chain struct {
		Network string `json:"network"`
	}
	type getInfo struct {
		Chains []chain `json:"chains"`
	}
	var gi getInfo
	if err := json.Unmarshal(getInfoJSON, &gi); err != nil {
		return fmt.Errorf("parse getinfo json: %w", err)
	}
	for _, ch := range gi.Chains {
		if strings.EqualFold(strings.TrimSpace(ch.Network), string(network)) {
			return nil
		}
	}
	return fmt.Errorf("lncli is not targeting %s (getinfo.chains[].network=%v)", network, gi.Chains)
}

func (c Client) DetectWalletState(ctx context.Context, network model.Network) WalletState {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := c.GetInfo(ctx, network)
	if err == nil {
		return WalletUnlocked
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "wallet locked"):
		return WalletLocked
	case strings.Contains(msg, "unlock"):
		return WalletLocked
	case strings.Contains(msg, "wallet not found"):
		return WalletUninitialized
	case strings.Contains(msg, "uninitialized"):
		return WalletUninitialized
	default:
		return WalletUnknown
	}
}

func (c Client) ConnectPeer(ctx context.Context, network model.Network, node model.ProviderNode) error {
	_, err := c.run(ctx, network, "connect", string(node))
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "already connected") {
		return nil
	}
	return err
}

func (c Client) HasOutboundLiquidity(ctx context.Context, network model.Network, peerID model.ProviderPeerID, minSats int64) (bool, error) {
	out, err := c.run(ctx, network, "listchannels")
	if err != nil {
		return false, err
	}
	type channel struct {
		Active       bool   `json:"active"`
		RemotePubkey string `json:"remote_pubkey"`
		LocalBalance string `json:"local_balance"`
	}
	type list struct {
		Channels []channel `json:"channels"`
	}
	var lc list
	if err := json.Unmarshal(out, &lc); err != nil {
		return false, fmt.Errorf("parse listchannels json: %w", err)
	}
	for _, ch := range lc.Channels {
		if !ch.Active {
			continue
		}
		if strings.TrimSpace(ch.RemotePubkey) != string(peerID) {
			continue
		}
		localBal, err := strconv.ParseInt(strings.TrimSpace(ch.LocalBalance), 10, 64)
		if err != nil {
			continue
		}
		if localBal >= minSats {
			return true, nil
		}
	}
	return false, nil
}

func (c Client) OpenChannel(ctx context.Context, network model.Network, peerID model.ProviderPeerID, localAmtSats int64) error {
	if localAmtSats <= 0 {
		return fmt.Errorf("local_amt must be > 0, got %d", localAmtSats)
	}
	_, err := c.run(
		ctx,
		network,
		"openchannel",
		"--node_key", string(peerID),
		"--local_amt", strconv.FormatInt(localAmtSats, 10),
	)
	return err
}

func (c Client) run(ctx context.Context, network model.Network, args ...string) ([]byte, error) {
	bin := strings.TrimSpace(c.Path)
	if bin == "" {
		bin = "lncli"
	}
	cmdArgs := make([]string, 0, len(args)+1)
	if network == model.NetworkTestnet {
		cmdArgs = append(cmdArgs, "--network=testnet")
	}
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		out := strings.TrimSpace(combined.String())
		if out != "" {
			return nil, fmt.Errorf("lncli %s failed: %w\n%s", strings.Join(args, " "), err, out)
		}
		if errors.Is(err, exec.ErrNotFound) {
			if strings.Contains(bin, string(filepath.Separator)) {
				return nil, fmt.Errorf("lncli not found: %s", bin)
			}
			return nil, fmt.Errorf("lncli not found in PATH")
		}
		return nil, fmt.Errorf("lncli %s failed: %w", strings.Join(args, " "), err)
	}
	return bytes.TrimSpace(combined.Bytes()), nil
}
