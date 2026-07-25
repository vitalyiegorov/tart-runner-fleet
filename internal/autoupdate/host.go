package autoupdate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	InstalledGenerationFile         = "installed-generation.json"
	CurrentGenerationLink           = "current"
	UpdateJournalFile               = "update-transaction.json"
	CanonicalPlist                  = "com.vitalyiegorov.tart-runner-fleet.plist"
	UpdaterPlist                    = "com.vitalyiegorov.tart-runner-fleet.updater.plist"
	UpdaterHandoffPlist             = "com.vitalyiegorov.tart-runner-fleet.updater-handoff.plist"
	updateBackupFile                = "update-previous.plist"
	updateBackupUpdaterFile         = "update-previous-updater.plist"
	minimumLaunchdBootstrapAttempts = 3
	defaultConfigTemplateArgument   = "--config=__STATE_DIR__/fleet.json"
)

var (
	ErrChecksum = errors.New("autoupdate: release checksum mismatch")
	ErrBusy     = errors.New("autoupdate: fleet is not quiescent")
	safeVersion = regexp.MustCompile(`^v[0-9A-Za-z][0-9A-Za-z.+_-]{0,127}$`)
)

type Command interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type LocalHostConfig struct {
	RootDir, StateDir, LaunchAgentsDir string
	Domain                             string
	Repository                         string
	UpdateInterval                     time.Duration
	ReadyAttempts                      int
	ReadyDelay                         time.Duration
}

type LocalHost struct {
	rootDir, stateDir, launchAgentsDir string
	domain                             string
	repository                         string
	updateInterval                     time.Duration
	readyAttempts                      int
	readyDelay                         time.Duration
	command                            Command
}

// Adopt records an already-running, already-persisted generation and enables
// the periodic updater. It cannot be used to start or change authority.
func (h *LocalHost) Adopt(ctx context.Context, candidate Generation) error {
	if _, err := os.Stat(filepath.Join(h.stateDir, UpdateJournalFile)); err == nil || !errors.Is(err, os.ErrNotExist) {
		return ErrInvalidGeneration
	}
	if err := h.Validate(ctx, candidate); err != nil {
		return err
	}
	canonical, err := os.ReadFile(filepath.Join(h.launchAgentsDir, CanonicalPlist)) // #nosec G304 -- fixed launch agent path.
	if err != nil || !namesPlistArgument(string(canonical), candidate.ReleaseDir+"/fleet") ||
		!namesPlistArgument(string(canonical), "--mode="+candidate.Mode) {
		return ErrInvalidGeneration
	}
	if err := h.Ready(ctx, candidate); err != nil {
		return err
	}
	return h.Commit(ctx, candidate)
}

func NewLocalHost(cfg LocalHostConfig, command Command) (*LocalHost, error) {
	if command == nil || !filepath.IsAbs(cfg.RootDir) || !filepath.IsAbs(cfg.StateDir) || !filepath.IsAbs(cfg.LaunchAgentsDir) ||
		strings.TrimSpace(cfg.Domain) == "" || !safeRepository.MatchString(cfg.Repository) || cfg.ReadyAttempts <= 0 || cfg.ReadyDelay < 0 {
		return nil, ErrInvalidGeneration
	}
	if cfg.UpdateInterval == 0 {
		cfg.UpdateInterval = 5 * time.Minute
	}
	if cfg.UpdateInterval < time.Minute || cfg.UpdateInterval > 24*time.Hour {
		return nil, ErrInvalidGeneration
	}
	return &LocalHost{rootDir: filepath.Clean(cfg.RootDir), stateDir: filepath.Clean(cfg.StateDir),
		launchAgentsDir: filepath.Clean(cfg.LaunchAgentsDir), domain: cfg.Domain, repository: cfg.Repository,
		updateInterval: cfg.UpdateInterval,
		readyAttempts:  cfg.ReadyAttempts, readyDelay: cfg.ReadyDelay, command: command}, nil
}

