package tart

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const hostBridgeListing = `bridge100: flags=8a63<UP> mtu 1500
	member: vmenet1 flags=20803<LEARNING>
	Address cache:
		e6:a3:6c:32:91:36 Vlan1 vmenet1 1200 flags=0<>
`

const hostNetstatTable = `Name       Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
en0        1500  <Link#4>    a4:7e:fa:27:93:11  1234567     0  314828521   654321     0   50000000     0
vmenet1    1500  <Link#24>   26:90:03:af:2e:02   402185     0   32389634  1252561     0 1807739328     0
`

type hostCommands struct {
	mu     sync.Mutex
	calls  []string
	output map[string]string
	err    map[string]error
}

func (h *hostCommands) run(_ context.Context, binary string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{binary}, args...), " ")
	h.mu.Lock()
	h.calls = append(h.calls, key)
	h.mu.Unlock()
	if err := h.err[key]; err != nil {
		return nil, err
	}
	return []byte(h.output[key]), nil
}

func hostFixture(t *testing.T, names ...string) (HostTraffic, *hostCommands) {
	t.Helper()
	home := t.TempDir()
	macs := map[string]string{"trf-linux-4x8-aa": "E6:A3:6C:32:91:36", "trf-linux-4x8-bb": "1a:c0:9b:92:c6:ce"}
	for _, name := range names {
		dir := filepath.Join(home, "vms", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"macAddress" : "`+macs[name]+`","cpuCount":4}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commands := &hostCommands{output: map[string]string{
		"ifconfig -l":        "lo0 en0 bridge0 bridge100 vmenet1",
		"ifconfig bridge0":   "bridge0: flags=8863<UP>\n",
		"ifconfig bridge100": hostBridgeListing,
		"netstat -ibn":       hostNetstatTable,
	}, err: map[string]error{}}
	return HostTraffic{Run: commands.run, Home: home}, commands
}

func TestHostTrafficReadsAGuestsCountersFromItsVmenetWithoutTouchingTheGuest(t *testing.T) {
	traffic, commands := hostFixture(t, "trf-linux-4x8-aa", "trf-linux-4x8-bb")
	got, err := traffic.Read(context.Background(), []string{"trf-linux-4x8-aa", "trf-linux-4x8-bb", "trf-linux-4x8-gone"})
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Read() = %v, want only the guest whose MAC the bridge learned", got)
	}
	if c := got["trf-linux-4x8-aa"]; c.ReceiveBytes != 1807739328 || c.TransmitBytes != 32389634 {
		t.Fatalf("counters = %+v", c)
	}
	for _, call := range commands.calls {
		if strings.Contains(call, "tart") || strings.Contains(call, "exec") {
			t.Fatalf("a host read must never touch a guest: %q", call)
		}
	}
}

func TestHostTrafficReadingTheHostFailsClosed(t *testing.T) {
	for _, command := range []string{"ifconfig -l", "ifconfig bridge100", "netstat -ibn"} {
		traffic, commands := hostFixture(t, "trf-linux-4x8-aa")
		commands.err[command] = errors.New("boom")
		if got, err := traffic.Read(context.Background(), []string{"trf-linux-4x8-aa"}); err == nil || got != nil {
			t.Fatalf("%s failing: Read() = %v, %v, want an error and no counters", command, got, err)
		}
	}
	traffic, commands := hostFixture(t, "trf-linux-4x8-aa")
	commands.output["netstat -ibn"] = "garbage"
	if got, err := traffic.Read(context.Background(), []string{"trf-linux-4x8-aa"}); err == nil || got != nil {
		t.Fatalf("an unreadable table: Read() = %v, %v", got, err)
	}
}

func TestHostTrafficSkipsAGuestWithoutAReadableMAC(t *testing.T) {
	traffic, _ := hostFixture(t)
	home := traffic.Home
	if err := os.MkdirAll(filepath.Join(home, "vms", "trf-linux-4x8-aa"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "vms", "trf-linux-4x8-aa", "config.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := traffic.Read(context.Background(), []string{"trf-linux-4x8-aa", "../escape", ""})
	if err != nil || len(got) != 0 {
		t.Fatalf("Read() = %v, %v, want nothing for an unreadable or invalid name", got, err)
	}
}

func TestHostTrafficHonoursTheContext(t *testing.T) {
	traffic, _ := hostFixture(t, "trf-linux-4x8-aa")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := traffic.Read(ctx, []string{"trf-linux-4x8-aa"}); err == nil || got != nil {
		t.Fatalf("a cancelled read = %v, %v", got, err)
	}
}

func TestHostTrafficRunsRealHostCommandsByDefault(t *testing.T) {
	out, err := (HostTraffic{}).run(context.Background(), "true")
	if err != nil || len(out) != 0 {
		t.Fatalf("run(true) = %q, %v", out, err)
	}
}

func TestHostTrafficWithoutAKnownTartHomeSeesNoGuest(t *testing.T) {
	t.Setenv("TART_HOME", "")
	t.Setenv("HOME", "")
	traffic, _ := hostFixture(t)
	traffic.Home = ""
	got, err := traffic.Read(context.Background(), []string{"trf-linux-4x8-aa"})
	if err != nil || len(got) != 0 {
		t.Fatalf("Read() = %v, %v, want no guest when tart's home is unknown", got, err)
	}
}
