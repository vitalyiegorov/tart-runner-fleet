package autoupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// systemdUserDomain is the domain a `systemd --user` node names, which is
	// what hostpaths.Layout.ServiceDomain reports there: the bare word `user`,
	// because every command this transaction issues carries `--user` rather than
	// a domain target the way launchctl does.
	systemdUserDomain = "user"
	// systemdAuthorityUnit is the boot definition a Linux generation must carry a
	// verified copy of (Target.ServiceDefinition), so that a generation is a
	// complete thing to boot from rather than an executable plus whatever the
	// machine happened to have.
	systemdAuthorityUnit        = "tart-runner-fleet-authority.service"
	systemdObserveUnit          = "tart-runner-fleet.service"
	systemdUpdaterUnit          = "tart-runner-fleet-updater.service"
	systemdUpdaterTimer         = "tart-runner-fleet-updater.timer"
	updateBackupUnitFile        = "update-previous.service"
	updateBackupUpdaterUnitFile = "update-previous-updater.service"
	updateBackupTimerFile       = "update-previous-updater.timer"
	// quotedConfigTemplateArgument is defaultConfigTemplateArgument as the
	// systemd templates spell it. Every argument in a unit is quoted, because
	// systemd splits an unquoted ExecStart on whitespace.
	quotedConfigTemplateArgument = `"--config" "__STATE_DIR__/fleet.json"`
)

// SystemdHost is the `systemd --user` twin of LocalHost: the same generation
// discipline — verify, stage, back up, journal, activate, prove ready, commit,
// and restore as one unit — expressed in the commands a Linux node has.
//
// Its units are not written here. They are rendered from the templates the
// release itself carries (render-systemd.sh renders the same files by hand), so
// a node never runs a unit that came from somewhere other than the generation
// it is booting.
type SystemdHost struct {
	rootDir, stateDir, unitsDir string
	target                      Target
	repository                  string
	updateInterval              time.Duration
	readyAttempts               int
	readyDelay                  time.Duration
	command                     Command
}

func NewSystemdHost(cfg LocalHostConfig, command Command) (*SystemdHost, error) {
	cfg, err := normalizeHostConfig(cfg, command)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Domain) != systemdUserDomain {
		return nil, fmt.Errorf("%w: %q", ErrUnsupervised, cfg.Domain)
	}
	return &SystemdHost{rootDir: filepath.Clean(cfg.RootDir), stateDir: filepath.Clean(cfg.StateDir),
		unitsDir: filepath.Clean(cfg.LaunchAgentsDir), target: cfg.Target, repository: cfg.Repository,
		updateInterval: cfg.UpdateInterval,
		readyAttempts:  cfg.ReadyAttempts, readyDelay: cfg.ReadyDelay, command: command}, nil
}

func (h *SystemdHost) Current(context.Context) (Generation, error) {
	return installedGeneration(h.stateDir)
}

func (h *SystemdHost) Validate(ctx context.Context, candidate Generation) error {
	// A SystemdHost is `systemd --user` supervised by construction, so the
	// definition its generation must carry is the authority unit.
	return validateCandidate(ctx, h.command, h.rootDir, candidate, h.target)
}

func (h *SystemdHost) Prepare(ctx context.Context, current, candidate Generation) error {
	if err := ensureQuiescent(ctx, h.command, current); err != nil {
		return err
	}
	unit := systemdUnitName(candidate.Mode)
	rendered, err := h.renderUnit(candidate, unit)
	if err != nil {
		return err
	}
	prepared := filepath.Join(h.rootDir, "systemd", candidate.Version, unit)
	if err := atomicWrite(prepared, rendered, 0o600); err != nil {
		return err
	}
	canonical := filepath.Join(h.unitsDir, unit)
	oldUnit, err := os.ReadFile(canonical) // #nosec G304 -- mode-enumerated unit path.
	if err != nil {
		return err
	}
	backup := filepath.Join(h.stateDir, updateBackupUnitFile)
	if err := atomicWrite(backup, oldUnit, 0o600); err != nil {
		return err
	}
	updaterBackup := filepath.Join(h.stateDir, updateBackupUpdaterUnitFile)
	hadUpdater, err := backupIfPresent(filepath.Join(h.unitsDir, systemdUpdaterUnit), updaterBackup)
	if err != nil {
		return err
	}
	timerBackup := filepath.Join(h.stateDir, updateBackupTimerFile)
	hadTimer, err := backupIfPresent(filepath.Join(h.unitsDir, systemdUpdaterTimer), timerBackup)
	if err != nil {
		return err
	}
	journal, _ := json.Marshal(updateJournal{Current: current, Candidate: candidate, PreparedPlist: prepared,
		BackupPlist: backup, BackupUpdater: updaterBackup, HadUpdater: hadUpdater,
		BackupTimer: timerBackup, HadTimer: hadTimer})
	return atomicWrite(filepath.Join(h.stateDir, UpdateJournalFile), journal, 0o600)
}

