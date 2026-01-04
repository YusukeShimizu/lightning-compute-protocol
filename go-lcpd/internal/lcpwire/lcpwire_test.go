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
		maxStream  uint64 = 4_194_304
		maxJob     uint64 = 8_388_608
	)

	openaiParams := lcpwire.OpenAIChatCompletionsV1Params{Model: "test-model"}
	openaiParamsBytes, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(openaiParams)
	if err != nil {
		t.Fatalf("EncodeOpenAIChatCompletionsV1Params: %v", err)
	}

	customParams := []byte{0x01, 0x02, 0x03}

	want := lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV02,
		MaxPayloadBytes: maxPayload,
		MaxStreamBytes:  maxStream,
		MaxJobBytes:     maxJob,
		SupportedTasks: []lcpwire.TaskTemplate{
			{
				TaskKind:    "openai.chat_completions.v1",
				ParamsBytes: &openaiParamsBytes,
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

func TestEncodeDecode_QuoteRequest_OpenAIChatCompletionsV1_RoundTrip(t *testing.T) {
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
		expiry uint64 = 1234
	)

	openaiParams := lcpwire.OpenAIChatCompletionsV1Params{Model: "model-a"}
	openaiParamsBytes, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(openaiParams)
	if err != nil {
		t.Fatalf("EncodeOpenAIChatCompletionsV1Params: %v", err)
	}

	want := lcpwire.QuoteRequest{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV02,
			JobID:           jobID,
			MsgID:           msgID,
			Expiry:          expiry,
		},
		TaskKind:    "openai.chat_completions.v1",
		ParamsBytes: &openaiParamsBytes,
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

	pv := lcpwire.ProtocolVersionV02
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
	if _, err := buf.Write([]byte{0x00, byte(lcpwire.ProtocolVersionV02)}); err != nil {
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
