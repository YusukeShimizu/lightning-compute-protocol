package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	lcpdv1 "github.com/bruwbird/lcp/apps/openai-serve/gen/go/lcpd/v1"
)

func (s *Server) discoverModels(ctx context.Context) (map[string]struct{}, error) {
	if len(s.cfg.ModelAllowlist) > 0 {
		return copySet(s.cfg.ModelAllowlist), nil
	}

	resp, err := s.lcpd.ListLCPPeers(ctx, &lcpdv1.ListLCPPeersRequest{})
	if err != nil {
		return nil, err
	}
	return collectModelsFromPeers(resp), nil
}

func (s *Server) validateModel(model string, peersResp *lcpdv1.ListLCPPeersResponse) error {
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

	discovered := collectModelsFromPeers(peersResp)
	if len(discovered) == 0 {
		// Some providers do not advertise supported_tasks. If we have no allowlist
		// and no discovery signal, skip validation to keep the gateway usable.
		return nil
	}

	if _, ok := discovered[model]; ok || s.cfg.AllowUnlistedModels {
		return nil
	}
	return fmt.Errorf("model is not advertised by any connected LCP peer: %q", model)
}

func (s *Server) resolvePeerID(model string, peersResp *lcpdv1.ListLCPPeersResponse) (string, error) {
	model = strings.TrimSpace(model)

	if s.cfg.ModelMap != nil {
		if peerID, ok := s.cfg.ModelMap[model]; ok {
			if !peerInList(peersResp, peerID) {
				return "", fmt.Errorf("peer_id for model %q is not connected/LCP-ready: %s", model, peerID)
			}
			return peerID, nil
		}
	}

	if strings.TrimSpace(s.cfg.DefaultPeerID) != "" {
		if !peerInList(peersResp, s.cfg.DefaultPeerID) {
			return "", fmt.Errorf("OPENAI_SERVE_DEFAULT_PEER_ID is not connected/LCP-ready: %s", s.cfg.DefaultPeerID)
		}
		return s.cfg.DefaultPeerID, nil
	}

	// Prefer peers that advertise the model in supported_tasks.
	for _, p := range peersResp.GetPeers() {
		manifest := p.GetRemoteManifest()
		for _, tmpl := range manifest.GetSupportedTasks() {
			if tmpl.GetKind() != lcpdv1.LCPTaskKind_LCP_TASK_KIND_OPENAI_CHAT_COMPLETIONS_V1 {
				continue
			}
			if tmpl.GetOpenaiChatCompletionsV1().GetModel() == model {
				return p.GetPeerId(), nil
			}
		}
	}

	// Fallback: any connected LCP peer.
	if peers := peersResp.GetPeers(); len(peers) > 0 {
		return peers[0].GetPeerId(), nil
	}

	return "", errors.New("no connected LCP peers available")
}

func collectModelsFromPeers(resp *lcpdv1.ListLCPPeersResponse) map[string]struct{} {
	if resp == nil {
		return nil
	}

	out := make(map[string]struct{})
	for _, p := range resp.GetPeers() {
		for _, tmpl := range p.GetRemoteManifest().GetSupportedTasks() {
			if tmpl.GetKind() != lcpdv1.LCPTaskKind_LCP_TASK_KIND_OPENAI_CHAT_COMPLETIONS_V1 {
				continue
			}
			model := strings.TrimSpace(tmpl.GetOpenaiChatCompletionsV1().GetModel())
			if model == "" {
				continue
			}
			out[model] = struct{}{}
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
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
