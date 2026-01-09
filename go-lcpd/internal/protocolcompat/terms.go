package protocolcompat

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
)

const (
	termsTLVTypeProtocolVersion = 1
	termsTLVTypeCallID          = 2
	termsTLVTypeMethod          = 20

	termsTLVTypePriceMsat   = 30
	termsTLVTypeQuoteExpiry = 31

	termsTLVTypeRequestHash            = 50
	termsTLVTypeParamsHash             = 51
	termsTLVTypeRequestLen             = 52
	termsTLVTypeRequestContentType     = 53
	termsTLVTypeRequestContentEncoding = 54

	termsTLVTypeResponseContentType     = 55
	termsTLVTypeResponseContentEncoding = 56
)

type TermsCommit struct {
	Method string

	Request                []byte
	RequestContentType     string
	RequestContentEncoding string

	Params []byte // nil means "absent" (hash is SHA256(empty))

	ResponseContentType     *string
	ResponseContentEncoding *string
}

func NewJobID() (lcp.JobID, error) {
	var id lcp.JobID
	if _, err := rand.Read(id[:]); err != nil {
		return lcp.JobID{}, fmt.Errorf("rand read: %w", err)
	}

	return id, nil
}

func ComputeTermsHash(terms lcp.Terms, commit TermsCommit) (lcp.Hash32, error) {
	if commit.Method == "" {
		return lcp.Hash32{}, errors.New("method is required for terms_hash")
	}
	if !utf8.ValidString(commit.Method) {
		return lcp.Hash32{}, errors.New("method must be valid UTF-8 for terms_hash")
	}
	if commit.RequestContentType == "" {
		return lcp.Hash32{}, errors.New("request_content_type is required for terms_hash")
	}
	if !utf8.ValidString(commit.RequestContentType) {
		return lcp.Hash32{}, errors.New("request_content_type must be valid UTF-8 for terms_hash")
	}
	if commit.RequestContentEncoding == "" {
		return lcp.Hash32{}, errors.New("request_content_encoding is required for terms_hash")
	}
	if !utf8.ValidString(commit.RequestContentEncoding) {
		return lcp.Hash32{}, errors.New("request_content_encoding must be valid UTF-8 for terms_hash")
	}

	if (commit.ResponseContentType != nil) != (commit.ResponseContentEncoding != nil) {
		return lcp.Hash32{}, errors.New("response_content_type and response_content_encoding must be set together for terms_hash")
	}
	if commit.ResponseContentType != nil {
		if *commit.ResponseContentType == "" {
			return lcp.Hash32{}, errors.New("response_content_type must be non-empty when present for terms_hash")
		}
		if !utf8.ValidString(*commit.ResponseContentType) {
			return lcp.Hash32{}, errors.New("response_content_type must be valid UTF-8 for terms_hash")
		}
	}
	if commit.ResponseContentEncoding != nil {
		if *commit.ResponseContentEncoding == "" {
			return lcp.Hash32{}, errors.New("response_content_encoding must be non-empty when present for terms_hash")
		}
		if !utf8.ValidString(*commit.ResponseContentEncoding) {
			return lcp.Hash32{}, errors.New("response_content_encoding must be valid UTF-8 for terms_hash")
		}
	}

	requestHash := sha256.Sum256(commit.Request)
	requestLen := uint64(len(commit.Request))

	paramsHashBytes := commit.Params
	if paramsHashBytes == nil {
		paramsHashBytes = nil
	}
	paramsHash := sha256.Sum256(paramsHashBytes)

	encoded := encodeTermsTLVStream(
		terms,
		commit.Method,
		requestHash,
		paramsHash,
		requestLen,
		commit.RequestContentType,
		commit.RequestContentEncoding,
		commit.ResponseContentType,
		commit.ResponseContentEncoding,
	)
	sum := sha256.Sum256(encoded)
	return lcp.Hash32(sum), nil
}

