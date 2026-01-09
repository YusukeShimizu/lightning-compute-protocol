package lcpwire

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	errorTLVTypeCode    = uint64(80)
	errorTLVTypeMessage = uint64(81)
)

func EncodeError(e Error) ([]byte, error) {
	envelopeRecords, err := encodeCallEnvelope(e.Envelope)
	if err != nil {
		return nil, err
	}

	if e.Code == 0 {
		return nil, errors.New("code is required")
	}

	code := uint16(e.Code)

	records := append([]tlv.Record(nil), envelopeRecords...)
	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(errorTLVTypeCode), &code))

	if e.Message != nil {
		if validateErr := validateUTF8String(*e.Message, "message"); validateErr != nil {
			return nil, validateErr
		}
		msgBytes := []byte(*e.Message)
		records = append(records, tlv.MakePrimitiveRecord(tlv.Type(errorTLVTypeMessage), &msgBytes))
	}

	return encodeTLVStream(records)
}

func DecodeError(payload []byte) (Error, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return Error{}, err
	}

	env, err := decodeCallEnvelope(m)
	if err != nil {
		return Error{}, err
	}

	codeBytes, err := requireTLV(m, errorTLVTypeCode)
	if err != nil {
		return Error{}, err
	}
	codeU16, err := readU16(codeBytes)
	if err != nil {
		return Error{}, fmt.Errorf("code: %w", err)
	}
	if codeU16 == 0 {
		return Error{}, errors.New("code is required")
	}

	var msg *string
	if b, ok := m[errorTLVTypeMessage]; ok {
		s, readErr := readUTF8String(b, "message")
		if readErr != nil {
			return Error{}, readErr
		}
		msg = &s
	}

	return Error{
		Envelope: env,
		Code:     ErrorCode(codeU16),
		Message:  msg,
	}, nil
}
