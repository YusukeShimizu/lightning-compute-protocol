package lcpwire

import "github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"

// MessageType is a BOLT #1 custom message type.
//
// LCP v0.3 uses odd types >= 32768.
type MessageType uint16

const (
	// ProtocolVersionV03 is the protocol_version for LCP v0.3 as defined in
	// `docs/protocol/protocol.md`.
	// v0.3 encodes to 3 (major*100 + minor).
	ProtocolVersionV03 = uint16(3)

	// ProtocolVersionV02 is a deprecated name kept for compatibility with
	// older code in this repo. LCP v0.3 uses protocol_version=3.
	ProtocolVersionV02 = ProtocolVersionV03

	MessageTypeManifest    MessageType = 42101
	MessageTypeCall        MessageType = 42103
	MessageTypeQuote       MessageType = 42105
	MessageTypeComplete    MessageType = 42107
	MessageTypeStreamBegin MessageType = 42109
	MessageTypeStreamChunk MessageType = 42111
	MessageTypeStreamEnd   MessageType = 42113
	MessageTypeCancel      MessageType = 42115
	MessageTypeError       MessageType = 42117
)

type MsgID [32]byte

type CallEnvelope struct {
	ProtocolVersion uint16
	CallID          lcp.JobID
	MsgID           MsgID
	Expiry          uint64 // tu64 (unix epoch seconds)
}

type Manifest struct {
	ProtocolVersion uint16

	MaxPayloadBytes  uint32
	MaxStreamBytes   uint64
	MaxCallBytes     uint64
	MaxInflightCalls *uint16
	SupportedMethods []MethodDescriptor
}

type MethodDescriptor struct {
	Method string

	// RequestContentTypes and ResponseContentTypes are optional hints for
	// request/response stream content type selection.
	RequestContentTypes  []string
	ResponseContentTypes []string

	DocsURI      *string
	DocsSHA256   *lcp.Hash32
	PolicyNotice *string
}

type Call struct {
	Envelope CallEnvelope

	Method string

	// ParamsBytes is method-dependent opaque bytes.
	ParamsBytes *[]byte

	ParamsContentType *string
}

type Quote struct {
	Envelope CallEnvelope

	PriceMsat   uint64 // tu64
	QuoteExpiry uint64 // tu64
	TermsHash   lcp.Hash32

	PaymentRequest string // BOLT11 invoice string (UTF-8)

	ResponseContentType     *string
	ResponseContentEncoding *string
}

type StreamKind uint16

const (
	StreamKindRequest  StreamKind = 1
	StreamKindResponse StreamKind = 2
)

type StreamBegin struct {
	Envelope CallEnvelope

	StreamID lcp.Hash32
	Kind     StreamKind

	TotalLen        *uint64
	SHA256          *lcp.Hash32
	ContentType     string
	ContentEncoding string
}

type StreamChunk struct {
	Envelope CallEnvelope

	StreamID lcp.Hash32
	Seq      uint32 // tu32
	Data     []byte
}

type StreamEnd struct {
	Envelope CallEnvelope

	StreamID lcp.Hash32
	TotalLen uint64 // tu64
	SHA256   lcp.Hash32
}

type CompleteStatus uint16

const (
	CompleteStatusOK        CompleteStatus = 0
	CompleteStatusFailed    CompleteStatus = 1
	CompleteStatusCancelled CompleteStatus = 2
)

type CompleteOK struct {
	ResponseStreamID lcp.Hash32
	ResponseHash     lcp.Hash32
	ResponseLen      uint64 // tu64

	ResponseContentType     string
	ResponseContentEncoding string
}

// Complete represents the v0.3 terminal call completion (`lcp_complete`).
type Complete struct {
	Envelope CallEnvelope

	Status  CompleteStatus
	OK      *CompleteOK
	Message *string // UTF-8 (only used when Status != ok)
}

type Cancel struct {
	Envelope CallEnvelope

	Reason *string // UTF-8
}

type ErrorCode uint16

const (
	ErrorCodeUnsupportedVersion  ErrorCode = 1
	ErrorCodeManifestRequired    ErrorCode = 2
	ErrorCodeUnsupportedMethod   ErrorCode = 3
	ErrorCodeQuoteExpired        ErrorCode = 4
	ErrorCodePaymentRequired     ErrorCode = 5
	ErrorCodePaymentInvalid      ErrorCode = 6
	ErrorCodePayloadTooLarge     ErrorCode = 7
	ErrorCodeRateLimited         ErrorCode = 8
	ErrorCodeUnsupportedEncoding ErrorCode = 9
	ErrorCodeInvalidState        ErrorCode = 10
	ErrorCodeChunkOutOfOrder     ErrorCode = 11
	ErrorCodeChecksumMismatch    ErrorCode = 12
	ErrorCodeStreamLimitExceeded ErrorCode = 13
)

type Error struct {
	Envelope CallEnvelope

	Code    ErrorCode
	Message *string // UTF-8
}

type OpenAIChatCompletionsV1Params struct {
	Model string

	// Unknown contains unrecognized TLV values keyed by TLV type.
	Unknown map[uint64][]byte
}

type OpenAIResponsesV1Params struct {
	Model string

	// Unknown contains unrecognized TLV values keyed by TLV type.
	Unknown map[uint64][]byte
}
