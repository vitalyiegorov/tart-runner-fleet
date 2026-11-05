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

func TestCacheGateNPMRegistryValidation(t *testing.T) {
	tart := CacheGate{URL: "http://192.168.1.64:3017/", PublicKey: testGateKey, NPMRegistry: "http://192.168.1.64:3017/npm/"}
	podman := CacheGate{URL: "http://127.0.0.1:3017/", PublicKey: testGateKey, NPMRegistry: "http://host.containers.internal:3017/npm/"}
	with := func(gate CacheGate, change func(*CacheGate)) CacheGate {
		change(&gate)
		return gate
	}
	registry := func(value string) func(*CacheGate) { return func(g *CacheGate) { g.NPMRegistry = value } }
	gateURL := func(value string) func(*CacheGate) { return func(g *CacheGate) { g.URL = value } }
	tests := []struct {
		name    string
		gate    CacheGate
		backend ExecutorBackend
		want    string
	}{
		{name: "a tart node at the LAN address", gate: tart},
		{name: "omarchy's own podman guests", gate: podman, backend: ExecutorPodman},
		{name: "a podman node may verify at its LAN address", backend: ExecutorPodman,
			gate: with(podman, gateURL("http://192.168.1.64:3017/"))},
		{name: "ten-net", gate: CacheGate{URL: "http://10.1.2.3:3017/", PublicKey: testGateKey, NPMRegistry: "http://10.1.2.3:3017/npm/"}},

		{name: "a podman node with no npm registry has nothing to verify", backend: ExecutorPodman,
			gate: with(podman, registry("")), want: "podman"},
		{name: "a tunnel hostname", gate: with(tart, registry("http://cache.example.dev:3017/npm/")), want: "cacheGate.npmRegistry"},
		{name: "a tunnel gate", gate: CacheGate{URL: "https://cache.example.dev/", PublicKey: testGateKey,
			NPMRegistry: "https://cache.example.dev/npm/"}, want: "cacheGate.url"},
		{name: "a public gate address", gate: CacheGate{URL: "http://8.8.8.8:3017/", PublicKey: testGateKey,
			NPMRegistry: "http://8.8.8.8:3017/npm/"}, want: "cacheGate.url"},
		{name: "a carrier-grade NAT address", gate: CacheGate{URL: "http://100.64.0.1:3017/", PublicKey: testGateKey,
			NPMRegistry: "http://100.64.0.1:3017/npm/"}, want: "cacheGate.url"},
		{name: "https, which the gate does not speak", gate: with(tart, registry("https://192.168.1.64:3017/npm/")), want: "cacheGate.npmRegistry"},
		{name: "no explicit port", gate: with(tart, registry("http://192.168.1.64/npm/")), want: "cacheGate.npmRegistry"},
		{name: "another port than the gate", gate: with(tart, registry("http://192.168.1.64:3018/npm/")), want: "cacheGate.npmRegistry"},
		{name: "another host than the gate", gate: with(tart, registry("http://192.168.1.65:3017/npm/")), want: "cacheGate.npmRegistry"},
		{name: "not the route", gate: with(tart, registry("http://192.168.1.64:3017/")), want: "cacheGate.npmRegistry"},
		{name: "no trailing slash", gate: with(tart, registry("http://192.168.1.64:3017/npm")), want: "cacheGate.npmRegistry"},
		{name: "credentials", gate: with(tart, registry("http://u:p@192.168.1.64:3017/npm/")), want: "cacheGate.npmRegistry"},
		{name: "a query", gate: with(tart, registry("http://192.168.1.64:3017/npm/?a=1")), want: "cacheGate.npmRegistry"},
		{name: "a fragment", gate: with(tart, registry("http://192.168.1.64:3017/npm/#a")), want: "cacheGate.npmRegistry"},
		{name: "a newline", gate: with(tart, registry("http://192.168.1.64:3017/npm/\nregistry=http://evil/")), want: "cacheGate.npmRegistry"},
		{name: "unparseable", gate: with(tart, registry("http://[::1")), want: "cacheGate.npmRegistry"},
		{name: "a tart node cannot resolve the podman name", gate: with(tart, registry("http://host.containers.internal:3017/npm/")),
			want: "cacheGate.npmRegistry"},
		{name: "a podman guest cannot reach the host's LAN address", backend: ExecutorPodman,
			gate: with(podman, registry("http://192.168.1.64:3017/npm/")), want: "cacheGate.npmRegistry"},
		{name: "a tart node's loopback is not its guests' gate", gate: CacheGate{URL: "http://127.0.0.1:3017/", PublicKey: testGateKey,
			NPMRegistry: "http://127.0.0.1:3017/npm/"}, want: "cacheGate.url"},
		{name: "a podman node's verify address over https", backend: ExecutorPodman,
			gate: with(podman, gateURL("https://127.0.0.1:3017/")), want: "cacheGate.url"},
		{name: "a podman node's verify port differs", backend: ExecutorPodman,
			gate: with(podman, gateURL("http://127.0.0.1:3018/")), want: "cacheGate.npmRegistry"},
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

// TestAnNPMRegistryIsOffByDefault: a node's file that has never heard of it encodes
// and decodes exactly as it did, and a Tart gate keeps validating without one.
func TestAnNPMRegistryIsOffByDefault(t *testing.T) {
	valid := multiScopeAuthorityConfig()
	valid.CacheGate = CacheGate{URL: "http://192.168.1.64:3017/", PublicKey: testGateKey}
	var encoded bytes.Buffer
	if err := Encode(&encoded, valid); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded.Bytes(), []byte("npmRegistry")) {
		t.Fatal("a node with no npm registry encoded the key")
	}
	valid.CacheGate.NPMRegistry = "http://192.168.1.64:3017/npm/"
	encoded.Reset()
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
}

func TestNPMHost(t *testing.T) {
	for raw, want := range map[string]string{
		"": "", "http://192.168.1.64:3017/npm/": "192.168.1.64", "http://host.containers.internal:3017/npm/": "host.containers.internal", "http://[::1": "",
	} {
		if got := (CacheGate{NPMRegistry: raw}).NPMHost(); got != want {
			t.Fatalf("%q: %q", raw, got)
		}
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
