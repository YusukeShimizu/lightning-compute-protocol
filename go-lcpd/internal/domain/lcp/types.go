package lcp

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	Hash32Len       = 32
	hexCharsPerByte = 2
)

type JobID [Hash32Len]byte

func (id *JobID) String() string {
	if id == nil {
		return ""
	}

	return hex.EncodeToString(id[:])
}

func (id *JobID) MarshalJSON() ([]byte, error) {
	if id == nil {
		return []byte("null"), nil
	}

	return json.Marshal(id.String())
}

func (id *JobID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("unmarshal job_id json string: %w", err)
	}

	if len(s) != Hash32Len*hexCharsPerByte {
		return fmt.Errorf("job_id must be %d hex chars, got %d", Hash32Len*hexCharsPerByte, len(s))
	}

	decoded, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("decode job_id hex: %w", err)
	}
	if len(decoded) != Hash32Len {
		return fmt.Errorf("job_id must be %d bytes, got %d", Hash32Len, len(decoded))
	}

	copy(id[:], decoded)
	return nil
}

type Hash32 [Hash32Len]byte

func (h *Hash32) String() string {
	if h == nil {
		return ""
	}

	return hex.EncodeToString(h[:])
}

func (h *Hash32) MarshalJSON() ([]byte, error) {
	if h == nil {
		return []byte("null"), nil
	}

	return json.Marshal(h.String())
}

func (h *Hash32) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("unmarshal hash32 json string: %w", err)
	}

	if len(s) != Hash32Len*hexCharsPerByte {
		return fmt.Errorf("hash32 must be %d hex chars, got %d", Hash32Len*hexCharsPerByte, len(s))
	}

	decoded, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("decode hash32 hex: %w", err)
	}
	if len(decoded) != Hash32Len {
		return fmt.Errorf("hash32 must be %d bytes, got %d", Hash32Len, len(decoded))
	}

	copy(h[:], decoded)
	return nil
}

type Terms struct {
	ProtocolVersion uint16
	JobID           JobID
	PriceMsat       uint64
	QuoteExpiry     uint64
}

type TaskSummary struct {
	TaskKind string `json:"task_kind"`
	Model    string `json:"model"`
}

type Quote struct {
	ProtocolVersion uint16      `json:"protocol_version"`
	JobID           *JobID      `json:"job_id"`
	PriceMsat       uint64      `json:"price_msat"`
	QuoteExpiry     uint64      `json:"quote_expiry"`
	TermsHash       *Hash32     `json:"terms_hash"`
	TaskSummary     TaskSummary `json:"task_summary"`
}
