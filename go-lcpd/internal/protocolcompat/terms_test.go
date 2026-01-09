package protocolcompat_test

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
	"github.com/bruwbird/lcp/go-lcpd/internal/protocolcompat"
	"github.com/google/go-cmp/cmp"
)

func TestComputeTermsHash_MatchesSpecEncodingVector(t *testing.T) {
	t.Parallel()

	var jobID lcp.JobID
	for i := range jobID {
		jobID[i] = 0x11
	}

	terms := lcp.Terms{
		ProtocolVersion: lcpwire.ProtocolVersionV02,
		JobID:           jobID,
		PriceMsat:       1000,
		QuoteExpiry:     500,
	}

	commit := protocolcompat.TermsCommit{
		Method:                 "openai.chat_completions.v1",
		Request:                []byte(`{"model":"a","messages":[{"role":"user","content":"hi"}]}`),
		RequestContentType:     "application/json; charset=utf-8",
		RequestContentEncoding: "identity",
		Params:                 []byte{0x01, 0x01, 0x61}, // type=1(model), len=1, value="a"
	}

	requestHash := sha256.Sum256(commit.Request)
	paramsHash := sha256.Sum256(commit.Params)

	expectedTLV := make([]byte, 0, 122)
	expectedTLV = append(expectedTLV, 0x01, 0x02, 0x00, 0x03)
	expectedTLV = append(expectedTLV, 0x02, 0x20)
	expectedTLV = append(expectedTLV, bytes.Repeat([]byte{0x11}, 32)...)
	expectedTLV = append(expectedTLV, 0x14, 0x1a)
	expectedTLV = append(expectedTLV, []byte(commit.Method)...)
	expectedTLV = append(expectedTLV, 0x1e, 0x02, 0x03, 0xE8)
	expectedTLV = append(expectedTLV, 0x1f, 0x02, 0x01, 0xF4)
	expectedTLV = append(expectedTLV, 0x32, 0x20)
	expectedTLV = append(expectedTLV, requestHash[:]...)
	expectedTLV = append(expectedTLV, 0x33, 0x20)
	expectedTLV = append(expectedTLV, paramsHash[:]...)
	if len(commit.Request) == 0 || len(commit.Request) > 0xfc {
		t.Fatalf("unexpected request length for test vector: %d", len(commit.Request))
	}
	expectedTLV = append(expectedTLV, 0x34, 0x01, byte(len(commit.Request)))
	expectedTLV = append(expectedTLV, 0x35, 0x1f)
	expectedTLV = append(expectedTLV, []byte(commit.RequestContentType)...)
	expectedTLV = append(expectedTLV, 0x36, 0x08)
	expectedTLV = append(expectedTLV, []byte(commit.RequestContentEncoding)...)

	expectedSum := sha256.Sum256(expectedTLV)
	expectedHash := lcp.Hash32(expectedSum)

	got, err := protocolcompat.ComputeTermsHash(terms, commit)
	if err != nil {
		t.Fatalf("ComputeTermsHash: %v", err)
	}

	if diff := cmp.Diff(expectedHash, got); diff != "" {
		t.Fatalf("terms_hash mismatch (-want +got):\n%s", diff)
	}
}
