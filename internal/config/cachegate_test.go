package config

import (
	"bytes"
	"strings"
	"testing"
)

const testGateKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func TestCacheGateValidation(t *testing.T) {
	good := CacheGate{URL: "http://192.168.1.64:3017/", PublicKey: testGateKey}
	tests := []struct {
		name    string
		gate    CacheGate
		backend ExecutorBackend
		want    string
	}{
		{name: "absent"},
		{name: "configured", gate: good},
		{name: "on podman", gate: good, backend: ExecutorPodman, want: "podman"},
		{name: "no key", gate: CacheGate{URL: good.URL}, want: "publicKey"},
		{name: "short key", gate: CacheGate{URL: good.URL, PublicKey: "c2hvcnQ="}, want: "publicKey"},
		{name: "no url", gate: CacheGate{PublicKey: testGateKey}, want: "cacheGate.url"},
		{name: "relative url", gate: CacheGate{URL: "192.168.1.64:3017", PublicKey: testGateKey}, want: "cacheGate.url"},
		{name: "bad scheme", gate: CacheGate{URL: "ftp://x/", PublicKey: testGateKey}, want: "cacheGate.url"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{CacheGate: test.gate}
			cfg.Executor.Backend = test.backend
			err := cfg.validateCacheGate()
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCacheGateRoundTrips(t *testing.T) {
	valid := multiScopeAuthorityConfig()
	valid.CacheGate = CacheGate{URL: "http://192.168.1.64:3017/", PublicKey: testGateKey}
	var encoded bytes.Buffer
	if err := Encode(&encoded, valid); err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.CacheGate != valid.CacheGate {
		t.Fatalf("decoded = %+v", decoded.CacheGate)
	}
	valid.CacheGate = CacheGate{}
	encoded.Reset()
	if err := Encode(&encoded, valid); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded.Bytes(), []byte("cacheGate")) {
		t.Fatal("a node with no gate encoded a cacheGate key")
	}
}
