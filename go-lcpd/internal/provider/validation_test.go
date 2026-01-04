//nolint:testpackage // White-box tests need access to unexported helpers.
package provider

import (
	"bytes"
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/google/go-cmp/cmp"
	"github.com/lightningnetwork/lnd/tlv"
)

func TestQuoteRequestValidator_UnsupportedProtocolVersion(t *testing.T) {
	t.Parallel()

	validator := defaultValidator()

	req := newOpenAIChatCompletionsV1QuoteRequest("model-a")
	req.Envelope.ProtocolVersion = 999

	err := validator.ValidateQuoteRequest(req, nil)
	if err == nil {
		t.Fatalf("expected validation error, got nil")
	}

	want := lcpwire.ErrorCodeUnsupportedVersion
	if diff := cmp.Diff(want, err.Code); diff != "" {
		t.Fatalf("error code mismatch (-want +got):\n%s", diff)
	}
}

func TestQuoteRequestValidator_UnsupportedTaskKind(t *testing.T) {
	t.Parallel()

	validator := defaultValidator()

	req := newOpenAIChatCompletionsV1QuoteRequest("model-a", withTaskKind("image.generation"))

	err := validator.ValidateQuoteRequest(req, nil)
	if err == nil {
		t.Fatalf("expected validation error, got nil")
	}

	want := lcpwire.ErrorCodeUnsupportedTask
	if diff := cmp.Diff(want, err.Code); diff != "" {
		t.Fatalf("error code mismatch (-want +got):\n%s", diff)
	}
}

func TestQuoteRequestValidator_SupportedTasksTemplateMismatch(t *testing.T) {
	t.Parallel()

	validator := defaultValidator()
	encoded := mustEncodeOpenAIChatCompletionsV1Params(
		lcpwire.OpenAIChatCompletionsV1Params{Model: "model-template"},
	)
	manifest := manifestWithTemplates(lcpwire.TaskTemplate{
		TaskKind:    taskKindOpenAIChatCompletionsV1,
		ParamsBytes: &encoded,
	})

	req := newOpenAIChatCompletionsV1QuoteRequest("model-request")

	err := validator.ValidateQuoteRequest(req, manifest)
	if err == nil {
		t.Fatalf("expected validation error, got nil")
	}

	want := lcpwire.ErrorCodeUnsupportedTask
	if diff := cmp.Diff(want, err.Code); diff != "" {
		t.Fatalf("error code mismatch (-want +got):\n%s", diff)
	}
}

func TestQuoteRequestValidator_SupportedTasksTemplateMatch(t *testing.T) {
	t.Parallel()

	validator := defaultValidator()
	encoded := mustEncodeOpenAIChatCompletionsV1Params(
		lcpwire.OpenAIChatCompletionsV1Params{Model: "model-template"},
	)
	manifest := manifestWithTemplates(lcpwire.TaskTemplate{
		TaskKind:    taskKindOpenAIChatCompletionsV1,
		ParamsBytes: &encoded,
	})

	req := newOpenAIChatCompletionsV1QuoteRequest("model-template")

	if err := validator.ValidateQuoteRequest(req, manifest); err != nil {
		t.Fatalf("validate: got error %v, want nil", err)
	}
}

func TestQuoteRequestValidator_OpenAIChatCompletionsV1_AcceptsValidParams(t *testing.T) {
	t.Parallel()

	validator := QuoteRequestValidator{
		SupportedProtocolVersions: map[uint16]struct{}{
			lcpwire.ProtocolVersionV02: {},
		},
		SupportedTaskKinds: map[string]struct{}{
			taskKindOpenAIChatCompletionsV1: {},
		},
	}

	req := newOpenAIChatCompletionsV1QuoteRequest("model-x")

	if err := validator.ValidateQuoteRequest(req, nil); err != nil {
		t.Fatalf("validate: got error %v, want nil", err)
	}
}

func TestQuoteRequestValidator_OpenAIChatCompletionsV1_RejectsMissingParams(t *testing.T) {
	t.Parallel()

	validator := QuoteRequestValidator{
		SupportedProtocolVersions: map[uint16]struct{}{
			lcpwire.ProtocolVersionV02: {},
		},
		SupportedTaskKinds: map[string]struct{}{
			taskKindOpenAIChatCompletionsV1: {},
		},
	}

	req := newOpenAIChatCompletionsV1QuoteRequest("gpt-5.2", func(r *lcpwire.QuoteRequest) {
		r.ParamsBytes = nil
	})

	err := validator.ValidateQuoteRequest(req, nil)
	if err == nil {
		t.Fatalf("expected validation error, got nil")
	}

	want := lcpwire.ErrorCodeUnsupportedParams
	if diff := cmp.Diff(want, err.Code); diff != "" {
		t.Fatalf("error code mismatch (-want +got):\n%s", diff)
	}
}

func TestQuoteRequestValidator_OpenAIChatCompletionsV1_RejectsUnknownParams(t *testing.T) {
	t.Parallel()

	validator := QuoteRequestValidator{
		SupportedProtocolVersions: map[uint16]struct{}{
			lcpwire.ProtocolVersionV02: {},
		},
		SupportedTaskKinds: map[string]struct{}{
			taskKindOpenAIChatCompletionsV1: {},
		},
	}

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
	encoded := buf.Bytes()

	req := newOpenAIChatCompletionsV1QuoteRequest("gpt-5.2", func(r *lcpwire.QuoteRequest) {
		r.ParamsBytes = &encoded
	})

	vErr := validator.ValidateQuoteRequest(req, nil)
	if vErr == nil {
		t.Fatalf("expected validation error, got nil")
	}

	want := lcpwire.ErrorCodeUnsupportedParams
	if diff := cmp.Diff(want, vErr.Code); diff != "" {
		t.Fatalf("error code mismatch (-want +got):\n%s", diff)
	}
}

func defaultValidator() QuoteRequestValidator {
	return QuoteRequestValidator{
		SupportedProtocolVersions: map[uint16]struct{}{
			lcpwire.ProtocolVersionV02: {},
		},
		SupportedTaskKinds: map[string]struct{}{
			taskKindOpenAIChatCompletionsV1: {},
		},
	}
}