func (h *SystemdHost) Activate(ctx context.Context, candidate Generation) error {
	journal, err := readUpdateJournal(h.stateDir)
	if err != nil || journal.Candidate != candidate {
		return ErrInvalidGeneration
	}
	body, err := os.ReadFile(journal.PreparedPlist) // #nosec G304 -- journal written by Prepare.
	if err != nil {
		return err
	}
	unit := systemdUnitName(candidate.Mode)
	if err := atomicWrite(filepath.Join(h.unitsDir, unit), body, 0o600); err != nil {
		return err
	}
	if err := h.systemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("reload systemd generation: %w", err)
	}
	// No enable is needed and none is wanted: the unit is already wanted by
	// default.target from the generation this one replaces, and a controller
	// enabled by an activation that is then rolled back would keep a symlink
	// naming a unit the rollback removed. Adopt is what enrolls a node.
	if err := h.systemctl(ctx, "restart", unit); err != nil {
		return fmt.Errorf("restart candidate: %w", err)
	}
	return nil
}

func (h *SystemdHost) Ready(ctx context.Context, candidate Generation) error {
	return awaitReady(ctx, h.command, candidate, h.readyAttempts, h.readyDelay)
}

func (h *SystemdHost) Commit(ctx context.Context, candidate Generation) error {
	for _, unit := range []string{systemdUpdaterUnit, systemdUpdaterTimer} {
		rendered, err := h.renderUnit(candidate, unit)
		if err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(h.unitsDir, unit), rendered, 0o600); err != nil {
			return err
		}
	}
	if err := h.systemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("reload automatic updater: %w", err)
	}
	// The timer is started, never the service. The service is the one-shot that
	// may be executing this very commit, and restarting it would terminate the
	// caller mid-transaction — the trap launchd needs a separate handoff job to
	// escape. Starting the timer only rearms the schedule, and the next tick
	// runs the generation just written. This is why systemd needs no handoff.
	if err := h.systemctl(ctx, "enable", "--now", systemdUpdaterTimer); err != nil {
		return fmt.Errorf("enable automatic updater timer: %w", err)
	}
	if err := atomicSymlink(candidate.ReleaseDir, filepath.Join(h.rootDir, CurrentGenerationLink)); err != nil {
		return err
	}
	body, _ := json.Marshal(candidate)
	if err := atomicWrite(filepath.Join(h.stateDir, InstalledGenerationFile), body, 0o600); err != nil {
		return err
	}
	return h.clearTransaction()
}

