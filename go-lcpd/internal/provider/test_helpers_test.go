//nolint:testpackage // White-box tests need access to unexported helpers.
package provider

import (
	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
)

func newLLMChatQuoteRequest(
	profile string,
	opts ...func(*lcpwire.QuoteRequest),
) lcpwire.QuoteRequest {
	var jobID lcp.JobID
	for i := range jobID {
		jobID[i] = 0x01
	}
	var msgID lcpwire.MsgID
	for i := range msgID {
		msgID[i] = 0x02
	}

	params := lcpwire.LLMChatParams{
		Profile: profile,
	}
	encoded := mustEncodeLLMChatParams(params)

	req := lcpwire.QuoteRequest{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV01,
			JobID:           jobID,
			MsgID:           msgID,
			Expiry:          1234,
		},
		TaskKind:      "llm.chat",
		Input:         []byte("prompt"),
		ParamsBytes:   &encoded,
		LLMChatParams: &params,
	}

	for _, opt := range opts {
		opt(&req)
	}
	return req
}

func withTaskKind(taskKind string) func(*lcpwire.QuoteRequest) {
	return func(req *lcpwire.QuoteRequest) {
		req.TaskKind = taskKind
		req.LLMChatParams = nil
		req.ParamsBytes = nil
	}
}

func withLLMChatTemperature(temp uint32) func(*lcpwire.QuoteRequest) {
	return func(req *lcpwire.QuoteRequest) {
		params := *req.LLMChatParams
		params.TemperatureMilli = ptrUint32(temp)
		req.LLMChatParams = &params

		encoded := mustEncodeLLMChatParams(params)
		req.ParamsBytes = &encoded
	}
}

func withLLMChatUnknownParam(typ uint64, value []byte) func(*lcpwire.QuoteRequest) {
	return func(req *lcpwire.QuoteRequest) {
		params := *req.LLMChatParams
		if params.Unknown == nil {
			params.Unknown = make(map[uint64][]byte)
		}
		params.Unknown[typ] = append([]byte(nil), value...)
		req.LLMChatParams = &params

		encoded := mustEncodeLLMChatParams(params)
		req.ParamsBytes = &encoded
	}
}

func manifestWithTemplates(templates ...lcpwire.TaskTemplate) *lcpwire.Manifest {
	m := &lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV01,
		SupportedTasks:  templates,
	}
	return m
}

func mustEncodeLLMChatParams(p lcpwire.LLMChatParams) []byte {
	p.Unknown = nil
	encoded, err := lcpwire.EncodeLLMChatParams(p)
	if err != nil {
		panic(err)
	}
	return encoded
}

func ptrUint32(v uint32) *uint32 {
	return &v
}
