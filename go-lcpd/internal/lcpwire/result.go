package lcpwire

import (
	"errors"
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	resultTLVTypeStatus = uint64(100)

	resultTLVTypeResultStreamID      = uint64(101)
	resultTLVTypeResultHash          = uint64(102)
	resultTLVTypeResultLen           = uint64(103)
	resultTLVTypeResultContentType   = uint64(104)
	resultTLVTypeResultContentEncode = uint64(105)

	// Reuse the same TLV type number as lcp_error.message.
	resultTLVTypeMessage = uint64(81)
)

func EncodeResult(r Result) ([]byte, error) {
	envelopeRecords, err := encodeJobEnvelope(r.Envelope)
	if err != nil {
		return nil, err
	}

	switch r.Status {
	case ResultStatusOK, ResultStatusFailed, ResultStatusCancelled:
	default:
		return nil, fmt.Errorf("invalid status: %d", r.Status)
	}

	status := uint16(r.Status)

	records := append([]tlv.Record(nil), envelopeRecords...)

	if r.Status != ResultStatusOK && r.Message != nil {
		if validateErr := validateUTF8String(*r.Message, "message"); validateErr != nil {
			return nil, validateErr
		}
		msgBytes := []byte(*r.Message)
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(resultTLVTypeMessage), &msgBytes))
	}

	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(resultTLVTypeStatus), &status))

	if r.Status != ResultStatusOK {
		if r.OK != nil {
			return nil, errors.New("ok metadata must be omitted for status!=ok")
		}
		return encodeTLVStream(records)
	}

	if r.OK == nil {
		return nil, errors.New("ok metadata is required for status=ok")
	}
	if r.Message != nil {
		return nil, errors.New("message must be omitted for status=ok")
	}

	if r.OK.ResultContentType == "" {
		return nil, errors.New("result_content_type is required for status=ok")
	}
	if validateErr := validateUTF8String(r.OK.ResultContentType, "result_content_type"); validateErr != nil {
		return nil, validateErr
	}
	if r.OK.ResultContentEncoding == "" {
		return nil, errors.New("result_content_encoding is required for status=ok")
	}
	if validateErr := validateUTF8String(r.OK.ResultContentEncoding, "result_content_encoding"); validateErr != nil {
		return nil, validateErr
	}

	resultStreamID := [32]byte(r.OK.ResultStreamID)
	resultHash := [32]byte(r.OK.ResultHash)
	resultLen := r.OK.ResultLen
	resultContentTypeBytes := []byte(r.OK.ResultContentType)
	resultContentEncodingBytes := []byte(r.OK.ResultContentEncoding)

	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(resultTLVTypeResultStreamID), &resultStreamID))
	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(resultTLVTypeResultHash), &resultHash))
	records = append(records, makeTU64Record(resultTLVTypeResultLen, &resultLen))
	records = append(
		records,
		tlv.MakePrimitiveRecord(tlv.Type(resultTLVTypeResultContentType), &resultContentTypeBytes),
	)
	records = append(
		records,
		tlv.MakePrimitiveRecord(tlv.Type(resultTLVTypeResultContentEncode), &resultContentEncodingBytes),
	)

	return encodeTLVStream(records)
}

func DecodeResult(payload []byte) (Result, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return Result{}, err
	}

	env, err := decodeJobEnvelope(m)
	if err != nil {
		return Result{}, err
	}

	status, err := decodeResultStatus(m)
	if err != nil {
		return Result{}, err
	}

	msg, hasMsg, err := decodeOptionalResultMessage(m)
	if err != nil {
		return Result{}, err
	}

	if status != ResultStatusOK {
		var msgPtr *string
		if hasMsg {
			msgPtr = &msg
		}
		if validateErr := ensureNoResultOKMetadata(m); validateErr != nil {
			return Result{}, validateErr
		}

		return Result{
			Envelope: env,
			Status:   status,
			Message:  msgPtr,
		}, nil
	}

	okMeta, err := decodeResultOKMetadata(m)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Envelope: env,
		Status:   status,
		OK:       okMeta,
	}, nil
}

func decodeResultStatus(m map[uint64][]byte) (ResultStatus, error) {
	statusBytes, err := requireTLV(m, resultTLVTypeStatus)
	if err != nil {
		return 0, err
	}
	statusU16, err := readU16(statusBytes)
	if err != nil {
		return 0, fmt.Errorf("status: %w", err)
	}
	status := ResultStatus(statusU16)
	switch status {
	case ResultStatusOK, ResultStatusFailed, ResultStatusCancelled:
		return status, nil
	default:
		return 0, fmt.Errorf("invalid status: %d", statusU16)
	}
}

func decodeOptionalResultMessage(m map[uint64][]byte) (string, bool, error) {
	b, ok := m[resultTLVTypeMessage]
	if !ok {
		return "", false, nil
	}
	s, err := readUTF8String(b, "message")
	if err != nil {
		return "", false, err
	}
	return s, true, nil
}

func ensureNoResultOKMetadata(m map[uint64][]byte) error {
	for _, t := range []uint64{
		resultTLVTypeResultStreamID,
		resultTLVTypeResultHash,
		resultTLVTypeResultLen,
		resultTLVTypeResultContentType,
		resultTLVTypeResultContentEncode,
	} {
		if _, ok := m[t]; ok {
			return errors.New("ok metadata must be omitted for status!=ok")
		}
	}
	return nil
}

func decodeResultOKMetadata(m map[uint64][]byte) (*ResultOK, error) {
	// status=ok requires ok metadata.
	streamIDBytes, err := requireTLV(m, resultTLVTypeResultStreamID)
	if err != nil {
		return nil, err
	}
	streamID32, err := readBytes32(streamIDBytes)
	if err != nil {
		return nil, fmt.Errorf("result_stream_id: %w", err)
	}

	hashBytes, err := requireTLV(m, resultTLVTypeResultHash)
	if err != nil {
		return nil, err
	}
	hash32, err := readBytes32(hashBytes)
	if err != nil {
		return nil, fmt.Errorf("result_hash: %w", err)
	}

	lenBytes, err := requireTLV(m, resultTLVTypeResultLen)
	if err != nil {
		return nil, err
	}
	resultLen, err := readTU64(lenBytes)
	if err != nil {
		return nil, fmt.Errorf("result_len: %w", err)
	}

	ctBytes, err := requireTLV(m, resultTLVTypeResultContentType)
	if err != nil {
		return nil, err
	}
	ct, err := readUTF8String(ctBytes, "result_content_type")
	if err != nil {
		return nil, err
	}
	if ct == "" {
		return nil, errors.New("result_content_type is required for status=ok")
	}

	ceBytes, err := requireTLV(m, resultTLVTypeResultContentEncode)
	if err != nil {
		return nil, err
	}
	ce, err := readUTF8String(ceBytes, "result_content_encoding")
	if err != nil {
		return nil, err
	}
	if ce == "" {
		return nil, errors.New("result_content_encoding is required for status=ok")
	}

	return &ResultOK{
		ResultStreamID:        lcp.Hash32(streamID32),
		ResultHash:            lcp.Hash32(hash32),
		ResultLen:             resultLen,
		ResultContentType:     ct,
		ResultContentEncoding: ce,
	}, nil
}
