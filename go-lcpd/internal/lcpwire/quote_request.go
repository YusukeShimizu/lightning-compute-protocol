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

	paramsBytesPtr, err := taskParamsBytes(q.TaskKind, q.ParamsBytes, q.LLMChatParams)
	if err != nil {
		return nil, err
	}
	if paramsBytesPtr != nil {
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(tlvTypeParams), paramsBytesPtr))
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

	var llmChatParams *LLMChatParams
	if taskKind == taskKindLLMChat {
		if paramsPtr == nil {
			return QuoteRequest{}, errors.New("params is required for task_kind=llm.chat")
		}
		decoded, decodeErr := DecodeLLMChatParams(*paramsPtr)
		if decodeErr != nil {
			return QuoteRequest{}, decodeErr
		}
		llmChatParams = &decoded
	}

	return QuoteRequest{
		Envelope:      env,
		TaskKind:      taskKind,
		ParamsBytes:   paramsPtr,
		LLMChatParams: llmChatParams,
	}, nil
}
