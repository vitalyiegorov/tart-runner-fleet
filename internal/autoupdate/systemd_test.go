package autoupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// systemdControllerTemplate is the shape of the controller unit a release
// ships: every argument quoted, and every node-specific value a placeholder.
func systemdControllerTemplate(mode string) string {
	return `[Unit]
Description=tart-runner-fleet controller (` + mode + `)

[Service]
ExecStart="__RELEASE_DIR__/fleet" run "--mode=` + mode + `" "--config=__STATE_DIR__/fleet.json" "--database=__STATE_DIR__/fleet.db"
StandardOutput=append:__STATE_DIR__/fleet.stdout.log

[Install]
WantedBy=default.target
`
}

const systemdUpdaterTemplate = `[Unit]
Description=tart-runner-fleet forward-only production release check

[Service]
Type=oneshot
ExecStart="__RELEASE_DIR__/fleet" update apply-latest "--repo" "__REPOSITORY__" "--root" "__ROOT__" "--state-dir" "__STATE_DIR__" "--launch-agents-dir" "__UNITS_DIR__" "--mode" "__MODE__" "--config" "__STATE_DIR__/fleet.json" "--endpoint" "__ENDPOINT__" "--domain" "user" "--confirm" "automatic-release-update"
`

const systemdTimerTemplate = `[Unit]
Description=tart-runner-fleet five-minute production release poll
Requires=tart-runner-fleet-updater.service

[Timer]
Unit=tart-runner-fleet-updater.service
OnUnitActiveSec=300

[Install]
WantedBy=timers.target
`

