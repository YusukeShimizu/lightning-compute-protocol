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
		TaskKind:             "llm.chat",
		Input:                []byte("hello"),
		InputContentType:     "text/plain; charset=utf-8",
		InputContentEncoding: "identity",
		Params:               []byte{0x01, 0x01, 0x61}, // type=1(profile), len=1, value="a"
	}

	inputHash := sha256.Sum256(commit.Input)
	paramsHash := sha256.Sum256(commit.Params)

	expectedTLV := make([]byte, 0, 122)
	expectedTLV = append(expectedTLV, 0x01, 0x02, 0x00, 0x02)
	expectedTLV = append(expectedTLV, 0x02, 0x20)
	expectedTLV = append(expectedTLV, bytes.Repeat([]byte{0x11}, 32)...)
	expectedTLV = append(expectedTLV, 0x03, 0x02, 0x03, 0xE8)
	expectedTLV = append(expectedTLV, 0x04, 0x02, 0x01, 0xF4)
	expectedTLV = append(expectedTLV, 0x14, 0x08)
	expectedTLV = append(expectedTLV, []byte(commit.TaskKind)...)
	expectedTLV = append(expectedTLV, 0x32, 0x20)
	expectedTLV = append(expectedTLV, inputHash[:]...)
	expectedTLV = append(expectedTLV, 0x33, 0x20)
	expectedTLV = append(expectedTLV, paramsHash[:]...)
	expectedTLV = append(expectedTLV, 0x34, 0x01, 0x05)
	expectedTLV = append(expectedTLV, 0x35, 0x19)
	expectedTLV = append(expectedTLV, []byte(commit.InputContentType)...)
	expectedTLV = append(expectedTLV, 0x36, 0x08)
	expectedTLV = append(expectedTLV, []byte(commit.InputContentEncoding)...)

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
