package lcpwire_test

import (
	"bytes"
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/google/go-cmp/cmp"
	"github.com/lightningnetwork/lnd/tlv"
)

func TestOpenAIResponsesV1Params_RoundTrip(t *testing.T) {
	t.Parallel()

	params := lcpwire.OpenAIResponsesV1Params{Model: "gpt-5.2"}

	encoded, err := lcpwire.EncodeOpenAIResponsesV1Params(params)
	if err != nil {
		t.Fatalf("EncodeOpenAIResponsesV1Params: %v", err)
	}

	decoded, err := lcpwire.DecodeOpenAIResponsesV1Params(encoded)
	if err != nil {
		t.Fatalf("DecodeOpenAIResponsesV1Params: %v", err)
	}

	if diff := cmp.Diff(params, decoded); diff != "" {
		t.Fatalf("params mismatch (-want +got):\n%s", diff)
	}
}

func TestDecodeOpenAIResponsesV1Params_PreservesUnknown(t *testing.T) {
	t.Parallel()

	modelBytes := []byte("gpt-5.2")
	unknownBytes := []byte{0x01, 0x02}

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(tlv.Type(1), &modelBytes),
		tlv.MakePrimitiveRecord(tlv.Type(99), &unknownBytes),
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	var buf bytes.Buffer
	if encodeErr := stream.Encode(&buf); encodeErr != nil {
		t.Fatalf("Encode: %v", encodeErr)
	}

	decoded, err := lcpwire.DecodeOpenAIResponsesV1Params(buf.Bytes())
	if err != nil {
		t.Fatalf("DecodeOpenAIResponsesV1Params: %v", err)
	}

	want := lcpwire.OpenAIResponsesV1Params{
		Model: "gpt-5.2",
		Unknown: map[uint64][]byte{
			99: []byte{0x01, 0x02},
		},
	}
	if diff := cmp.Diff(want, decoded); diff != "" {
		t.Fatalf("params mismatch (-want +got):\n%s", diff)
	}
}
