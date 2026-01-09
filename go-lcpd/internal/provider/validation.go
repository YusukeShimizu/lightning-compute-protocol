package provider

import (
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
)

const (
	taskKindOpenAIChatCompletionsV1 = "openai.chat_completions.v1"
	taskKindOpenAIResponsesV1       = "openai.responses.v1"
)

type QuoteRequestValidator struct {
	SupportedProtocolVersions map[uint16]struct{}
	SupportedMethods          map[string]struct{}
}

func DefaultValidator() QuoteRequestValidator {
	return QuoteRequestValidator{
		SupportedProtocolVersions: map[uint16]struct{}{
			lcpwire.ProtocolVersionV03: {},
		},
		SupportedMethods: map[string]struct{}{
			taskKindOpenAIChatCompletionsV1: {},
			taskKindOpenAIResponsesV1:       {},
		},
	}
}

type ValidationError struct {
	Code    lcpwire.ErrorCode
	Message string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s (%d)", e.Message, e.Code)
}

func (v QuoteRequestValidator) ValidateQuoteRequest(
	req lcpwire.Call,
	remoteManifest *lcpwire.Manifest,
) *ValidationError {
	if !v.protocolSupported(req.Envelope.ProtocolVersion) {
		return &ValidationError{
			Code:    lcpwire.ErrorCodeUnsupportedVersion,
			Message: fmt.Sprintf("unsupported protocol_version: %d", req.Envelope.ProtocolVersion),
		}
	}

	if !v.methodSupported(req.Method) {
		return &ValidationError{
			Code:    lcpwire.ErrorCodeUnsupportedMethod,
			Message: fmt.Sprintf("unsupported method: %q", req.Method),
		}
	}

	if err := validateTaskParams(req); err != nil {
		return err
	}

	if remoteManifest != nil && len(remoteManifest.SupportedMethods) > 0 {
		if !matchesSupportedMethod(req.Method, remoteManifest.SupportedMethods) {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedMethod,
				Message: "method does not match any supported_methods descriptor",
			}
		}
	}

	return nil
}

func (v QuoteRequestValidator) protocolSupported(protocolVersion uint16) bool {
	if len(v.SupportedProtocolVersions) == 0 {
		return protocolVersion == lcpwire.ProtocolVersionV03
	}
	_, ok := v.SupportedProtocolVersions[protocolVersion]
	return ok
}

func (v QuoteRequestValidator) methodSupported(method string) bool {
	if len(v.SupportedMethods) == 0 {
		return method == taskKindOpenAIChatCompletionsV1
	}
	_, ok := v.SupportedMethods[method]
	return ok
}

func validateTaskParams(req lcpwire.Call) *ValidationError {
	switch req.Method {
	case taskKindOpenAIChatCompletionsV1:
		if req.ParamsBytes == nil {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedMethod,
				Message: "openai.chat_completions.v1 params are required",
			}
		}

		params, err := lcpwire.DecodeOpenAIChatCompletionsV1Params(*req.ParamsBytes)
		if err != nil {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedMethod,
				Message: "openai.chat_completions.v1 params must be a valid TLV stream",
			}
		}

		if len(params.Unknown) > 0 {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedMethod,
				Message: "openai.chat_completions.v1 params contain unknown tlv types",
			}
		}

		return nil
	case taskKindOpenAIResponsesV1:
		if req.ParamsBytes == nil {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedMethod,
				Message: "openai.responses.v1 params are required",
			}
		}

		params, err := lcpwire.DecodeOpenAIResponsesV1Params(*req.ParamsBytes)
		if err != nil {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedMethod,
				Message: "openai.responses.v1 params must be a valid TLV stream",
			}
		}

		if len(params.Unknown) > 0 {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedMethod,
				Message: "openai.responses.v1 params contain unknown tlv types",
			}
		}

		return nil
	default:
		return nil
	}
}

func matchesSupportedMethod(method string, supported []lcpwire.MethodDescriptor) bool {
	for _, desc := range supported {
		if desc.Method == method {
			return true
		}
	}
	return false
}