func encodeTermsTLVStream(
	terms lcp.Terms,
	method string,
	requestHash [32]byte,
	paramsHash [32]byte,
	requestLen uint64,
	requestContentType string,
	requestContentEncoding string,
	responseContentType *string,
	responseContentEncoding *string,
) []byte {
	const u16ByteLen = 2

	v1 := make([]byte, u16ByteLen)
	binary.BigEndian.PutUint16(v1, terms.ProtocolVersion)

	t1 := encodeTLV(termsTLVTypeProtocolVersion, v1)
	t2 := encodeTLV(termsTLVTypeCallID, terms.JobID[:])
	t20 := encodeTLV(termsTLVTypeMethod, []byte(method))
	t30 := encodeTLV(termsTLVTypePriceMsat, encodeTU64(terms.PriceMsat))
	t31 := encodeTLV(termsTLVTypeQuoteExpiry, encodeTU64(terms.QuoteExpiry))
	t50 := encodeTLV(termsTLVTypeRequestHash, requestHash[:])
	t51 := encodeTLV(termsTLVTypeParamsHash, paramsHash[:])
	t52 := encodeTLV(termsTLVTypeRequestLen, encodeTU64(requestLen))
	t53 := encodeTLV(termsTLVTypeRequestContentType, []byte(requestContentType))
	t54 := encodeTLV(termsTLVTypeRequestContentEncoding, []byte(requestContentEncoding))

	var t55 []byte
	if responseContentType != nil {
		t55 = encodeTLV(termsTLVTypeResponseContentType, []byte(*responseContentType))
	}
	var t56 []byte
	if responseContentEncoding != nil {
		t56 = encodeTLV(termsTLVTypeResponseContentEncoding, []byte(*responseContentEncoding))
	}

	out := make([]byte, 0, len(t1)+len(t2)+len(t20)+len(t30)+len(t31)+len(t50)+len(t51)+len(t52)+len(t53)+len(t54)+len(t55)+len(t56))
	out = append(out, t1...)
	out = append(out, t2...)
	out = append(out, t20...)
	out = append(out, t30...)
	out = append(out, t31...)
	out = append(out, t50...)
	out = append(out, t51...)
	out = append(out, t52...)
	out = append(out, t53...)
	out = append(out, t54...)
	if len(t55) > 0 {
		out = append(out, t55...)
	}
	if len(t56) > 0 {
		out = append(out, t56...)
	}
	return out
}

func encodeTLV(t uint64, v []byte) []byte {
	tt := encodeBigSize(t)
	ll := encodeBigSize(uint64(len(v)))

	out := make([]byte, 0, len(tt)+len(ll)+len(v))
	out = append(out, tt...)
	out = append(out, ll...)
	out = append(out, v...)
	return out
}

// encodeTU64 encodes a BOLT "truncated uint64": big-endian, minimal length.
// Zero is encoded as a zero-length byte slice.
func encodeTU64(v uint64) []byte {
	if v == 0 {
		return nil
	}

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)

	i := 0
	for i < len(buf) && buf[i] == 0 {
		i++
	}

	out := make([]byte, len(buf)-i)
	copy(out, buf[i:])
	return out
}

// encodeBigSize encodes an integer using the BOLT bigsize encoding:
// - 0x00..0xfc: 1 byte
// - 0xfd..0xffff: 0xfd + 2 bytes
// - 0x10000..0xffffffff: 0xfe + 4 bytes
// - else: 0xff + 8 bytes.
func encodeBigSize(v uint64) []byte {
	const (
		prefix16 = 0xfd
		prefix32 = 0xfe
		prefix64 = 0xff

		max8  = 0xfc
		max16 = 0xffff
		max32 = 0xffffffff

		u16Len = 2
		u32Len = 4
		u64Len = 8
	)

	switch {
	case v <= max8:
		return []byte{byte(v)}
	case v <= max16:
		var b [u16Len]byte
		binary.BigEndian.PutUint16(b[:], uint16(v))
		return append([]byte{prefix16}, b[:]...)
	case v <= max32:
		var b [u32Len]byte
		binary.BigEndian.PutUint32(b[:], uint32(v))
		return append([]byte{prefix32}, b[:]...)
	default:
		var b [u64Len]byte
		binary.BigEndian.PutUint64(b[:], v)
		return append([]byte{prefix64}, b[:]...)
	}
}
