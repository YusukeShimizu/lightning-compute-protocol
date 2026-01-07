package cli

import (
	"testing"

	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/controller"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/lncli"
	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/model"
)

func TestUpCommand_TestnetMissingProvider(t *testing.T) {
	t.Parallel()

	got := upCommand(model.NetworkTestnet, model.NewState())
	want := "lcp-quickstart testnet up --provider <pubkey>@<host:port>"
	if got != want {
		t.Fatalf("upCommand() = %q, want %q", got, want)
	}
}

func TestUpCommand_Mainnet(t *testing.T) {
	t.Parallel()

	got := upCommand(model.NetworkMainnet, model.NewState())
	want := "lcp-quickstart mainnet up --i-understand-mainnet"
	if got != want {
		t.Fatalf("upCommand() = %q, want %q", got, want)
	}
}

func TestStatusNextSteps_LndUninitialized(t *testing.T) {
	t.Parallel()

	st := model.NewState()
	st.Readiness.LNDReady = false

	next := statusNextSteps(model.NetworkTestnet, st, lncli.WalletUninitialized)
	if len(next) == 0 {
		t.Fatalf("statusNextSteps() returned empty next steps")
	}
	want := "initialize lnd wallet: lcp-quickstart testnet lncli create"
	if next[0] != want {
		t.Fatalf("next[0] = %q, want %q", next[0], want)
	}
}

func TestStatusNextSteps_LndLocked(t *testing.T) {
	t.Parallel()

	st := model.NewState()
	st.Readiness.LNDReady = false

	next := statusNextSteps(model.NetworkMainnet, st, lncli.WalletLocked)
	if len(next) == 0 {
		t.Fatalf("statusNextSteps() returned empty next steps")
	}
	want := "unlock lnd wallet: lcp-quickstart mainnet lncli unlock"
	if next[0] != want {
		t.Fatalf("next[0] = %q, want %q", next[0], want)
	}
}

func TestStatusNextSteps_NeedProvider(t *testing.T) {
	t.Parallel()

	st := model.NewState()
	st.Readiness.LNDReady = true

	next := statusNextSteps(model.NetworkTestnet, st, lncli.WalletUnknown)
	if len(next) == 0 {
		t.Fatalf("statusNextSteps() returned empty next steps")
	}
	want := "set a Provider and run: lcp-quickstart testnet up --provider <pubkey>@<host:port>"
	if next[0] != want {
		t.Fatalf("next[0] = %q, want %q", next[0], want)
	}
}

func TestStatusNextSteps_NeedChannel(t *testing.T) {
	t.Parallel()

	st := model.NewState()
	st.ProviderNode = model.ProviderNode("02deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefde@127.0.0.1:9735")
	st.Readiness = model.Readiness{
		LNDReady:          true,
		ProviderConnected: true,
		ChannelReady:      false,
		LCPDReady:         false,
		OpenAIReady:       false,
	}

	next := statusNextSteps(model.NetworkTestnet, st, lncli.WalletUnknown)
	if len(next) == 0 {
		t.Fatalf("statusNextSteps() returned empty next steps")
	}
	wantPrefix := "need outbound liquidity; open a channel: lcp-quickstart testnet up --provider "
	if len(next[0]) < len(wantPrefix) || next[0][:len(wantPrefix)] != wantPrefix {
		t.Fatalf("next[0] = %q, want prefix %q", next[0], wantPrefix)
	}
	wantSuffix := " --open-channel --channel-sats <sats>"
	if len(next[0]) < len(wantSuffix) || next[0][len(next[0])-len(wantSuffix):] != wantSuffix {
		t.Fatalf("next[0] = %q, want suffix %q", next[0], wantSuffix)
	}
}

func TestNormalizeComponent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in     string
		want   string
		wantOK bool
	}{
		{in: "lnd", want: controller.ComponentLND, wantOK: true},
		{in: "grpcd", want: controller.ComponentLCPDGRPCD, wantOK: true},
		{in: "lcpd-grpcd", want: controller.ComponentLCPDGRPCD, wantOK: true},
		{in: "openai", want: controller.ComponentOpenAIServe, wantOK: true},
		{in: "openai-serve", want: controller.ComponentOpenAIServe, wantOK: true},
		{in: "unknown", want: "", wantOK: false},
	} {
		got, ok := normalizeComponent(tc.in)
		if ok != tc.wantOK {
			t.Fatalf("normalizeComponent(%q) ok=%v, want %v", tc.in, ok, tc.wantOK)
		}
		if got != tc.want {
			t.Fatalf("normalizeComponent(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}
