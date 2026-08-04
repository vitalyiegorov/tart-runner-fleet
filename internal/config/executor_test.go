package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// podmanNode is node B's configuration: the shared Linux definitions plus the
// executor block that tells the daemon it has a container runtime.
func podmanNode() Config {
	cfg := Default()
	cfg.Executor = Executor{Backend: ExecutorPodman, Image: "ghcr.io/vitalyiegorov/trf-runner-amd64:2026-08",
		Binary: "/usr/bin/podman", KVMProfiles: []string{"large"}, HoldCommand: []string{"sleep", "infinity"}}
	return cfg
}

// TestExecutorBlockRoundTripsAndStaysAbsentWhenUnset pins the compatibility rule
// every optional setting in this file obeys: a node that names no backend
// encodes no key, because decoding is strict and an older release must still
// read a file this one wrote.
func TestExecutorBlockRoundTripsAndStaysAbsentWhenUnset(t *testing.T) {
	var withoutExecutor bytes.Buffer
	if err := Encode(&withoutExecutor, Default()); err != nil {
		t.Fatalf("encode a node with no executor: %v", err)
	}
	if strings.Contains(withoutExecutor.String(), "executor") {
		t.Fatalf("an unset executor was encoded:\n%s", withoutExecutor.String())
	}
	if decoded, err := Decode(bytes.NewReader(withoutExecutor.Bytes())); err != nil || !reflect.DeepEqual(decoded.Executor, Executor{}) {
		t.Fatalf("round trip without an executor = %#v (err=%v)", decoded.Executor, err)
	}

	var withExecutor bytes.Buffer
	if err := Encode(&withExecutor, podmanNode()); err != nil {
		t.Fatalf("encode a container node: %v", err)
	}
	decoded, err := Decode(bytes.NewReader(withExecutor.Bytes()))
	if err != nil {
		t.Fatalf("decode a container node: %v", err)
	}
	if !reflect.DeepEqual(decoded.Executor, podmanNode().Executor) {
		t.Fatalf("executor round trip = %#v, want %#v", decoded.Executor, podmanNode().Executor)
	}
	// Clone must copy the slices, or two configurations would share the list of
	// profiles that get a device grant.
	clone := decoded.Clone()
	clone.Executor.KVMProfiles[0] = "tampered"
	clone.Executor.HoldCommand[0] = "tampered"
	if decoded.Executor.KVMProfiles[0] != "large" || decoded.Executor.HoldCommand[0] != "sleep" {
		t.Fatalf("Clone aliased the executor slices: %#v", decoded.Executor)
	}
}

func TestExecutorValidation(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		mutate   func(*Config)
		wantErr  string
		wantPass bool
	}{
		{name: "a container node", mutate: func(*Config) {}, wantPass: true},
		{
			name:     "a node with no executor at all",
			mutate:   func(c *Config) { c.Executor = Executor{} },
			wantPass: true,
		},
		{
			name:    "an image with no backend to run it",
			mutate:  func(c *Config) { c.Executor = Executor{Image: "ghcr.io/o/r:1"} },
			wantErr: "require an executor backend",
		},
		{
			name:    "a binary with no backend to run it",
			mutate:  func(c *Config) { c.Executor = Executor{Binary: "/usr/bin/podman"} },
			wantErr: "require an executor backend",
		},
		{
			name:    "a device grant with no backend to honour it",
			mutate:  func(c *Config) { c.Executor = Executor{KVMProfiles: []string{"large"}} },
			wantErr: "require an executor backend",
		},
		{
			name:    "a hold command with no backend to run it",
			mutate:  func(c *Config) { c.Executor = Executor{HoldCommand: []string{"sleep"}} },
			wantErr: "require an executor backend",
		},
		{
			name:    "a backend this build does not implement",
			mutate:  func(c *Config) { c.Executor.Backend = "docker" },
			wantErr: "unsupported executor backend",
		},
		{
			name:    "a container node with no image",
			mutate:  func(c *Config) { c.Executor.Image = "" },
			wantErr: "executor image",
		},
		{
			name:    "an image that reads as a command-line option",
			mutate:  func(c *Config) { c.Executor.Image = "--privileged" },
			wantErr: "executor image",
		},
		{
			name:    "a device grant to a profile this node does not declare",
			mutate:  func(c *Config) { c.Executor.KVMProfiles = []string{"android"} },
			wantErr: "undeclared profile",
		},
		{
			name:    "a hold command with an empty argument",
			mutate:  func(c *Config) { c.Executor.HoldCommand = []string{"sleep", "  "} },
			wantErr: "empty argument",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := podmanNode()
			testCase.mutate(&cfg)
			err := cfg.Validate()
			if testCase.wantPass {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, testCase.wantErr)
			}
		})
	}
}

// TestAnUnknownExecutorKeyIsRefused states the strict-decoding rule for the new
// block: a typo inside it is a refused configuration, not a silently ignored
// device grant.
func TestAnUnknownExecutorKeyIsRefused(t *testing.T) {
	var encoded bytes.Buffer
	if err := Encode(&encoded, podmanNode()); err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(encoded.String(), `"kvmProfiles"`, `"kvmProfile"`, 1)
	if _, err := Decode(strings.NewReader(tampered)); err == nil {
		t.Fatal("a misspelled executor key was accepted")
	}
}
