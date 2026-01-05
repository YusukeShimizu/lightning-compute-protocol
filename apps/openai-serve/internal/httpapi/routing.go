package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
)

func (s *Server) discoverModels(ctx context.Context) (map[string]struct{}, error) {
	if len(s.cfg.ModelAllowlist) > 0 {
		return copySet(s.cfg.ModelAllowlist), nil
	}

	resp, err := s.lcpd.ListLCPPeers(ctx, &lcpdv1.ListLCPPeersRequest{})
	if err != nil {
		return nil, err
	}
	return collectModelsFromPeersForKinds(
		resp,
		[]lcpdv1.LCPTaskKind{
			lcpdv1.LCPTaskKind_LCP_TASK_KIND_OPENAI_CHAT_COMPLETIONS_V1,
			lcpdv1.LCPTaskKind_LCP_TASK_KIND_OPENAI_RESPONSES_V1,
		},
	), nil
}

func (s *Server) validateModelForTaskKind(
	model string,
	taskKind lcpdv1.LCPTaskKind,
	peersResp *lcpdv1.ListLCPPeersResponse,
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

	discovered := collectModelsFromPeersForKind(peersResp, taskKind)
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

func (s *Server) resolvePeerIDForTaskKind(
	model string,
	taskKind lcpdv1.LCPTaskKind,
	peersResp *lcpdv1.ListLCPPeersResponse,
) (string, error) {
	model = strings.TrimSpace(model)

	if peerID, ok, err := s.resolvePeerIDFromModelMap(model, peersResp); ok || err != nil {
		return peerID, err
	}

	if peerID, ok, err := s.resolvePeerIDFromDefaultPeer(peersResp); ok || err != nil {
		return peerID, err
	}

	if peerID, ok := resolvePeerIDFromSupportedTasks(peersResp, taskKind, model); ok {
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

func resolvePeerIDFromSupportedTasks(
	peersResp *lcpdv1.ListLCPPeersResponse,
	taskKind lcpdv1.LCPTaskKind,
	model string,
) (string, bool) {
	for _, p := range peersResp.GetPeers() {
		for _, tmpl := range p.GetRemoteManifest().GetSupportedTasks() {
			if tmpl.GetKind() != taskKind {
				continue
			}
			if advertisedModel := modelFromTaskTemplate(taskKind, tmpl); strings.TrimSpace(advertisedModel) == model {
				return p.GetPeerId(), true
			}
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

func modelFromTaskTemplate(kind lcpdv1.LCPTaskKind, tmpl *lcpdv1.LCPTaskTemplate) string {
	if tmpl == nil {
		return ""
	}

	switch kind {
	case lcpdv1.LCPTaskKind_LCP_TASK_KIND_UNSPECIFIED:
		return ""
	case lcpdv1.LCPTaskKind_LCP_TASK_KIND_OPENAI_CHAT_COMPLETIONS_V1:
		return tmpl.GetOpenaiChatCompletionsV1().GetModel()
	case lcpdv1.LCPTaskKind_LCP_TASK_KIND_OPENAI_RESPONSES_V1:
		return tmpl.GetOpenaiResponsesV1().GetModel()
	}
	return ""
}

func collectModelsFromPeersForKinds(resp *lcpdv1.ListLCPPeersResponse, kinds []lcpdv1.LCPTaskKind) map[string]struct{} {
	if resp == nil {
		return nil
	}

	out := make(map[string]struct{})
	for _, kind := range kinds {
		for model := range collectModelsFromPeersForKind(resp, kind) {
			out[model] = struct{}{}
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

func collectModelsFromPeersForKind(resp *lcpdv1.ListLCPPeersResponse, kind lcpdv1.LCPTaskKind) map[string]struct{} {
	if resp == nil {
		return nil
	}

	out := make(map[string]struct{})
	for _, p := range resp.GetPeers() {
		for _, tmpl := range p.GetRemoteManifest().GetSupportedTasks() {
			if tmpl.GetKind() != kind {
				continue
			}

			model := strings.TrimSpace(modelFromTaskTemplate(kind, tmpl))
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
