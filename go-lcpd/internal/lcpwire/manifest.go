package lcpwire

import (
	"errors"
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	manifestTLVTypeMaxPayloadBytes  = uint64(11)
	manifestTLVTypeSupportedMethods = uint64(12)
	manifestTLVTypeMaxStreamBytes   = uint64(14)
	manifestTLVTypeMaxCallBytes     = uint64(15)
	manifestTLVTypeMaxInflightCalls = uint64(16)
)

const (
	methodTLVTypeMethod               = uint64(20)
	methodTLVTypeRequestContentTypes  = uint64(23)
	methodTLVTypeResponseContentTypes = uint64(24)
	methodTLVTypeDocsURI              = uint64(26)
	methodTLVTypeDocsSHA256           = uint64(27)
	methodTLVTypePolicyNotice         = uint64(28)
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
	if m.MaxCallBytes == 0 {
		return nil, errors.New("max_call_bytes is required")
	}

	pv := m.ProtocolVersion

	records := []tlv.Record{tlv.MakePrimitiveRecord(tlv.Type(tlvTypeProtocolVersion), &pv)}

	maxPayloadBytes := m.MaxPayloadBytes
	records = append(records, makeTU32Record(manifestTLVTypeMaxPayloadBytes, &maxPayloadBytes))

	if len(m.SupportedMethods) != 0 {
		encodedMethods := make([][]byte, 0, len(m.SupportedMethods))
		for i, desc := range m.SupportedMethods {
			methodBytes, err := EncodeMethodDescriptor(desc)
			if err != nil {
				return nil, fmt.Errorf("encode supported_methods[%d]: %w", i, err)
			}
			encodedMethods = append(encodedMethods, methodBytes)
		}

		listBytes, err := EncodeBytesList(encodedMethods)
		if err != nil {
			return nil, fmt.Errorf("encode supported_methods bytes_list: %w", err)
		}

		// tlv.MakePrimitiveRecord expects a *[]byte for varbytes.
		records = append(
			records,
			tlv.MakePrimitiveRecord(tlv.Type(manifestTLVTypeSupportedMethods), &listBytes),
		)
	}

	maxStreamBytes := m.MaxStreamBytes
	records = append(records, makeTU64Record(manifestTLVTypeMaxStreamBytes, &maxStreamBytes))

	maxCallBytes := m.MaxCallBytes
	records = append(records, makeTU64Record(manifestTLVTypeMaxCallBytes, &maxCallBytes))

	if m.MaxInflightCalls != nil {
		maxInflight := *m.MaxInflightCalls
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(manifestTLVTypeMaxInflightCalls), &maxInflight))
	}

	return encodeTLVStream(records)
}

