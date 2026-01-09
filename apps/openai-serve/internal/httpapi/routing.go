package httpapi

import (
	"errors"
	"fmt"
	"strings"

	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
)

func (s *Server) discoverModels() map[string]struct{} {
	if len(s.cfg.ModelAllowlist) > 0 {
		return copySet(s.cfg.ModelAllowlist)
	}

	if len(s.cfg.ModelMap) != 0 {
		out := make(map[string]struct{}, len(s.cfg.ModelMap))
		for model := range s.cfg.ModelMap {
			out[model] = struct{}{}
		}
		return out
	}

	// LCP v0.3 manifests do not advertise available models. If no allowlist or
	// explicit model map is configured, return an empty list.
	return nil
}

func (s *Server) validateModelForMethod(
	model string,
	_ string,
	_ *lcpdv1.ListLCPPeersResponse,
) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model is required")
	}

	if len(s.cfg.ModelAllowlist) > 0 {
		if _, ok := s.cfg.ModelAllowlist[model]; ok || s.cfg.AllowUnlistedModels {
			return nil
		}
		return fmt.Errorf(
			"model is not allowed: %q (set OPENAI_SERVE_MODEL_ALLOWLIST or OPENAI_SERVE_ALLOW_UNLISTED_MODELS=true)",
			model,
		)
	}

	// LCP v0.3 manifests do not advertise models. Without an explicit allowlist,
	// skip validation to keep the gateway usable (providers enforce their own
	// model policies).
	return nil
}

func (s *Server) resolvePeerIDForMethod(
	model string,
	method string,
	peersResp *lcpdv1.ListLCPPeersResponse,
) (string, error) {
	model = strings.TrimSpace(model)
	method = strings.TrimSpace(method)
	if method == "" {
		return "", errors.New("method is required")
	}

	if peerID, ok, err := s.resolvePeerIDFromModelMap(model, peersResp); ok || err != nil {
		return peerID, err
	}

	if peerID, ok, err := s.resolvePeerIDFromDefaultPeer(peersResp); ok || err != nil {
		return peerID, err
	}

	if peerID, ok := resolvePeerIDFromSupportedMethods(peersResp, method); ok {
		return peerID, nil
	}

	// Fallback: any connected LCP peer.
	if peerID, ok := resolvePeerIDFromAnyPeer(peersResp); ok {
		return peerID, nil
	}

	return "", errors.New("no connected LCP peers available")
}

func (s *Server) resolvePeerIDFromModelMap(
	model string,
	peersResp *lcpdv1.ListLCPPeersResponse,
) (string, bool, error) {
	if s.cfg.ModelMap == nil {
		return "", false, nil
	}

	peerID, ok := s.cfg.ModelMap[model]
	if !ok {
		return "", false, nil
	}
	if !peerInList(peersResp, peerID) {
		return "", true, fmt.Errorf("peer_id for model %q is not connected/LCP-ready: %s", model, peerID)
	}
	return peerID, true, nil
}

func (s *Server) resolvePeerIDFromDefaultPeer(
	peersResp *lcpdv1.ListLCPPeersResponse,
) (string, bool, error) {
	defaultPeerID := strings.TrimSpace(s.cfg.DefaultPeerID)
	if defaultPeerID == "" {
		return "", false, nil
	}
	if !peerInList(peersResp, defaultPeerID) {
		return "", true, fmt.Errorf("OPENAI_SERVE_DEFAULT_PEER_ID is not connected/LCP-ready: %s", defaultPeerID)
	}
	return defaultPeerID, true, nil
}

func resolvePeerIDFromSupportedMethods(
	peersResp *lcpdv1.ListLCPPeersResponse,
	method string,
) (string, bool) {
	for _, p := range peersResp.GetPeers() {
		m := p.GetRemoteManifest()
		if m == nil {
			continue
		}
		for _, desc := range m.GetSupportedMethods() {
			if strings.TrimSpace(desc.GetMethod()) != method {
				continue
			}
			return p.GetPeerId(), true
		}
	}
	return "", false
}

func resolvePeerIDFromAnyPeer(peersResp *lcpdv1.ListLCPPeersResponse) (string, bool) {
	if peers := peersResp.GetPeers(); len(peers) > 0 {
		return peers[0].GetPeerId(), true
	}
	return "", false
}

func peerInList(resp *lcpdv1.ListLCPPeersResponse, peerID string) bool {
	peerID = strings.TrimSpace(peerID)
	if peerID == "" || resp == nil {
		return false
	}
	for _, p := range resp.GetPeers() {
		if p.GetPeerId() == peerID {
			return true
		}
	}
	return false
}

func copySet(in map[string]struct{}) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}
