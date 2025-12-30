package lcpwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// maxBytesListCount is a soft limit used when decoding untrusted payloads.
	maxBytesListCount = 1024

	// maxBytesListTotalBytes is a soft limit used when decoding untrusted payloads.
	maxBytesListTotalBytes = 1 << 20 // 1MiB
)

// EncodeBytesList encodes a `bytes_list` as defined in `protocol/protocol.md`.
//
// data: bigsize(count) + count*(bigsize(len_i) + len_i bytes)
func EncodeBytesList(items [][]byte) ([]byte, error) {
	var (
		buf     bytes.Buffer
		scratch [8]byte
	)

	if err := tlv.WriteVarInt(&buf, uint64(len(items)), &scratch); err != nil {
		return nil, fmt.Errorf("write bytes_list count: %w", err)
	}

	for i, item := range items {
		if err := tlv.WriteVarInt(&buf, uint64(len(item)), &scratch); err != nil {
			return nil, fmt.Errorf("write bytes_list[%d] length: %w", i, err)
		}
		if _, err := buf.Write(item); err != nil {
			return nil, fmt.Errorf("write bytes_list[%d] bytes: %w", i, err)
		}
	}

	return buf.Bytes(), nil
}

// DecodeBytesList decodes a `bytes_list` as defined in `protocol/protocol.md`.
func DecodeBytesList(b []byte) ([][]byte, error) {
	var (
		r       = bytes.NewReader(b)
		scratch [8]byte
	)

	countU64, countErr := tlv.ReadVarInt(r, &scratch)
	if countErr != nil {
		return nil, fmt.Errorf("read bytes_list count: %w", countErr)
	}
	if countU64 > maxBytesListCount {
		return nil, fmt.Errorf("bytes_list count too large: %d", countU64)
	}
	count := int(countU64)

	items := make([][]byte, 0, count)
	var total uint64
	for i := range count {
		nU64, lenErr := tlv.ReadVarInt(r, &scratch)
		if lenErr != nil {
			return nil, fmt.Errorf("read bytes_list[%d] length: %w", i, lenErr)
		}
		n, convErr := uint64ToInt(nU64)
		if convErr != nil {
			return nil, fmt.Errorf("read bytes_list[%d] length: %w", i, convErr)
		}
		if n > r.Len() {
			return nil, fmt.Errorf("bytes_list[%d] length %d exceeds remaining %d", i, n, r.Len())
		}

		total += nU64
		if total > maxBytesListTotalBytes {
			return nil, fmt.Errorf("bytes_list too large: %d bytes", total)
		}

		item := make([]byte, n)
		if _, readErr := io.ReadFull(r, item); readErr != nil {
			return nil, fmt.Errorf("read bytes_list[%d] bytes: %w", i, readErr)
		}
		items = append(items, item)
	}

	if r.Len() != 0 {
		return nil, fmt.Errorf("bytes_list has trailing bytes: %d", r.Len())
	}

	return items, nil
}