func DecodeManifest(payload []byte) (Manifest, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return Manifest{}, err
	}
	if validateErr := validateCallScopeNotPresent(m); validateErr != nil {
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

	maxCallBytes, err := decodeManifestMaxCallBytes(m)
	if err != nil {
		return Manifest{}, err
	}

	maxInflight, hasMaxInflight, err := decodeManifestMaxInflightCalls(m)
	if err != nil {
		return Manifest{}, err
	}
	var maxInflightCalls *uint16
	if hasMaxInflight {
		maxInflightCalls = &maxInflight
	}

	supportedMethods, err := decodeManifestSupportedMethods(m)
	if err != nil {
		return Manifest{}, err
	}

	return Manifest{
		ProtocolVersion:  pv,
		MaxPayloadBytes:  maxPayloadBytes,
		MaxStreamBytes:   maxStreamBytes,
		MaxCallBytes:     maxCallBytes,
		MaxInflightCalls: maxInflightCalls,
		SupportedMethods: supportedMethods,
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

func decodeManifestMaxCallBytes(m map[uint64][]byte) (uint64, error) {
	maxCallBytesRaw, err := requireTLV(m, manifestTLVTypeMaxCallBytes)
	if err != nil {
		return 0, err
	}
	maxCallBytes, err := readTU64(maxCallBytesRaw)
	if err != nil {
		return 0, fmt.Errorf("max_call_bytes: %w", err)
	}
	if maxCallBytes == 0 {
		return 0, errors.New("max_call_bytes is required")
	}
	return maxCallBytes, nil
}

func decodeManifestMaxInflightCalls(m map[uint64][]byte) (uint16, bool, error) {
	b, ok := m[manifestTLVTypeMaxInflightCalls]
	if !ok {
		return 0, false, nil
	}
	v, err := readU16(b)
	if err != nil {
		return 0, false, fmt.Errorf("max_inflight_calls: %w", err)
	}
	return v, true, nil
}

func decodeManifestSupportedMethods(m map[uint64][]byte) ([]MethodDescriptor, error) {
	b, ok := m[manifestTLVTypeSupportedMethods]
	if !ok {
		return nil, nil
	}
	items, err := DecodeBytesList(b)
	if err != nil {
		return nil, fmt.Errorf("supported_methods: %w", err)
	}

	supportedMethods := make([]MethodDescriptor, 0, len(items))
	for i, item := range items {
		desc, descErr := DecodeMethodDescriptor(item)
		if descErr != nil {
			return nil, fmt.Errorf("supported_methods[%d]: %w", i, descErr)
		}
		supportedMethods = append(supportedMethods, desc)
	}
	return supportedMethods, nil
}

func EncodeMethodDescriptor(d MethodDescriptor) ([]byte, error) {
	if d.Method == "" {
		return nil, errors.New("method is required")
	}
	if err := validateUTF8String(d.Method, "method"); err != nil {
		return nil, err
	}

	methodBytes := []byte(d.Method)

	records := []tlv.Record{tlv.MakePrimitiveRecord(tlv.Type(methodTLVTypeMethod), &methodBytes)}

	if len(d.RequestContentTypes) != 0 {
		listBytes, err := EncodeStringList(d.RequestContentTypes)
		if err != nil {
			return nil, fmt.Errorf("encode request_content_types: %w", err)
		}
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(methodTLVTypeRequestContentTypes), &listBytes))
	}

	if len(d.ResponseContentTypes) != 0 {
		listBytes, err := EncodeStringList(d.ResponseContentTypes)
		if err != nil {
			return nil, fmt.Errorf("encode response_content_types: %w", err)
		}
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(methodTLVTypeResponseContentTypes), &listBytes))
	}

	if d.DocsURI != nil {
		if *d.DocsURI == "" {
			return nil, errors.New("docs_uri must be non-empty when present")
		}
		if err := validateUTF8String(*d.DocsURI, "docs_uri"); err != nil {
			return nil, err
		}
		docsURIBytes := []byte(*d.DocsURI)
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(methodTLVTypeDocsURI), &docsURIBytes))
	}

	if d.DocsSHA256 != nil {
		sum := [32]byte(*d.DocsSHA256)
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(methodTLVTypeDocsSHA256), &sum))
	}

	if d.PolicyNotice != nil {
		if *d.PolicyNotice == "" {
			return nil, errors.New("policy_notice must be non-empty when present")
		}
		if err := validateUTF8String(*d.PolicyNotice, "policy_notice"); err != nil {
			return nil, err
		}
		policyNoticeBytes := []byte(*d.PolicyNotice)
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(methodTLVTypePolicyNotice), &policyNoticeBytes))
	}

	return encodeTLVStream(records)
}

func DecodeMethodDescriptor(payload []byte) (MethodDescriptor, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return MethodDescriptor{}, err
	}

	methodRaw, err := requireTLV(m, methodTLVTypeMethod)
	if err != nil {
		return MethodDescriptor{}, err
	}
	method, err := readUTF8String(methodRaw, "method")
	if err != nil {
		return MethodDescriptor{}, err
	}
	if method == "" {
		return MethodDescriptor{}, errors.New("method is required")
	}

	var requestContentTypes []string
	if b, ok := m[methodTLVTypeRequestContentTypes]; ok {
		items, listErr := DecodeStringList(b)
		if listErr != nil {
			return MethodDescriptor{}, fmt.Errorf("request_content_types: %w", listErr)
		}
		requestContentTypes = items
	}

	var responseContentTypes []string
	if b, ok := m[methodTLVTypeResponseContentTypes]; ok {
		items, listErr := DecodeStringList(b)
		if listErr != nil {
			return MethodDescriptor{}, fmt.Errorf("response_content_types: %w", listErr)
		}
		responseContentTypes = items
	}

	var docsURIPtr *string
	if b, ok := m[methodTLVTypeDocsURI]; ok {
		s, sErr := readUTF8String(b, "docs_uri")
		if sErr != nil {
			return MethodDescriptor{}, sErr
		}
		if s == "" {
			return MethodDescriptor{}, errors.New("docs_uri must be non-empty when present")
		}
		docsURIPtr = &s
	}

	var docsSHA256Ptr *lcp.Hash32
	if b, ok := m[methodTLVTypeDocsSHA256]; ok {
		sum32, sumErr := readBytes32(b)
		if sumErr != nil {
			return MethodDescriptor{}, fmt.Errorf("docs_sha256: %w", sumErr)
		}
		sum := lcp.Hash32(sum32)
		docsSHA256Ptr = &sum
	}

	var policyNoticePtr *string
	if b, ok := m[methodTLVTypePolicyNotice]; ok {
		s, sErr := readUTF8String(b, "policy_notice")
		if sErr != nil {
			return MethodDescriptor{}, sErr
		}
		if s == "" {
			return MethodDescriptor{}, errors.New("policy_notice must be non-empty when present")
		}
		policyNoticePtr = &s
	}

	return MethodDescriptor{
		Method:               method,
		RequestContentTypes:  requestContentTypes,
		ResponseContentTypes: responseContentTypes,
		DocsURI:              docsURIPtr,
		DocsSHA256:           docsSHA256Ptr,
		PolicyNotice:         policyNoticePtr,
	}, nil
}
