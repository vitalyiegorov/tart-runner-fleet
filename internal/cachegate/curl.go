package cachegate

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// CurlFetch is Fetch through the curl at binary. On macOS that is /usr/bin/curl:
// Local Network privacy answers an ad-hoc signed launchd agent's own LAN connection
// with "no route to host", and an ad-hoc release never keeps a grant across
// updates, while an Apple platform binary is exempt (issue #537). The flags keep
// directFetch's contract: no proxy, no redirect, http or https only, a bounded body.
// --disable comes first so no .curlrc can change what curl writes.
func CurlFetch(binary string) Fetch {
	return func(ctx context.Context, target string) (int, []byte, error) {
		var stderr bytes.Buffer
		command := exec.CommandContext(ctx, binary, "--disable", "--silent", "--show-error", "--noproxy", "*", "--proto", "=http,https",
			"--max-redirs", "0", "--max-filesize", strconv.Itoa(maxIdentityAnswer), "--output", "-",
			"--write-out", "\n%{http_code}", "--url", target) // #nosec G204 -- a fixed binary and an argument vector.
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			return 0, nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		cut := bytes.LastIndexByte(output, '\n')
		if cut < 0 {
			return 0, nil, fmt.Errorf("curl wrote no status: %q", output)
		}
		status, err := strconv.Atoi(string(output[cut+1:]))
		if err != nil {
			return 0, nil, fmt.Errorf("curl wrote no status: %w", err)
		}
		body := output[:cut]
		if len(body) > maxIdentityAnswer {
			body = body[:maxIdentityAnswer]
		}
		return status, body, nil
	}
}
