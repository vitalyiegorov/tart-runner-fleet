package domain

import "testing"

func TestParseRunnerRefusalReadsOnlyTheRunnersOwnLine(t *testing.T) {
	version, ok := ParseRunnerRefusal([]byte("An error occurred: Runner version v2.336.0 is deprecated and cannot receive messages.\n"))
	if !ok || version != "2.336.0" {
		t.Fatalf("got %q %v", version, ok)
	}
	for _, line := range []string{
		"", "2026-11-05 08:54:16Z: Listening for Jobs\n",
		"2026-11-05 08:54:16Z: Running job: Runner version v2.336.0 is deprecated and cannot receive messages\n",
		"An error occurred: Runner version v2.336 is deprecated and cannot receive messages\n",
		"An error occurred: something else\n",
	} {
		if version, ok := ParseRunnerRefusal([]byte(line)); ok {
			t.Fatalf("%q read as a refusal of %q", line, version)
		}
	}
}
