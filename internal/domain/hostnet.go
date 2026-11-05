package domain

import (
	"regexp"
	"strings"
)

// The host's view of a Tart guest's network (ADR 0080, amendment 2026-11-05).
// Virtualization.framework gives every running guest its own `vmenetN` interface
// on the host's NAT bridge, and the host counts every frame that crosses it. The
// counters are the guest's traffic without a single byte of conversation with
// the guest, so reading them adds nothing to its vsock device (issue #478).

// VirtualNICPrefix names the host interface a Tart guest's NIC appears as.
const VirtualNICPrefix = "vmenet"

// The fixed argument vectors, run on the host, that name its interfaces and
// print every interface's byte counters.
var (
	HostInterfacesCommand = []string{"ifconfig", "-l"}
	HostCountersCommand   = []string{"netstat", "-ibn"}
)

// HostBridgeCommand is the vector that prints one bridge's members and its
// address cache.
func HostBridgeCommand(bridge string) []string { return []string{"ifconfig", bridge} }

// guestPort is one line of a bridge's address cache:
// `<mac> Vlan1 vmenet2 1199 flags=0<>`.
var guestPort = regexp.MustCompile(`^\s+([0-9a-fA-F]{2}(?::[0-9a-fA-F]{2}){5})\s+\S+\s+(` + VirtualNICPrefix + `\d+)\s`)

// ParseBridgeGuests maps each guest MAC address (lower-case) the bridge has
// learned to the vmenet port it was learned on. Ports that are not a virtual
// NIC are not guests and are ignored.
func ParseBridgeGuests(output []byte) map[string]string {
	guests := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if match := guestPort.FindStringSubmatch(line); match != nil {
			guests[strings.ToLower(match[1])] = match[2]
		}
	}
	return guests
}

// ParseVirtualNICs reads the host's `netstat -ibn` and returns each vmenet
// interface's counters as the guest sees them: the host receives what the guest
// transmits, and sends what the guest receives. A table it cannot read is an
// error; a readable table with no vmenet is a host with no guest.
func ParseVirtualNICs(output []byte) (map[string]GuestNetwork, error) {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	header := strings.Fields(lines[0])
	toHost, fromHost := fromRight(header, "Ibytes"), fromRight(header, "Obytes")
	if toHost == 0 || fromHost == 0 {
		return nil, ErrGuestNetworkUnreadable
	}
	nics := map[string]GuestNetwork{}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.HasPrefix(fields[2], "<Link#") || !strings.HasPrefix(fields[0], VirtualNICPrefix) {
			continue
		}
		if len(fields) < toHost {
			return nil, ErrGuestNetworkUnreadable
		}
		var counters GuestNetwork
		if err := counters.add(fields[len(fields)-fromHost], fields[len(fields)-toHost]); err != nil {
			return nil, err
		}
		nics[fields[0]] = counters
	}
	return nics, nil
}
