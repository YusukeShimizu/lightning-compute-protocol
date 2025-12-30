package lcpwire

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	llmChatParamsTLVTypeProfile          = uint64(1)
	llmChatParamsTLVTypeTemperatureMilli = uint64(2)
	llmChatParamsTLVTypeMaxOutputTokens  = uint64(3)
)

func EncodeLLMChatParams(p LLMChatParams) ([]byte, error) {
	if p.Profile == "" {
		return nil, errors.New("profile is required")
	}
	if err := validateUTF8String(p.Profile, "profile"); err != nil {
		return nil, err
	}

	profileBytes := []byte(p.Profile)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(tlv.Type(llmChatParamsTLVTypeProfile), &profileBytes),
	}

	if p.TemperatureMilli != nil {
		temperatureMilli := *p.TemperatureMilli
		records = append(
			records,
			makeTU32Record(llmChatParamsTLVTypeTemperatureMilli, &temperatureMilli),
		)
	}
	if p.MaxOutputTokens != nil {
		maxOutputTokens := *p.MaxOutputTokens
		records = append(
			records,
			makeTU32Record(llmChatParamsTLVTypeMaxOutputTokens, &maxOutputTokens),
		)
	}

	return encodeTLVStream(records)
}

func DecodeLLMChatParams(payload []byte) (LLMChatParams, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return LLMChatParams{}, err
	}

	profileBytes, err := requireTLV(m, llmChatParamsTLVTypeProfile)
	if err != nil {
		return LLMChatParams{}, err
	}
	profile, err := readUTF8String(profileBytes, "profile")
	if err != nil {
		return LLMChatParams{}, err
	}
	if profile == "" {
		return LLMChatParams{}, errors.New("profile is required")
	}

	var temperatureMilli *uint32
	if b, ok := m[llmChatParamsTLVTypeTemperatureMilli]; ok {
		v, parseErr := readTU32(b)
		if parseErr != nil {
			return LLMChatParams{}, fmt.Errorf("temperature_milli: %w", parseErr)
		}
		temperatureMilli = &v
	}

	var maxOutputTokens *uint32
	if b, ok := m[llmChatParamsTLVTypeMaxOutputTokens]; ok {
		v, parseErr := readTU32(b)
		if parseErr != nil {
			return LLMChatParams{}, fmt.Errorf("max_output_tokens: %w", parseErr)
		}
		maxOutputTokens = &v
	}

	unknown := make(map[uint64][]byte)
	for typ, val := range m {
		switch typ {
		case llmChatParamsTLVTypeProfile,
			llmChatParamsTLVTypeTemperatureMilli,
			llmChatParamsTLVTypeMaxOutputTokens:
			continue
		default:
			unknown[typ] = cloneBytes(val)
		}
	}
	if len(unknown) == 0 {
		unknown = nil
	}

	return LLMChatParams{
		Profile:          profile,
		TemperatureMilli: temperatureMilli,
		MaxOutputTokens:  maxOutputTokens,
		Unknown:          unknown,
	}, nil
}
