package lcpwire

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/lightningnetwork/lnd/tlv"
)

func ComputeDeterministicStreamChunkMsgID(streamID lcp.Hash32, seq uint32) MsgID {
	var b [36]byte
	copy(b[:32], streamID[:])
	binary.BigEndian.PutUint32(b[32:], seq)

	sum := sha256.Sum256(b[:])

	var id MsgID
	copy(id[:], sum[:])
	return id
}

func EncodeStreamChunk(c StreamChunk) ([]byte, MsgID, error) {
	if c.Data == nil {
		return nil, MsgID{}, errors.New("data is required")
	}

	msgID := ComputeDeterministicStreamChunkMsgID(c.StreamID, c.Seq)

	env := c.Envelope
	env.MsgID = msgID

	envelopeRecords, err := encodeCallEnvelope(env)
	if err != nil {
		return nil, MsgID{}, err
	}

	streamID := [32]byte(c.StreamID)
	seq := c.Seq
	data := c.Data

	records := append([]tlv.Record(nil), envelopeRecords...)
	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(streamTLVTypeStreamID), &streamID))
	records = append(records, makeTU32Record(streamTLVTypeSeq, &seq))
	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(streamTLVTypeData), &data))

	payload, err := encodeTLVStream(records)
	if err != nil {
		return nil, MsgID{}, err
	}

	return payload, msgID, nil
}

func DecodeStreamChunk(payload []byte) (StreamChunk, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return StreamChunk{}, err
	}

	env, err := decodeCallEnvelope(m)
	if err != nil {
		return StreamChunk{}, err
	}

	streamIDBytes, err := requireTLV(m, streamTLVTypeStreamID)
	if err != nil {
		return StreamChunk{}, err
	}
	streamID32, err := readBytes32(streamIDBytes)
	if err != nil {
		return StreamChunk{}, fmt.Errorf("stream_id: %w", err)
	}
	streamID := lcp.Hash32(streamID32)

	seqBytes, err := requireTLV(m, streamTLVTypeSeq)
	if err != nil {
		return StreamChunk{}, err
	}
	seq, err := readTU32(seqBytes)
	if err != nil {
		return StreamChunk{}, fmt.Errorf("seq: %w", err)
	}

	dataBytes, err := requireTLV(m, streamTLVTypeData)
	if err != nil {
		return StreamChunk{}, err
	}
	data := cloneBytes(dataBytes)

	wantMsgID := ComputeDeterministicStreamChunkMsgID(streamID, seq)
	if env.MsgID != wantMsgID {
		return StreamChunk{}, errors.New("deterministic msg_id mismatch for lcp_stream_chunk")
	}

	return StreamChunk{
		Envelope: env,
		StreamID: streamID,
		Seq:      seq,
		Data:     data,
	}, nil
}
