//nolint:testpackage // White-box tests need access to unexported helpers.
package provider

import (
	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/peerdirectory"
)

func newOpenAIChatCompletionsV1QuoteRequest(
	model string,
	opts ...func(*lcpwire.Call),
) lcpwire.Call {
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

	req := lcpwire.Call{
		Envelope: lcpwire.CallEnvelope{
			ProtocolVersion: lcpwire.ProtocolVersionV02,
			CallID:          jobID,
			MsgID:           msgID,
			Expiry:          1234,
		},
		Method:      "openai.chat_completions.v1",
		ParamsBytes: &encoded,
	}

	for _, opt := range opts {
		opt(&req)
	}
	return req
}

func withTaskKind(taskKind string) func(*lcpwire.Call) {
	return func(req *lcpwire.Call) {
		req.Method = taskKind
		req.ParamsBytes = nil
	}
}

func manifestWithTemplates(templates ...lcpwire.MethodDescriptor) *lcpwire.Manifest {
	m := &lcpwire.Manifest{
		ProtocolVersion:  lcpwire.ProtocolVersionV02,
		MaxPayloadBytes:  65535,
		MaxStreamBytes:   4_194_304,
		MaxCallBytes:     8_388_608,
		SupportedMethods: templates,
	}
	return m
}

func mustEncodeOpenAIChatCompletionsV1Params(p lcpwire.OpenAIChatCompletionsV1Params) []byte {
	p.Unknown = nil
	encoded, err := lcpwire.EncodeOpenAIChatCompletionsV1Params(p)
	if err != nil {
		panic(err)
	}
	return encoded
}

func manifestPeerDirectory() *peerdirectory.Directory {
	peers := peerdirectory.New()
	peers.MarkLCPReady("peer1", lcpwire.Manifest{
		ProtocolVersion: lcpwire.ProtocolVersionV02,
		MaxPayloadBytes: 65535,
		MaxStreamBytes:  4_194_304,
		MaxCallBytes:    8_388_608,
	})
	return peers
}
