package lcpwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	tlvTypeProtocolVersion = uint64(1)
	tlvTypeCallID          = uint64(2)
	tlvTypeMsgID           = uint64(3)
	tlvTypeExpiry          = uint64(4)

	u16ByteLen     = 2
	bytes32ByteLen = 32
)

func encodeTLVStream(records []tlv.Record) ([]byte, error) {
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("new tlv stream: %w", err)
	}

	var buf bytes.Buffer
	if encodeErr := stream.Encode(&buf); encodeErr != nil {
		return nil, fmt.Errorf("encode tlv stream: %w", encodeErr)
	}

	return buf.Bytes(), nil
}

func decodeTLVMap(payload []byte) (map[uint64][]byte, error) {
	stream, err := tlv.NewStream()
	if err != nil {
		return nil, fmt.Errorf("new empty tlv stream: %w", err)
	}

	parsed, err := stream.DecodeWithParsedTypesP2P(bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("decode tlv stream: %w", err)
	}

	out := make(map[uint64][]byte, len(parsed))
	for k, v := range parsed {
		out[uint64(k)] = v
	}

	return out, nil
}

func requireTLV(m map[uint64][]byte, typ uint64) ([]byte, error) {
	v, ok := m[typ]
	if !ok {
		return nil, fmt.Errorf("missing required tlv type=%d", typ)
	}
	return v, nil
}

func readU16(b []byte) (uint16, error) {
	if len(b) != u16ByteLen {
		return 0, fmt.Errorf("u16 must be %d bytes, got %d", u16ByteLen, len(b))
	}
	return binary.BigEndian.Uint16(b), nil
}

func readBytes32(b []byte) ([32]byte, error) {
	if len(b) != bytes32ByteLen {
		return [32]byte{}, fmt.Errorf("32*byte must be %d bytes, got %d", bytes32ByteLen, len(b))
	}
	var out [32]byte
	copy(out[:], b)
	return out, nil
}

func readTU32(b []byte) (uint32, error) {
	var (
		v   uint32
		buf [8]byte
	)

	if err := tlv.DTUint32(bytes.NewReader(b), &v, &buf, uint64(len(b))); err != nil {
		return 0, err
	}

	return v, nil
}

func readTU64(b []byte) (uint64, error) {
	var (
		v   uint64
		buf [8]byte
	)

	if err := tlv.DTUint64(bytes.NewReader(b), &v, &buf, uint64(len(b))); err != nil {
		return 0, err
	}

	return v, nil
}

func readUTF8String(b []byte, field string) (string, error) {
	if !utf8.Valid(b) {
		return "", fmt.Errorf("%s must be valid UTF-8", field)
	}
	return string(b), nil
}

func makeTU32Record(typ uint64, v *uint32) tlv.Record {
	sizeFunc := func() uint64 { return tlv.SizeTUint32(*v) }
	return tlv.MakeDynamicRecord(tlv.Type(typ), v, sizeFunc, tlv.ETUint32, tlv.DTUint32)
}

func makeTU64Record(typ uint64, v *uint64) tlv.Record {
	sizeFunc := func() uint64 { return tlv.SizeTUint64(*v) }
	return tlv.MakeDynamicRecord(tlv.Type(typ), v, sizeFunc, tlv.ETUint64, tlv.DTUint64)
}

func uint64ToInt(v uint64) (int, error) {
	maxIntValue := uint64(int(^uint(0) >> 1))
	if v > maxIntValue {
		return 0, fmt.Errorf("value too large for int: %d", v)
	}
	return int(v), nil
}

func validateUTF8String(s string, field string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	return nil
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func ptrCopyBytes(b []byte) *[]byte {
	c := cloneBytes(b)
	return &c
}

func validateCallScopeNotPresent(m map[uint64][]byte) error {
	for _, t := range []uint64{tlvTypeCallID, tlvTypeMsgID, tlvTypeExpiry} {
		if _, ok := m[t]; ok {
			return errors.New("call-scope envelope tlvs must not be present")
		}
	}
	return nil
}
