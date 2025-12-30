package main

import (
	"context"
	"os"
	"testing"
)

func TestParseEnvConfig_DefaultProviderConfigPathWhenConfigYAMLExists(t *testing.T) {
	t.Setenv("LCPD_PROVIDER_CONFIG_PATH", "")

	tmp := t.TempDir()
	t.Chdir(tmp)

	if err := os.WriteFile("config.yaml", []byte("enabled: false\n"), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	cfg, err := parseEnvConfig(context.Background())
	if err != nil {
		t.Fatalf("parseEnvConfig: %v", err)
	}
	if cfg.ProviderConfigPath != "config.yaml" {
		t.Fatalf("ProviderConfigPath: got %q, want %q", cfg.ProviderConfigPath, "config.yaml")
	}
}

func TestParseEnvConfig_DefaultProviderConfigPathEmptyWhenNoFile(t *testing.T) {
	t.Setenv("LCPD_PROVIDER_CONFIG_PATH", "")

	tmp := t.TempDir()
	t.Chdir(tmp)

	cfg, err := parseEnvConfig(context.Background())
	if err != nil {
		t.Fatalf("parseEnvConfig: %v", err)
	}
	if cfg.ProviderConfigPath != "" {
		t.Fatalf("ProviderConfigPath: got %q, want empty", cfg.ProviderConfigPath)
	}
}
