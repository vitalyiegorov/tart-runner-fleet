package githubscaleset

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/actions/scaleset"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// auditListName is the scale-set name the auditor asks for. It is never sent to
// GitHub: the tap removes it before the request leaves the process, which is
// what turns the by-name lookup into the by-group listing the admin API already
// serves.
//
// The runner scale-set admin API lists a runner group's sets at the same
// `_apis/runtime/runnerscalesets` resource the fleet already creates and looks
// sets up at, but the official client exposes only the name-filtered form of
// that call. Listing through the client this way is deliberate: the audit then
// reuses the exact client, the exact GitHub App credential path and the exact
// retry policy `scale-sets provision` runs with, instead of re-implementing the
// Actions-service admin handshake a second time, where it could drift or hold a
// different authority (ADR 0054).
const auditListName = "fleet-audit-list-all"

// maxAuditListBytes bounds one listing. A runner group holds tens of scale sets,
// so this is three orders of magnitude of headroom and still a ceiling.
const maxAuditListBytes = 8 << 20

// scaleSetListPath is the admin-API resource a runner group's scale sets are
// listed at. It is spelled here rather than imported because the official client
// keeps its copy unexported.
const scaleSetListPath = "_apis/runtime/runnerscalesets"

// listingContextKey marks the ONE call whose response the tap may read.
//
// The marker travels in the request context and not in the request or its URL,
// because neither survives a retry intact: retryablehttp shallow-copies the
// request between attempts, so the pointer changes, and the first attempt has
// already stripped the sentinel name from the shared URL, so the retried attempt
// carries no sentinel either. Matching on either of those would silently drop a
// listing that succeeded on its second attempt and report the scope as unread --
// which this audit must never turn into "no set is parked". A context value is
// copied along with the request and is therefore true of every attempt.
type listingContextKey struct{}

func listingContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, listingContextKey{}, listingContextKey{})
}

// listingRequest is the conjunction the tap acts on: the marked call, addressed
// to the group-listing resource. The marker alone is not enough -- the client
// makes its App handshake calls on the same context -- and the resource alone is
// not enough, because a by-id read lives under the same prefix.
func listingRequest(req *http.Request) bool {
	if req == nil || req.URL == nil || req.Context().Value(listingContextKey{}) == nil {
		return false
	}
	// GitHub's Actions service URL carries a per-tenant path prefix
	// (`https://pipelines….actions.githubusercontent.com/<tenant>/`), so the
	// resource is matched by suffix: an exact match fired only against the bare
	// hosts the fakes used and never against GitHub.
	return strings.HasSuffix(strings.TrimSuffix(req.URL.Path, "/"), "/"+scaleSetListPath) && req.URL.Query().Has("runnerGroupId")
}

// ScaleSetStatistics is GitHub's own count of what a scale set is holding. The
// field names are the admin API's, unabbreviated: `totalAssignedJobs` is the
// number this audit exists for, because a job GitHub has ASSIGNED to a set is a
// job it will offer to nobody else.
type ScaleSetStatistics struct {
	AvailableJobs     int `json:"availableJobs"`
	AcquiredJobs      int `json:"acquiredJobs"`
	AssignedJobs      int `json:"assignedJobs"`
	RunningJobs       int `json:"runningJobs"`
	RegisteredRunners int `json:"registeredRunners"`
	BusyRunners       int `json:"busyRunners"`
	IdleRunners       int `json:"idleRunners"`
}

// ScaleSetSummary is one scale set as GitHub reports it. Statistics is nil when
// the listing carried none, which is why the auditor can fill it in per set.
type ScaleSetSummary struct {
	ID         int
	Name       string
	Statistics *ScaleSetStatistics
}

// Auditor reads the scale sets that EXIST on GitHub for a scope, whether or not
// this node is configured to serve them.
type Auditor struct {
	client scaleSetAuditAdmin
	tap    *listTap
}

type scaleSetAuditAdmin interface {
	GetRunnerGroupByName(context.Context, string) (*scaleset.RunnerGroup, error)
	GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error)
	GetRunnerScaleSetByID(context.Context, int) (*scaleset.RunnerScaleSet, error)
}

func NewAuditor(c GitHubAppAdminConfig) (*Auditor, error) {
	if c.PrivateKey == nil || c.PrivateKey.reveal() == "" || strings.TrimSpace(c.GitHubConfigURL) == "" ||
		strings.TrimSpace(c.ClientID) == "" || c.InstallationID <= 0 {
		return nil, operations.ErrInvalid
	}
	tap := &listTap{}
	retryable := retryablehttp.NewClient()
	retryable.RequestLogHook = tap.beforeRequest
	retryable.ResponseLogHook = tap.afterResponse
	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: c.GitHubConfigURL,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID: c.ClientID, InstallationID: c.InstallationID, PrivateKey: c.PrivateKey.reveal(),
		},
		SystemInfo: scaleset.SystemInfo{System: c.System, Version: c.Version, CommitSHA: c.CommitSHA, Subsystem: c.Subsystem},
	}, scaleset.WithRetryableHTTPClint(retryable))
	if err != nil {
		return nil, fmt.Errorf("create official GitHub App admin client: %w", err)
	}
	return &Auditor{client: client, tap: tap}, nil
}

