package model

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
)

type ProviderNode string
type ProviderPeerID string

const (
	peerIDHexLen  = 66
	peerIDByteLen = peerIDHexLen / 2
)

const DefaultMainnetProviderNode ProviderNode = "03737b4a2e44b45f786a18e43c3cf462ab97891e9f8992a0d493394691ac0db983@54.214.32.132:20309"

func ParseProviderNode(s string) (ProviderNode, ProviderPeerID, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return "", "", errors.New("provider is required")
	}
	peerID, hostport, ok := strings.Cut(raw, "@")
	if !ok {
		return "", "", fmt.Errorf("provider must be <pubkey>@<host:port>, got %q", raw)
	}
	peerID = strings.TrimSpace(peerID)
	hostport = strings.TrimSpace(hostport)
	if err := ValidatePeerID(peerID); err != nil {
		return "", "", fmt.Errorf("provider pubkey: %w", err)
	}
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		return "", "", fmt.Errorf("provider host:port: %w", err)
	}
	return ProviderNode(raw), ProviderPeerID(peerID), nil
}

func ValidatePeerID(peerID string) error {
	peerID = strings.TrimSpace(peerID)
	if peerID == "" {
		return errors.New("peer_id is empty")
	}
	if len(peerID) != peerIDHexLen {
		return fmt.Errorf("peer_id must be %d hex chars (got %d)", peerIDHexLen, len(peerID))
	}
	decoded, err := hex.DecodeString(peerID)
	if err != nil {
		return errors.New("peer_id must be hex")
	}
	if len(decoded) != peerIDByteLen {
		return fmt.Errorf("peer_id must be %d bytes (got %d)", peerIDByteLen, len(decoded))
	}
	return nil
}

