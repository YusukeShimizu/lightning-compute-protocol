package build

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func GoBinary(ctx context.Context, moduleDir, pkg, outPath string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", outPath, pkg)
	cmd.Dir = moduleDir

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	if err := cmd.Run(); err != nil {
		out := strings.TrimSpace(combined.String())
		if out != "" {
			return fmt.Errorf("go build failed: %w\n%s", err, out)
		}
		return fmt.Errorf("go build failed: %w", err)
	}
	return nil
}

