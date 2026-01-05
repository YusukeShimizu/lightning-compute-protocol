package lcpwire

import (
	"errors"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	openAIResponsesV1ParamsTLVTypeModel = uint64(1)
)

func EncodeOpenAIResponsesV1Params(p OpenAIResponsesV1Params) ([]byte, error) {
	if p.Model == "" {
		return nil, errors.New("model is required")
	}
	if err := validateUTF8String(p.Model, "model"); err != nil {
		return nil, err
	}

	modelBytes := []byte(p.Model)
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(tlv.Type(openAIResponsesV1ParamsTLVTypeModel), &modelBytes),
	}

	return encodeTLVStream(records)
}

func DecodeOpenAIResponsesV1Params(payload []byte) (OpenAIResponsesV1Params, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return OpenAIResponsesV1Params{}, err
	}

	modelBytes, err := requireTLV(m, openAIResponsesV1ParamsTLVTypeModel)
	if err != nil {
		return OpenAIResponsesV1Params{}, err
	}
	model, err := readUTF8String(modelBytes, "model")
	if err != nil {
		return OpenAIResponsesV1Params{}, err
	}
	if model == "" {
		return OpenAIResponsesV1Params{}, errors.New("model is required")
	}

	unknown := make(map[uint64][]byte)
	for typ, val := range m {
		switch typ {
		case openAIResponsesV1ParamsTLVTypeModel:
			continue
		default:
			unknown[typ] = cloneBytes(val)
		}
	}
	if len(unknown) == 0 {
		unknown = nil
	}

	return OpenAIResponsesV1Params{
		Model:   model,
		Unknown: unknown,
	}, nil
}