func (h *LocalHost) Current(context.Context) (Generation, error) {
	file, err := os.Open(filepath.Join(h.stateDir, InstalledGenerationFile)) // #nosec G304 -- fixed state path.
	if err != nil {
		return Generation{}, err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var generation Generation
	if err := decoder.Decode(&generation); err != nil {
		return Generation{}, err
	}
	if err := generation.validate(); err != nil {
		return Generation{}, err
	}
	return generation, nil
}

func (h *LocalHost) Validate(ctx context.Context, candidate Generation) error {
	if err := candidate.validate(); err != nil {
		return err
	}
	if candidate.Mode == "canary" || !safeVersion.MatchString(candidate.Version) || filepath.Clean(candidate.ReleaseDir) != filepath.Join(h.rootDir, "releases", candidate.Version) {
		return ErrInvalidGeneration
	}
	manifest, err := os.ReadFile(filepath.Join(candidate.ReleaseDir, "RELEASE_VERSION")) // #nosec G304 -- validated immutable release path.
	if err != nil || strings.TrimSpace(string(manifest)) != candidate.Version {
		return fmt.Errorf("release identity: %w", ErrInvalidGeneration)
	}
	if err := verifyChecksums(candidate.ReleaseDir); err != nil {
		return err
	}
	if _, err := h.command.Run(ctx, filepath.Join(candidate.ReleaseDir, "fleet"), "config", "validate", "--mode", candidate.Mode, candidate.ConfigPath); err != nil {
		return fmt.Errorf("candidate config validation: %w", err)
	}
	return nil
}

func verifyChecksums(releaseDir string) error {
	file, err := os.Open(filepath.Join(releaseDir, "SHA256SUMS")) // #nosec G304 -- validated immutable release path.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	required := map[string]bool{"RELEASE_VERSION": false, "fleet": false,
		"com.vitalyiegorov.tart-runner-fleet.authority.plist": false}
	scanner := bufio.NewScanner(io.LimitReader(file, 1<<20))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 || filepath.Base(fields[1]) != fields[1] {
			return ErrChecksum
		}
		if _, tracked := required[fields[1]]; !tracked {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(releaseDir, fields[1])) // #nosec G304 -- basename constrained above.
		if readErr != nil {
			return readErr
		}
		digest := sha256.Sum256(body)
		if !strings.EqualFold(hex.EncodeToString(digest[:]), fields[0]) {
			return ErrChecksum
		}
		required[fields[1]] = true
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for _, found := range required {
		if !found {
			return ErrChecksum
		}
	}
	return nil
}

type updateJournal struct {
	Current       Generation `json:"current"`
	Candidate     Generation `json:"candidate"`
	PreparedPlist string     `json:"preparedPlist"`
	BackupPlist   string     `json:"backupPlist"`
	BackupUpdater string     `json:"backupUpdater,omitempty"`
	HadUpdater    bool       `json:"hadUpdater"`
}

