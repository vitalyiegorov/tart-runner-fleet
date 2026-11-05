package tart

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// HostTraffic reads what each running guest's virtual NIC has carried, from the
// host's own interface counters (ADR 0080, amendment 2026-11-05).
//
// It never talks to a guest. `tart exec` rides the guest's vsock device, and
// repeated vsock connections are what trap Virtualization.framework's VM helper
// (issue #478), so a periodic read of a guest is not an option. Virtualization
// instead gives each guest a `vmenetN` interface on the host's NAT bridge, and
// the bridge's address cache says which MAC lives on which one. The guest's MAC
// is in its own `config.json`, which the fleet already reads for its power
// reading. Three short host commands answer for every guest at once.
type HostTraffic struct {
	// Run executes a host command. Nil runs the real one.
	Run func(ctx context.Context, binary string, args ...string) ([]byte, error)
	// Home is the tart home directory; empty means the one tart itself uses.
	Home string
}

func (h HostTraffic) run(ctx context.Context, binary string, args ...string) ([]byte, error) {
	if h.Run != nil {
		return h.Run(ctx, binary, args...)
	}
	// #nosec G204 -- the binary and every argument are fixed by domain, never external input.
	return exec.CommandContext(ctx, binary, args...).Output()
}

// Read returns the counters of each named guest that is on a bridge right now.
// A guest the host cannot see (powered off, MAC unreadable, not yet on the
// bridge) is absent from the answer, never zero. If the host's own tables
// cannot be read, nothing is returned and the error says so.
func (h HostTraffic) Read(ctx context.Context, names []string) (map[string]domain.GuestNetwork, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	listing, err := h.run(ctx, domain.HostInterfacesCommand[0], domain.HostInterfacesCommand[1:]...)
	if err != nil {
		return nil, err
	}
	ports := map[string]string{}
	for _, name := range strings.Fields(string(listing)) {
		if !strings.HasPrefix(name, "bridge") {
			continue
		}
		command := domain.HostBridgeCommand(name)
		output, err := h.run(ctx, command[0], command[1:]...)
		if err != nil {
			return nil, err
		}
		for mac, port := range domain.ParseBridgeGuests(output) {
			ports[mac] = port
		}
	}
	table, err := h.run(ctx, domain.HostCountersCommand[0], domain.HostCountersCommand[1:]...)
	if err != nil {
		return nil, err
	}
	nics, err := domain.ParseVirtualNICs(table)
	if err != nil {
		return nil, err
	}
	counters := map[string]domain.GuestNetwork{}
	for _, name := range names {
		mac, ok := h.macAddress(name)
		if !ok {
			continue
		}
		if nic, ok := nics[ports[mac]]; ok {
			counters[name] = nic
		}
	}
	return counters, nil
}

// macAddress is the guest's NIC address from its tart configuration, lower-case.
func (h HostTraffic) macAddress(name string) (string, bool) {
	if validateName(name) != nil {
		return "", false
	}
	home := h.Home
	if home == "" {
		home = TartHome()
	}
	if home == "" {
		return "", false
	}
	// #nosec G304 -- the path is the node's own tart home joined with a name the
	// fleet's instance grammar has already validated; no external value reaches it.
	contents, err := os.ReadFile(filepath.Join(home, "vms", name, "config.json"))
	if err != nil {
		return "", false
	}
	var config struct {
		MACAddress string `json:"macAddress"`
	}
	if json.Unmarshal(contents, &config) != nil || config.MACAddress == "" {
		return "", false
	}
	return strings.ToLower(config.MACAddress), true
}
