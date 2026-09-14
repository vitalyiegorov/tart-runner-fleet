package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
)

// policyDigestPrefix is how much of the digest an operator reads. Twelve hex
// characters is 48 bits: far beyond any accidental collision between two nodes
// of one fleet, and short enough to compare two status lines by eye, which is
// the comparison ADR 0053 is for.
const policyDigestPrefix = 12

// policyValueAbsent is what a key present in one node's policy and missing from
// another's renders as. It is deliberately not `false` and not an empty cell:
// #304 is precisely the case where a missing key and a stated `false` were
// treated as the same thing, and the whole point of the projection is that they
// are no longer confusable.
const policyValueAbsent = "absent"

// shortDigest renders the readable head of a policy digest, and renders a digest
// this client was never given as a dash rather than as an empty column.
func shortDigest(digest string) string {
	if digest == "" {
		return "-"
	}
	if len(digest) <= policyDigestPrefix {
		return digest
	}
	return digest[:policyDigestPrefix]
}

// policyDocument is one node's declared policy and where it was read from.
type policyDocument struct {
	source string
	policy adminapi.Policy
}

// readPolicyDocument accepts either of the two artifacts an operator has to
// hand: a node's configuration file, or the `fleet status --output json`
// document that node published. The second is what makes the command usable
// without SSH and without file access, which is the constraint issue #304
// states.
//
// A status document is recognised by `data.policy` rather than by extension or
// by `apiVersion`: the presence of the object IS the capability, so a daemon too
// old to publish one falls through to being read as a configuration file and
// fails with the parse error it deserves rather than with a silent empty policy.
func readPolicyDocument(path string) (policyDocument, error) {
	// #nosec G304 -- the operator explicitly selects the document to project.
	body, err := os.ReadFile(path)
	if err != nil {
		return policyDocument{}, err
	}
	var envelope struct {
		Data struct {
			Policy *adminapi.Policy `json:"policy"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Data.Policy != nil {
		// A decoded policy always carries a field map, empty at worst, because the
		// DTO's own decoder builds one; a `"policy": null` is not a declaration at
		// all and falls through to be read as a configuration file.
		return policyDocument{source: path, policy: *envelope.Data.Policy}, nil
	}
	cfg, err := config.Decode(bytes.NewReader(body))
	if err != nil {
		return policyDocument{}, err
	}
	projected := config.ProjectPolicy(cfg)
	return policyDocument{source: path, policy: adminapi.Policy{Digest: projected.Digest(),
		Fields: projected.Fields()}}, nil
}

// runConfigPolicy prints one node's policy, or names every key on which two or
// more nodes disagree. It is the consumer half of ADR 0053: publishing the
// declaration answers "what is this node running with", and this answers the
// question #304 actually asked, "do these two nodes agree".
func runConfigPolicy(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: fleet config policy <path>... (a node configuration or a "+
			"`fleet status --output json` document)")
		return exitUsage
	}
	documents := make([]policyDocument, 0, len(args))
	for _, path := range args {
		document, err := readPolicyDocument(path)
		if err != nil {
			fmt.Fprintf(stderr, "read policy %s: %v\n", path, err)
			return exitUsage
		}
		documents = append(documents, document)
	}
	if len(documents) == 1 {
		_ = writeJSON(stdout, documents[0].policy)
		return exitSuccess
	}
	flattened := flattenPolicies(documents)
	drift := policyDrift(flattened)
	if len(drift) == 0 {
		fmt.Fprintf(stdout, "no policy drift across %d nodes: %s\n", len(documents),
			shortDigest(documents[0].policy.Digest))
		return exitSuccess
	}
	renderPolicyDrift(stdout, documents, flattened, drift)
	return exitDegraded
}

// flattenPolicies reduces each document to its key paths exactly once, so the
// judgement and the table it is rendered into can never walk the documents
// differently.
func flattenPolicies(documents []policyDocument) []map[string]string {
	flattened := make([]map[string]string, 0, len(documents))
	for _, document := range documents {
		values := map[string]string{}
		flattenPolicy("", document.policy.Fields, values)
		flattened = append(flattened, values)
	}
	return flattened
}

// policyDrift is every key path on which the documents do not all state the same
// value, sorted. A key one node does not project at all is drift too: silence is
// the failure mode, not the baseline.
func policyDrift(flattened []map[string]string) []string {
	keys := map[string]struct{}{}
	for _, values := range flattened {
		for key := range values {
			keys[key] = struct{}{}
		}
	}
	drift := make([]string, 0, len(keys))
	for key := range keys {
		first, agreed := flattened[0][key], true
		for _, values := range flattened[1:] {
			if values[key] != first {
				agreed = false
				break
			}
		}
		if !agreed {
			drift = append(drift, key)
		}
	}
	sort.Strings(drift)
	return drift
}

// flattenPolicy walks the projection into `a.b.c` key paths so a disagreement is
// named at the exact key an operator must edit — `macosBurst.mixedPlatformAdmission`
// rather than `macosBurst`.
//
// A list is a leaf. Advertised labels and capability declarations are sets whose
// drift is "this node advertises something the other does not", which reads
// better as two whole lists side by side than as a disagreement at index 3.
func flattenPolicy(prefix string, value any, into map[string]string) {
	nested, isObject := value.(map[string]any)
	if !isObject {
		into[prefix] = renderPolicyValue(value)
		return
	}
	for key, child := range nested {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		flattenPolicy(path, child, into)
	}
}

// renderPolicyValue prints one leaf the way the configuration file spells it, so
// a cell can be pasted back into the key it names. Re-encoding a value that was
// itself decoded from JSON cannot fail, so there is no error branch to pretend
// to handle.
func renderPolicyValue(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// renderPolicyDrift prints one row per disagreeing key and one column per node,
// then says how many nodes were compared. The node columns are the paths the
// operator typed, because two nodes' files are routinely both called
// `fleet.json` and a basename would make the table ambiguous.
func renderPolicyDrift(stdout io.Writer, documents []policyDocument, flattened []map[string]string, drift []string) {
	headers := make([]string, 0, len(documents))
	for _, document := range documents {
		headers = append(headers, document.source)
	}
	table := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(table, "KEY\t%s\n", strings.Join(headers, "\t"))
	for _, key := range drift {
		cells := make([]string, 0, len(flattened))
		for _, values := range flattened {
			value, stated := values[key]
			if !stated {
				value = policyValueAbsent
			}
			cells = append(cells, value)
		}
		fmt.Fprintf(table, "%s\t%s\n", key, strings.Join(cells, "\t"))
	}
	_ = table.Flush()
	disagreement := "policy keys disagree"
	if len(drift) == 1 {
		disagreement = "policy key disagrees"
	}
	fmt.Fprintf(stdout, "%d %s across %d nodes\n", len(drift), disagreement, len(documents))
}

// policyDetail is the doctor row's text. The row never fails on its own — a
// digest is an identity, not a judgement, and no single node can know whether
// its own policy is the right one — so the whole content is the digest and how
// to compare it with a peer's.
func policyDetail(status adminapi.Status) string {
	policy := status.EffectivePolicy()
	if policy.Digest == "" {
		return "not declared by this daemon"
	}
	return fmt.Sprintf("%s (%d declared keys); compare two nodes with fleet config policy",
		shortDigest(policy.Digest), len(policy.Fields))
}