// List reports every scale set GitHub holds in the scope's runner group. It is
// ONE request per scope: the audit's whole cost is bounded by design, because a
// finding that cost a poll loop would be refused on rate limits long before it
// was believed (ADR 0054).
func (a *Auditor) List(ctx context.Context, runnerGroup string) ([]ScaleSetSummary, error) {
	if a == nil || a.client == nil || a.tap == nil {
		return nil, operations.ErrInvalid
	}
	groupID := defaultRunnerGroupID
	name := strings.TrimSpace(runnerGroup)
	if name != "" && !strings.EqualFold(name, "default") {
		group, err := a.client.GetRunnerGroupByName(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("resolve runner group: %w", err)
		}
		if group == nil || group.ID <= 0 {
			return nil, operations.ErrUncertain
		}
		groupID = group.ID
	}
	a.tap.arm()
	if _, err := a.client.GetRunnerScaleSet(listingContext(ctx), groupID, auditListName); err != nil {
		return nil, fmt.Errorf("list runner scale sets: %w", err)
	}
	sets, observed := a.tap.take()
	if !observed {
		// The listing never reached the tap, so nothing was read. Reporting an
		// empty scope here would say "GitHub holds no scale set", which is the one
		// answer this audit must never invent: it would read as "no set is parked".
		return nil, operations.ErrUncertain
	}
	return sets, nil
}

// Statistics reads one scale set by id, for the sets a listing described without
// counts. It is called only for a PARKED set, so the per-scope cost stays one
// list plus one get per set this node does not serve.
func (a *Auditor) Statistics(ctx context.Context, id int) (ScaleSetStatistics, error) {
	if a == nil || a.client == nil || id <= 0 {
		return ScaleSetStatistics{}, operations.ErrInvalid
	}
	set, err := a.client.GetRunnerScaleSetByID(ctx, id)
	if err != nil {
		return ScaleSetStatistics{}, fmt.Errorf("read runner scale set %d: %w", id, err)
	}
	if set == nil || set.Statistics == nil {
		return ScaleSetStatistics{}, operations.ErrUncertain
	}
	return convertStatistics(set.Statistics), nil
}

func convertStatistics(s *scaleset.RunnerScaleSetStatistic) ScaleSetStatistics {
	return ScaleSetStatistics{AvailableJobs: s.TotalAvailableJobs, AcquiredJobs: s.TotalAcquiredJobs,
		AssignedJobs: s.TotalAssignedJobs, RunningJobs: s.TotalRunningJobs,
		RegisteredRunners: s.TotalRegisteredRunners, BusyRunners: s.TotalBusyRunners, IdleRunners: s.TotalIdleRunners}
}

// listTap turns the client's name-filtered lookup into the by-group listing and
// keeps the answer.
//
// It is a pair of retryablehttp hooks rather than a transport because the
// official client asserts that its transport is an *http.Transport and rejects
// anything else; the hooks are the seam it does leave open. The request hook
// removes the sentinel name, so what GitHub is asked for is the runner group.
// The response hook keeps the listing and hands the client a well-formed empty
// one, so the client decodes "no scale set has that name" and reaches no
// conclusion of its own.
//
// Anything the hooks cannot read is simply not recorded. List then reports that
// it observed nothing, which is an error -- never an empty scope, because an
// empty scope would read as "no set is parked".
type listTap struct {
	mu       sync.Mutex
	sets     []ScaleSetSummary
	observed bool
	armed    bool
}

func (t *listTap) arm() {
	t.mu.Lock()
	t.sets, t.observed, t.armed = nil, false, true
	t.mu.Unlock()
}

func (t *listTap) take() ([]ScaleSetSummary, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sets, observed := t.sets, t.observed
	t.sets, t.observed, t.armed = nil, false, false
	return sets, observed
}

func (t *listTap) record(sets []ScaleSetSummary) {
	t.mu.Lock()
	t.sets, t.observed = sets, true
	t.mu.Unlock()
}

// beforeRequest runs on EVERY attempt, and removing the name is idempotent: the
// first attempt strips it from a URL the retried attempt shares, so a retry
// simply finds nothing to remove and is still recognised as the listing.
func (t *listTap) beforeRequest(_ retryablehttp.Logger, req *http.Request, _ int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.armed || !listingRequest(req) {
		return
	}
	query := req.URL.Query()
	if !query.Has("name") {
		return
	}
	query.Del("name")
	req.URL.RawQuery = query.Encode()
}

const emptyScaleSetListing = `{"count":0,"value":[]}`

func (t *listTap) afterResponse(_ retryablehttp.Logger, resp *http.Response) {
	if !t.listing(resp) || resp.StatusCode != http.StatusOK {
		return
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAuditListBytes))
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		return
	}
	var payload struct {
		Value []scaleset.RunnerScaleSet `json:"value"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return
	}
	sets := make([]ScaleSetSummary, 0, len(payload.Value))
	for _, set := range payload.Value {
		summary := ScaleSetSummary{ID: set.ID, Name: set.Name}
		if set.Statistics != nil {
			statistics := convertStatistics(set.Statistics)
			summary.Statistics = &statistics
		}
		sets = append(sets, summary)
	}
	t.record(sets)
	resp.Body = io.NopCloser(strings.NewReader(emptyScaleSetListing))
	resp.ContentLength = int64(len(emptyScaleSetListing))
	resp.Header.Set("Content-Length", strconv.Itoa(len(emptyScaleSetListing)))
}

// listing matches a response to the ONE call this tap was armed for, by the
// marker its context carries. Every other call the client makes -- the App
// handshake, a by-id read -- is left entirely alone.
func (t *listTap) listing(resp *http.Response) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return resp != nil && resp.Body != nil && t.armed && listingRequest(resp.Request)
}