func (h *SystemdHost) Rollback(ctx context.Context, current Generation) error {
	journal, err := readUpdateJournal(h.stateDir)
	if err != nil {
		return err
	}
	if journal.BackupPlist != "" {
		backup, readErr := os.ReadFile(journal.BackupPlist) // #nosec G304 -- journal written by Prepare.
		if readErr != nil {
			return readErr
		}
		if err := atomicWrite(filepath.Join(h.unitsDir, systemdUnitName(journal.Candidate.Mode)), backup, 0o600); err != nil {
			return err
		}
	}
	if err := h.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err := h.systemctl(ctx, "restart", systemdUnitName(current.Mode)); err != nil {
		return err
	}
	if !journal.HadTimer {
		// A node that had no timer when the transaction opened must have none
		// running after it, and it is disarmed here, while its unit file is still
		// on disk: systemd cannot stop by name a unit it can no longer load, and
		// removing the file alone would leave the timer armed in the manager. A
		// timer that was never installed refuses this, which is not a failure.
		_ = h.systemctl(ctx, "disable", "--now", systemdUpdaterTimer)
	}
	if err := h.restoreUnit(journal.HadUpdater, journal.BackupUpdater, systemdUpdaterUnit); err != nil {
		return err
	}
	if err := h.restoreUnit(journal.HadTimer, journal.BackupTimer, systemdUpdaterTimer); err != nil {
		return err
	}
	if err := h.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if journal.HadTimer {
		if err := h.systemctl(ctx, "enable", "--now", systemdUpdaterTimer); err != nil {
			return err
		}
	}
	body, _ := json.Marshal(current)
	if err := atomicWrite(filepath.Join(h.stateDir, InstalledGenerationFile), body, 0o600); err != nil {
		return err
	}
	if err := atomicSymlink(current.ReleaseDir, filepath.Join(h.rootDir, CurrentGenerationLink)); err != nil {
		return err
	}
	return h.clearTransaction()
}

// Adopt records an already-running, already-persisted generation and enables
// the periodic updater. It cannot be used to start or change authority.
func (h *SystemdHost) Adopt(ctx context.Context, candidate Generation) error {
	if _, err := os.Stat(filepath.Join(h.stateDir, UpdateJournalFile)); err == nil || !errors.Is(err, os.ErrNotExist) {
		return ErrInvalidGeneration
	}
	if err := h.Validate(ctx, candidate); err != nil {
		return err
	}
	canonical, err := os.ReadFile(filepath.Join(h.unitsDir, systemdUnitName(candidate.Mode))) // #nosec G304 -- mode-enumerated unit path.
	if err != nil || !namesUnitArgument(string(canonical), candidate.ReleaseDir+"/fleet") ||
		!namesUnitArgument(string(canonical), "--mode="+candidate.Mode) {
		return ErrInvalidGeneration
	}
	if err := h.Ready(ctx, candidate); err != nil {
		return err
	}
	return h.Commit(ctx, candidate)
}

// FinishUpdaterHandoff exists here only to answer the unit the release still
// ships. A systemd commit never has to replace the process running it, so there
// is nothing to hand off; what remains is the assertion the handoff was for —
// that the committed generation's periodic updater really is armed. It refuses
// until Commit has durably published this exact candidate.
func (h *SystemdHost) FinishUpdaterHandoff(ctx context.Context, candidate Generation) error {
	if err := requireCommittedGeneration(ctx, h, h.rootDir, h.stateDir, candidate); err != nil {
		return err
	}
	if err := h.systemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("reload automatic updater: %w", err)
	}
	if err := h.systemctl(ctx, "enable", "--now", systemdUpdaterTimer); err != nil {
		return fmt.Errorf("enable automatic updater timer: %w", err)
	}
	// `enable --now` reports success once the manager accepted the job, which is
	// not the same claim as the timer being armed — a timer whose unit failed to
	// load leaves automatic updates silently off.
	state, err := h.command.Run(ctx, "systemctl", "--user", "show", "-p", "ActiveState", systemdUpdaterTimer)
	if err != nil || !namesProperty(string(state), "ActiveState", "active") {
		return fmt.Errorf("verify automatic updater timer: %w", ErrInvalidGeneration)
	}
	return nil
}

func (h *SystemdHost) clearTransaction() error {
	return clearTransaction(h.stateDir, updateBackupUnitFile, updateBackupUpdaterUnitFile, updateBackupTimerFile)
}

