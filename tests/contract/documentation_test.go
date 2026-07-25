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
		"README.md":             {"Linux medium runner", "docs/AGENT_RUNBOOK.md"},
		"USAGE.md":              {"$ROOT/current/fleet"},
		"docs/AGENT_RUNBOOK.md": {"$ROOT/current/fleet", "installed-generation.json", "exit status 0"},
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
