package model

import "time"

const StateVersion = 1

type ComponentState struct {
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
	Addr      string `json:"addr,omitempty"`
	LastError string `json:"last_error,omitempty"`
	Managed   bool   `json:"managed,omitempty"`
}

type Readiness struct {
	LNDReady          bool `json:"lnd_ready"`
	ProviderConnected bool `json:"provider_connected"`
	ChannelReady      bool `json:"channel_ready"`
	LCPDReady         bool `json:"lcpd_ready"`
	OpenAIReady       bool `json:"openai_ready"`
}

type State struct {
	Version int     `json:"version"`
	Network Network `json:"network,omitempty"`

	ProviderNode   ProviderNode   `json:"provider_node,omitempty"`
	ProviderPeerID ProviderPeerID `json:"provider_peer_id,omitempty"`

	Readiness  Readiness                 `json:"readiness"`
	Components map[string]ComponentState `json:"components"`

	UpdatedAtRFC3339 string `json:"updated_at_rfc3339,omitempty"`
}

func NewState() State {
	return State{
		Version:    StateVersion,
		Components: make(map[string]ComponentState),
	}
}

func (s *State) EnsureDefaults() {
	if s.Version == 0 {
		s.Version = StateVersion
	}
	if s.Components == nil {
		s.Components = make(map[string]ComponentState)
	}
}

func (s State) WithUpdatedAt(t time.Time) State {
	s.UpdatedAtRFC3339 = t.UTC().Format(time.RFC3339)
	return s
}

