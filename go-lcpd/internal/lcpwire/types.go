package lcpwire

import "github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"

// MessageType is a BOLT #1 custom message type.
//
// LCP v0.2 uses odd types >= 32768.
type MessageType uint16

const (
	// ProtocolVersionV02 is the protocol_version for LCP v0.2 as defined in
	// `protocol/protocol.md`.
	// v0.2 encodes to 2 (major*100 + minor).
	ProtocolVersionV02 = uint16(2)

	MessageTypeManifest      MessageType = 42081
	MessageTypeQuoteRequest  MessageType = 42083
	MessageTypeQuoteResponse MessageType = 42085
	MessageTypeResult        MessageType = 42087
	MessageTypeStreamBegin   MessageType = 42089
	MessageTypeStreamChunk   MessageType = 42091
	MessageTypeStreamEnd     MessageType = 42093
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

	MaxPayloadBytes uint32
	MaxStreamBytes  uint64
	MaxJobBytes     uint64
	MaxInflightJobs *uint16
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

type StreamKind uint16

const (
	StreamKindInput  StreamKind = 1
	StreamKindResult StreamKind = 2
)

type StreamBegin struct {
	Envelope JobEnvelope

	StreamID lcp.Hash32
	Kind     StreamKind

	TotalLen        *uint64
	SHA256          *lcp.Hash32
	ContentType     string
	ContentEncoding string
}

type StreamChunk struct {
	Envelope JobEnvelope

	StreamID lcp.Hash32
	Seq      uint32 // tu32
	Data     []byte
}

type StreamEnd struct {
	Envelope JobEnvelope

	StreamID lcp.Hash32
	TotalLen uint64 // tu64
	SHA256   lcp.Hash32
}

type ResultStatus uint16

const (
	ResultStatusOK        ResultStatus = 0
	ResultStatusFailed    ResultStatus = 1
	ResultStatusCancelled ResultStatus = 2
)

type ResultOK struct {
	ResultStreamID lcp.Hash32
	ResultHash     lcp.Hash32
	ResultLen      uint64 // tu64

	ResultContentType     string
	ResultContentEncoding string
}

// Result represents the v0.2 terminal job completion (`lcp_result`).
type Result struct {
	Envelope JobEnvelope

	Status  ResultStatus
	OK      *ResultOK
	Message *string // UTF-8 (only used when Status != ok)
}

type Cancel struct {
	Envelope JobEnvelope

	Reason *string // UTF-8
}

type ErrorCode uint16

const (
	ErrorCodeUnsupportedVersion  ErrorCode = 1
	ErrorCodeUnsupportedTask     ErrorCode = 2
	ErrorCodeQuoteExpired        ErrorCode = 3
	ErrorCodePaymentRequired     ErrorCode = 4
	ErrorCodePaymentInvalid      ErrorCode = 5
	ErrorCodePayloadTooLarge     ErrorCode = 6
	ErrorCodeRateLimited         ErrorCode = 7
	ErrorCodeUnsupportedParams   ErrorCode = 8
	ErrorCodeUnsupportedEncoding ErrorCode = 9
	ErrorCodeInvalidState        ErrorCode = 10
	ErrorCodeChunkOutOfOrder     ErrorCode = 11
	ErrorCodeChecksumMismatch    ErrorCode = 12
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
