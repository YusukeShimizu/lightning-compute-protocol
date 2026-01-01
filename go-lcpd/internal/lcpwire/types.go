package lcpwire

import "github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"

// MessageType is a BOLT #1 custom message type.
//
// LCP v0.1 uses odd types >= 32768.
type MessageType uint16

const (
	// ProtocolVersionV01 is the protocol_version for LCP v0.1 as defined in
	// `docs/protocol/protocol.md`.
	// v0.1 encodes to 1 (major*100 + minor).
	ProtocolVersionV01 = uint16(1)

	MessageTypeManifest      MessageType = 42081
	MessageTypeQuoteRequest  MessageType = 42083
	MessageTypeQuoteResponse MessageType = 42085
	MessageTypeResult        MessageType = 42087
	MessageTypeCancel        MessageType = 42095
	MessageTypeError         MessageType = 42097
)

type MsgID [32]byte

type JobEnvelope struct {
	ProtocolVersion uint16
	JobID           lcp.JobID
	MsgID           MsgID
	Expiry          uint64 // tu64 (unix epoch seconds)
}

type Manifest struct {
	ProtocolVersion uint16

	MaxPayloadBytes *uint32
	SupportedTasks  []TaskTemplate
}

type TaskTemplate struct {
	TaskKind string

	// ParamsBytes is task_kind dependent. If TaskKind == "llm.chat", this MUST
	// be an encoded llm_chat_params_tlvs stream.
	ParamsBytes *[]byte

	// LLMChatParams is set iff TaskKind == "llm.chat" and ParamsBytes is present.
	LLMChatParams *LLMChatParams
}

type QuoteRequest struct {
	Envelope JobEnvelope

	TaskKind string
	Input    []byte

	// ParamsBytes is task_kind dependent. If TaskKind == "llm.chat", this MUST
	// be an encoded llm_chat_params_tlvs stream.
	ParamsBytes *[]byte

	// LLMChatParams is set iff TaskKind == "llm.chat" and ParamsBytes is present.
	LLMChatParams *LLMChatParams
}

type QuoteResponse struct {
	Envelope JobEnvelope

	PriceMsat   uint64 // tu64
	QuoteExpiry uint64 // tu64
	TermsHash   lcp.Hash32

	PaymentRequest string // BOLT11 invoice string (UTF-8)
}

type Result struct {
	Envelope JobEnvelope

	Result []byte

	ContentType *string // UTF-8
}

type Cancel struct {
	Envelope JobEnvelope

	Reason *string // UTF-8
}

type ErrorCode uint16

const (
	ErrorCodeUnsupportedVersion ErrorCode = 1
	ErrorCodeUnsupportedTask    ErrorCode = 2
	ErrorCodeQuoteExpired       ErrorCode = 3
	ErrorCodePaymentRequired    ErrorCode = 4
	ErrorCodePaymentInvalid     ErrorCode = 5
	ErrorCodePayloadTooLarge    ErrorCode = 6
	ErrorCodeRateLimited        ErrorCode = 7
	ErrorCodeUnsupportedParams  ErrorCode = 8
)

type Error struct {
	Envelope JobEnvelope

	Code    ErrorCode
	Message *string // UTF-8
}

type LLMChatParams struct {
	Profile string

	TemperatureMilli *uint32 // tu32
	MaxOutputTokens  *uint32 // tu32

	// Unknown contains unrecognized TLV values keyed by TLV type.
	Unknown map[uint64][]byte
}
