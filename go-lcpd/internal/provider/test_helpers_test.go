//nolint:testpackage // White-box tests need access to unexported helpers.
package provider

import (
	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/peerdirectory"
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
			ProtocolVersion: lcpwire.ProtocolVersionV02,
			JobID:           jobID,
			MsgID:           msgID,
			Expiry:          1234,
		},
		TaskKind:      "llm.chat",
		ParamsBytes:   &encoded,
		LLMChatParams: &params,
	}

	for _, opt := range opts {
		opt(&req)
	}
	return req
}

func newOpenAIChatCompletionsV1QuoteRequest(
	model string,
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

	params := lcpwire.OpenAIChatCompletionsV1Params{
		Model: model,
	}
	encoded := mustEncodeOpenAIChatCompletionsV1Params(params)

	req := lcpwire.QuoteRequest{
		Envelope: lcpwire.JobEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV02,
			JobID:           jobID,
			MsgID:           msgID,
			Expiry:          1234,
		},
		TaskKind:    "openai.chat_completions.v1",
		ParamsBytes: &encoded,
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
		ProtocolVersion: lcpwire.ProtocolVersionV02,
		MaxPayloadBytes: 65535,
		MaxStreamBytes:  4_194_304,
		MaxJobBytes:     8_388_608,
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

func mustEncodeOpenAIChatCompletionsV1Params(p lcpwire.OpenAIChatCompletionsV1Params) []byte {
	p.Unknown = nil
	encoded, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(p)
	if err != nil {
		panic(err)
	}
	return encoded
}

func ptrUint32(v uint32) *uint32 {
	return &v
}

func manifestPeerDirectory() *peerdirectory.Directory {
	peers := peerdirectory.New()
	peers.MarkLCPReady("peer1", lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV02,
		MaxPayloadBytes: 65535,
		MaxStreamBytes:  4_194_304,
		MaxJobBytes:     8_388_608,
	})
	return peers
}
