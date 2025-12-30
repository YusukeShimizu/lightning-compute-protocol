//nolint:testpackage // White-box tests need access to unexported helpers.
package provider

import (
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/google/go-cmp/cmp"
)

func TestQuoteRequestValidator_UnsupportedProtocolVersion(t *testing.T) {
	t.Parallel()

	validator := defaultValidator()

	req := newLLMChatQuoteRequest("profile-a")
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

	req := newLLMChatQuoteRequest("profile-a", withTaskKind("image.generation"))

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
	manifest := manifestWithTemplates(lcpwire.TaskTemplate{
		TaskKind:      "llm.chat",
		LLMChatParams: &lcpwire.LLMChatParams{Profile: "profile-template"},
	})

	req := newLLMChatQuoteRequest("profile-request")

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
	manifest := manifestWithTemplates(lcpwire.TaskTemplate{
		TaskKind:      "llm.chat",
		LLMChatParams: &lcpwire.LLMChatParams{Profile: "profile-template"},
	})

	req := newLLMChatQuoteRequest(
		"profile-template",
		withLLMChatTemperature(900),
	)

	if err := validator.ValidateQuoteRequest(req, manifest); err != nil {
		t.Fatalf("validate: got error %v, want nil", err)
	}
}

func TestQuoteRequestValidator_TemplateRequiresExactParams(t *testing.T) {
	t.Parallel()

	validator := defaultValidator()
	manifest := manifestWithTemplates(lcpwire.TaskTemplate{
		TaskKind: "llm.chat",
		LLMChatParams: &lcpwire.LLMChatParams{
			Profile:          "profile-template",
			TemperatureMilli: ptrUint32(500),
		},
	})

	req := newLLMChatQuoteRequest(
		"profile-template",
		withLLMChatTemperature(600),
	)

	err := validator.ValidateQuoteRequest(req, manifest)
	if err == nil {
		t.Fatalf("expected validation error, got nil")
	}

	want := lcpwire.ErrorCodeUnsupportedTask
	if diff := cmp.Diff(want, err.Code); diff != "" {
		t.Fatalf("error code mismatch (-want +got):\n%s", diff)
	}
}

func TestQuoteRequestValidator_RejectsUnknownParams(t *testing.T) {
	t.Parallel()

	validator := defaultValidator()

	req := newLLMChatQuoteRequest(
		"profile-a",
		withLLMChatUnknownParam(99, []byte{0x01, 0x02}),
	)

	err := validator.ValidateQuoteRequest(req, nil)
	if err == nil {
		t.Fatalf("expected validation error, got nil")
	}

	want := lcpwire.ErrorCodeUnsupportedParams
	if diff := cmp.Diff(want, err.Code); diff != "" {
		t.Fatalf("error code mismatch (-want +got):\n%s", diff)
	}
}

func defaultValidator() QuoteRequestValidator {
	return QuoteRequestValidator{
		SupportedProtocolVersions: map[uint16]struct{}{
			lcpwire.ProtocolVersionV01: {},
		},
		SupportedTaskKinds: map[string]struct{}{
			"llm.chat": {},
		},
	}
}
