package controller

import "testing"

func TestRequireLoopbackListenAddr(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{name: "ipv4 loopback", addr: "127.0.0.1:1234", wantErr: false},
		{name: "localhost", addr: "localhost:1234", wantErr: false},
		{name: "ipv6 loopback", addr: "[::1]:1234", wantErr: false},
		{name: "unspecified ipv4", addr: "0.0.0.0:1234", wantErr: true},
		{name: "missing host", addr: ":1234", wantErr: true},
		{name: "bad", addr: "not-an-addr", wantErr: true},
	} {
		err := requireLoopbackListenAddr("x", tc.addr)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

func TestValidateOpenAIServeHTTPAddr(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                      string
		addr                      string
		apiKeys                   string
		iUnderstandExposingOpenAI bool
		wantErr                   bool
	}{
		{
			name:    "loopback no keys ok",
			addr:    "127.0.0.1:8080",
			apiKeys: "",
			wantErr: false,
		},
		{
			name:    "non-loopback without consent",
			addr:    "0.0.0.0:8080",
			apiKeys: "k",
			wantErr: true,
		},
		{
			name:                      "non-loopback with consent but no keys",
			addr:                      "0.0.0.0:8080",
			apiKeys:                   "",
			iUnderstandExposingOpenAI: true,
			wantErr:                   true,
		},
		{
			name:                      "non-loopback with consent and keys",
			addr:                      "0.0.0.0:8080",
			apiKeys:                   "k",
			iUnderstandExposingOpenAI: true,
			wantErr:                   false,
		},
	} {
		err := validateOpenAIServeHTTPAddr(tc.addr, tc.apiKeys, tc.iUnderstandExposingOpenAI)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}
