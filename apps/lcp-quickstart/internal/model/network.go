package model

import (
	"fmt"
	"strings"
)

type Network string

const (
	NetworkTestnet Network = "testnet"
	NetworkMainnet Network = "mainnet"
)

func ParseNetwork(s string) (Network, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(NetworkTestnet):
		return NetworkTestnet, nil
	case string(NetworkMainnet):
		return NetworkMainnet, nil
	default:
		return "", fmt.Errorf("unsupported network %q (expected %q or %q)", s, NetworkTestnet, NetworkMainnet)
	}
}

