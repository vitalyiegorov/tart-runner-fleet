package autoupdate

import (
	"context"
	"errors"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrServiceReferences means release collection has no complete, fresh view of
// the boot paths. It defers housekeeping, never the release transaction itself.
var ErrServiceReferences = errors.New("autoupdate: service references unavailable")

var releaseReference = regexp.MustCompile(`/releases/(v[0-9][0-9A-Za-z.+_-]*)/`)

// referencedVersions conservatively retains every immutable release path in a
// service observation, including retired fleetd executables and helper argv.
// Extra references cost disk; omitting one can strand the next service restart.
func referencedVersions(bodies []string) []string {
	seen := map[string]bool{}
	for _, body := range bodies {
		body = html.UnescapeString(body)
		for _, match := range releaseReference.FindAllStringSubmatch(body, -1) {
			seen[match[1]] = true
		}
	}
	result := make([]string, 0, len(seen))
	for version := range seen {
		result = append(result, version)
	}
	sort.Strings(result)
	return result
}

// serviceFiles reads only boot definitions that can launch this fleet. Backups
// are not executable definitions. A failed read never becomes an empty snapshot.
func serviceFiles(dir, prefix, suffix string) ([]string, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, ErrServiceReferences
	}
	var bodies, names []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(dir, entry.Name())) // #nosec G304 -- fleet-prefixed boot definitions in the configured service directory.
		if readErr != nil || !utf8.Valid(body) || strings.IndexByte(string(body), 0) >= 0 || strings.HasPrefix(string(body), "bplist") {
			return nil, nil, ErrServiceReferences
		}
		names = append(names, entry.Name())
		bodies = append(bodies, string(body))
	}
	return bodies, names, nil
}

// ReferencedReleases observes launchd's cached executables independently of the
// manifest/current link, and retains all on-disk fleet boot references too.
func (h *LocalHost) ReferencedReleases(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	bodies, _, err := serviceFiles(h.launchAgentsDir, "com.vitalyiegorov.tart-runner-fleet", ".plist")
	if err != nil {
		return nil, err
	}
	labels := []string{"com.vitalyiegorov.tart-runner-fleet.svc"}
	for _, mode := range []string{"observe", "shadow", "canary", "authority", "updater", "updater-handoff"} {
		labels = append(labels, serviceLabel(mode))
	}
	for _, label := range labels {
		body, readErr := h.command.Run(ctx, "launchctl", "print", h.domain+"/"+label)
		if ctx.Err() != nil {
			return nil, ErrServiceReferences
		}
		if exitedWith(readErr, 113) { // launchctl's explicit service-not-found result.
			continue
		}
		if readErr != nil || !hasLoadedProgram(string(body)) {
			return nil, ErrServiceReferences
		}
		bodies = append(bodies, string(body))
	}
	return referencedVersions(bodies), nil
}

func hasLoadedProgram(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(key) == "program" && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// ReferencedReleases reads both loaded systemd properties and boot files. A
// not-found LoadState is explicit absence; a failed/partial query is unknown.
func (h *SystemdHost) ReferencedReleases(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	bodies, units, err := serviceFiles(h.unitsDir, "tart-runner-fleet", ".service")
	if err != nil {
		return nil, err
	}
	for _, mode := range []string{"observe", "shadow", "canary", "authority"} {
		units = append(units, systemdUnitName(mode))
	}
	units = append(units, systemdUpdaterUnit)
	slices.Sort(units)
	for _, unit := range slices.Compact(units) {
		body, readErr := h.command.Run(ctx, "systemctl", "--user", "show", unit, "--property=LoadState", "--property=ExecStart")
		if ctx.Err() != nil {
			return nil, ErrServiceReferences
		}
		text := string(body)
		notFound := namesProperty(text, "LoadState", "not-found")
		if readErr != nil && (!notFound || !exitedWith(readErr, 1)) {
			return nil, ErrServiceReferences
		}
		if notFound {
			continue
		}
		// A masked unit may report an empty ExecStart; a loaded one must name its program.
		switch {
		case !strings.Contains(text, "ExecStart="),
			namesProperty(text, "LoadState", "loaded") && namesProperty(text, "ExecStart", ""),
			!namesProperty(text, "LoadState", "loaded") && !namesProperty(text, "LoadState", "masked"):
			return nil, ErrServiceReferences
		}
		bodies = append(bodies, text)
	}
	return referencedVersions(bodies), nil
}

func exitedWith(err error, code int) bool {
	var status interface{ ExitCode() int }
	return errors.As(err, &status) && status.ExitCode() == code
}
