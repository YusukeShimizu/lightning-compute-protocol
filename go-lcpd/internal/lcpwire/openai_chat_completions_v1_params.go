//nolint:dupl // Chat Completions and Responses params intentionally mirror each other.
package lcpwire

import (
	"errors"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	openAIChatCompletionsV1ParamsTLVTypeModel = uint64(1)
)

func EncodeOpenAIChatCompletionsV1Params(p OpenAIChatCompletionsV1Params) ([]byte, error) {
	if p.Model == "" {
		return nil, errors.New("model is required")
	}
	if err := validateUTF8String(p.Model, "model"); err != nil {
		return nil, err
	}

	modelBytes := []byte(p.Model)
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(tlv.Type(openAIChatCompletionsV1ParamsTLVTypeModel), &modelBytes),
	}

	return encodeTLVStream(records)
}

func DecodeOpenAIChatCompletionsV1Params(payload []byte) (OpenAIChatCompletionsV1Params, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return OpenAIChatCompletionsV1Params{}, err
	}

	modelBytes, err := requireTLV(m, openAIChatCompletionsV1ParamsTLVTypeModel)
	if err != nil {
		return OpenAIChatCompletionsV1Params{}, err
	}
	model, err := readUTF8String(modelBytes, "model")
	if err != nil {
		return OpenAIChatCompletionsV1Params{}, err
	}
	if model == "" {
		return OpenAIChatCompletionsV1Params{}, errors.New("model is required")
	}

	unknown := make(map[uint64][]byte)
	for typ, val := range m {
		switch typ {
		case openAIChatCompletionsV1ParamsTLVTypeModel:
			continue
		default:
			unknown[typ] = cloneBytes(val)
		}
	}
	if len(unknown) == 0 {
		unknown = nil
	}

	return OpenAIChatCompletionsV1Params{
		Model:   model,
		Unknown: unknown,
	}, nil
}
