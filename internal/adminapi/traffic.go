package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// TrafficPath serves per-job guest network accounting (ADR 0080). It is a
// fleet.v1 addition: an older daemon answers 404, which the CLI reports as an
// error, never as a node that moved no bytes.
const (
	TrafficPath = "/v1/traffic"
	TrafficKind = "Traffic"
	// MaxTrafficJobs bounds one response under MaxResponseBytes. A window with
	// more samples is answered newest first with Truncated set.
	MaxTrafficJobs = 1500
	// MaxTrafficWindow bounds how far back one request may look.
	MaxTrafficWindow = 30 * 24 * time.Hour
)

type TrafficEnvelope struct {
	APIVersion  string    `json:"apiVersion"`
	Kind        string    `json:"kind"`
	GeneratedAt time.Time `json:"generatedAt"`
	Data        Traffic   `json:"data"`
}

type Traffic struct {
	Since time.Time `json:"since"`
	// Truncated means the window held more than MaxTrafficJobs samples; Jobs and
	// Totals then cover only the newest of them.
	Truncated bool           `json:"truncated"`
	Totals    []TrafficTotal `json:"totals"`
	Jobs      []TrafficJob   `json:"jobs"`
}

// TrafficJob is one instance's guest counters at the end of its job. The byte
// fields are absent, not zero, when Measured is false.
type TrafficJob struct {
	Instance      string    `json:"instance"`
	Repo          string    `json:"repo"`
	Platform      string    `json:"platform"`
	Profile       string    `json:"profile"`
	ScaleSetID    int64     `json:"scaleSetId,omitempty"`
	RunID         int64     `json:"runId,omitempty"`
	WorkflowRef   string    `json:"workflowRef,omitempty"`
	Job           string    `json:"job,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	SampledAt     time.Time `json:"sampledAt"`
	Measured      bool      `json:"measured"`
	ReceiveBytes  *int64    `json:"receiveBytes,omitempty"`
	TransmitBytes *int64    `json:"transmitBytes,omitempty"`
	Unmeasured    string    `json:"unmeasured,omitempty"`
}

// TrafficTotal is the bytes one repository's workflow moved on one profile.
// Workflow is the workflow file without its `@ref`, so every pull request of
// one workflow adds up. Bytes sum measured jobs only; UnmeasuredJobs says how
// much of the picture is missing.
type TrafficTotal struct {
	Repo           string `json:"repo"`
	Workflow       string `json:"workflow,omitempty"`
	Profile        string `json:"profile"`
	Jobs           int    `json:"jobs"`
	MeasuredJobs   int    `json:"measuredJobs"`
	UnmeasuredJobs int    `json:"unmeasuredJobs"`
	ReceiveBytes   int64  `json:"receiveBytes"`
	TransmitBytes  int64  `json:"transmitBytes"`
}

// SummarizeTraffic groups jobs by repository, workflow and profile, largest
// receive first. It is pure so the daemon and any reader agree on the totals.
func SummarizeTraffic(jobs []TrafficJob) []TrafficTotal {
	index := map[[3]string]int{}
	totals := []TrafficTotal{}
	for _, job := range jobs {
		workflow, _, _ := strings.Cut(job.WorkflowRef, "@")
		key := [3]string{job.Repo, workflow, job.Profile}
		at, seen := index[key]
		if !seen {
			at = len(totals)
			index[key] = at
			totals = append(totals, TrafficTotal{Repo: job.Repo, Workflow: workflow, Profile: job.Profile})
		}
		total := &totals[at]
		total.Jobs++
		if !job.Measured || job.ReceiveBytes == nil || job.TransmitBytes == nil {
			total.UnmeasuredJobs++
			continue
		}
		total.MeasuredJobs++
		total.ReceiveBytes += *job.ReceiveBytes
		total.TransmitBytes += *job.TransmitBytes
	}
	sort.SliceStable(totals, func(i, j int) bool { return totals[i].ReceiveBytes > totals[j].ReceiveBytes })
	return totals
}

// Traffic reads the samples taken in the last window.
func (c *Client) Traffic(ctx context.Context, window time.Duration) (TrafficEnvelope, error) {
	body, err := c.get(ctx, fmt.Sprintf("%s?sinceSeconds=%d", TrafficPath, int64(window/time.Second)), "application/json")
	if err != nil {
		return TrafficEnvelope{}, err
	}
	var traffic TrafficEnvelope
	if err := json.Unmarshal(body, &traffic); err != nil || traffic.APIVersion != APIVersion || traffic.Kind != TrafficKind {
		return TrafficEnvelope{}, ErrInvalidResponse
	}
	return traffic, nil
}
