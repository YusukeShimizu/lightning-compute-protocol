package deps

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/workspace"
)

const DefaultLNDVersion = "v0.19.3-beta"

type LND struct {
	Version   string
	LNDPath   string
	LNCLIPath string
}

func EnsureLND(ctx context.Context, ws workspace.Workspace, version string) (LND, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		version = DefaultLNDVersion
	}
	if !strings.HasPrefix(version, "v") {
		return LND{}, fmt.Errorf("invalid lnd version %q (expected leading 'v')", version)
	}
	if err := ws.Ensure(); err != nil {
		return LND{}, err
	}

	lndPath := filepath.Join(ws.BinDir, "lnd")
	lncliPath := filepath.Join(ws.BinDir, "lncli")
	if ok, err := binariesMatchVersion(ws.BinDir, version, lndPath, lncliPath); err != nil {
		return LND{}, err
	} else if ok {
		return LND{Version: version, LNDPath: lndPath, LNCLIPath: lncliPath}, nil
	}

	assetName, err := lndAssetName(runtime.GOOS, runtime.GOARCH, version)
	if err != nil {
		return LND{}, err
	}

	manifestURL := fmt.Sprintf("https://github.com/lightningnetwork/lnd/releases/download/%s/manifest-%s.txt", version, version)
	expectedSHA, err := fetchManifestSHA256(ctx, manifestURL, assetName)
	if err != nil {
		return LND{}, err
	}

	tarballURL := fmt.Sprintf("https://github.com/lightningnetwork/lnd/releases/download/%s/%s", version, assetName)
	tarPath, gotSHA, err := downloadToTemp(ctx, tarballURL, ws.DataDir)
	if err != nil {
		return LND{}, err
	}
	defer func() { _ = os.Remove(tarPath) }()

	if expectedSHA != "" && !strings.EqualFold(expectedSHA, gotSHA) {
		return LND{}, fmt.Errorf("lnd download sha256 mismatch: expected %s, got %s", expectedSHA, gotSHA)
	}

	tmpLND, err := extractTarballBinary(tarPath, ws.BinDir, "lnd")
	if err != nil {
		return LND{}, err
	}
	defer func() { _ = os.Remove(tmpLND) }()
	tmpLNCLI, err := extractTarballBinary(tarPath, ws.BinDir, "lncli")
	if err != nil {
		return LND{}, err
	}
	defer func() { _ = os.Remove(tmpLNCLI) }()

	if err := os.Rename(tmpLND, lndPath); err != nil {
		return LND{}, err
	}
	if err := os.Rename(tmpLNCLI, lncliPath); err != nil {
		return LND{}, err
	}

	if err := os.WriteFile(versionMarkerPath(ws.BinDir), []byte(version+"\n"), 0o600); err != nil {
		return LND{}, err
	}

	return LND{Version: version, LNDPath: lndPath, LNCLIPath: lncliPath}, nil
}

func binariesMatchVersion(binDir, version, lndPath, lncliPath string) (bool, error) {
	vb, err := os.ReadFile(versionMarkerPath(binDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if strings.TrimSpace(string(vb)) != version {
		return false, nil
	}
	if !fileExists(lndPath) || !fileExists(lncliPath) {
		return false, nil
	}
	return true, nil
}

func versionMarkerPath(binDir string) string {
	return filepath.Join(binDir, "lnd.version")
}

func lndAssetName(goos, goarch, version string) (string, error) {
	goos = strings.TrimSpace(goos)
	goarch = strings.TrimSpace(goarch)

	var arch string
	switch goarch {
	case "amd64", "arm64", "386":
		arch = goarch
	default:
		return "", fmt.Errorf("unsupported arch %q (expected amd64/arm64/386)", goarch)
	}

	switch goos {
	case "darwin", "linux", "freebsd", "netbsd", "openbsd":
		return fmt.Sprintf("lnd-%s-%s-%s.tar.gz", goos, arch, version), nil
	case "windows":
		return fmt.Sprintf("lnd-windows-%s-%s.zip", arch, version), nil
	default:
		return "", fmt.Errorf("unsupported os %q (expected darwin/linux)", goos)
	}
}

func fetchManifestSHA256(ctx context.Context, manifestURL, assetName string) (string, error) {
	b, err := httpGet(ctx, manifestURL, 2*time.Minute, 1<<20)
	if err != nil {
		return "", err
	}
	entries, err := parseManifest(b)
	if err != nil {
		return "", err
	}
	expected, ok := entries[assetName]
	if !ok {
		return "", fmt.Errorf("lnd manifest missing entry for %s", assetName)
	}
	if _, err := hex.DecodeString(expected); err != nil || len(expected) != 64 {
		return "", fmt.Errorf("invalid sha256 in manifest for %s: %q", assetName, expected)
	}
	return expected, nil
}

func parseManifest(contents []byte) (map[string]string, error) {
	out := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		sha := strings.TrimSpace(fields[0])
		name := strings.TrimSpace(fields[1])
		if sha == "" || name == "" {
			continue
		}
		out[name] = sha
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func httpGet(ctx context.Context, url string, timeout time.Duration, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http get %s: unexpected status %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}

func downloadToTemp(ctx context.Context, url, dir string) (path string, shaHex string, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp(dir, ".download.*")
	if err != nil {
		return "", "", err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("http get %s: unexpected status %s", url, resp.Status)
	}

	h := sha256.New()
	w := io.MultiWriter(tmp, h)
	if _, err := io.Copy(w, resp.Body); err != nil {
		return "", "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	return tmpPath, hex.EncodeToString(h.Sum(nil)), nil
}

func extractTarballBinary(tarGzPath, outDir, wantBase string) (string, error) {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer func() { _ = zr.Close() }()

	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(h.Name) != wantBase {
			continue
		}
		if err := os.MkdirAll(outDir, 0o700); err != nil {
			return "", err
		}
		out, err := os.CreateTemp(outDir, "."+wantBase+".tmp.*")
		if err != nil {
			return "", err
		}
		outPath := out.Name()
		if err := out.Chmod(0o700); err != nil {
			_ = out.Close()
			_ = os.Remove(outPath)
			return "", err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			_ = os.Remove(outPath)
			return "", err
		}
		if err := out.Sync(); err != nil {
			_ = out.Close()
			_ = os.Remove(outPath)
			return "", err
		}
		if err := out.Close(); err != nil {
			_ = os.Remove(outPath)
			return "", err
		}
		return outPath, nil
	}
	return "", fmt.Errorf("binary %q not found in %s", wantBase, tarGzPath)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
