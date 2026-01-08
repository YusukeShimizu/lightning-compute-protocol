package deps

import "testing"

func TestLndAssetName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		goos    string
		goarch  string
		version string
		want    string
		wantErr bool
	}{
		{
			name:    "darwin arm64",
			goos:    "darwin",
			goarch:  "arm64",
			version: "v0.19.3-beta",
			want:    "lnd-darwin-arm64-v0.19.3-beta.tar.gz",
		},
		{
			name:    "linux amd64",
			goos:    "linux",
			goarch:  "amd64",
			version: "v0.19.3-beta",
			want:    "lnd-linux-amd64-v0.19.3-beta.tar.gz",
		},
		{
			name:    "unsupported arch",
			goos:    "linux",
			goarch:  "riscv64",
			version: "v0.19.3-beta",
			wantErr: true,
		},
		{
			name:    "unsupported os",
			goos:    "plan9",
			goarch:  "amd64",
			version: "v0.19.3-beta",
			wantErr: true,
		},
	} {
		got, err := lndAssetName(tc.goos, tc.goarch, tc.version)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
		if got != tc.want {
			t.Fatalf("%s: asset=%q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseManifest(t *testing.T) {
	t.Parallel()

	contents := []byte("" +
		"abc  file1\n" +
		"def  file2\n" +
		"bad line\n" +
		"\n",
	)
	got, err := parseManifest(contents)
	if err != nil {
		t.Fatalf("parseManifest() error = %v", err)
	}
	if got["file1"] != "abc" {
		t.Fatalf("file1=%q, want %q", got["file1"], "abc")
	}
	if got["file2"] != "def" {
		t.Fatalf("file2=%q, want %q", got["file2"], "def")
	}
	if _, ok := got["bad"]; ok {
		t.Fatalf("unexpected entry for bad")
	}
}