func makeSystemdRelease(t *testing.T, root, version string) string {
	t.Helper()
	dir := filepath.Join(root, "releases", version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"RELEASE_VERSION": []byte(version + "\n"), "fleet": []byte("control-plane-" + version),
		systemdAuthorityUnit: []byte(systemdControllerTemplate("authority")),
		systemdObserveUnit:   []byte(systemdControllerTemplate("observe")),
		systemdUpdaterUnit:   []byte(systemdUpdaterTemplate),
		systemdUpdaterTimer:  []byte(systemdTimerTemplate),
	}
	var sums strings.Builder
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Only the executable, the identity manifest, and the authority unit are the
	// generation's verified contents, exactly as Target.ServiceDefinition says.
	for _, name := range []string{"RELEASE_VERSION", "fleet", systemdAuthorityUnit} {
		digest := sha256.Sum256(files[name])
		sums.WriteString(hex.EncodeToString(digest[:]) + "  " + name + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func systemdFixture(t *testing.T) (*SystemdHost, *fakeCommand, Generation, Generation, string) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	units := filepath.Join(root, "units")
	for _, dir := range []string{state, units} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(state, "fleet.json")
	if err := os.WriteFile(configPath, []byte(`{"valid":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	v1 := Generation{Version: "v1", Mode: "authority", ReleaseDir: makeSystemdRelease(t, root, "v1"), ConfigPath: configPath, Endpoint: "unix:///state/fleet.sock"}
	v2 := Generation{Version: "v2", Mode: "authority", ReleaseDir: makeSystemdRelease(t, root, "v2"), ConfigPath: configPath, Endpoint: "unix:///state/fleet.sock"}
	installed, _ := json.Marshal(v1)
	if err := os.WriteFile(filepath.Join(state, InstalledGenerationFile), installed, 0o600); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(units, systemdAuthorityUnit)
	if err := os.WriteFile(canonical, []byte(systemdInstalledUnit(v1)), 0o600); err != nil {
		t.Fatal(err)
	}
	command := &fakeCommand{
		ready:   `{"data":{"controllerVersion":"v2","controllerMode":"authority","ready":{"ok":true}}}`,
		current: `{"data":{"controllerVersion":"v1","controllerMode":"authority","ready":{"ok":true},"queues":[],"instances":[],"operations":{"retrying":0,"dead":0}}}`,
	}
	host, err := NewSystemdHost(LocalHostConfig{RootDir: root, StateDir: state, LaunchAgentsDir: units,
		Domain: "user", Repository: "owner/repo", ReadyAttempts: 1, ReadyDelay: time.Millisecond}, command)
	if err != nil {
		t.Fatal(err)
	}
	return host, command, v1, v2, canonical
}

// systemdInstalledUnit is a canonical unit as a node already running generation
// carries it: rendered, with no placeholder left.
func systemdInstalledUnit(generation Generation) string {
	return `[Service]
ExecStart="` + generation.ReleaseDir + `/fleet" run "--mode=` + generation.Mode + `" "--config=` + generation.ConfigPath + `"
`
}

func systemctlCalls(command *fakeCommand) []string {
	var calls []string
	for _, call := range command.calls {
		if strings.HasPrefix(call, "systemctl ") {
			calls = append(calls, call)
		}
	}
	return calls
}

func requireCalls(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("systemctl calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestSystemdHostDrivesOnlyAUserServiceManager(t *testing.T) {
	valid := LocalHostConfig{RootDir: "/root", StateDir: "/state", LaunchAgentsDir: "/units", Domain: "user",
		Repository: "owner/repo", ReadyAttempts: 1, ReadyDelay: time.Second}
	for _, domain := range []string{"", "gui/501", "system", "pid/42", "user/1000", "systemd"} {
		unsupervised := valid
		unsupervised.Domain = domain
		if _, err := NewSystemdHost(unsupervised, &fakeCommand{}); !errors.Is(err, ErrUnsupervised) {
			t.Fatalf("domain %q error=%v, want ErrUnsupervised", domain, err)
		}
	}
	invalid := valid
	invalid.Repository = "bad"
	if _, err := NewSystemdHost(invalid, &fakeCommand{}); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("invalid configuration error=%v", err)
	}
	host, err := NewSystemdHost(valid, &fakeCommand{})
	if err != nil || host.updateInterval != 5*time.Minute {
		t.Fatalf("host=%+v err=%v", host, err)
	}
}

func TestNewHostSelectsTheTransactionTheDomainNames(t *testing.T) {
	cfg := LocalHostConfig{RootDir: "/root", StateDir: "/state", LaunchAgentsDir: "/units",
		Repository: "owner/repo", ReadyAttempts: 1, ReadyDelay: time.Second}
	cfg.Domain = " user "
	host, err := NewHost(cfg, &fakeCommand{})
	if err != nil {
		t.Fatal(err)
	}
	if _, systemd := host.(*SystemdHost); !systemd {
		t.Fatalf("user domain host=%T", host)
	}
	cfg.Domain = "gui/501"
	host, err = NewHost(cfg, &fakeCommand{})
	if err != nil {
		t.Fatal(err)
	}
	if _, launchd := host.(*LocalHost); !launchd {
		t.Fatalf("launchd domain host=%T", host)
	}
	cfg.Domain = "systemd"
	if _, err := NewHost(cfg, &fakeCommand{}); !errors.Is(err, ErrUnsupervised) {
		t.Fatalf("unsupervised domain error=%v", err)
	}
	cfg.Domain = "user"
	cfg.ReadyAttempts = 0
	if _, err := NewHost(cfg, &fakeCommand{}); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("invalid systemd configuration error=%v", err)
	}
}

func TestSystemdHostPromotesAGenerationAndArmsItsTimer(t *testing.T) {
	host, command, _, candidate, canonical := systemdFixture(t)

	if err := (Controller{Host: host}).Apply(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(canonical)
	if err != nil || !strings.Contains(string(body), `ExecStart="`+candidate.ReleaseDir+`/fleet"`) ||
		!strings.Contains(string(body), `"--config=`+candidate.ConfigPath+`"`) || strings.Contains(string(body), "__") {
		t.Fatalf("canonical unit=%q err=%v", body, err)
	}
	current, err := host.Current(context.Background())
	if err != nil || current != candidate {
		t.Fatalf("installed=%+v err=%v", current, err)
	}
	link, err := os.Readlink(filepath.Join(host.rootDir, CurrentGenerationLink))
	if err != nil || link != candidate.ReleaseDir {
		t.Fatalf("current link=%q err=%v", link, err)
	}
	if _, err := os.Stat(filepath.Join(host.stateDir, UpdateJournalFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal survived commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(host.rootDir, "systemd", candidate.Version, systemdAuthorityUnit)); err != nil {
		t.Fatalf("prepared generation unit: %v", err)
	}
	updater, err := os.ReadFile(filepath.Join(host.unitsDir, systemdUpdaterUnit))
	if err != nil || !strings.Contains(string(updater), `"--root" "`+host.rootDir+`"`) ||
		!strings.Contains(string(updater), `"--launch-agents-dir" "`+host.unitsDir+`"`) ||
		!strings.Contains(string(updater), `"--repo" "owner/repo"`) ||
		!strings.Contains(string(updater), `"--endpoint" "`+candidate.Endpoint+`"`) ||
		!strings.Contains(string(updater), `"--config" "`+candidate.ConfigPath+`"`) ||
		!strings.Contains(string(updater), `"--mode" "authority"`) {
		t.Fatalf("updater unit=%q err=%v", updater, err)
	}
	if _, err := os.Stat(filepath.Join(host.unitsDir, systemdUpdaterTimer)); err != nil {
		t.Fatalf("updater timer: %v", err)
	}
	requireCalls(t, systemctlCalls(command), []string{
		"systemctl --user daemon-reload",
		"systemctl --user restart " + systemdAuthorityUnit,
		"systemctl --user daemon-reload",
		"systemctl --user enable --now " + systemdUpdaterTimer,
	})
	// The updater service is the one-shot that may be running this transaction.
	// Nothing may ever restart or stop it.
	for _, call := range command.calls {
		for _, forbidden := range []string{"restart " + systemdUpdaterUnit, "stop " + systemdUpdaterUnit, "start " + systemdUpdaterUnit} {
			if strings.Contains(call, forbidden) {
				t.Fatalf("transaction terminated its own updater: %s", call)
			}
		}
	}
}

func TestSystemdHostRendersTheModeUnitAndTheCandidateConfiguration(t *testing.T) {
	host, _, current, candidate, _ := systemdFixture(t)
	candidate.ConfigPath = filepath.Join(host.stateDir, "profiles", "geekom-2x4.json")

	if err := host.Prepare(context.Background(), current, candidate); err != nil {
		t.Fatal(err)
	}
	journal, err := readUpdateJournal(host.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := os.ReadFile(journal.PreparedPlist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prepared), `"--config=`+candidate.ConfigPath+`"`) ||
		strings.Contains(string(prepared), "--config="+filepath.Join(host.stateDir, "fleet.json")) {
		t.Fatalf("prepared unit did not carry the candidate configuration: %s", prepared)
	}
	if journal.Current != current || journal.Candidate != candidate || journal.HadUpdater || journal.HadTimer {
		t.Fatalf("journal=%+v", journal)
	}
	backup, err := os.ReadFile(journal.BackupPlist)
	if err != nil || string(backup) != systemdInstalledUnit(current) {
		t.Fatalf("backup=%q err=%v", backup, err)
	}
	if unit := systemdUnitName("observe"); unit != systemdObserveUnit {
		t.Fatalf("observe unit=%q", unit)
	}
}

func TestSystemdHostBacksUpAnAlreadyArmedUpdater(t *testing.T) {
	host, _, current, candidate, _ := systemdFixture(t)
	for _, unit := range []string{systemdUpdaterUnit, systemdUpdaterTimer} {
		if err := os.WriteFile(filepath.Join(host.unitsDir, unit), []byte("previous-"+unit), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := host.Prepare(context.Background(), current, candidate); err != nil {
		t.Fatal(err)
	}
	journal, err := readUpdateJournal(host.stateDir)
	if err != nil || !journal.HadUpdater || !journal.HadTimer {
		t.Fatalf("journal=%+v err=%v", journal, err)
	}
	for backup, want := range map[string]string{journal.BackupUpdater: "previous-" + systemdUpdaterUnit,
		journal.BackupTimer: "previous-" + systemdUpdaterTimer} {
		body, readErr := os.ReadFile(backup)
		if readErr != nil || string(body) != want {
			t.Fatalf("backup %s=%q err=%v", backup, body, readErr)
		}
	}
}

func TestSystemdHostRefusesToPrepareWhatItCannotRenderFaithfully(t *testing.T) {
	for _, test := range []struct {
		name    string
		arrange func(*testing.T, *SystemdHost, *Generation)
		want    error
	}{
		{name: "busy fleet", want: ErrBusy, arrange: func(_ *testing.T, _ *SystemdHost, _ *Generation) {}},
		{name: "missing template", want: os.ErrNotExist, arrange: func(t *testing.T, _ *SystemdHost, candidate *Generation) {
			if err := os.Remove(filepath.Join(candidate.ReleaseDir, systemdAuthorityUnit)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unknown placeholder", want: ErrInvalidGeneration, arrange: func(t *testing.T, _ *SystemdHost, candidate *Generation) {
			body := systemdControllerTemplate("authority") + "Environment=TRF_NODE=__NODE__\n"
			if err := os.WriteFile(filepath.Join(candidate.ReleaseDir, systemdAuthorityUnit), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unrendered configuration", want: ErrInvalidGeneration, arrange: func(t *testing.T, _ *SystemdHost, candidate *Generation) {
			body := strings.ReplaceAll(systemdControllerTemplate("authority"), `"--config=__STATE_DIR__/fleet.json"`, `"--config=/etc/fleet.json"`)
			if err := os.WriteFile(filepath.Join(candidate.ReleaseDir, systemdAuthorityUnit), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "quoted configuration path", want: ErrInvalidGeneration, arrange: func(_ *testing.T, host *SystemdHost, candidate *Generation) {
			candidate.ConfigPath = filepath.Join(host.stateDir, `fleet".json`)
		}},
		{name: "newline in release path", want: ErrInvalidGeneration, arrange: func(_ *testing.T, _ *SystemdHost, candidate *Generation) {
			candidate.ReleaseDir += "\nExecStartPost=/bin/false"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			host, command, current, candidate, canonical := systemdFixture(t)
			if test.name == "busy fleet" {
				command.current = `{"data":{"controllerVersion":"v1","controllerMode":"authority","instances":[{"count":1}],"operations":{"retrying":0}}}`
			}
			test.arrange(t, host, &candidate)
			if err := host.Prepare(context.Background(), current, candidate); !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
			body, err := os.ReadFile(canonical)
			if err != nil || string(body) != systemdInstalledUnit(current) {
				t.Fatalf("refused preparation moved the canonical unit: %q err=%v", body, err)
			}
			if calls := systemctlCalls(command); len(calls) != 0 {
				t.Fatalf("refused preparation touched systemd: %v", calls)
			}
		})
	}
}

func TestSystemdHostValidateRefusesAGenerationItCannotProve(t *testing.T) {
	t.Run("identity", func(t *testing.T) {
		host, _, _, candidate, _ := systemdFixture(t)
		if err := os.WriteFile(filepath.Join(candidate.ReleaseDir, "RELEASE_VERSION"), []byte("v9\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := host.Validate(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("identity error=%v", err)
		}
	})
	t.Run("authority unit checksum", func(t *testing.T) {
		host, _, _, candidate, _ := systemdFixture(t)
		if err := os.WriteFile(filepath.Join(candidate.ReleaseDir, systemdAuthorityUnit), []byte("[Unit]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := host.Validate(context.Background(), candidate); !errors.Is(err, ErrChecksum) {
			t.Fatalf("checksum error=%v", err)
		}
	})
	t.Run("configuration", func(t *testing.T) {
		host, command, _, candidate, _ := systemdFixture(t)
		command.fail = map[string]error{"config validate": errors.New("invalid configuration")}
		if err := host.Validate(context.Background(), candidate); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	})
}

func TestSystemdActivateRefusesAJournalNamingAnotherGeneration(t *testing.T) {
	host, command, current, candidate, _ := systemdFixture(t)
	if err := host.Prepare(context.Background(), current, candidate); err != nil {
		t.Fatal(err)
	}
	other := candidate
	other.Version = "v3"
	if err := host.Activate(context.Background(), other); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("foreign candidate error=%v", err)
	}
	if calls := systemctlCalls(command); len(calls) != 0 {
		t.Fatalf("foreign activation touched systemd: %v", calls)
	}
}

func TestSystemdActivateFailsClosedAtEverySystemdBoundary(t *testing.T) {
	for _, needle := range []string{"systemctl --user daemon-reload", "systemctl --user restart"} {
		t.Run(needle, func(t *testing.T) {
			host, command, current, candidate, _ := systemdFixture(t)
			if err := host.Prepare(context.Background(), current, candidate); err != nil {
				t.Fatal(err)
			}
			command.fail = map[string]error{needle: errors.New("systemd refused")}
			if err := host.Activate(context.Background(), candidate); err == nil {
				t.Fatal("systemd failure accepted")
			}
		})
	}
}

func TestSystemdRollbackRestoresTheUnitAndDisarmsATimerItNeverHad(t *testing.T) {
	host, command, current, candidate, canonical := systemdFixture(t)
	command.readyErr = errors.New("offline")

	if err := (Controller{Host: host}).Apply(context.Background(), candidate); err == nil {
		t.Fatal("readiness failure accepted")
	}

	body, err := os.ReadFile(canonical)
	if err != nil || string(body) != systemdInstalledUnit(current) {
		t.Fatalf("rollback unit=%q err=%v", body, err)
	}
	installed, err := host.Current(context.Background())
	if err != nil || installed != current {
		t.Fatalf("installed=%+v err=%v", installed, err)
	}
	link, err := os.Readlink(filepath.Join(host.rootDir, CurrentGenerationLink))
	if err != nil || link != current.ReleaseDir {
		t.Fatalf("rollback link=%q err=%v", link, err)
	}
	for _, unit := range []string{systemdUpdaterUnit, systemdUpdaterTimer} {
		if _, err := os.Stat(filepath.Join(host.unitsDir, unit)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rollback left %s behind: %v", unit, err)
		}
	}
	if _, err := os.Stat(filepath.Join(host.stateDir, UpdateJournalFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal survived rollback: %v", err)
	}
	requireCalls(t, systemctlCalls(command), []string{
		"systemctl --user daemon-reload",
		"systemctl --user restart " + systemdAuthorityUnit,
		"systemctl --user daemon-reload",
		"systemctl --user restart " + systemdAuthorityUnit,
		"systemctl --user disable --now " + systemdUpdaterTimer,
		"systemctl --user daemon-reload",
	})
}

func TestSystemdRollbackRearmsTheTimerTheNodeAlreadyHad(t *testing.T) {
	host, command, current, candidate, _ := systemdFixture(t)
	for _, unit := range []string{systemdUpdaterUnit, systemdUpdaterTimer} {
		if err := os.WriteFile(filepath.Join(host.unitsDir, unit), []byte("previous-"+unit), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := host.Prepare(context.Background(), current, candidate); err != nil {
		t.Fatal(err)
	}
	// The transaction got as far as writing the candidate's updater pair, which
	// is what a rollback after a failed commit finds; only the journal's backups
	// can put back the pair the node was running.
	for _, unit := range []string{systemdUpdaterUnit, systemdUpdaterTimer} {
		if err := os.WriteFile(filepath.Join(host.unitsDir, unit), []byte("candidate-"+unit), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command.calls = nil

	if err := host.Rollback(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{systemdUpdaterUnit, systemdUpdaterTimer} {
		body, err := os.ReadFile(filepath.Join(host.unitsDir, unit))
		if err != nil || string(body) != "previous-"+unit {
			t.Fatalf("restored %s=%q err=%v", unit, body, err)
		}
	}
	requireCalls(t, systemctlCalls(command), []string{
		"systemctl --user daemon-reload",
		"systemctl --user restart " + systemdAuthorityUnit,
		"systemctl --user daemon-reload",
		"systemctl --user enable --now " + systemdUpdaterTimer,
	})
}

func TestSystemdRollbackFailsClosedAtEverySystemdBoundary(t *testing.T) {
	for _, test := range []struct {
		name     string
		hadTimer bool
		fail     string
	}{
		{name: "reload", fail: "systemctl --user daemon-reload"},
		{name: "restart", fail: "systemctl --user restart"},
		{name: "rearm", hadTimer: true, fail: "systemctl --user enable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			host, command, current, candidate, _ := systemdFixture(t)
			if test.hadTimer {
				for _, unit := range []string{systemdUpdaterUnit, systemdUpdaterTimer} {
					if err := os.WriteFile(filepath.Join(host.unitsDir, unit), []byte("previous"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := host.Prepare(context.Background(), current, candidate); err != nil {
				t.Fatal(err)
			}
			command.fail = map[string]error{test.fail: errors.New("systemd refused")}
			if err := host.Rollback(context.Background(), current); err == nil {
				t.Fatal("systemd failure accepted")
			}
		})
	}
}

func TestSystemdAdoptEnrollsARunningGenerationOnce(t *testing.T) {
	host, command, _, candidate, canonical := systemdFixture(t)
	if err := os.WriteFile(canonical, []byte(systemdInstalledUnit(candidate)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := host.Adopt(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	installed, err := host.Current(context.Background())
	if err != nil || installed != candidate {
		t.Fatalf("installed=%+v err=%v", installed, err)
	}
	requireCalls(t, systemctlCalls(command), []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now " + systemdUpdaterTimer,
	})
}

func TestSystemdAdoptRefusesAnythingItCannotObserveRunning(t *testing.T) {
	t.Run("open transaction", func(t *testing.T) {
		host, _, current, candidate, _ := systemdFixture(t)
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
		if err := host.Adopt(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("open transaction error=%v", err)
		}
	})
	t.Run("unverifiable generation", func(t *testing.T) {
		host, _, _, candidate, _ := systemdFixture(t)
		candidate.ReleaseDir = filepath.Join(host.rootDir, "elsewhere", candidate.Version)
		if err := host.Adopt(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("unverifiable generation error=%v", err)
		}
	})
	t.Run("another executable", func(t *testing.T) {
		host, _, current, candidate, canonical := systemdFixture(t)
		// The incumbent unit still names the previous generation, which is exactly
		// the drift an adoption must refuse rather than record.
		if err := os.WriteFile(canonical, []byte(systemdInstalledUnit(current)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := host.Adopt(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("drifted unit error=%v", err)
		}
	})
	t.Run("commented path", func(t *testing.T) {
		host, _, _, candidate, canonical := systemdFixture(t)
		body := "# ExecStart=\"" + candidate.ReleaseDir + "/fleet\" run \"--mode=authority\"\n[Service]\nExecStart=/usr/bin/false\n"
		if err := os.WriteFile(canonical, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := host.Adopt(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("commented executable error=%v", err)
		}
	})
	t.Run("not ready", func(t *testing.T) {
		host, command, _, candidate, canonical := systemdFixture(t)
		if err := os.WriteFile(canonical, []byte(systemdInstalledUnit(candidate)), 0o600); err != nil {
			t.Fatal(err)
		}
		command.readyErr = errors.New("offline")
		if err := host.Adopt(context.Background(), candidate); err == nil {
			t.Fatal("unready generation adopted")
		}
	})
}

func TestSystemdHandoffVerifiesTheTimerRatherThanReplacingTheUpdater(t *testing.T) {
	host, command, _, candidate, _ := systemdFixture(t)
	installed, _ := json.Marshal(candidate)
	if err := os.WriteFile(filepath.Join(host.stateDir, InstalledGenerationFile), installed, 0o600); err != nil {
		t.Fatal(err)
	}
	command.systemdShow = "ActiveState=active\n"

	if err := host.FinishUpdaterHandoff(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, systemctlCalls(command), []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now " + systemdUpdaterTimer,
		"systemctl --user show -p ActiveState " + systemdUpdaterTimer,
	})
}

func TestSystemdHandoffRefusesAnythingButACommittedGeneration(t *testing.T) {
	t.Run("open transaction", func(t *testing.T) {
		host, _, current, candidate, _ := systemdFixture(t)
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
		if err := host.FinishUpdaterHandoff(context.Background(), candidate); !errors.Is(err, ErrBusy) {
			t.Fatalf("open transaction error=%v", err)
		}
	})
	t.Run("another installed generation", func(t *testing.T) {
		host, _, _, candidate, _ := systemdFixture(t)
		if err := host.FinishUpdaterHandoff(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("uncommitted candidate error=%v", err)
		}
	})
	t.Run("timer not armed", func(t *testing.T) {
		host, command, _, candidate, _ := systemdFixture(t)
		installed, _ := json.Marshal(candidate)
		if err := os.WriteFile(filepath.Join(host.stateDir, InstalledGenerationFile), installed, 0o600); err != nil {
			t.Fatal(err)
		}
		command.systemdShow = "ActiveState=failed\n"
		if err := host.FinishUpdaterHandoff(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("unarmed timer error=%v", err)
		}
	})
	for _, needle := range []string{"systemctl --user daemon-reload", "systemctl --user enable"} {
		t.Run(needle, func(t *testing.T) {
			host, command, _, candidate, _ := systemdFixture(t)
			installed, _ := json.Marshal(candidate)
			if err := os.WriteFile(filepath.Join(host.stateDir, InstalledGenerationFile), installed, 0o600); err != nil {
				t.Fatal(err)
			}
			command.fail = map[string]error{needle: errors.New("systemd refused")}
			if err := host.FinishUpdaterHandoff(context.Background(), candidate); err == nil {
				t.Fatal("systemd failure accepted")
			}
		})
	}
}

// TestSystemdTransactionFailsClosedAtEveryFilesystemBoundary obstructs one file
// at a time, because a transaction that ignores a write it could not perform is
// how a node ends up recorded as running a generation it is not.
func TestSystemdTransactionFailsClosedAtEveryFilesystemBoundary(t *testing.T) {
	blockDirectory := func(t *testing.T, path string) {
		t.Helper()
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("prepare", func(t *testing.T) {
		for _, test := range []struct {
			name  string
			block func(*testing.T, *SystemdHost)
		}{
			{name: "prepared unit", block: func(t *testing.T, host *SystemdHost) {
				if err := os.WriteFile(filepath.Join(host.rootDir, "systemd"), []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			}},
			{name: "canonical unit", block: func(t *testing.T, host *SystemdHost) {
				if err := os.Remove(filepath.Join(host.unitsDir, systemdAuthorityUnit)); err != nil {
					t.Fatal(err)
				}
			}},
			{name: "unit backup", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.stateDir, updateBackupUnitFile))
			}},
			{name: "updater backup", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.unitsDir, systemdUpdaterUnit))
			}},
			{name: "timer backup", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.unitsDir, systemdUpdaterTimer))
			}},
			{name: "journal", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.stateDir, UpdateJournalFile))
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				host, _, current, candidate, _ := systemdFixture(t)
				test.block(t, host)
				if err := host.Prepare(context.Background(), current, candidate); err == nil {
					t.Fatal("obstructed preparation accepted")
				}
			})
		}
	})
	t.Run("activate", func(t *testing.T) {
		for _, test := range []struct {
			name  string
			block func(*testing.T, *SystemdHost)
		}{
			{name: "prepared unit", block: func(t *testing.T, host *SystemdHost) {
				journal, err := readUpdateJournal(host.stateDir)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(journal.PreparedPlist); err != nil {
					t.Fatal(err)
				}
			}},
			{name: "canonical unit", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.unitsDir, systemdAuthorityUnit))
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				host, _, current, candidate, _ := systemdFixture(t)
				if err := host.Prepare(context.Background(), current, candidate); err != nil {
					t.Fatal(err)
				}
				test.block(t, host)
				if err := host.Activate(context.Background(), candidate); err == nil {
					t.Fatal("obstructed activation accepted")
				}
			})
		}
	})
	t.Run("commit", func(t *testing.T) {
		for _, test := range []struct {
			name  string
			block func(*testing.T, *SystemdHost)
		}{
			{name: "missing template", block: func(t *testing.T, host *SystemdHost) {
				if err := os.Remove(filepath.Join(host.rootDir, "releases", "v2", systemdUpdaterUnit)); err != nil {
					t.Fatal(err)
				}
			}},
			{name: "updater unit", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.unitsDir, systemdUpdaterUnit))
			}},
			{name: "current link", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.rootDir, CurrentGenerationLink))
			}},
			{name: "installed generation", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.stateDir, InstalledGenerationFile))
			}},
			{name: "transaction artifact", block: func(t *testing.T, host *SystemdHost) {
				backup := filepath.Join(host.stateDir, updateBackupUnitFile)
				if err := os.Remove(backup); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(backup, backup); err != nil {
					t.Fatal(err)
				}
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				host, _, current, candidate, _ := systemdFixture(t)
				if err := host.Prepare(context.Background(), current, candidate); err != nil {
					t.Fatal(err)
				}
				test.block(t, host)
				if err := host.Commit(context.Background(), candidate); err == nil {
					t.Fatal("obstructed commit accepted")
				}
			})
		}
	})
	t.Run("rollback", func(t *testing.T) {
		for _, test := range []struct {
			name     string
			hadTimer bool
			block    func(*testing.T, *SystemdHost)
		}{
			{name: "no journal", block: func(t *testing.T, host *SystemdHost) {
				if err := os.Remove(filepath.Join(host.stateDir, UpdateJournalFile)); err != nil {
					t.Fatal(err)
				}
			}},
			{name: "missing backup", block: func(t *testing.T, host *SystemdHost) {
				if err := os.Remove(filepath.Join(host.stateDir, updateBackupUnitFile)); err != nil {
					t.Fatal(err)
				}
			}},
			{name: "canonical unit", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.unitsDir, systemdAuthorityUnit))
			}},
			{name: "updater removal", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.unitsDir, systemdUpdaterUnit))
			}},
			{name: "timer removal", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.unitsDir, systemdUpdaterTimer))
			}},
			{name: "updater restore", hadTimer: true, block: func(t *testing.T, host *SystemdHost) {
				if err := os.Remove(filepath.Join(host.stateDir, updateBackupUpdaterUnitFile)); err != nil {
					t.Fatal(err)
				}
			}},
			{name: "timer restore", hadTimer: true, block: func(t *testing.T, host *SystemdHost) {
				if err := os.Remove(filepath.Join(host.stateDir, updateBackupTimerFile)); err != nil {
					t.Fatal(err)
				}
			}},
			{name: "installed generation", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.stateDir, InstalledGenerationFile))
			}},
			{name: "current link", block: func(t *testing.T, host *SystemdHost) {
				blockDirectory(t, filepath.Join(host.rootDir, CurrentGenerationLink))
			}},
			{name: "transaction artifact", block: func(t *testing.T, host *SystemdHost) {
				backup := filepath.Join(host.stateDir, updateBackupUpdaterUnitFile)
				if err := os.Symlink(backup, backup); err != nil {
					t.Fatal(err)
				}
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				host, _, current, candidate, _ := systemdFixture(t)
				if test.hadTimer {
					for _, unit := range []string{systemdUpdaterUnit, systemdUpdaterTimer} {
						if err := os.WriteFile(filepath.Join(host.unitsDir, unit), []byte("previous"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := host.Prepare(context.Background(), current, candidate); err != nil {
					t.Fatal(err)
				}
				test.block(t, host)
				if err := host.Rollback(context.Background(), current); err == nil {
					t.Fatal("obstructed rollback accepted")
				}
			})
		}
	})
}

