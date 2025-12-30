package main

import (
	"fmt"
	"os"

	pgcclient "github.com/NathanBaulch/protoc-gen-cobra/client"
	"github.com/NathanBaulch/protoc-gen-cobra/naming"
	lcpdv1 "github.com/bruwbird/lcp/go-lcpd/gen/go/lcpd/v1"
	"github.com/spf13/cobra"

	_ "github.com/NathanBaulch/protoc-gen-cobra/iocodec/yaml"
)

const defaultServerAddr = "127.0.0.1:50051"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "lcpdctl",
		Short:         "CLI client for go-lcpd gRPC API (generated via protoc-gen-cobra).",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	commandNamer := func(s string) string {
		switch s {
		case "LCPDService":
			return "lcpd"
		default:
			return naming.LowerKebab(s)
		}
	}

	cmd.AddCommand(lcpdv1.LCPDServiceClientCommand(
		pgcclient.WithServerAddr(defaultServerAddr),
		pgcclient.WithEnvVars("LCPDCTL"),
		pgcclient.WithCommandNamer(commandNamer),
	))

	return cmd
}
