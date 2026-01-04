package lcpwire

import (
	"errors"

	"github.com/lightningnetwork/lnd/tlv"
)

func EncodeQuoteRequest(q QuoteRequest) ([]byte, error) {
	envelopeRecords, err := encodeJobEnvelope(q.Envelope)
	if err != nil {
		return nil, err
	}

	if q.TaskKind == "" {
		return nil, errors.New("task_kind is required")
	}
	if validateErr := validateUTF8String(q.TaskKind, "task_kind"); validateErr != nil {
		return nil, validateErr
	}

	taskKindBytes := []byte(q.TaskKind)

	records := append([]tlv.Record(nil), envelopeRecords...)
	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(tlvTypeTaskKind), &taskKindBytes))

	if q.ParamsBytes != nil {
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(tlvTypeParams), q.ParamsBytes))
	}

	return encodeTLVStream(records)
}

func DecodeQuoteRequest(payload []byte) (QuoteRequest, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return QuoteRequest{}, err
	}

	env, err := decodeJobEnvelope(m)
	if err != nil {
		return QuoteRequest{}, err
	}

	taskKindBytes, err := requireTLV(m, tlvTypeTaskKind)
	if err != nil {
		return QuoteRequest{}, err
	}
	taskKind, err := readUTF8String(taskKindBytes, "task_kind")
	if err != nil {
		return QuoteRequest{}, err
	}
	if taskKind == "" {
		return QuoteRequest{}, errors.New("task_kind is required")
	}

	var paramsPtr *[]byte
	if b, ok := m[tlvTypeParams]; ok {
		paramsPtr = ptrCopyBytes(b)
	}

	return QuoteRequest{
		Envelope:    env,
		TaskKind:    taskKind,
		ParamsBytes: paramsPtr,
	}, nil
}
