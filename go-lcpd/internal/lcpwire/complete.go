package lcpwire

import (
	"errors"
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	completeTLVTypeStatus = uint64(100)

	completeTLVTypeResponseStreamID      = uint64(101)
	completeTLVTypeResponseHash          = uint64(102)
	completeTLVTypeResponseLen           = uint64(103)
	completeTLVTypeResponseContentType   = uint64(104)
	completeTLVTypeResponseContentEncode = uint64(105)

	// Reuse the same TLV type number as lcp_error.message.
	completeTLVTypeMessage = uint64(81)
)

func EncodeComplete(c Complete) ([]byte, error) {
	envelopeRecords, err := encodeCallEnvelope(c.Envelope)
	if err != nil {
		return nil, err
	}

	switch c.Status {
	case CompleteStatusOK, CompleteStatusFailed, CompleteStatusCancelled:
	default:
		return nil, fmt.Errorf("invalid status: %d", c.Status)
	}

	status := uint16(c.Status)

	records := append([]tlv.Record(nil), envelopeRecords...)

	if c.Message != nil {
		if c.Status == CompleteStatusOK {
			return nil, errors.New("message must be omitted for status=ok")
		}
		if validateErr := validateUTF8String(*c.Message, "message"); validateErr != nil {
			return nil, validateErr
		}
		msgBytes := []byte(*c.Message)
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(completeTLVTypeMessage), &msgBytes))
	}

	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(completeTLVTypeStatus), &status))

	if c.Status == CompleteStatusOK && c.OK == nil {
		return nil, errors.New("response metadata is required for status=ok")
	}

	if c.OK == nil {
		return encodeTLVStream(records)
	}

	if c.OK.ResponseContentType == "" {
		return nil, errors.New("response_content_type is required when response metadata is present")
	}
	if validateErr := validateUTF8String(c.OK.ResponseContentType, "response_content_type"); validateErr != nil {
		return nil, validateErr
	}
	if c.OK.ResponseContentEncoding == "" {
		return nil, errors.New("response_content_encoding is required when response metadata is present")
	}
	if validateErr := validateUTF8String(c.OK.ResponseContentEncoding, "response_content_encoding"); validateErr != nil {
		return nil, validateErr
	}

	responseStreamID := [32]byte(c.OK.ResponseStreamID)
	responseHash := [32]byte(c.OK.ResponseHash)
	responseLen := c.OK.ResponseLen
	responseContentTypeBytes := []byte(c.OK.ResponseContentType)
	responseContentEncodingBytes := []byte(c.OK.ResponseContentEncoding)

	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(completeTLVTypeResponseStreamID), &responseStreamID))
	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(completeTLVTypeResponseHash), &responseHash))
	records = append(records, makeTU64Record(completeTLVTypeResponseLen, &responseLen))
	records = append(
		records,
		tlv.MakePrimitiveRecord(tlv.Type(completeTLVTypeResponseContentType), &responseContentTypeBytes),
	)
	records = append(
		records,
		tlv.MakePrimitiveRecord(tlv.Type(completeTLVTypeResponseContentEncode), &responseContentEncodingBytes),
	)

	return encodeTLVStream(records)
}

func DecodeComplete(payload []byte) (Complete, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return Complete{}, err
	}

	env, err := decodeCallEnvelope(m)
	if err != nil {
		return Complete{}, err
	}

	status, err := decodeCompleteStatus(m)
	if err != nil {
		return Complete{}, err
	}

	msg, hasMsg, err := decodeOptionalCompleteMessage(m)
	if err != nil {
		return Complete{}, err
	}

	okMeta, err := decodeOptionalCompleteResponseMetadata(m)
	if err != nil {
		return Complete{}, err
	}

	if status == CompleteStatusOK {
		if hasMsg {
			return Complete{}, errors.New("message must be omitted for status=ok")
		}
		if okMeta == nil {
			return Complete{}, errors.New("response metadata is required for status=ok")
		}
		return Complete{
			Envelope: env,
			Status:   status,
			OK:       okMeta,
		}, nil
	}

	var msgPtr *string
	if hasMsg {
		msgPtr = &msg
	}

	return Complete{
		Envelope: env,
		Status:   status,
		OK:       okMeta,
		Message:  msgPtr,
	}, nil
}

