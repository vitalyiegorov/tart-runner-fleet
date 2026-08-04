package contract_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
)

// docsIngestReason matches the leading `| \`reason\` |` cell of one row of the
// ingest `detail` table in docs/OPERATIONS.md. A row may name two reasons that
// share a repair, which the table writes as `a` / `b`.
var docsIngestReason = regexp.MustCompile("(?m)^\\| `([a-z_]+)`(?: / `([a-z_]+)`)? \\|")

// TestIngestFailureVocabularyIsDocumented binds the closed ingest vocabulary to
// the table an operator actually reads.
//
// The vocabulary is the only part of a broker failure that reaches a human, and
// a reason nobody can look up is worth very little: for three days
// `message_poll_failed` was the entire published account of a durable write
// conflict (issue #165). A new reason that is not in the table is therefore a
// failure of this test, and a table row for a reason the code cannot emit is
// documentation of something that does not exist.
func TestIngestFailureVocabularyIsDocumented(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(documentationRoot(t), "docs", "OPERATIONS.md"))
	if err != nil {
		t.Fatalf("read operations guide: %v", err)
	}
	section := ingestDetailTable(t, string(body))
	documented := map[string]bool{}
	for _, match := range docsIngestReason.FindAllStringSubmatch(section, -1) {
		for _, reason := range match[1:] {
			if reason != "" {
				documented[reason] = true
			}
		}
	}
	emitted := []string{
		githubscaleset.ReasonSessionExpired, githubscaleset.ReasonSessionReleaseFailed,
		githubscaleset.ReasonSessionCreateFailed, githubscaleset.ReasonRecreatedAfterFailures,
		githubscaleset.ReasonMessagePollFailed, githubscaleset.ReasonQueueObservationFailed,
		githubscaleset.ReasonQueueObservationStale, githubscaleset.ReasonQueueReconcileFailed,
		githubscaleset.ReasonDemandCommitConflict,
	}
	for _, reason := range emitted {
		if !githubscaleset.ValidFailureReason(reason) {
			t.Errorf("reason %q is not in the closed vocabulary it is listed under", reason)
		}
		if !documented[reason] {
			t.Errorf("reason %q reaches operators but docs/OPERATIONS.md does not explain it", reason)
		}
		delete(documented, reason)
	}
	if len(documented) != 0 {
		undocumented := make([]string, 0, len(documented))
		for reason := range documented {
			undocumented = append(undocumented, reason)
		}
		sort.Strings(undocumented)
		t.Errorf("docs/OPERATIONS.md documents reasons the fleet cannot emit: %v", undocumented)
	}
}

// ingestDetailTable extracts the rows between the ingest `detail` table's header
// and the end of the table, so the scheduler `reason` table below it -- a
// different closed vocabulary, owned by internal/app -- is not read as part of
// this one.
func ingestDetailTable(t *testing.T, body string) string {
	t.Helper()
	const header = "| `detail` | Meaning | Action |"
	start := strings.Index(body, header)
	if start < 0 {
		t.Fatalf("docs/OPERATIONS.md no longer carries the ingest %s table", "`detail`")
	}
	section := body[start+len(header):]
	if end := strings.Index(section, "\n\n"); end >= 0 {
		section = section[:end]
	}
	return section
}
