package lcpwire

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	manifestTLVTypeMaxPayloadBytes = uint64(11)
	manifestTLVTypeMaxStreamBytes  = uint64(14)
	manifestTLVTypeMaxJobBytes     = uint64(15)
	manifestTLVTypeMaxInflightJobs = uint64(16)
	manifestTLVTypeSupportedTasks  = uint64(12)
)

func EncodeManifest(m Manifest) ([]byte, error) {
	if m.ProtocolVersion == 0 {
		return nil, errors.New("protocol_version is required")
	}
	if m.MaxPayloadBytes == 0 {
		return nil, errors.New("max_payload_bytes is required")
	}
	if m.MaxStreamBytes == 0 {
		return nil, errors.New("max_stream_bytes is required")
	}
	if m.MaxJobBytes == 0 {
		return nil, errors.New("max_job_bytes is required")
	}

	pv := m.ProtocolVersion

	records := []tlv.Record{tlv.MakePrimitiveRecord(tlv.Type(tlvTypeProtocolVersion), &pv)}

	maxPayloadBytes := m.MaxPayloadBytes
	records = append(records, makeTU32Record(manifestTLVTypeMaxPayloadBytes, &maxPayloadBytes))

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

	maxStreamBytes := m.MaxStreamBytes
	records = append(records, makeTU64Record(manifestTLVTypeMaxStreamBytes, &maxStreamBytes))

	maxJobBytes := m.MaxJobBytes
	records = append(records, makeTU64Record(manifestTLVTypeMaxJobBytes, &maxJobBytes))

	if m.MaxInflightJobs != nil {
		maxInflight := *m.MaxInflightJobs
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(manifestTLVTypeMaxInflightJobs), &maxInflight))
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

	maxPayloadBytes, err := decodeManifestMaxPayloadBytes(m)
	if err != nil {
		return Manifest{}, err
	}

	maxStreamBytes, err := decodeManifestMaxStreamBytes(m)
	if err != nil {
		return Manifest{}, err
	}

	maxJobBytes, err := decodeManifestMaxJobBytes(m)
	if err != nil {
		return Manifest{}, err
	}

	maxInflight, hasMaxInflight, err := decodeManifestMaxInflightJobs(m)
	if err != nil {
		return Manifest{}, err
	}
	var maxInflightJobs *uint16
	if hasMaxInflight {
		maxInflightJobs = &maxInflight
	}

	supportedTasks, err := decodeManifestSupportedTasks(m)
	if err != nil {
		return Manifest{}, err
	}

	return Manifest{
		ProtocolVersion: pv,
		MaxPayloadBytes: maxPayloadBytes,
		MaxStreamBytes:  maxStreamBytes,
		MaxJobBytes:     maxJobBytes,
		MaxInflightJobs: maxInflightJobs,
		SupportedTasks:  supportedTasks,
	}, nil
}

func decodeManifestMaxPayloadBytes(m map[uint64][]byte) (uint32, error) {
	maxPayloadBytesRaw, err := requireTLV(m, manifestTLVTypeMaxPayloadBytes)
	if err != nil {
		return 0, err
	}
	maxPayloadBytes, err := readTU32(maxPayloadBytesRaw)
	if err != nil {
		return 0, fmt.Errorf("max_payload_bytes: %w", err)
	}
	if maxPayloadBytes == 0 {
		return 0, errors.New("max_payload_bytes is required")
	}
	return maxPayloadBytes, nil
}

func decodeManifestMaxStreamBytes(m map[uint64][]byte) (uint64, error) {
	maxStreamBytesRaw, err := requireTLV(m, manifestTLVTypeMaxStreamBytes)
	if err != nil {
		return 0, err
	}
	maxStreamBytes, err := readTU64(maxStreamBytesRaw)
	if err != nil {
		return 0, fmt.Errorf("max_stream_bytes: %w", err)
	}
	if maxStreamBytes == 0 {
		return 0, errors.New("max_stream_bytes is required")
	}
	return maxStreamBytes, nil
}

func decodeManifestMaxJobBytes(m map[uint64][]byte) (uint64, error) {
	maxJobBytesRaw, err := requireTLV(m, manifestTLVTypeMaxJobBytes)
	if err != nil {
		return 0, err
	}
	maxJobBytes, err := readTU64(maxJobBytesRaw)
	if err != nil {
		return 0, fmt.Errorf("max_job_bytes: %w", err)
	}
	if maxJobBytes == 0 {
		return 0, errors.New("max_job_bytes is required")
	}
	return maxJobBytes, nil
}

func decodeManifestMaxInflightJobs(m map[uint64][]byte) (uint16, bool, error) {
	b, ok := m[manifestTLVTypeMaxInflightJobs]
	if !ok {
		return 0, false, nil
	}
	v, err := readU16(b)
	if err != nil {
		return 0, false, fmt.Errorf("max_inflight_jobs: %w", err)
	}
	return v, true, nil
}

func decodeManifestSupportedTasks(m map[uint64][]byte) ([]TaskTemplate, error) {
	b, ok := m[manifestTLVTypeSupportedTasks]
	if !ok {
		return nil, nil
	}
	items, err := DecodeBytesList(b)
	if err != nil {
		return nil, fmt.Errorf("supported_tasks: %w", err)
	}

	supportedTasks := make([]TaskTemplate, 0, len(items))
	for i, item := range items {
		tmpl, tmplErr := DecodeTaskTemplate(item)
		if tmplErr != nil {
			return nil, fmt.Errorf("supported_tasks[%d]: %w", i, tmplErr)
		}
		supportedTasks = append(supportedTasks, tmpl)
	}
	return supportedTasks, nil
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

	if t.ParamsBytes != nil {
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(tlvTypeParams), t.ParamsBytes))
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

	return TaskTemplate{
		TaskKind:    taskKind,
		ParamsBytes: paramsPtr,
	}, nil
}