func (h *LocalHost) Prepare(ctx context.Context, current, candidate Generation) error {
	if err := h.ensureQuiescent(ctx, current); err != nil {
		return err
	}
	templateName := "com.vitalyiegorov.tart-runner-fleet." + candidate.Mode + ".plist"
	if candidate.Mode == "observe" {
		templateName = CanonicalPlist
	}
	template, err := os.ReadFile(filepath.Join(candidate.ReleaseDir, templateName)) // #nosec G304 -- mode is enumerated.
	if err != nil {
		return err
	}
	if strings.Count(string(template), defaultConfigTemplateArgument) != 1 {
		return ErrInvalidGeneration
	}
	rendered := strings.Replace(string(template), defaultConfigTemplateArgument, "--config="+xmlEscape(candidate.ConfigPath), 1)
	rendered = strings.ReplaceAll(rendered, "__RELEASE_DIR__", candidate.ReleaseDir)
	rendered = strings.ReplaceAll(rendered, "__STATE_DIR__", h.stateDir)
	if strings.Contains(rendered, "__") {
		return ErrInvalidGeneration
	}
	generationDir := filepath.Join(h.rootDir, "launchd", candidate.Version)
	if err := os.MkdirAll(generationDir, 0o700); err != nil {
		return err
	}
	prepared := filepath.Join(generationDir, candidate.Mode+".plist")
	if err := atomicWrite(prepared, []byte(rendered), 0o600); err != nil {
		return err
	}
	if _, err := h.command.Run(ctx, "plutil", "-lint", prepared); err != nil {
		return fmt.Errorf("lint launchd generation: %w", err)
	}
	canonical := filepath.Join(h.launchAgentsDir, CanonicalPlist)
	oldPlist, err := os.ReadFile(canonical) // #nosec G304 -- fixed launch agent path.
	if err != nil {
		return err
	}
	backup := filepath.Join(h.stateDir, updateBackupFile)
	if err := atomicWrite(backup, oldPlist, 0o600); err != nil {
		return err
	}
	updaterPath := filepath.Join(h.launchAgentsDir, UpdaterPlist)
	updaterBackup := filepath.Join(h.stateDir, updateBackupUpdaterFile)
	hadUpdater := false
	if updater, readErr := os.ReadFile(updaterPath); readErr == nil { // #nosec G304 -- fixed LaunchAgents path.
		if writeErr := atomicWrite(updaterBackup, updater, 0o600); writeErr != nil {
			return writeErr
		}
		hadUpdater = true
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	journal, _ := json.Marshal(updateJournal{Current: current, Candidate: candidate, PreparedPlist: prepared,
		BackupPlist: backup, BackupUpdater: updaterBackup, HadUpdater: hadUpdater})
	return atomicWrite(filepath.Join(h.stateDir, UpdateJournalFile), journal, 0o600)
}

func (h *LocalHost) Activate(ctx context.Context, candidate Generation) error {
	journal, err := h.readJournal()
	if err != nil || journal.Candidate != candidate {
		return ErrInvalidGeneration
	}
	body, err := os.ReadFile(journal.PreparedPlist) // #nosec G304 -- journal written by Prepare.
	if err != nil {
		return err
	}
	canonical := filepath.Join(h.launchAgentsDir, CanonicalPlist)
	if err := atomicWrite(canonical, body, 0o600); err != nil {
		return err
	}
	label := serviceLabel(candidate.Mode)
	_, _ = h.command.Run(ctx, "launchctl", "bootout", h.domain+"/"+label)
	if err := h.bootstrapService(ctx, canonical); err != nil {
		return err
	}
	_, err = h.command.Run(ctx, "launchctl", "kickstart", "-k", h.domain+"/"+label)
	return err
}

func (h *LocalHost) Ready(ctx context.Context, candidate Generation) error {
	var last error
	for attempt := 0; attempt < h.readyAttempts; attempt++ {
		body, err := h.command.Run(ctx, filepath.Join(candidate.ReleaseDir, "fleet"), "status", "--require-ready", "--output", "json", "--endpoint", candidate.Endpoint)
		if err == nil {
			var status struct {
				Data struct {
					ControllerVersion string `json:"controllerVersion"`
					ControllerMode    string `json:"controllerMode"`
					Ready             struct {
						OK bool `json:"ok"`
					} `json:"ready"`
				} `json:"data"`
			}
			if json.Unmarshal(body, &status) == nil && status.Data.Ready.OK && status.Data.ControllerVersion == candidate.Version && status.Data.ControllerMode == candidate.Mode {
				return nil
			}
			err = errors.New("candidate identity or readiness mismatch")
		}
		last = err
		if attempt+1 < h.readyAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(h.readyDelay):
			}
		}
	}
	return last
}

