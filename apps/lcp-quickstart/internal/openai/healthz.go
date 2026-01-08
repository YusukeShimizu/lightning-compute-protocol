package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxHealthzBytes = 1024

func Healthz(ctx context.Context, httpAddr string) error {
	url := "http://" + httpAddr + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthzBytes))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(body)) != "ok" {
		return fmt.Errorf("healthz: unexpected body %q", strings.TrimSpace(string(body)))
	}
	return nil
}
