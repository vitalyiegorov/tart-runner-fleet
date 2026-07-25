package contract_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

var markdownLink = regexp.MustCompile(`\[[^]]+\]\(([^)]+)\)`)

// operatorFacingDocs are the documents an operator or agent follows while
// installing, upgrading, or working an incident. They must describe only
// executables a release actually ships. ADR records are excluded on purpose:
// they narrate history, so ADR 0011 keeps its amended `fleetctl` text and ADR
// 0019 keeps its before/after comparison.
var operatorFacingDocs = []string{
	"README.md", "INSTALL.md", "USAGE.md", "AGENTS.md",
	"docs/AGENT_RUNBOOK.md", "docs/API.md", "docs/CLI.md", "docs/OPERATIONS.md",
}

// retiredBinaryName matches the executables ADR 0019 merged into `fleet`. The
// positive fragment assertions cannot catch a survivor, because `fleet` is a
// strict prefix of both names and so `Contains(body, ".../fleet")` is satisfied
// by a document that still says `.../fleetctl`.
var retiredBinaryName = regexp.MustCompile(`fleetctl|fleetd`)

// retainedSocketName is the one deliberate survivor of the rename: an operator
// contract with 28 call sites that ADR 0019 explicitly declines to touch.
const retainedSocketName = "fleetd.sock"

// releaseExecutableArgument matches one `"$RELEASE_DIR/<name>"` argument of the
// install guide's permission command.
var releaseExecutableArgument = regexp.MustCompile(`"\$RELEASE_DIR/([^"]+)"`)

// TestOperatorDocsNameNoRetiredExecutable fails on any operator-facing document
// that still names `fleetd` or `fleetctl`. Renaming to a name that is a prefix
// of both predecessors made every `strings.Contains` fragment check unable to
// fail on a stale reference, so the honest guard is a negative one.
func TestOperatorDocsNameNoRetiredExecutable(t *testing.T) {
	root := documentationRoot(t)
	for _, name := range operatorFacingDocs {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		for index, line := range strings.Split(string(body), "\n") {
			retired := retiredBinaryName.FindString(strings.ReplaceAll(line, retainedSocketName, ""))
			if retired == "" {
				continue
			}
			t.Errorf("%s:%d names retired executable %q, which no release ships: %s",
				name, index+1, retired, strings.TrimSpace(line))
		}
	}
}

// TestInstallGuidePermitsEachShippedExecutableExactlyOnce pins the install
// guide's permission command against the other failure mode of a collapsing
// rename: two arguments that named different binaries become one name written
// twice, which reads as a typo and hides whether anything stopped being covered.
func TestInstallGuidePermitsEachShippedExecutableExactlyOnce(t *testing.T) {
	root := documentationRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "INSTALL.md"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for index, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "chmod ") || !strings.Contains(line, "$RELEASE_DIR/") {
			continue
		}
		found = true
		seen := map[string]int{}
		var order []string
		for _, match := range releaseExecutableArgument.FindAllStringSubmatch(line, -1) {
			if seen[match[1]] == 0 {
				order = append(order, match[1])
			}
			seen[match[1]]++
		}
		for _, argument := range order {
			if seen[argument] > 1 {
				t.Errorf("INSTALL.md:%d makes %q executable %d times; each shipped executable belongs exactly once: %s",
					index+1, argument, seen[argument], strings.TrimSpace(line))
			}
		}
		for _, required := range []string{"fleet", "render-launchd.sh"} {
			if seen[required] == 0 {
				t.Errorf("INSTALL.md:%d does not make %q executable", index+1, required)
			}
		}
	}
	if !found {
		t.Error("INSTALL.md no longer documents making the shipped executables runnable")
	}
}

func documentationRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve documentation contract path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func TestOperatorAndAgentDocumentationIsCompleteAndLinked(t *testing.T) {
	root := documentationRoot(t)
	required := []string{
		"README.md", "INSTALL.md", "USAGE.md", "AGENTS.md",
		"docs/AGENT_RUNBOOK.md", "docs/API.md", "docs/CLI.md",
		"docs/OPERATIONS.md", "docs/SECURITY.md", "docs/TESTING.md",
	}
	for _, name := range required {
		if info, err := os.Stat(filepath.Join(root, name)); err != nil || info.IsDir() {
			t.Errorf("required documentation %s: %v", name, err)
		}
	}

	var markdown []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "vendor") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
			markdown = append(markdown, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(markdown)
	for _, path := range markdown {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
			continue
		}
		for _, match := range markdownLink.FindAllStringSubmatch(string(body), -1) {
			target := strings.TrimSpace(strings.SplitN(match[1], "#", 2)[0])
			if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), filepath.FromSlash(target))); err != nil {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s has broken local link %q: %v", rel, match[1], err)
			}
		}
	}
}

func TestArchitectureDecisionNumbersAreUnique(t *testing.T) {
	root := documentationRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "docs", "adr"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		number := strings.SplitN(entry.Name(), "-", 2)[0]
		if prior := seen[number]; prior != "" {
			t.Errorf("ADR number %s is duplicated by %s and %s", number, prior, entry.Name())
		}
		seen[number] = entry.Name()
	}
}

func TestOperationalDocsTrackCurrentRuntimeContracts(t *testing.T) {
	root := documentationRoot(t)
	requireText := map[string][]string{
		"README.md": {"Linux medium runner", "docs/AGENT_RUNBOOK.md"},
		// Match the complete assignment, not the bare path: `fleet` is a strict
		// prefix of the retired `fleetctl`, so `$ROOT/current/fleet` alone is
		// satisfied by a document that still says `$ROOT/current/fleetctl`.
		// TestOperatorDocsNameNoRetiredExecutable is the other half of this guard.
		"USAGE.md":              {`FLEET="$ROOT/current/fleet"`},
		"docs/AGENT_RUNBOOK.md": {`FLEET="$ROOT/current/fleet"`, "installed-generation.json", "exit status 0"},
		"docs/OPERATIONS.md":    {"five-minute readiness budget", "launchd bootstrap", "not running", "exit status 0"},
	}
	for name, fragments := range requireText {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(body), fragment) {
				t.Errorf("%s does not document %q", name, fragment)
			}
		}
	}
}
