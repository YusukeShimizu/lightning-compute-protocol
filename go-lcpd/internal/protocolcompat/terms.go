package protocolcompat

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	termsTLVTypeProtocolVersion      = 1
	termsTLVTypeJobID                = 2
	termsTLVTypePriceMsat            = 3
	termsTLVTypeQuoteExpiry          = 4
	termsTLVTypeTaskKind             = 20
	termsTLVTypeInputHash            = 50
	termsTLVTypeParamsHash           = 51
	termsTLVTypeInputLen             = 52
	termsTLVTypeInputContentType     = 53
	termsTLVTypeInputContentEncoding = 54
)

type TermsCommit struct {
	TaskKind             string
	Input                []byte
	InputContentType     string
	InputContentEncoding string
	Params               []byte // nil means "absent" (hash is SHA256(empty))
}

func NewJobID() (lcp.JobID, error) {
	var id lcp.JobID
	if _, err := rand.Read(id[:]); err != nil {
		return lcp.JobID{}, fmt.Errorf("rand read: %w", err)
	}

	return id, nil
}

func ComputeTermsHash(terms lcp.Terms, commit TermsCommit) (lcp.Hash32, error) {
	if commit.TaskKind == "" {
		return lcp.Hash32{}, errors.New("task_kind is required for terms_hash")
	}
	if !utf8.ValidString(commit.TaskKind) {
		return lcp.Hash32{}, errors.New("task_kind must be valid UTF-8 for terms_hash")
	}
	if commit.InputContentType == "" {
		return lcp.Hash32{}, errors.New("input_content_type is required for terms_hash")
	}
	if !utf8.ValidString(commit.InputContentType) {
		return lcp.Hash32{}, errors.New("input_content_type must be valid UTF-8 for terms_hash")
	}
	if commit.InputContentEncoding == "" {
		return lcp.Hash32{}, errors.New("input_content_encoding is required for terms_hash")
	}
	if !utf8.ValidString(commit.InputContentEncoding) {
		return lcp.Hash32{}, errors.New("input_content_encoding must be valid UTF-8 for terms_hash")
	}

	inputHash := sha256.Sum256(commit.Input)
	inputLen := uint64(len(commit.Input))

	paramsCanonical := commit.Params
	if commit.Params != nil {
		canonical, err := canonicalizeTLVStream(commit.Params)
		if err != nil {
			return lcp.Hash32{}, fmt.Errorf("canonicalize params tlv: %w", err)
		}
		paramsCanonical = canonical
	}
	paramsHash := sha256.Sum256(paramsCanonical)

	encoded := encodeTermsTLVStream(
		terms,
		commit.TaskKind,
		inputHash,
		paramsHash,
		inputLen,
		commit.InputContentType,
		commit.InputContentEncoding,
	)
	sum := sha256.Sum256(encoded)
	return lcp.Hash32(sum), nil
}

func encodeTermsTLVStream(
	terms lcp.Terms,
	taskKind string,
	inputHash [32]byte,
	paramsHash [32]byte,
	inputLen uint64,
	inputContentType string,
	inputContentEncoding string,
) []byte {
	const u16ByteLen = 2

	v1 := make([]byte, u16ByteLen)
	binary.BigEndian.PutUint16(v1, terms.ProtocolVersion)

	t1 := encodeTLV(termsTLVTypeProtocolVersion, v1)
	t2 := encodeTLV(termsTLVTypeJobID, terms.JobID[:])
	t3 := encodeTLV(termsTLVTypePriceMsat, encodeTU64(terms.PriceMsat))
	t4 := encodeTLV(termsTLVTypeQuoteExpiry, encodeTU64(terms.QuoteExpiry))
	t20 := encodeTLV(termsTLVTypeTaskKind, []byte(taskKind))
	t50 := encodeTLV(termsTLVTypeInputHash, inputHash[:])
	t51 := encodeTLV(termsTLVTypeParamsHash, paramsHash[:])
	t52 := encodeTLV(termsTLVTypeInputLen, encodeTU64(inputLen))
	t53 := encodeTLV(termsTLVTypeInputContentType, []byte(inputContentType))
	t54 := encodeTLV(termsTLVTypeInputContentEncoding, []byte(inputContentEncoding))

	out := make(
		[]byte,
		0,
		len(t1)+len(t2)+len(t3)+len(t4)+len(t20)+len(t50)+len(t51)+len(t52)+len(t53)+len(t54),
	)
	out = append(out, t1...)
	out = append(out, t2...)
	out = append(out, t3...)
	out = append(out, t4...)
	out = append(out, t20...)
	out = append(out, t50...)
	out = append(out, t51...)
	out = append(out, t52...)
	out = append(out, t53...)
	out = append(out, t54...)
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

func canonicalizeTLVStream(payload []byte) ([]byte, error) {
	stream, err := tlv.NewStream()
	if err != nil {
		return nil, fmt.Errorf("new empty tlv stream: %w", err)
	}

	parsed, err := stream.DecodeWithParsedTypesP2P(bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("decode tlv stream: %w", err)
	}

	types := make([]uint64, 0, len(parsed))
	for typ := range parsed {
		types = append(types, uint64(typ))
	}
	slices.Sort(types)

	out := make([]byte, 0, len(payload))
	for _, typ := range types {
		v := parsed[tlv.Type(typ)]
		out = append(out, encodeTLV(typ, v)...)
	}
	return out, nil
}
