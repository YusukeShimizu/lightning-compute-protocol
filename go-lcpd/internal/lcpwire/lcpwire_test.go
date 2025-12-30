package lcpwire_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/google/go-cmp/cmp"
	"github.com/lightningnetwork/lnd/tlv"
)

func TestEncodeDecode_Manifest_RoundTrip(t *testing.T) {
	t.Parallel()

	var (
		maxPayload uint32 = 16384
		tempMilli  uint32 = 700
		maxTokens  uint32 = 256
	)

	llmParams := lcpwire.LLMChatParams{
		Profile:          "test-profile",
		TemperatureMilli: &tempMilli,
		MaxOutputTokens:  &maxTokens,
	}
	llmParamsBytes, err := lcpwire.EncodeLLMChatParams(llmParams)
	if err != nil {
		t.Fatalf("EncodeLLMChatParams: %v", err)
	}

	customParams := []byte{0x01, 0x02, 0x03}

	want := lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV01,
		MaxPayloadBytes: &maxPayload,
		SupportedTasks: []lcpwire.TaskTemplate{
			{
				TaskKind:      "llm.chat",
				ParamsBytes:   &llmParamsBytes,
				LLMChatParams: &llmParams,
			},
			{
				TaskKind:    "custom.task",
				ParamsBytes: &customParams,
			},
		},
	}

	encoded, err := lcpwire.EncodeManifest(want)
	if err != nil {
		t.Fatalf("EncodeManifest: %v", err)
	}

	got, err := lcpwire.DecodeManifest(encoded)
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("manifest mismatch (-want +got):\n%s", diff)
	}
}

func TestEncodeDecode_QuoteRequest_LLMChat_RoundTrip(t *testing.T) {
	t.Parallel()

	var (
		jobID lcp.JobID
		msgID lcpwire.MsgID
	)
	for i := range jobID {
		jobID[i] = 0x11
	}
	for i := range msgID {
		msgID[i] = 0x22
	}

	var (
		expiry       uint64 = 1234
		tempMilli    uint32 = 800
		maxOutTokens uint32 = 64
	)

	llmParams := lcpwire.LLMChatParams{
		Profile:          "profile-a",
		TemperatureMilli: &tempMilli,
		MaxOutputTokens:  &maxOutTokens,
	}
	llmParamsBytes, err := lcpwire.EncodeLLMChatParams(llmParams)
	if err != nil {
		t.Fatalf("EncodeLLMChatParams: %v", err)
	}

	want := lcpwire.QuoteRequest{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV01,
			JobID:           jobID,
			MsgID:           msgID,
			Expiry:          expiry,
		},
		TaskKind:      "llm.chat",
		Input:         []byte("hello"),
		ParamsBytes:   &llmParamsBytes,
		LLMChatParams: &llmParams,
	}

	encoded, err := lcpwire.EncodeQuoteRequest(want)
	if err != nil {
		t.Fatalf("EncodeQuoteRequest: %v", err)
	}

	got, err := lcpwire.DecodeQuoteRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeQuoteRequest: %v", err)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("quote_request mismatch (-want +got):\n%s", diff)
	}
}

func TestDecodeManifest_RejectsJobEnvelopeTLVs(t *testing.T) {
	t.Parallel()

	pv := lcpwire.ProtocolVersionV01
	var jobID [32]byte
	for i := range jobID {
		jobID[i] = 0x11
	}

	stream := tlv.MustNewStream(
		tlv.MakePrimitiveRecord(1, &pv),
		tlv.MakePrimitiveRecord(2, &jobID),
	)

	var b bytes.Buffer
	if err := stream.Encode(&b); err != nil {
		t.Fatalf("encode: %v", err)
	}

	if _, err := lcpwire.DecodeManifest(b.Bytes()); err == nil {
		t.Fatalf("DecodeManifest: expected error")
	}
}

func TestDecodeManifest_ErrStreamNotCanonical(t *testing.T) {
	t.Parallel()

	var (
		buf     bytes.Buffer
		scratch [8]byte
	)

	// Write TLV type 11 (max_payload_bytes) first, then type 1, to violate
	// canonical ordering.
	if err := tlv.WriteVarInt(&buf, 11, &scratch); err != nil {
		t.Fatalf("write type 11: %v", err)
	}
	if err := tlv.WriteVarInt(&buf, 0, &scratch); err != nil {
		t.Fatalf("write len 0: %v", err)
	}

	if err := tlv.WriteVarInt(&buf, 1, &scratch); err != nil {
		t.Fatalf("write type 1: %v", err)
	}
	if err := tlv.WriteVarInt(&buf, 2, &scratch); err != nil {
		t.Fatalf("write len 2: %v", err)
	}
	if _, err := buf.Write([]byte{0x00, byte(lcpwire.ProtocolVersionV01)}); err != nil {
		t.Fatalf("write protocol_version: %v", err)
	}

	_, err := lcpwire.DecodeManifest(buf.Bytes())
	if err == nil {
		t.Fatalf("DecodeManifest: expected error")
	}
	if !errors.Is(err, tlv.ErrStreamNotCanonical) {
		t.Fatalf(
			"DecodeManifest error mismatch (-want +got):\n%s",
			cmp.Diff(tlv.ErrStreamNotCanonical, err),
		)
	}
}

func TestDecodeQuoteRequest_LLMChat_MissingParams(t *testing.T) {
	t.Parallel()

	pv := lcpwire.ProtocolVersionV01
	var jobID [32]byte
	var msgID [32]byte
	for i := range jobID {
		jobID[i] = 0x11
	}
	for i := range msgID {
		msgID[i] = 0x22
	}

	expiry := uint64(1)
	sizeExpiry := func() uint64 { return tlv.SizeTUint64(expiry) }

	taskKindBytes := []byte("llm.chat")
	inputBytes := []byte("hi")

	stream := tlv.MustNewStream(
		tlv.MakePrimitiveRecord(1, &pv),
		tlv.MakePrimitiveRecord(2, &jobID),
		tlv.MakePrimitiveRecord(3, &msgID),
		tlv.MakeDynamicRecord(4, &expiry, sizeExpiry, tlv.ETUint64, tlv.DTUint64),
		tlv.MakePrimitiveRecord(20, &taskKindBytes),
		tlv.MakePrimitiveRecord(21, &inputBytes),
	)

	var b bytes.Buffer
	if err := stream.Encode(&b); err != nil {
		t.Fatalf("encode: %v", err)
	}

	if _, err := lcpwire.DecodeQuoteRequest(b.Bytes()); err == nil {
		t.Fatalf("DecodeQuoteRequest: expected error")
	}
}

func TestDecodeLLMChatParams_CollectsUnknownTLVs(t *testing.T) {
	t.Parallel()

	profileBytes := []byte("p")
	unknownValue := []byte{0x01}

	stream := tlv.MustNewStream(
		tlv.MakePrimitiveRecord(1, &profileBytes),
		tlv.MakePrimitiveRecord(9, &unknownValue),
	)

	var b bytes.Buffer
	if err := stream.Encode(&b); err != nil {
		t.Fatalf("encode: %v", err)
	}

	got, err := lcpwire.DecodeLLMChatParams(b.Bytes())
	if err != nil {
		t.Fatalf("DecodeLLMChatParams: %v", err)
	}

	wantUnknown := map[uint64][]byte{9: unknownValue}
	if diff := cmp.Diff(wantUnknown, got.Unknown); diff != "" {
		t.Fatalf("unknown tlvs mismatch (-want +got):\n%s", diff)
	}
}
