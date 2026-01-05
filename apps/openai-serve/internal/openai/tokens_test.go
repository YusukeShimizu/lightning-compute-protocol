package openai_test

import (
	"testing"

	"github.com/bruwbird/lcp/apps/openai-serve/internal/openai"
	"github.com/google/go-cmp/cmp"
)

func TestApproxTokensFromBytes(t *testing.T) {
	cases := []struct {
		bytes int
		want  int
	}{
		{bytes: 0, want: 0},
		{bytes: 1, want: 1},
		{bytes: 4, want: 1},
		{bytes: 5, want: 2},
	}
	for _, tc := range cases {
		got := openai.ApproxTokensFromBytes(tc.bytes)
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Fatalf("ApproxTokensFromBytes(%d) mismatch (-want +got):\n%s", tc.bytes, diff)
		}
	}
}
