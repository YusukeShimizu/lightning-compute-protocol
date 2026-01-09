package lcpwire

import (
	"errors"

	"github.com/lightningnetwork/lnd/tlv"
)

func EncodeCall(c Call) ([]byte, error) {
	envelopeRecords, err := encodeCallEnvelope(c.Envelope)
	if err != nil {
		return nil, err
	}

	if c.Method == "" {
		return nil, errors.New("method is required")
	}
	if validateErr := validateUTF8String(c.Method, "method"); validateErr != nil {
		return nil, validateErr
	}

	methodBytes := []byte(c.Method)

	records := append([]tlv.Record(nil), envelopeRecords...)
	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(tlvTypeMethod), &methodBytes))

	if c.ParamsBytes != nil {
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(tlvTypeParams), c.ParamsBytes))
	}

	if c.ParamsContentType != nil {
		if *c.ParamsContentType == "" {
			return nil, errors.New("params_content_type must be non-empty when present")
		}
		if validateErr := validateUTF8String(*c.ParamsContentType, "params_content_type"); validateErr != nil {
			return nil, validateErr
		}

		paramsContentTypeBytes := []byte(*c.ParamsContentType)
		records = append(records, tlv.MakePrimitiveRecord(
			tlv.Type(tlvTypeParamsContentType),
			&paramsContentTypeBytes,
		))
	}

	return encodeTLVStream(records)
}

func DecodeCall(payload []byte) (Call, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return Call{}, err
	}

	env, err := decodeCallEnvelope(m)
	if err != nil {
		return Call{}, err
	}

	methodBytes, err := requireTLV(m, tlvTypeMethod)
	if err != nil {
		return Call{}, err
	}
	method, err := readUTF8String(methodBytes, "method")
	if err != nil {
		return Call{}, err
	}
	if method == "" {
		return Call{}, errors.New("method is required")
	}

	var paramsPtr *[]byte
	if b, ok := m[tlvTypeParams]; ok {
		paramsPtr = ptrCopyBytes(b)
	}

	var paramsContentTypePtr *string
	if b, ok := m[tlvTypeParamsContentType]; ok {
		s, sErr := readUTF8String(b, "params_content_type")
		if sErr != nil {
			return Call{}, sErr
		}
		if s == "" {
			return Call{}, errors.New("params_content_type must be non-empty when present")
		}
		paramsContentTypePtr = &s
	}

	return Call{
		Envelope:          env,
		Method:            method,
		ParamsBytes:       paramsPtr,
		ParamsContentType: paramsContentTypePtr,
	}, nil
}
