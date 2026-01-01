package provider

import (
	"bytes"
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
)

const taskKindLLMChat = "llm.chat"

type QuoteRequestValidator struct {
	SupportedProtocolVersions map[uint16]struct{}
	SupportedTaskKinds        map[string]struct{}
}

func DefaultValidator() QuoteRequestValidator {
	return QuoteRequestValidator{
		SupportedProtocolVersions: map[uint16]struct{}{
			lcpwire.ProtocolVersionV02: {},
		},
		SupportedTaskKinds: map[string]struct{}{
			taskKindLLMChat: {},
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
	req lcpwire.QuoteRequest,
	remoteManifest *lcpwire.Manifest,
) *ValidationError {
	if !v.protocolSupported(req.Envelope.ProtocolVersion) {
		return &ValidationError{
			Code:    lcpwire.ErrorCodeUnsupportedVersion,
			Message: fmt.Sprintf("unsupported protocol_version: %d", req.Envelope.ProtocolVersion),
		}
	}

	if !v.taskKindSupported(req.TaskKind) {
		return &ValidationError{
			Code:    lcpwire.ErrorCodeUnsupportedTask,
			Message: fmt.Sprintf("unsupported task_kind: %q", req.TaskKind),
		}
	}

	if err := validateTaskParams(req); err != nil {
		return err
	}

	if remoteManifest != nil && len(remoteManifest.SupportedTasks) > 0 {
		if !matchesSupportedTemplate(req, remoteManifest.SupportedTasks) {
			return &ValidationError{
				Code:    lcpwire.ErrorCodeUnsupportedTask,
				Message: "task does not match any supported_tasks template",
			}
		}
	}

	return nil
}

func (v QuoteRequestValidator) protocolSupported(protocolVersion uint16) bool {
	if len(v.SupportedProtocolVersions) == 0 {
		return protocolVersion == lcpwire.ProtocolVersionV02
	}
	_, ok := v.SupportedProtocolVersions[protocolVersion]
	return ok
}

func (v QuoteRequestValidator) taskKindSupported(taskKind string) bool {
	if len(v.SupportedTaskKinds) == 0 {
		return taskKind == taskKindLLMChat
	}
	_, ok := v.SupportedTaskKinds[taskKind]
	return ok
}

func validateTaskParams(req lcpwire.QuoteRequest) *ValidationError {
	if req.TaskKind != taskKindLLMChat {
		return nil
	}

	if req.LLMChatParams == nil {
		return &ValidationError{
			Code:    lcpwire.ErrorCodeUnsupportedParams,
			Message: "llm.chat params are required",
		}
	}

	if len(req.LLMChatParams.Unknown) > 0 {
		return &ValidationError{
			Code:    lcpwire.ErrorCodeUnsupportedParams,
			Message: "llm.chat params contain unknown tlv types",
		}
	}

	return nil
}

func matchesSupportedTemplate(
	req lcpwire.QuoteRequest,
	templates []lcpwire.TaskTemplate,
) bool {
	for _, tmpl := range templates {
		if matchesTemplate(req, tmpl) {
			return true
		}
	}
	return false
}

func matchesTemplate(req lcpwire.QuoteRequest, tmpl lcpwire.TaskTemplate) bool {
	if req.TaskKind != tmpl.TaskKind {
		return false
	}

	if req.TaskKind == taskKindLLMChat {
		if tmpl.LLMChatParams == nil || req.LLMChatParams == nil {
			return false
		}
		return llmChatParamsInclude(*req.LLMChatParams, *tmpl.LLMChatParams)
	}

	if tmpl.ParamsBytes != nil {
		if req.ParamsBytes == nil {
			return false
		}
		return bytes.Equal(*req.ParamsBytes, *tmpl.ParamsBytes)
	}

	return true
}

func llmChatParamsInclude(req lcpwire.LLMChatParams, tmpl lcpwire.LLMChatParams) bool {
	if tmpl.Profile != "" && req.Profile != tmpl.Profile {
		return false
	}
	if tmpl.TemperatureMilli != nil {
		if req.TemperatureMilli == nil || *req.TemperatureMilli != *tmpl.TemperatureMilli {
			return false
		}
	}
	if tmpl.MaxOutputTokens != nil {
		if req.MaxOutputTokens == nil || *req.MaxOutputTokens != *tmpl.MaxOutputTokens {
			return false
		}
	}
	for typ, val := range tmpl.Unknown {
		reqVal, ok := req.Unknown[typ]
		if !ok {
			return false
		}
		if !bytes.Equal(reqVal, val) {
			return false
		}
	}
	return true
}