func (h *LocalHost) Commit(ctx context.Context, candidate Generation) error {
	updater := h.renderUpdater(candidate)
	updaterPath := filepath.Join(h.launchAgentsDir, UpdaterPlist)
	if err := atomicWrite(updaterPath, updater, 0o600); err != nil {
		return err
	}
	updaterLabel := h.domain + "/com.vitalyiegorov.tart-runner-fleet.updater"
	if _, err := h.command.Run(ctx, "launchctl", "print", updaterLabel); err == nil {
		// A loaded updater may be the process executing this commit. Asking
		// launchd to boot it out here terminates the caller before it can
		// bootstrap the replacement. Delegate that boundary to a distinct,
		// retrying one-shot job which waits for the durable commit first.
		handoffPath := filepath.Join(h.launchAgentsDir, UpdaterHandoffPlist)
		if err := atomicWrite(handoffPath, h.renderUpdaterHandoff(candidate), 0o600); err != nil {
			return err
		}
		handoffLabel := h.domain + "/com.vitalyiegorov.tart-runner-fleet.updater-handoff"
		_, _ = h.command.Run(ctx, "launchctl", "bootout", handoffLabel)
		if err := h.bootstrapService(ctx, handoffPath); err != nil {
			return fmt.Errorf("bootstrap automatic updater handoff: %w", err)
		}
	} else if err := h.bootstrapService(ctx, updaterPath); err != nil {
		return fmt.Errorf("bootstrap automatic updater: %w", err)
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

// FinishUpdaterHandoff reloads the periodic updater from an independent
// launchd process. It refuses to act until Commit has durably published the
// exact candidate and cleared its transaction, so a failed commit or rollback
// cannot strand launchd on the wrong executable.
func (h *LocalHost) FinishUpdaterHandoff(ctx context.Context, candidate Generation) error {
	if err := candidate.validate(); err != nil || !safeVersion.MatchString(candidate.Version) ||
		filepath.Clean(candidate.ReleaseDir) != filepath.Join(h.rootDir, "releases", candidate.Version) {
		return ErrInvalidGeneration
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, UpdateJournalFile)); err == nil {
		return ErrBusy
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	current, err := h.Current(ctx)
	if err != nil || current != candidate {
		return ErrInvalidGeneration
	}
	updaterPath := filepath.Join(h.launchAgentsDir, UpdaterPlist)
	updater, err := os.ReadFile(updaterPath) // #nosec G304 -- fixed LaunchAgents path.
	if err != nil || string(updater) != string(h.renderUpdater(candidate)) {
		return ErrInvalidGeneration
	}
	updaterLabel := h.domain + "/com.vitalyiegorov.tart-runner-fleet.updater"
	if _, err := h.command.Run(ctx, "launchctl", "print", updaterLabel); err == nil {
		if _, err := h.command.Run(ctx, "launchctl", "bootout", updaterLabel); err != nil {
			return fmt.Errorf("unload automatic updater: %w", err)
		}
	}
	if err := h.bootstrapService(ctx, updaterPath); err != nil {
		return fmt.Errorf("bootstrap automatic updater: %w", err)
	}
	loaded, err := h.command.Run(ctx, "launchctl", "print", updaterLabel)
	if err != nil || !launchdNamesProgram(string(loaded), filepath.Join(candidate.ReleaseDir, "fleet")) {
		return fmt.Errorf("verify automatic updater generation: %w", ErrInvalidGeneration)
	}
	return nil
}

// namesPlistArgument reports whether a launchd plist body carries value as a
// complete `ProgramArguments` element. ADR 0019 merged `fleetd` and `fleetctl`
// into `fleet`, which is a strict prefix of both retired names, so a bare
// substring test on a path can no longer prove which executable a boot tuple
// names. ADR 0011 requires the exact generation executable, so match the element.
func namesPlistArgument(body, value string) bool {
	return strings.Contains(body, "<string>"+value+"</string>")
}

// launchdNamesProgram reports whether `launchctl print` output names program as
// a complete path. Real output carries it twice: as a `program = <path>` line and
// as argv[0] on its own line inside the `arguments = { ... }` block. Comparing a
// whole line keeps ADR 0011's gate exact, where a substring test would accept a
// job still running `<releaseDir>/fleetd`. Output that names no program at all is
// rejected rather than assumed healthy.
func launchdNamesProgram(output, program string) bool {
	for _, line := range strings.Split(output, "\n") {
		field := strings.TrimSpace(line)
		if remainder, isProgram := strings.CutPrefix(field, "program"); isProgram {
			if remainder = strings.TrimSpace(remainder); strings.HasPrefix(remainder, "=") {
				if strings.TrimSpace(strings.TrimPrefix(remainder, "=")) == program {
					return true
				}
				continue
			}
		}
		if field == program {
			return true
		}
	}
	return false
}

func (h *LocalHost) ensureQuiescent(ctx context.Context, current Generation) error {
	body, err := h.command.Run(ctx, filepath.Join(current.ReleaseDir, "fleet"), "status", "--require-ready", "--output", "json", "--endpoint", current.Endpoint)
	if err != nil {
		return err
	}
	var status struct {
		Data struct {
			ControllerVersion string `json:"controllerVersion"`
			ControllerMode    string `json:"controllerMode"`
			Queues            []struct {
				Jobs int `json:"jobs"`
			} `json:"queues"`
			Instances []struct {
				Count int `json:"count"`
			} `json:"instances"`
			Operations struct {
				Retrying int `json:"retrying"`
				Dead     int `json:"dead"`
			} `json:"operations"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &status) != nil || status.Data.ControllerVersion != current.Version || status.Data.ControllerMode != current.Mode {
		return ErrInvalidGeneration
	}
	if status.Data.Operations.Retrying != 0 || status.Data.Operations.Dead != 0 {
		return ErrBusy
	}
	for _, queue := range status.Data.Queues {
		if queue.Jobs != 0 {
			return ErrBusy
		}
	}
	for _, instance := range status.Data.Instances {
		if instance.Count != 0 {
			return ErrBusy
		}
	}
	return nil
}

func (h *LocalHost) Rollback(ctx context.Context, current Generation) error {
	journal, err := h.readJournal()
	if err != nil {
		return err
	}
	backup, err := os.ReadFile(journal.BackupPlist) // #nosec G304 -- journal written by Prepare.
	if err != nil {
		return err
	}
	_, _ = h.command.Run(ctx, "launchctl", "bootout", h.domain+"/"+serviceLabel(journal.Candidate.Mode))
	canonical := filepath.Join(h.launchAgentsDir, CanonicalPlist)
	if err := atomicWrite(canonical, backup, 0o600); err != nil {
		return err
	}
	if err := h.bootstrapService(ctx, canonical); err != nil {
		return err
	}
	if _, err := h.command.Run(ctx, "launchctl", "kickstart", "-k", h.domain+"/"+serviceLabel(current.Mode)); err != nil {
		return err
	}
	handoffPath := filepath.Join(h.launchAgentsDir, UpdaterHandoffPlist)
	_, _ = h.command.Run(ctx, "launchctl", "bootout", h.domain+"/com.vitalyiegorov.tart-runner-fleet.updater-handoff")
	if err := os.Remove(handoffPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	updaterPath := filepath.Join(h.launchAgentsDir, UpdaterPlist)
	if journal.HadUpdater {
		updater, readErr := os.ReadFile(journal.BackupUpdater) // #nosec G304 -- journal written by Prepare.
		if readErr != nil {
			return readErr
		}
		if err := atomicWrite(updaterPath, updater, 0o600); err != nil {
			return err
		}
	} else if err := os.Remove(updaterPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
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

// bootstrapService absorbs the short unload-to-load race launchd can expose
// after bootout. Retrying this single idempotent boundary is safe; the rest of
// activation and rollback remains fail-closed and is never replayed.
func (h *LocalHost) bootstrapService(ctx context.Context, plist string) error {
	attempts := h.readyAttempts
	if attempts < minimumLaunchdBootstrapAttempts {
		attempts = minimumLaunchdBootstrapAttempts
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if _, err := h.command.Run(ctx, "launchctl", "bootstrap", h.domain, plist); err == nil {
			return nil
		} else {
			last = err
		}
		if attempt+1 < attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(h.readyDelay):
			}
		}
	}
	return last
}

func (h *LocalHost) readJournal() (updateJournal, error) {
	body, err := os.ReadFile(filepath.Join(h.stateDir, UpdateJournalFile)) // #nosec G304 -- fixed state path.
	if err != nil {
		return updateJournal{}, err
	}
	var journal updateJournal
	if err := json.Unmarshal(body, &journal); err != nil {
		return updateJournal{}, err
	}
	return journal, nil
}

func (h *LocalHost) clearTransaction() error {
	// Validate every cleanup target before removing any rollback evidence, then
	// remove the journal last. The handoff treats journal absence as the durable
	// commit marker and must never observe it before cleanup is complete.
	paths := []string{filepath.Join(h.stateDir, updateBackupFile), filepath.Join(h.stateDir, updateBackupUpdaterFile), filepath.Join(h.stateDir, UpdateJournalFile)}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("invalid update transaction artifact %s", filepath.Base(path))
		}
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (h *LocalHost) renderUpdater(candidate Generation) []byte {
	values := []string{filepath.Join(candidate.ReleaseDir, "fleet"), "update", "apply-latest", "--repo", h.repository,
		"--root", h.rootDir, "--state-dir", h.stateDir, "--launch-agents-dir", h.launchAgentsDir,
		"--mode", candidate.Mode, "--config", candidate.ConfigPath, "--endpoint", candidate.Endpoint, "--domain", h.domain,
		"--confirm", "automatic-release-update"}
	var arguments strings.Builder
	for _, value := range values {
		arguments.WriteString("    <string>" + xmlEscape(value) + "</string>\n")
	}
	seconds := int64(h.updateInterval / time.Second)
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.vitalyiegorov.tart-runner-fleet.updater</string>
  <key>ProgramArguments</key><array>
%s  </array>
  <key>RunAtLoad</key><true/>
  <key>StartInterval</key><integer>%d</integer>
  <key>EnvironmentVariables</key><dict>
    <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>ProcessType</key><string>Background</string>
  <key>LowPriorityIO</key><true/>
  <key>StandardOutPath</key><string>%s/update.stdout.log</string>
  <key>StandardErrorPath</key><string>%s/update.stderr.log</string>
</dict></plist>
`, arguments.String(), seconds, xmlEscape(h.stateDir), xmlEscape(h.stateDir)))
}

func (h *LocalHost) renderUpdaterHandoff(candidate Generation) []byte {
	values := []string{filepath.Join(candidate.ReleaseDir, "fleet"), "update", "finish-updater-handoff",
		"--repo", h.repository, "--root", h.rootDir, "--state-dir", h.stateDir,
		"--launch-agents-dir", h.launchAgentsDir, "--mode", candidate.Mode, "--config", candidate.ConfigPath,
		"--endpoint", candidate.Endpoint, "--domain", h.domain, "--release-dir", candidate.ReleaseDir,
		"--interval", h.updateInterval.String(), "--confirm", "automatic-updater-handoff"}
	var arguments strings.Builder
	for _, value := range values {
		arguments.WriteString("    <string>" + xmlEscape(value) + "</string>\n")
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.vitalyiegorov.tart-runner-fleet.updater-handoff</string>
  <key>ProgramArguments</key><array>
%s  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>EnvironmentVariables</key><dict>
    <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>%s/update.stdout.log</string>
  <key>StandardErrorPath</key><string>%s/update.stderr.log</string>
</dict></plist>
`, arguments.String(), xmlEscape(h.stateDir), xmlEscape(h.stateDir)))
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}

func serviceLabel(mode string) string {
	if mode == "observe" {
		return "com.vitalyiegorov.tart-runner-fleet"
	}
	return "com.vitalyiegorov.tart-runner-fleet." + mode
}

func atomicWrite(path string, body []byte, mode os.FileMode) error {
	return atomicWriteWith(path, body, mode, atomicWriteOps{
		mkdirAll: os.MkdirAll,
		createTemp: func(directory, pattern string) (atomicWriteFile, error) {
			return os.CreateTemp(directory, pattern)
		},
		remove: os.Remove, rename: os.Rename,
		openDirectory: func(path string) (atomicSyncCloser, error) { return os.Open(path) }, // #nosec G304 -- parent of caller-selected trusted path.
	})
}

type atomicWriteFile interface {
	io.Writer
	Name() string
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

type atomicSyncCloser interface {
	Sync() error
	Close() error
}

type atomicWriteOps struct {
	mkdirAll      func(string, os.FileMode) error
	createTemp    func(string, string) (atomicWriteFile, error)
	remove        func(string) error
	rename        func(string, string) error
	openDirectory func(string) (atomicSyncCloser, error)
}

type atomicSymlinkOps struct {
	createTemp    func(string, string) (atomicWriteFile, error)
	remove        func(string) error
	symlink       func(string, string) error
	rename        func(string, string) error
	openDirectory func(string) (atomicSyncCloser, error)
}

func atomicSymlink(target, path string) error {
	return atomicSymlinkWith(target, path, atomicSymlinkOps{
		createTemp: func(directory, pattern string) (atomicWriteFile, error) {
			return os.CreateTemp(directory, pattern)
		},
		remove: os.Remove, symlink: os.Symlink, rename: os.Rename,
		openDirectory: func(path string) (atomicSyncCloser, error) { return os.Open(path) }, // #nosec G304 -- parent of trusted current-generation link.
	})
}

func atomicSymlinkWith(target, path string, ops atomicSymlinkOps) error {
	temporary, err := ops.createTemp(filepath.Dir(path), ".fleet-current-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	remove := true
	defer func() {
		if remove {
			_ = ops.remove(temporaryPath)
		}
	}()
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := ops.remove(temporaryPath); err != nil {
		return err
	}
	if err := ops.symlink(target, temporaryPath); err != nil {
		return err
	}
	if err := ops.rename(temporaryPath, path); err != nil {
		return err
	}
	remove = false
	directory, err := ops.openDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func atomicWriteWith(path string, body []byte, mode os.FileMode, ops atomicWriteOps) error {
	if err := ops.mkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := ops.createTemp(filepath.Dir(path), ".fleet-update-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	remove := true
	defer func() {
		if remove {
			_ = ops.remove(temporary)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ops.rename(temporary, path); err != nil {
		return err
	}
	remove = false
	directory, err := ops.openDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	return closeErr
}
