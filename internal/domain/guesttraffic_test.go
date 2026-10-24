package domain

import (
	"strings"
	"testing"
)

// procNetDev is a podman container's /proc/net/dev during the #414 incident: a
// pasta interface carrying a 1.1 GB pnpm store restore, and loopback traffic
// the guest generated talking to itself, which is not network cost.
const procNetDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9000000     120    0    0    0     0          0         0  9000000     120    0    0    0     0       0          0
enp1s0:1345000000 912345    0    0    0     0          0         0 21000000  40000    0    0    0     0       0          0
  tap1:      10       1    0    0    0     0          0         0       20      1    0    0    0     0       0          0
`

// netstatIBN is `netstat -ibn` in a macOS guest: one link row per interface,
// plus one row per address that repeats the same counters and must not be
// summed twice. lo0 has no address column, which shifts every field left.
const netstatIBN = `Name       Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
lo0        16384 <Link#1>                          5821     0     751285     5821     0     751285     0
lo0        16384 127           127.0.0.1           5821     -     751285     5821     -     751285     -
gif0*      1280  <Link#2>                             0     0          0        0     0          0     0
en0        1500  <Link#4>    ee:9a:8c:2f:a1:01  1234567     0  314828521   654321     0   50000000     0
en0        1500  192.168.64    192.168.64.5      1234567     -  314828521   654321     -   50000000     -
en1        1500  <Link#5>    ee:9a:8c:2f:a1:02       10     0       1000       10     0       2000     0
`

func TestParseProcNetDevSumsEveryInterfaceButLoopback(t *testing.T) {
	counters, err := ParseGuestNetwork(PlatformLinux, []byte(procNetDev))
	if err != nil {
		t.Fatalf("ParseGuestNetwork() = %v", err)
	}
	if counters.ReceiveBytes != 1345000010 || counters.TransmitBytes != 21000020 {
		t.Fatalf("counters = %+v, want non-loopback sums", counters)
	}
}

func TestParseNetstatCountsEachInterfaceOnce(t *testing.T) {
	counters, err := ParseGuestNetwork(PlatformMacOS, []byte(netstatIBN))
	if err != nil {
		t.Fatalf("ParseGuestNetwork() = %v", err)
	}
	if counters.ReceiveBytes != 314829521 || counters.TransmitBytes != 50002000 {
		t.Fatalf("counters = %+v, want link rows only, loopback excluded", counters)
	}
}

// TestUnreadableCountersAreAnErrorNeverZero is AGENTS.md rule 4: output the
// parser cannot read is an unavailable observation, not a guest that received
// nothing.
func TestUnreadableCountersAreAnErrorNeverZero(t *testing.T) {
	for name, testCase := range map[string]struct {
		platform Platform
		output   string
	}{
		"empty linux":           {PlatformLinux, ""},
		"loopback only linux":   {PlatformLinux, strings.Join(strings.Split(procNetDev, "\n")[:3], "\n")},
		"short linux row":       {PlatformLinux, "Inter-|\n face |\n eth0: 1 2 3\n"},
		"non-numeric linux":     {PlatformLinux, "Inter-|\n face |\n eth0: x 0 0 0 0 0 0 0 9 0 0 0 0 0 0 0\n"},
		"negative linux":        {PlatformLinux, "Inter-|\n face |\n eth0: -1 0 0 0 0 0 0 0 9 0 0 0 0 0 0 0\n"},
		"empty macOS":           {PlatformMacOS, ""},
		"headerless macOS":      {PlatformMacOS, "en0 1500 <Link#4> aa 1 0 2 3 0 4 0\n"},
		"loopback only macOS":   {PlatformMacOS, strings.Join(strings.Split(netstatIBN, "\n")[:2], "\n")},
		"short macOS row":       {PlatformMacOS, "Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll\nen0 1500 <Link#4> 1\n"},
		"non-numeric macOS":     {PlatformMacOS, "Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll\nen0 1500 <Link#4> aa 1 0 x 3 0 4 0\n"},
		"unknown platform":      {Platform("windows"), procNetDev},
		"loopback named lo-ish": {PlatformLinux, "Inter-|\n face |\n lo: 1 0 0 0 0 0 0 0 9 0 0 0 0 0 0 0\n"},
	} {
		t.Run(name, func(t *testing.T) {
			if counters, err := ParseGuestNetwork(testCase.platform, []byte(testCase.output)); err == nil {
				t.Fatalf("ParseGuestNetwork() = %+v, want an error", counters)
			}
		})
	}
}

func TestGuestNetworkCommandIsAFixedVectorPerPlatform(t *testing.T) {
	if got := strings.Join(GuestNetworkCommand(PlatformLinux), " "); got != "cat /proc/net/dev" {
		t.Errorf("linux command = %q", got)
	}
	if got := strings.Join(GuestNetworkCommand(PlatformMacOS), " "); got != "netstat -ibn" {
		t.Errorf("macOS command = %q", got)
	}
	if GuestNetworkCommand(Platform("windows")) != nil {
		t.Error("an unknown platform has no command")
	}
}
