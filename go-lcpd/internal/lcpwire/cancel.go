package lcpwire

import "github.com/lightningnetwork/lnd/tlv"

const (
	cancelTLVTypeReason = uint64(70)
)

func EncodeCancel(c Cancel) ([]byte, error) {
	envelopeRecords, err := encodeCallEnvelope(c.Envelope)
	if err != nil {
		return nil, err
	}

	records := append([]tlv.Record(nil), envelopeRecords...)

	if c.Reason != nil {
		if validateErr := validateUTF8String(*c.Reason, "reason"); validateErr != nil {
			return nil, validateErr
		}
		reasonBytes := []byte(*c.Reason)
		records = append(
			records,
			tlv.MakePrimitiveRecord(tlv.Type(cancelTLVTypeReason), &reasonBytes),
		)
	}

	return encodeTLVStream(records)
}

func DecodeCancel(payload []byte) (Cancel, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return Cancel{}, err
	}

	env, err := decodeCallEnvelope(m)
	if err != nil {
		return Cancel{}, err
	}

	var reason *string
	if b, ok := m[cancelTLVTypeReason]; ok {
		s, readErr := readUTF8String(b, "reason")
		if readErr != nil {
			return Cancel{}, readErr
		}
		reason = &s
	}

	return Cancel{
		Envelope: env,
		Reason:   reason,
	}, nil
}