func TestSystemdRollbackKeepsAUnitTheJournalDoesNotName(t *testing.T) {
	host, _, current, candidate, canonical := systemdFixture(t)
	if err := host.Prepare(context.Background(), current, candidate); err != nil {
		t.Fatal(err)
	}
	journal, err := readUpdateJournal(host.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	// A journal written before this transaction knew how to back a unit up is
	// evidence of a candidate, not of a unit to restore; the running unit is
	// then the only one there is.
	journal.BackupPlist = ""
	body, _ := json.Marshal(journal)
	if err := atomicWrite(filepath.Join(host.stateDir, UpdateJournalFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte("running-unit"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := host.Rollback(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if restored, err := os.ReadFile(canonical); err != nil || string(restored) != "running-unit" {
		t.Fatalf("canonical unit=%q err=%v", restored, err)
	}
}

func TestSystemdRenderSubstitutesEveryValueTheTemplatesCarry(t *testing.T) {
	host, _, _, candidate, _ := systemdFixture(t)
	template := `[Service]
ExecStart="__RELEASE_DIR__/fleet" x "--root" "__ROOT__" "--state-dir" "__STATE_DIR__" "--units" "__UNITS_DIR__" "--repo" "__REPOSITORY__" "--endpoint" "__ENDPOINT__" "--mode" "__MODE__" "--interval" "__INTERVAL__"
`
	name := "tart-runner-fleet-render-probe.service"
	if err := os.WriteFile(filepath.Join(candidate.ReleaseDir, name), []byte(template), 0o600); err != nil {
		t.Fatal(err)
	}

	rendered, err := host.renderUnit(candidate, name)
	if err != nil {
		t.Fatal(err)
	}
	want := `ExecStart="` + candidate.ReleaseDir + `/fleet" x "--root" "` + host.rootDir + `" "--state-dir" "` + host.stateDir +
		`" "--units" "` + host.unitsDir + `" "--repo" "owner/repo" "--endpoint" "` + candidate.Endpoint + `" "--mode" "authority" "--interval" "5m0s"`
	if !strings.Contains(string(rendered), want) {
		t.Fatalf("rendered=%q want=%q", rendered, want)
	}
}

// systemdNthFailure fails one occurrence of a repeated command, which is the
// only way to reach a boundary a transaction crosses more than once.
type systemdNthFailure struct {
	inner      *fakeCommand
	needle     string
	occurrence int
	seen       int
}

func (c *systemdNthFailure) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if strings.Contains(name+" "+strings.Join(args, " "), c.needle) {
		c.seen++
		if c.seen == c.occurrence {
			return nil, errors.New("systemd refused")
		}
	}
	return c.inner.Run(ctx, name, args...)
}

func TestSystemdCommitFailsClosedAtEverySystemdBoundary(t *testing.T) {
	for _, needle := range []string{"systemctl --user daemon-reload", "systemctl --user enable"} {
		t.Run(needle, func(t *testing.T) {
			host, command, current, candidate, _ := systemdFixture(t)
			if err := host.Prepare(context.Background(), current, candidate); err != nil {
				t.Fatal(err)
			}
			command.fail = map[string]error{needle: errors.New("systemd refused")}
			if err := host.Commit(context.Background(), candidate); err == nil {
				t.Fatal("systemd failure accepted")
			}
			if _, err := os.Stat(filepath.Join(host.stateDir, UpdateJournalFile)); err != nil {
				t.Fatalf("failed commit cleared its own rollback evidence: %v", err)
			}
		})
	}
}

func TestSystemdRollbackFailsClosedReloadingTheRestoredUpdater(t *testing.T) {
	host, command, current, candidate, _ := systemdFixture(t)
	if err := host.Prepare(context.Background(), current, candidate); err != nil {
		t.Fatal(err)
	}
	host.command = &systemdNthFailure{inner: command, needle: "daemon-reload", occurrence: 2}

	if err := host.Rollback(context.Background(), current); err == nil {
		t.Fatal("systemd failure accepted")
	}
}
