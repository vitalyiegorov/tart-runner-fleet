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
	UpdateJournalFile               = "update-transaction.json"
	CanonicalPlist                  = "com.vitalyiegorov.tart-runner-fleet.plist"
	UpdaterPlist                    = "com.vitalyiegorov.tart-runner-fleet.updater.plist"
	updateBackupFile                = "update-previous.plist"
	updateBackupUpdaterFile         = "update-previous-updater.plist"
	minimumLaunchdBootstrapAttempts = 3
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
	if err != nil || !strings.Contains(string(canonical), candidate.ReleaseDir+"/fleetd") ||
		!strings.Contains(string(canonical), "--mode="+candidate.Mode) {
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
	if _, err := h.command.Run(ctx, filepath.Join(candidate.ReleaseDir, "fleetctl"), "config", "validate", candidate.ConfigPath); err != nil {
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
	required := map[string]bool{"RELEASE_VERSION": false, "fleetd": false, "fleetctl": false,
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
	rendered := strings.ReplaceAll(string(template), "__RELEASE_DIR__", candidate.ReleaseDir)
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
		body, err := h.command.Run(ctx, filepath.Join(candidate.ReleaseDir, "fleetctl"), "status", "--require-ready", "--output", "json", "--endpoint", candidate.Endpoint)
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
	if _, err := h.command.Run(ctx, "launchctl", "print", updaterLabel); err != nil {
		if err := h.bootstrapService(ctx, updaterPath); err != nil {
			return fmt.Errorf("bootstrap automatic updater: %w", err)
		}
	}
	body, _ := json.Marshal(candidate)
	if err := atomicWrite(filepath.Join(h.stateDir, InstalledGenerationFile), body, 0o600); err != nil {
		return err
	}
	return h.clearTransaction()
}

func (h *LocalHost) ensureQuiescent(ctx context.Context, current Generation) error {
	body, err := h.command.Run(ctx, filepath.Join(current.ReleaseDir, "fleetctl"), "status", "--require-ready", "--output", "json", "--endpoint", current.Endpoint)
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
	for _, path := range []string{filepath.Join(h.stateDir, UpdateJournalFile), filepath.Join(h.stateDir, updateBackupFile), filepath.Join(h.stateDir, updateBackupUpdaterFile)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (h *LocalHost) renderUpdater(candidate Generation) []byte {
	values := []string{filepath.Join(candidate.ReleaseDir, "fleetctl"), "update", "apply-latest", "--repo", h.repository,
		"--root", h.rootDir, "--state-dir", h.stateDir, "--launch-agents-dir", h.launchAgentsDir,
		"--mode", candidate.Mode, "--endpoint", candidate.Endpoint, "--domain", h.domain,
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