func decodeCompleteStatus(m map[uint64][]byte) (CompleteStatus, error) {
	statusBytes, err := requireTLV(m, completeTLVTypeStatus)
	if err != nil {
		return 0, err
	}
	statusU16, err := readU16(statusBytes)
	if err != nil {
		return 0, fmt.Errorf("status: %w", err)
	}
	status := CompleteStatus(statusU16)
	switch status {
	case CompleteStatusOK, CompleteStatusFailed, CompleteStatusCancelled:
		return status, nil
	default:
		return 0, fmt.Errorf("invalid status: %d", statusU16)
	}
}

func decodeOptionalCompleteMessage(m map[uint64][]byte) (string, bool, error) {
	b, ok := m[completeTLVTypeMessage]
	if !ok {
		return "", false, nil
	}
	s, err := readUTF8String(b, "message")
	if err != nil {
		return "", false, err
	}
	return s, true, nil
}

func decodeOptionalCompleteResponseMetadata(m map[uint64][]byte) (*CompleteOK, error) {
	_, hasStreamID := m[completeTLVTypeResponseStreamID]
	_, hasHash := m[completeTLVTypeResponseHash]
	_, hasLen := m[completeTLVTypeResponseLen]
	_, hasCT := m[completeTLVTypeResponseContentType]
	_, hasCE := m[completeTLVTypeResponseContentEncode]

	hasAny := hasStreamID || hasHash || hasLen || hasCT || hasCE
	if !hasAny {
		return nil, nil
	}

	if !(hasStreamID && hasHash && hasLen && hasCT && hasCE) {
		return nil, errors.New("incomplete response metadata: all of stream_id/hash/len/content_type/content_encoding are required when any are present")
	}

	streamIDBytes, err := requireTLV(m, completeTLVTypeResponseStreamID)
	if err != nil {
		return nil, err
	}
	streamID32, err := readBytes32(streamIDBytes)
	if err != nil {
		return nil, fmt.Errorf("response_stream_id: %w", err)
	}

	hashBytes, err := requireTLV(m, completeTLVTypeResponseHash)
	if err != nil {
		return nil, err
	}
	hash32, err := readBytes32(hashBytes)
	if err != nil {
		return nil, fmt.Errorf("response_hash: %w", err)
	}

	lenBytes, err := requireTLV(m, completeTLVTypeResponseLen)
	if err != nil {
		return nil, err
	}
	responseLen, err := readTU64(lenBytes)
	if err != nil {
		return nil, fmt.Errorf("response_len: %w", err)
	}

	ctBytes, err := requireTLV(m, completeTLVTypeResponseContentType)
	if err != nil {
		return nil, err
	}
	ct, err := readUTF8String(ctBytes, "response_content_type")
	if err != nil {
		return nil, err
	}
	if ct == "" {
		return nil, errors.New("response_content_type must be non-empty when response metadata is present")
	}

	ceBytes, err := requireTLV(m, completeTLVTypeResponseContentEncode)
	if err != nil {
		return nil, err
	}
	ce, err := readUTF8String(ceBytes, "response_content_encoding")
	if err != nil {
		return nil, err
	}
	if ce == "" {
		return nil, errors.New("response_content_encoding must be non-empty when response metadata is present")
	}

	return &CompleteOK{
		ResponseStreamID:        lcp.Hash32(streamID32),
		ResponseHash:            lcp.Hash32(hash32),
		ResponseLen:             responseLen,
		ResponseContentType:     ct,
		ResponseContentEncoding: ce,
	}, nil
}
