package lcpwire

import (
	"bytes"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// maxStringListCount is a soft limit used when decoding untrusted payloads.
	maxStringListCount = 1024

	// maxStringListTotalBytes is a soft limit used when decoding untrusted payloads.
	maxStringListTotalBytes = 1 << 20 // 1MiB
)

// EncodeStringList encodes a `string_list` as defined in `docs/protocol/protocol.md`.
//
// data: bigsize(count) + count*(bigsize(len_i) + len_i bytes)
func EncodeStringList(items []string) ([]byte, error) {
	var (
		buf     bytes.Buffer
		scratch [8]byte
	)

	if err := tlv.WriteVarInt(&buf, uint64(len(items)), &scratch); err != nil {
		return nil, fmt.Errorf("write string_list count: %w", err)
	}

	for i, item := range items {
		if !utf8.ValidString(item) {
			return nil, fmt.Errorf("string_list[%d] must be valid UTF-8", i)
		}

		b := []byte(item)
		if err := tlv.WriteVarInt(&buf, uint64(len(b)), &scratch); err != nil {
			return nil, fmt.Errorf("write string_list[%d] length: %w", i, err)
		}
		if _, err := buf.Write(b); err != nil {
			return nil, fmt.Errorf("write string_list[%d] bytes: %w", i, err)
		}
	}

	return buf.Bytes(), nil
}

// DecodeStringList decodes a `string_list` as defined in `docs/protocol/protocol.md`.
func DecodeStringList(b []byte) ([]string, error) {
	var (
		r       = bytes.NewReader(b)
		scratch [8]byte
	)

	countU64, countErr := tlv.ReadVarInt(r, &scratch)
	if countErr != nil {
		return nil, fmt.Errorf("read string_list count: %w", countErr)
	}
	if countU64 > maxStringListCount {
		return nil, fmt.Errorf("string_list count too large: %d", countU64)
	}
	count := int(countU64)

	items := make([]string, 0, count)
	var total uint64
	for i := range count {
		nU64, lenErr := tlv.ReadVarInt(r, &scratch)
		if lenErr != nil {
			return nil, fmt.Errorf("read string_list[%d] length: %w", i, lenErr)
		}
		n, convErr := uint64ToInt(nU64)
		if convErr != nil {
			return nil, fmt.Errorf("read string_list[%d] length: %w", i, convErr)
		}
		if n > r.Len() {
			return nil, fmt.Errorf("string_list[%d] length %d exceeds remaining %d", i, n, r.Len())
		}

		total += nU64
		if total > maxStringListTotalBytes {
			return nil, fmt.Errorf("string_list too large: %d bytes", total)
		}

		itemBytes := make([]byte, n)
		if _, readErr := io.ReadFull(r, itemBytes); readErr != nil {
			return nil, fmt.Errorf("read string_list[%d] bytes: %w", i, readErr)
		}
		if !utf8.Valid(itemBytes) {
			return nil, fmt.Errorf("string_list[%d] must be valid UTF-8", i)
		}

		items = append(items, string(itemBytes))
	}

	if r.Len() != 0 {
		return nil, fmt.Errorf("string_list has trailing bytes: %d", r.Len())
	}

	return items, nil
}
