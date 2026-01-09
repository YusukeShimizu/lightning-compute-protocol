package lcpwire

import (
	"errors"
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/lightningnetwork/lnd/tlv"
)

func decodeCallEnvelope(m map[uint64][]byte) (CallEnvelope, error) {
	pvBytes, err := requireTLV(m, tlvTypeProtocolVersion)
	if err != nil {
		return CallEnvelope{}, err
	}
	pv, err := readU16(pvBytes)
	if err != nil {
		return CallEnvelope{}, fmt.Errorf("protocol_version: %w", err)
	}

	callIDBytes, err := requireTLV(m, tlvTypeCallID)
	if err != nil {
		return CallEnvelope{}, err
	}
	callID32, err := readBytes32(callIDBytes)
	if err != nil {
		return CallEnvelope{}, fmt.Errorf("call_id: %w", err)
	}
	callID := lcp.JobID(callID32)

	msgIDBytes, err := requireTLV(m, tlvTypeMsgID)
	if err != nil {
		return CallEnvelope{}, err
	}
	msgID32, err := readBytes32(msgIDBytes)
	if err != nil {
		return CallEnvelope{}, fmt.Errorf("msg_id: %w", err)
	}
	var msgID MsgID
	copy(msgID[:], msgID32[:])

	expiryBytes, err := requireTLV(m, tlvTypeExpiry)
	if err != nil {
		return CallEnvelope{}, err
	}
	expiry, err := readTU64(expiryBytes)
	if err != nil {
		return CallEnvelope{}, fmt.Errorf("expiry: %w", err)
	}

	return CallEnvelope{
		ProtocolVersion: pv,
		CallID:          callID,
		MsgID:           msgID,
		Expiry:          expiry,
	}, nil
}

func encodeCallEnvelope(env CallEnvelope) ([]tlv.Record, error) {
	if env.ProtocolVersion == 0 {
		return nil, errors.New("protocol_version is required")
	}

	pv := env.ProtocolVersion
	callID := [32]byte(env.CallID)
	msgID := [32]byte(env.MsgID)
	expiry := env.Expiry

	return []tlv.Record{
		tlv.MakePrimitiveRecord(tlv.Type(tlvTypeProtocolVersion), &pv),
		tlv.MakePrimitiveRecord(tlv.Type(tlvTypeCallID), &callID),
		tlv.MakePrimitiveRecord(tlv.Type(tlvTypeMsgID), &msgID),
		makeTU64Record(tlvTypeExpiry, &expiry),
	}, nil
}

func decodeProtocolVersionOnly(m map[uint64][]byte) (uint16, error) {
	pvBytes, err := requireTLV(m, tlvTypeProtocolVersion)
	if err != nil {
		return 0, err
	}
	pv, err := readU16(pvBytes)
	if err != nil {
		return 0, fmt.Errorf("protocol_version: %w", err)
	}
	return pv, nil
}
