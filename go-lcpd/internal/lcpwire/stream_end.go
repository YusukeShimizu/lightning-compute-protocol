package lcpwire

import (
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/lightningnetwork/lnd/tlv"
)

func EncodeStreamEnd(e StreamEnd) ([]byte, error) {
	envelopeRecords, err := encodeJobEnvelope(e.Envelope)
	if err != nil {
		return nil, err
	}

	streamID := [32]byte(e.StreamID)
	totalLen := e.TotalLen
	sha256Sum := [32]byte(e.SHA256)

	records := append([]tlv.Record(nil), envelopeRecords...)
	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(streamTLVTypeStreamID), &streamID))
	records = append(records, makeTU64Record(streamTLVTypeTotalLen, &totalLen))
	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(streamTLVTypeSHA256), &sha256Sum))

	return encodeTLVStream(records)
}

func DecodeStreamEnd(payload []byte) (StreamEnd, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return StreamEnd{}, err
	}

	env, err := decodeJobEnvelope(m)
	if err != nil {
		return StreamEnd{}, err
	}

	streamIDBytes, err := requireTLV(m, streamTLVTypeStreamID)
	if err != nil {
		return StreamEnd{}, err
	}
	streamID32, err := readBytes32(streamIDBytes)
	if err != nil {
		return StreamEnd{}, fmt.Errorf("stream_id: %w", err)
	}

	totalLenBytes, err := requireTLV(m, streamTLVTypeTotalLen)
	if err != nil {
		return StreamEnd{}, err
	}
	totalLen, err := readTU64(totalLenBytes)
	if err != nil {
		return StreamEnd{}, fmt.Errorf("total_len: %w", err)
	}

	sha256Bytes, err := requireTLV(m, streamTLVTypeSHA256)
	if err != nil {
		return StreamEnd{}, err
	}
	sha256Sum32, err := readBytes32(sha256Bytes)
	if err != nil {
		return StreamEnd{}, fmt.Errorf("sha256: %w", err)
	}

	return StreamEnd{
		Envelope: env,
		StreamID: lcp.Hash32(streamID32),
		TotalLen: totalLen,
		SHA256:   lcp.Hash32(sha256Sum32),
	}, nil
}