// restoreUnit puts one of the updater's two units back the way the transaction
// found it: the backed-up body if there was one, and no file at all if there
// was not.
func (h *SystemdHost) restoreUnit(had bool, backup, unit string) error {
	path := filepath.Join(h.unitsDir, unit)
	if !had {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	body, err := os.ReadFile(backup) // #nosec G304 -- journal written by Prepare.
	if err != nil {
		return err
	}
	return atomicWrite(path, body, 0o600)
}

func (h *SystemdHost) systemctl(ctx context.Context, args ...string) error {
	_, err := h.command.Run(ctx, "systemctl", append([]string{"--user"}, args...)...)
	return err
}

// renderUnit renders one of the release's own unit templates for this
// generation, exactly as render-systemd.sh does, and refuses anything it cannot
// render faithfully.
func (h *SystemdHost) renderUnit(candidate Generation, name string) ([]byte, error) {
	// A unit is executed by the service manager with no shell in between, but
	// systemd splits unquoted words itself, so every argument in the templates is
	// quoted. Refusing a quote or a newline in the two paths this host
	// substitutes is what makes that quoting unescapable.
	if strings.ContainsAny(candidate.ReleaseDir, "\"\n") || strings.ContainsAny(candidate.ConfigPath, "\"\n") {
		return nil, ErrInvalidGeneration
	}
	body, err := os.ReadFile(filepath.Join(candidate.ReleaseDir, name)) // #nosec G304 -- enumerated name under a validated release directory.
	if err != nil {
		return nil, err
	}
	rendered := string(body)
	// A configuration path is a property of the generation rather than of the
	// release, so it is not a placeholder: a template carries the default and
	// exactly one occurrence of it is swapped. Every unit that starts the
	// executable names a configuration once; the timer starts nothing and names
	// none. Anything else is a template this transaction cannot render.
	configured := strings.Count(rendered, defaultConfigTemplateArgument) + strings.Count(rendered, quotedConfigTemplateArgument)
	if configured > 1 || configured != strings.Count(rendered, "--config") {
		return nil, ErrInvalidGeneration
	}
	rendered = strings.Replace(rendered, defaultConfigTemplateArgument, "--config="+candidate.ConfigPath, 1)
	rendered = strings.Replace(rendered, quotedConfigTemplateArgument, `"--config" "`+candidate.ConfigPath+`"`, 1)
	rendered = strings.NewReplacer(
		"__RELEASE_DIR__", candidate.ReleaseDir,
		"__STATE_DIR__", h.stateDir,
		"__ROOT__", h.rootDir,
		"__UNITS_DIR__", h.unitsDir,
		"__REPOSITORY__", h.repository,
		"__ENDPOINT__", candidate.Endpoint,
		"__MODE__", candidate.Mode,
		"__INTERVAL__", h.updateInterval.String()).Replace(rendered)
	// A retained placeholder is a template this build does not know all of, and
	// a unit half-rendered is a daemon pointed at a path that does not exist.
	if strings.Contains(rendered, "__") {
		return nil, ErrInvalidGeneration
	}
	return []byte(rendered), nil
}

// systemdUnitName is the unit a mode is supervised by. Observe keeps the plain
// name because it is the mode a node installs first.
func systemdUnitName(mode string) string {
	if mode == "observe" {
		return systemdObserveUnit
	}
	return "tart-runner-fleet-" + mode + ".service"
}

// namesUnitArgument reports whether a unit's ExecStart carries value as a
// complete quoted argument. ADR 0019 merged `fleetd` and `fleetctl` into
// `fleet`, a strict prefix of both retired names, so a bare substring test on a
// path cannot prove which executable a boot tuple names; ADR 0011 requires the
// exact generation executable. Only ExecStart is consulted, because a comment
// in the same unit naming a path proves nothing about what will run.
func namesUnitArgument(body, value string) bool {
	for _, line := range strings.Split(body, "\n") {
		if remainder, isExec := strings.CutPrefix(strings.TrimSpace(line), "ExecStart"); isExec {
			if strings.Contains(remainder, `"`+value+`"`) {
				return true
			}
		}
	}
	return false
}

// namesProperty reports whether `systemctl show -p` output assigns property
// exactly value. A whole-line comparison keeps the gate exact where a substring
// test would accept `ActiveState=activating` as active.
func namesProperty(output, property, value string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == property+"="+value {
			return true
		}
	}
	return false
}
