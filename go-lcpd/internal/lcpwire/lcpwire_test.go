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
		maxCall    uint64 = 8_388_608
	)

	want := lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV03,
		MaxPayloadBytes: maxPayload,
		MaxStreamBytes:  maxStream,
		MaxCallBytes:    maxCall,
		SupportedMethods: []lcpwire.MethodDescriptor{
			{
				Method:              "openai.chat_completions.v1",
				RequestContentTypes: []string{"application/json; charset=utf-8"},
			},
			{
				Method: "custom.method",
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
		callID lcp.JobID
		msgID  lcpwire.MsgID
	)
	for i := range callID {
		callID[i] = 0x11
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

	want := lcpwire.Call{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV03,
			CallID:          callID,
			MsgID:           msgID,
			Expiry:          expiry,
		},
		Method:      "openai.chat_completions.v1",
		ParamsBytes: &openaiParamsBytes,
	}

	encoded, err := lcpwire.EncodeCall(want)
	if err != nil {
		t.Fatalf("EncodeCall: %v", err)
	}

	got, err := lcpwire.DecodeCall(encoded)
	if err != nil {
		t.Fatalf("DecodeCall: %v", err)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("call mismatch (-want +got):\n%s", diff)
	}
}

func TestDecodeManifest_RejectsJobEnvelopeTLVs(t *testing.T) {
	t.Parallel()

	pv := lcpwire.ProtocolVersionV03
	var callID [32]byte
	for i := range callID {
		callID[i] = 0x11
	}

	stream := tlv.MustNewStream(
		tlv.MakePrimitiveRecord(1, &pv),
		tlv.MakePrimitiveRecord(2, &callID),
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
	if _, err := buf.Write([]byte{0x00, byte(lcpwire.ProtocolVersionV03)}); err != nil {
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
