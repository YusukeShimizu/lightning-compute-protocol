package main

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRootCmd_HasLCPDSubcommandWithDefaultServerAddr(t *testing.T) {
	t.Parallel()

	cmd := newRootCmd()

	var lcpdCmdName string
	for _, c := range cmd.Commands() {
		lcpdCmdName = c.Name()
		if lcpdCmdName == "lcpd" {
			flag := c.PersistentFlags().Lookup("server-addr")
			if flag == nil {
				t.Fatalf("server-addr flag not found on lcpd command")
			}
			if diff := cmp.Diff(defaultServerAddr, flag.DefValue); diff != "" {
				t.Fatalf("server-addr default mismatch (-want +got):\n%s", diff)
			}
			return
		}
	}

	t.Fatalf("lcpd command not found")
}
