package lcpwire

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	manifestTLVTypeMaxPayloadBytes = uint64(11)
	manifestTLVTypeSupportedTasks  = uint64(12)
)

func EncodeManifest(m Manifest) ([]byte, error) {
	if m.ProtocolVersion == 0 {
		return nil, errors.New("protocol_version is required")
	}

	pv := m.ProtocolVersion

	records := []tlv.Record{tlv.MakePrimitiveRecord(tlv.Type(tlvTypeProtocolVersion), &pv)}

	if m.MaxPayloadBytes != nil {
		maxPayloadBytes := *m.MaxPayloadBytes
		records = append(records, makeTU32Record(manifestTLVTypeMaxPayloadBytes, &maxPayloadBytes))
	}

	if len(m.SupportedTasks) != 0 {
		encodedTasks := make([][]byte, 0, len(m.SupportedTasks))
		for i, tmpl := range m.SupportedTasks {
			taskBytes, err := EncodeTaskTemplate(tmpl)
			if err != nil {
				return nil, fmt.Errorf("encode supported_tasks[%d]: %w", i, err)
			}
			encodedTasks = append(encodedTasks, taskBytes)
		}

		listBytes, err := EncodeBytesList(encodedTasks)
		if err != nil {
			return nil, fmt.Errorf("encode supported_tasks bytes_list: %w", err)
		}

		// tlv.MakePrimitiveRecord expects a *[]byte for varbytes.
		records = append(
			records,
			tlv.MakePrimitiveRecord(tlv.Type(manifestTLVTypeSupportedTasks), &listBytes),
		)
	}

	return encodeTLVStream(records)
}

func DecodeManifest(payload []byte) (Manifest, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return Manifest{}, err
	}
	if validateErr := validateJobScopeNotPresent(m); validateErr != nil {
		return Manifest{}, validateErr
	}

	pv, err := decodeProtocolVersionOnly(m)
	if err != nil {
		return Manifest{}, err
	}

	var maxPayloadBytes *uint32
	if b, ok := m[manifestTLVTypeMaxPayloadBytes]; ok {
		v, parseErr := readTU32(b)
		if parseErr != nil {
			return Manifest{}, fmt.Errorf("max_payload_bytes: %w", parseErr)
		}
		maxPayloadBytes = &v
	}

	var supportedTasks []TaskTemplate
	if b, ok := m[manifestTLVTypeSupportedTasks]; ok {
		items, listErr := DecodeBytesList(b)
		if listErr != nil {
			return Manifest{}, fmt.Errorf("supported_tasks: %w", listErr)
		}

		supportedTasks = make([]TaskTemplate, 0, len(items))
		for i, item := range items {
			tmpl, tmplErr := DecodeTaskTemplate(item)
			if tmplErr != nil {
				return Manifest{}, fmt.Errorf("supported_tasks[%d]: %w", i, tmplErr)
			}
			supportedTasks = append(supportedTasks, tmpl)
		}
	}

	return Manifest{
		ProtocolVersion: pv,
		MaxPayloadBytes: maxPayloadBytes,
		SupportedTasks:  supportedTasks,
	}, nil
}

func EncodeTaskTemplate(t TaskTemplate) ([]byte, error) {
	if t.TaskKind == "" {
		return nil, errors.New("task_kind is required")
	}
	if err := validateUTF8String(t.TaskKind, "task_kind"); err != nil {
		return nil, err
	}

	taskKindBytes := []byte(t.TaskKind)

	records := []tlv.Record{tlv.MakePrimitiveRecord(tlv.Type(tlvTypeTaskKind), &taskKindBytes)}

	paramsBytes, err := taskParamsBytes(t.TaskKind, t.ParamsBytes, t.LLMChatParams)
	if err != nil {
		return nil, err
	}
	if paramsBytes != nil {
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(tlvTypeParams), paramsBytes))
	}

	return encodeTLVStream(records)
}

func DecodeTaskTemplate(payload []byte) (TaskTemplate, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return TaskTemplate{}, err
	}

	taskKindBytes, err := requireTLV(m, tlvTypeTaskKind)
	if err != nil {
		return TaskTemplate{}, err
	}
	taskKind, err := readUTF8String(taskKindBytes, "task_kind")
	if err != nil {
		return TaskTemplate{}, err
	}
	if taskKind == "" {
		return TaskTemplate{}, errors.New("task_kind is required")
	}

	var paramsPtr *[]byte
	if b, ok := m[tlvTypeParams]; ok {
		paramsPtr = ptrCopyBytes(b)
	}

	var llmChatParams *LLMChatParams
	if taskKind == taskKindLLMChat {
		if paramsPtr == nil {
			return TaskTemplate{}, errors.New("params is required for task_kind=llm.chat")
		}
		decoded, decodeErr := DecodeLLMChatParams(*paramsPtr)
		if decodeErr != nil {
			return TaskTemplate{}, decodeErr
		}
		llmChatParams = &decoded
	}

	return TaskTemplate{
		TaskKind:      taskKind,
		ParamsBytes:   paramsPtr,
		LLMChatParams: llmChatParams,
	}, nil
}

func taskParamsBytes(
	taskKind string,
	paramsBytesPtr *[]byte,
	llmChatParams *LLMChatParams,
) (*[]byte, error) {
	if taskKind != taskKindLLMChat {
		if llmChatParams != nil {
			return nil, fmt.Errorf("llm_chat_params set for non-llm task_kind=%q", taskKind)
		}
		return paramsBytesPtr, nil
	}

	if llmChatParams != nil {
		encoded, err := EncodeLLMChatParams(*llmChatParams)
		if err != nil {
			return nil, err
		}
		return &encoded, nil
	}

	if paramsBytesPtr == nil {
		return nil, errors.New("params is required for task_kind=llm.chat")
	}

	// Validate and normalize llm.chat params.
	if _, decodeErr := DecodeLLMChatParams(*paramsBytesPtr); decodeErr != nil {
		return nil, decodeErr
	}

	return paramsBytesPtr, nil
}
