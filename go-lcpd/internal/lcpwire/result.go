package lcpwire

import "github.com/lightningnetwork/lnd/tlv"

const (
	resultTLVTypeResult      = uint64(40)
	resultTLVTypeContentType = uint64(41)
)

func EncodeResult(r Result) ([]byte, error) {
	envelopeRecords, err := encodeJobEnvelope(r.Envelope)
	if err != nil {
		return nil, err
	}

	resultBytes := r.Result

	records := append([]tlv.Record(nil), envelopeRecords...)
	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(resultTLVTypeResult), &resultBytes))

	if r.ContentType != nil {
		if validateErr := validateUTF8String(*r.ContentType, "content_type"); validateErr != nil {
			return nil, validateErr
		}
		contentTypeBytes := []byte(*r.ContentType)
		records = append(
			records,
			tlv.MakePrimitiveRecord(tlv.Type(resultTLVTypeContentType), &contentTypeBytes),
		)
	}

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

	resultBytes, err := requireTLV(m, resultTLVTypeResult)
	if err != nil {
		return Result{}, err
	}
	result := cloneBytes(resultBytes)

	var contentType *string
	if b, ok := m[resultTLVTypeContentType]; ok {
		s, readErr := readUTF8String(b, "content_type")
		if readErr != nil {
			return Result{}, readErr
		}
		contentType = &s
	}

	return Result{
		Envelope:    env,
		Result:      result,
		ContentType: contentType,
	}, nil
}
