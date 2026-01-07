package lcpwire

import (
	"errors"
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	streamTLVTypeStreamID        = uint64(90)
	streamTLVTypeStreamKind      = uint64(91)
	streamTLVTypeTotalLen        = uint64(92)
	streamTLVTypeSHA256          = uint64(93)
	streamTLVTypeContentType     = uint64(94)
	streamTLVTypeContentEncoding = uint64(95)

	streamTLVTypeSeq  = uint64(96)
	streamTLVTypeData = uint64(97)
)

func EncodeStreamBegin(b StreamBegin) ([]byte, error) {
	envelopeRecords, err := encodeJobEnvelope(b.Envelope)
	if err != nil {
		return nil, err
	}

	if b.Kind != StreamKindInput && b.Kind != StreamKindResult {
		return nil, fmt.Errorf("invalid stream_kind: %d", b.Kind)
	}
	if b.ContentType == "" {
		return nil, errors.New("content_type is required")
	}
	if validateErr := validateUTF8String(b.ContentType, "content_type"); validateErr != nil {
		return nil, validateErr
	}
	if b.ContentEncoding == "" {
		return nil, errors.New("content_encoding is required")
	}
	if validateErr := validateUTF8String(b.ContentEncoding, "content_encoding"); validateErr != nil {
		return nil, validateErr
	}

	streamID := [32]byte(b.StreamID)
	streamKind := uint16(b.Kind)
	contentTypeBytes := []byte(b.ContentType)
	contentEncodingBytes := []byte(b.ContentEncoding)

	records := append([]tlv.Record(nil), envelopeRecords...)
	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(streamTLVTypeStreamID), &streamID))
	records = append(
		records,
		tlv.MakePrimitiveRecord(tlv.Type(streamTLVTypeStreamKind), &streamKind),
	)

	if b.TotalLen != nil {
		totalLen := *b.TotalLen
		records = append(records, makeTU64Record(streamTLVTypeTotalLen, &totalLen))
	}
	if b.SHA256 != nil {
		sum := [32]byte(*b.SHA256)
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(streamTLVTypeSHA256), &sum))
	}

	records = append(
		records,
		tlv.MakePrimitiveRecord(tlv.Type(streamTLVTypeContentType), &contentTypeBytes),
	)
	records = append(
		records,
		tlv.MakePrimitiveRecord(tlv.Type(streamTLVTypeContentEncoding), &contentEncodingBytes),
	)

	return encodeTLVStream(records)
}

func DecodeStreamBegin(payload []byte) (StreamBegin, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return StreamBegin{}, err
	}

	env, err := decodeJobEnvelope(m)
	if err != nil {
		return StreamBegin{}, err
	}

	begin, err := decodeStreamBeginBody(m)
	if err != nil {
		return StreamBegin{}, err
	}
	begin.Envelope = env
	return begin, nil
}

func decodeStreamBeginBody(m map[uint64][]byte) (StreamBegin, error) {
	streamIDBytes, err := requireTLV(m, streamTLVTypeStreamID)
	if err != nil {
		return StreamBegin{}, err
	}
	streamID32, err := readBytes32(streamIDBytes)
	if err != nil {
		return StreamBegin{}, fmt.Errorf("stream_id: %w", err)
	}
	streamID := lcp.Hash32(streamID32)

	streamKindBytes, err := requireTLV(m, streamTLVTypeStreamKind)
	if err != nil {
		return StreamBegin{}, err
	}
	streamKindU16, err := readU16(streamKindBytes)
	if err != nil {
		return StreamBegin{}, fmt.Errorf("stream_kind: %w", err)
	}
	kind := StreamKind(streamKindU16)
	if kind != StreamKindInput && kind != StreamKindResult {
		return StreamBegin{}, fmt.Errorf("invalid stream_kind: %d", streamKindU16)
	}

	var totalLen *uint64
	if b, ok := m[streamTLVTypeTotalLen]; ok {
		v, parseErr := readTU64(b)
		if parseErr != nil {
			return StreamBegin{}, fmt.Errorf("total_len: %w", parseErr)
		}
		totalLen = &v
	}

	var sha256Sum *lcp.Hash32
	if b, ok := m[streamTLVTypeSHA256]; ok {
		sum32, parseErr := readBytes32(b)
		if parseErr != nil {
			return StreamBegin{}, fmt.Errorf("sha256: %w", parseErr)
		}
		sum := lcp.Hash32(sum32)
		sha256Sum = &sum
	}

	contentTypeBytes, err := requireTLV(m, streamTLVTypeContentType)
	if err != nil {
		return StreamBegin{}, err
	}
	contentType, err := readUTF8String(contentTypeBytes, "content_type")
	if err != nil {
		return StreamBegin{}, err
	}
	if contentType == "" {
		return StreamBegin{}, errors.New("content_type is required")
	}

	contentEncodingBytes, err := requireTLV(m, streamTLVTypeContentEncoding)
	if err != nil {
		return StreamBegin{}, err
	}
	contentEncoding, err := readUTF8String(contentEncodingBytes, "content_encoding")
	if err != nil {
		return StreamBegin{}, err
	}
	if contentEncoding == "" {
		return StreamBegin{}, errors.New("content_encoding is required")
	}

	return StreamBegin{
		StreamID:        streamID,
		Kind:            kind,
		TotalLen:        totalLen,
		SHA256:          sha256Sum,
		ContentType:     contentType,
		ContentEncoding: contentEncoding,
	}, nil
}
