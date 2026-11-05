package domain

import "testing"

// bridgeIfconfig is `ifconfig bridge100` on a Tart host with three running
// guests (macmini, 2026-11-05): the NAT bridge lists each guest's MAC against
// the vmenet port it was learned on.
const bridgeIfconfig = `bridge100: flags=8a63<UP,BROADCAST,SMART,RUNNING,ALLMULTI,SIMPLEX,MULTICAST> mtu 1500
	member: vmenet0 flags=20803<LEARNING,DISCOVER,PRIVATE,VIRTIO>
	        ifmaxaddr 0 port 23 priority 0 path cost 0
	member: vmenet1 flags=20803<LEARNING,DISCOVER,PRIVATE,VIRTIO>
	        ifmaxaddr 0 port 24 priority 0 path cost 0
	Address cache:
		1a:c0:9b:92:c6:ce Vlan1 vmenet2 1199 flags=0<>
		E6:A3:6C:32:91:36 Vlan1 vmenet1 1200 flags=0<>
		5e:4b:95:57:4d:41 Vlan1 en0 1200 flags=0<>
	nd6 options=201<PERFORMNUD,DAD>
`

// hostNetstat is the host's `netstat -ibn`: vmenet rows count the frames the
// guest sent to the host (Ibytes) and the host sent to the guest (Obytes).
const hostNetstat = `Name       Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
lo0        16384 <Link#1>                          5821     0     751285     5821     0     751285     0
en0        1500  <Link#4>    a4:7e:fa:27:93:11  1234567     0  314828521   654321     0   50000000     0
vmenet1    1500  <Link#24>   26:90:03:af:2e:02   402185     0   32389634  1252561     0 1807739328     0
vmenet2    1500  <Link#33>   c2:c1:08:ab:96:f3   102622     0    7627686   391234     0  586478361     0
bridge100  1500  <Link#25>   1e:f6:4c:65:63:64  1459464     0  186376413  8584775     0 12600144702     0
bridge100  1500  192.168.64    192.168.64.1     1459464     -  186376413  8584775     - 12600144702     -
`

func TestParseBridgeGuestsMapsEachLearnedMACToItsVmenet(t *testing.T) {
	got := ParseBridgeGuests([]byte(bridgeIfconfig))
	want := map[string]string{"1a:c0:9b:92:c6:ce": "vmenet2", "e6:a3:6c:32:91:36": "vmenet1"}
	if len(got) != len(want) {
		t.Fatalf("ParseBridgeGuests() = %v, want %v (other ports are not guests)", got, want)
	}
	for mac, nic := range want {
		if got[mac] != nic {
			t.Fatalf("ParseBridgeGuests()[%s] = %q, want %q", mac, got[mac], nic)
		}
	}
}

func TestParseVirtualNICsReadsTheGuestsViewOfEachVmenet(t *testing.T) {
	got, err := ParseVirtualNICs([]byte(hostNetstat))
	if err != nil {
		t.Fatalf("ParseVirtualNICs() = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ParseVirtualNICs() = %v, want only the two vmenet link rows", got)
	}
	// The host receives what the guest transmits, and sends what the guest receives.
	if c := got["vmenet1"]; c.ReceiveBytes != 1807739328 || c.TransmitBytes != 32389634 {
		t.Fatalf("vmenet1 = %+v, want receive=Obytes transmit=Ibytes", c)
	}
	if c := got["vmenet2"]; c.ReceiveBytes != 586478361 || c.TransmitBytes != 7627686 {
		t.Fatalf("vmenet2 = %+v", c)
	}
}

// TestUnreadableHostCountersAreAnErrorNeverEmpty is AGENTS.md rule 4: a table
// the parser cannot read is not a host with no guests.
func TestUnreadableHostCountersAreAnErrorNeverEmpty(t *testing.T) {
	for name, output := range map[string]string{
		"empty":         "",
		"no byte cols":  "Name Mtu Network Address\nvmenet1 1500 <Link#24> 26:90:03:af:2e:02\n",
		"bad number":    "Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll\nvmenet1 1500 <Link#24> 26:90:03:af:2e:02 1 0 x 1 0 2 0\n",
		"short row":     "Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll\nvmenet1 1500 <Link#24> 1 0\n",
		"negative byte": "Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll\nvmenet1 1500 <Link#24> 26:90:03:af:2e:02 1 0 -5 1 0 2 0\n",
	} {
		if got, err := ParseVirtualNICs([]byte(output)); err == nil {
			t.Fatalf("%s: ParseVirtualNICs() = %v, want an error", name, got)
		}
	}
	// A host with a readable table and no vmenet is a host with no guests.
	got, err := ParseVirtualNICs([]byte("Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll\nen0 1500 <Link#4> a4:7e:fa:27:93:11 1 0 2 1 0 3 0\n"))
	if err != nil || len(got) != 0 {
		t.Fatalf("a readable table without vmenet = %v, %v", got, err)
	}
}
