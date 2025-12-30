package lcpwire

import (
	"errors"
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/lightningnetwork/lnd/tlv"
)

func decodeJobEnvelope(m map[uint64][]byte) (JobEnvelope, error) {
	pvBytes, err := requireTLV(m, tlvTypeProtocolVersion)
	if err != nil {
		return JobEnvelope{}, err
	}
	pv, err := readU16(pvBytes)
	if err != nil {
		return JobEnvelope{}, fmt.Errorf("protocol_version: %w", err)
	}

	jobIDBytes, err := requireTLV(m, tlvTypeJobID)
	if err != nil {
		return JobEnvelope{}, err
	}
	jobID32, err := readBytes32(jobIDBytes)
	if err != nil {
		return JobEnvelope{}, fmt.Errorf("job_id: %w", err)
	}
	jobID := lcp.JobID(jobID32)

	msgIDBytes, err := requireTLV(m, tlvTypeMsgID)
	if err != nil {
		return JobEnvelope{}, err
	}
	msgID32, err := readBytes32(msgIDBytes)
	if err != nil {
		return JobEnvelope{}, fmt.Errorf("msg_id: %w", err)
	}
	var msgID MsgID
	copy(msgID[:], msgID32[:])

	expiryBytes, err := requireTLV(m, tlvTypeExpiry)
	if err != nil {
		return JobEnvelope{}, err
	}
	expiry, err := readTU64(expiryBytes)
	if err != nil {
		return JobEnvelope{}, fmt.Errorf("expiry: %w", err)
	}

	return JobEnvelope{
		ProtocolVersion: pv,
		JobID:           jobID,
		MsgID:           msgID,
		Expiry:          expiry,
	}, nil
}

func encodeJobEnvelope(env JobEnvelope) ([]tlv.Record, error) {
	if env.ProtocolVersion == 0 {
		return nil, errors.New("protocol_version is required")
	}

	pv := env.ProtocolVersion
	jobID := [32]byte(env.JobID)
	msgID := [32]byte(env.MsgID)
	expiry := env.Expiry

	return []tlv.Record{
		tlv.MakePrimitiveRecord(tlv.Type(tlvTypeProtocolVersion), &pv),
		tlv.MakePrimitiveRecord(tlv.Type(tlvTypeJobID), &jobID),
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
