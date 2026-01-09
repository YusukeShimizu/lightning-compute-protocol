package httpapi

import (
	"encoding/binary"
	"errors"
	"strings"
	"unicode/utf8"
)

// encodeOpenAIModelParamsTLV encodes an OpenAI method params TLV stream that
// contains only:
// - type=1: model (UTF-8 string)
//
// This format matches the provider-side expectations for:
// - openai.chat_completions.v1
// - openai.responses.v1.
func encodeOpenAIModelParamsTLV(model string) ([]byte, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, errors.New("model is required")
	}
	if !utf8.ValidString(model) {
		return nil, errors.New("model must be valid UTF-8")
	}

	modelBytes := []byte(model)
	return encodeTLVBigSize(1, modelBytes), nil
}

func encodeTLVBigSize(t uint64, v []byte) []byte {
	tt := encodeBigSize(t)
	ll := encodeBigSize(uint64(len(v)))

	out := make([]byte, 0, len(tt)+len(ll)+len(v))
	out = append(out, tt...)
	out = append(out, ll...)
	out = append(out, v...)
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
