package domain

import (
	"bufio"
	"bytes"
	"errors"
	"strconv"
	"strings"
)

// GuestNetwork is what a guest's own interfaces counted since it booted,
// summed over every interface except loopback. Each guest here is ephemeral and
// boots for exactly one job, so a sample taken at the end of that job is the
// job's traffic (ADR 0080). LAN traffic to the node's Actions cache is included;
// the cache gate counts that share separately.
type GuestNetwork struct {
	ReceiveBytes  int64
	TransmitBytes int64
}

// ErrGuestNetworkUnreadable is any output the parsers cannot read. It is never
// rendered as zero bytes (AGENTS.md rule 4).
var ErrGuestNetworkUnreadable = errors.New("guest network counters unreadable")

// GuestNetworkCommand is the fixed argument vector, run inside the guest, that
// prints its interface counters. Linux guests (podman containers and Tart
// Linux VMs) read the kernel's own table; macOS guests use netstat's byte
// counters. Nil for a platform with no known command.
func GuestNetworkCommand(platform Platform) []string {
	switch platform {
	case PlatformLinux:
		return []string{"cat", "/proc/net/dev"}
	case PlatformMacOS:
		return []string{"netstat", "-ibn"}
	default:
		return nil
	}
}

// ParseGuestNetwork reads GuestNetworkCommand's output for platform. It
// refuses output that names no non-loopback interface: a guest with no network
// is not a guest that downloaded nothing, it is a reading that did not happen.
func ParseGuestNetwork(platform Platform, output []byte) (GuestNetwork, error) {
	switch platform {
	case PlatformLinux:
		return parseProcNetDev(output)
	case PlatformMacOS:
		return parseNetstat(output)
	default:
		return GuestNetwork{}, ErrGuestNetworkUnreadable
	}
}

// parseProcNetDev reads /proc/net/dev: two header lines, then `name: rx-bytes
// ... (8 receive fields) tx-bytes ...`. The kernel may omit the space after
// the colon once a counter is wide enough, so the line is split on the colon.
func parseProcNetDev(output []byte) (GuestNetwork, error) {
	var total GuestNetwork
	counted := 0
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for line := 0; scanner.Scan(); line++ {
		name, counters, found := strings.Cut(scanner.Text(), ":")
		if line < 2 || !found {
			continue
		}
		if strings.TrimSpace(name) == "lo" {
			continue
		}
		fields := strings.Fields(counters)
		if len(fields) < 16 {
			return GuestNetwork{}, ErrGuestNetworkUnreadable
		}
		if err := total.add(fields[0], fields[8]); err != nil {
			return GuestNetwork{}, err
		}
		counted++
	}
	return total.atLeastOne(counted)
}

// parseNetstat reads `netstat -ibn`. Each interface has one `<Link#N>` row and
// one row per address repeating the same counters, so only link rows count.
// The Address column is blank for some interfaces, which shifts fields left;
// the byte columns are therefore located from the right, by their offset in
// the header row.
func parseNetstat(output []byte) (GuestNetwork, error) {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	header := strings.Fields(lines[0])
	receive, transmit := fromRight(header, "Ibytes"), fromRight(header, "Obytes")
	if receive == 0 || transmit == 0 {
		return GuestNetwork{}, ErrGuestNetworkUnreadable
	}
	var total GuestNetwork
	counted := 0
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.HasPrefix(fields[2], "<Link#") || strings.HasPrefix(fields[0], "lo") {
			continue
		}
		if len(fields) < receive {
			return GuestNetwork{}, ErrGuestNetworkUnreadable
		}
		if err := total.add(fields[len(fields)-receive], fields[len(fields)-transmit]); err != nil {
			return GuestNetwork{}, err
		}
		counted++
	}
	return total.atLeastOne(counted)
}

// fromRight is a column's 1-based offset from the end of header, 0 if absent.
func fromRight(header []string, column string) int {
	for index, name := range header {
		if name == column {
			return len(header) - index
		}
	}
	return 0
}

func (g *GuestNetwork) add(receive, transmit string) error {
	rx, rxErr := strconv.ParseInt(receive, 10, 64)
	tx, txErr := strconv.ParseInt(transmit, 10, 64)
	if rxErr != nil || txErr != nil || rx < 0 || tx < 0 {
		return ErrGuestNetworkUnreadable
	}
	g.ReceiveBytes += rx
	g.TransmitBytes += tx
	return nil
}

func (g GuestNetwork) atLeastOne(counted int) (GuestNetwork, error) {
	if counted == 0 {
		return GuestNetwork{}, ErrGuestNetworkUnreadable
	}
	return g, nil
}
