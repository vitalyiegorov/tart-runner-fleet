package githubscaleset

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type fakeAuditAdmin struct {
	group     *scaleset.RunnerGroup
	groupErr  error
	lookupErr error
	set       *scaleset.RunnerScaleSet
	setErr    error
	groupID   int
	name      string
	onLookup  func()
}

func (f *fakeAuditAdmin) GetRunnerGroupByName(context.Context, string) (*scaleset.RunnerGroup, error) {
	return f.group, f.groupErr
}

func (f *fakeAuditAdmin) GetRunnerScaleSet(_ context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	f.groupID, f.name = groupID, name
	if f.onLookup != nil {
		f.onLookup()
	}
	return nil, f.lookupErr
}

func (f *fakeAuditAdmin) GetRunnerScaleSetByID(context.Context, int) (*scaleset.RunnerScaleSet, error) {
	return f.set, f.setErr
}

func jsonResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)),
		Header: http.Header{}, Request: req}
}

func listRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(listingContext(context.Background()), http.MethodGet,
		"https://actions.example/_apis/runtime/runnerscalesets?runnerGroupId=1&name="+auditListName, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// The tap turns the client's name-filtered lookup into the by-group listing and
// keeps the answer. The sentinel name never leaves the process: GitHub is asked
// for the group, not for a scale set that does not exist.
func TestTheTapListsTheGroupAndKeepsTheAnswer(t *testing.T) {
	tap := &listTap{}
	tap.arm()
	req := listRequest(t)

	tap.beforeRequest(nil, req, 0)
	if query := req.URL.Query(); query.Has("name") || query.Get("runnerGroupId") != "1" {
		t.Fatalf("the sentinel must not reach GitHub: %q", req.URL.RawQuery)
	}
	resp := jsonResponse(req, http.StatusOK,
		`{"count":2,"value":[{"id":1,"name":"mini","statistics":{"totalAssignedJobs":0}},`+
			`{"id":7,"name":"studio","statistics":{"totalAssignedJobs":2,"totalBusyRunners":2,"totalIdleRunners":1}}]}`)
	tap.afterResponse(nil, resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != emptyScaleSetListing {
		t.Fatalf("the client must see a well-formed empty listing: %q %v", body, err)
	}
	sets, observed := tap.take()
	if !observed || len(sets) != 2 || sets[1].ID != 7 || sets[1].Statistics == nil {
		t.Fatalf("the listing must be kept: observed=%v sets=%#v", observed, sets)
	}
	if sets[1].Statistics.AssignedJobs != 2 || sets[1].Statistics.BusyRunners != 2 || sets[1].Statistics.IdleRunners != 1 {
		t.Fatalf("GitHub's own counts must survive: %#v", *sets[1].Statistics)
	}
	if sets[0].Statistics == nil || sets[0].Statistics.AssignedJobs != 0 {
		t.Fatalf("a set reporting zero is not a set reporting nothing: %#v", sets[0])
	}
}

// Every other request the client makes -- the App handshake above all -- passes
// through untouched, and a disarmed tap is inert even for the sentinel, so a
// stale in-flight request can never be attributed to the next audit.
func TestTheTapLeavesEveryOtherRequestAlone(t *testing.T) {
	tap := &listTap{}
	tap.arm()
	handshake, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://api.example/actions/runner-registration", nil)
	if err != nil {
		t.Fatal(err)
	}

	tap.beforeRequest(nil, handshake, 0)
	handshakeResponse := jsonResponse(handshake, http.StatusOK, `{"token":"t"}`)
	defer func() { _ = handshakeResponse.Body.Close() }()
	tap.afterResponse(nil, handshakeResponse)
	if _, observed := tap.take(); observed {
		t.Fatal("an unrelated request must not be read as a listing")
	}

	stale := listRequest(t)
	tap.beforeRequest(nil, stale, 0)
	staleResponse := jsonResponse(stale, http.StatusOK, `{"count":0,"value":[]}`)
	defer func() { _ = staleResponse.Body.Close() }()
	tap.afterResponse(nil, staleResponse)
	if _, observed := tap.take(); observed {
		t.Fatal("a disarmed tap must observe nothing")
	}

	// An unmarked call to the very same resource is not this tap's listing,
	// however much it looks like one, and neither is a missing response.
	tap.arm()
	unmarked, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://actions.example/_apis/runtime/runnerscalesets?runnerGroupId=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	unmarkedResponse := jsonResponse(unmarked, http.StatusOK, `{"count":0,"value":[]}`)
	defer func() { _ = unmarkedResponse.Body.Close() }()
	tap.afterResponse(nil, unmarkedResponse)
	tap.afterResponse(nil, nil)
	if _, observed := tap.take(); observed {
		t.Fatal("only the marked call may be read as this tap's listing")
	}

	// A by-id read rides the marked context too -- Statistics is called inside an
	// audit -- and must never be mistaken for the group listing.
	tap.arm()
	byID, err := http.NewRequestWithContext(listingContext(context.Background()), http.MethodGet,
		"https://actions.example/_apis/runtime/runnerscalesets/7", nil)
	if err != nil {
		t.Fatal(err)
	}
	tap.beforeRequest(nil, byID, 0)
	byIDResponse := jsonResponse(byID, http.StatusOK, `{"id":7}`)
	defer func() { _ = byIDResponse.Body.Close() }()
	tap.afterResponse(nil, byIDResponse)
	if _, observed := tap.take(); observed {
		t.Fatal("a by-id read is not the group listing")
	}
}

// TestARetriedListingIsStillRead is the failure mode a pointer or sentinel match
// hides. retryablehttp shallow-copies the request between attempts and the first
// attempt has already stripped the sentinel from the URL both attempts share, so
// a listing that succeeds on its SECOND attempt carries neither marker the naive
// match looks for -- and the scope would be reported as unread, one step away
// from being read as "no set is parked".
func TestARetriedListingIsStillRead(t *testing.T) {
	tap := &listTap{}
	tap.arm()
	attempts := 0
	retryable := retryablehttp.NewClient()
	retryable.Logger = nil
	retryable.RetryMax = 2
	retryable.RetryWaitMin, retryable.RetryWaitMax = time.Millisecond, time.Millisecond
	retryable.RequestLogHook = tap.beforeRequest
	retryable.ResponseLogHook = tap.afterResponse
	retryable.HTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return jsonResponse(req, http.StatusInternalServerError, `{}`), nil
		}
		if req.URL.Query().Has("name") {
			t.Errorf("the sentinel reached GitHub on attempt %d: %q", attempts, req.URL.RawQuery)
		}
		return jsonResponse(req, http.StatusOK,
			`{"count":1,"value":[{"id":7,"name":"studio","statistics":{"totalAssignedJobs":2}}]}`), nil
	})

	request, err := retryablehttp.FromRequest(listRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := retryable.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != emptyScaleSetListing {
		t.Fatalf("the client must still see an empty listing: %q %v", body, err)
	}
	sets, observed := tap.take()
	if attempts != 2 || !observed || len(sets) != 1 || sets[0].ID != 7 {
		t.Fatalf("a retried listing must be read: attempts=%d observed=%v sets=%#v", attempts, observed, sets)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// A refusal, an unreadable body and an undecodable listing are all recorded as
// nothing, which List reports as an error. None of them may become an empty
// group: that would read as "no set is parked".
func TestTheTapRefusesRatherThanInventingAnEmptyGroup(t *testing.T) {
	for name, respond := range map[string]func(*http.Request) *http.Response{
		"refused": func(req *http.Request) *http.Response { return jsonResponse(req, http.StatusForbidden, `{}`) },
		"garbled": func(req *http.Request) *http.Response { return jsonResponse(req, http.StatusOK, `{"value":`) },
		"unreadable": func(req *http.Request) *http.Response {
			return &http.Response{StatusCode: http.StatusOK, Body: errorBody{}, Header: http.Header{}, Request: req}
		},
	} {
		t.Run(name, func(t *testing.T) {
			tap := &listTap{}
			tap.arm()
			req := listRequest(t)
			tap.beforeRequest(nil, req, 0)
			tap.afterResponse(nil, respond(req))
			if _, observed := tap.take(); observed {
				t.Fatal("nothing readable was read, so nothing may be reported")
			}
		})
	}
}

type errorBody struct{}

func (errorBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (errorBody) Close() error             { return errors.New("close failed") }

// The auditor asks for the scope's own runner group, defaulting to the group
// every provisioned set lands in when the scope names none.
func TestTheAuditorListsTheScopesRunnerGroup(t *testing.T) {
	admin := &fakeAuditAdmin{group: &scaleset.RunnerGroup{ID: 4, Name: "macos"}}
	tap := &listTap{}
	admin.onLookup = func() { tap.record([]ScaleSetSummary{{ID: 7, Name: "studio"}}) }
	auditor := &Auditor{client: admin, tap: tap}

	sets, err := auditor.List(context.Background(), "macos")
	if err != nil || len(sets) != 1 || sets[0].ID != 7 {
		t.Fatalf("List() = %#v, %v", sets, err)
	}
	if admin.groupID != 4 || admin.name != auditListName {
		t.Fatalf("the named group must be resolved: group=%d name=%q", admin.groupID, admin.name)
	}
	for _, group := range []string{"", "default", "  "} {
		if _, err := auditor.List(context.Background(), group); err != nil {
			t.Fatal(err)
		}
		if admin.groupID != defaultRunnerGroupID {
			t.Fatalf("group %q must resolve to the default: %d", group, admin.groupID)
		}
	}
}

// Every way the listing can fail to produce an answer is an error. None of them
// may return an empty scope, which would read as "GitHub holds no scale set".
func TestTheAuditorNeverReportsAnEmptyScopeItDidNotRead(t *testing.T) {
	silent := &Auditor{client: &fakeAuditAdmin{}, tap: &listTap{}}
	if _, err := silent.List(context.Background(), ""); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("an unobserved listing is uncertain: %v", err)
	}
	groupRefused := &Auditor{client: &fakeAuditAdmin{groupErr: errors.New("denied")}, tap: &listTap{}}
	if _, err := groupRefused.List(context.Background(), "macos"); err == nil {
		t.Fatal("a group that cannot be resolved is a refusal")
	}
	groupMissing := &Auditor{client: &fakeAuditAdmin{}, tap: &listTap{}}
	if _, err := groupMissing.List(context.Background(), "macos"); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("an absent group is uncertain: %v", err)
	}
	listRefused := &Auditor{client: &fakeAuditAdmin{lookupErr: errors.New("rate limited")}, tap: &listTap{}}
	if _, err := listRefused.List(context.Background(), ""); err == nil {
		t.Fatal("a refused listing is a refusal")
	}
	var absent *Auditor
	if _, err := absent.List(context.Background(), ""); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("an unconstructed auditor is invalid: %v", err)
	}
}

// Statistics answers the one question a listing without counts leaves open, and
// refuses when GitHub answers without them.
func TestTheAuditorReadsOneSetsStatistics(t *testing.T) {
	admin := &fakeAuditAdmin{set: &scaleset.RunnerScaleSet{ID: 7, Statistics: &scaleset.RunnerScaleSetStatistic{
		TotalAvailableJobs: 1, TotalAcquiredJobs: 2, TotalAssignedJobs: 3, TotalRunningJobs: 4,
		TotalRegisteredRunners: 5, TotalBusyRunners: 6, TotalIdleRunners: 7}}}
	auditor := &Auditor{client: admin, tap: &listTap{}}

	statistics, err := auditor.Statistics(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := ScaleSetStatistics{AvailableJobs: 1, AcquiredJobs: 2, AssignedJobs: 3, RunningJobs: 4,
		RegisteredRunners: 5, BusyRunners: 6, IdleRunners: 7}
	if statistics != want {
		t.Fatalf("statistics = %#v, want %#v", statistics, want)
	}
	countless := &Auditor{client: &fakeAuditAdmin{set: &scaleset.RunnerScaleSet{ID: 7}}, tap: &listTap{}}
	if _, err := countless.Statistics(context.Background(), 7); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("a set reported without counts is uncertain, not empty: %v", err)
	}
	refused := &Auditor{client: &fakeAuditAdmin{setErr: errors.New("denied")}, tap: &listTap{}}
	if _, err := refused.Statistics(context.Background(), 7); err == nil {
		t.Fatal("a refused read is a refusal")
	}
	if _, err := auditor.Statistics(context.Background(), 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatal("a set with no id cannot be read")
	}
}

// The auditor is constructed from exactly the credential `scale-sets provision`
// uses, and refuses an incomplete one rather than producing a client that will
// fail later with GitHub's words instead of the fleet's.
func TestNewAuditorRequiresACompleteAppCredential(t *testing.T) {
	valid := GitHubAppAdminConfig{GitHubConfigURL: "https://github.com/owner/repo", ClientID: "client",
		InstallationID: 7, PrivateKey: NewPrivateKeySecret("pem")}
	auditor, err := NewAuditor(valid)
	if err != nil || auditor == nil || auditor.tap == nil {
		t.Fatalf("NewAuditor() = %#v, %v", auditor, err)
	}
	for name, edit := range map[string]func(*GitHubAppAdminConfig){
		"no key":            func(c *GitHubAppAdminConfig) { c.PrivateKey = nil },
		"empty key":         func(c *GitHubAppAdminConfig) { c.PrivateKey = NewPrivateKeySecret("") },
		"no config URL":     func(c *GitHubAppAdminConfig) { c.GitHubConfigURL = " " },
		"no client":         func(c *GitHubAppAdminConfig) { c.ClientID = "" },
		"no installation":   func(c *GitHubAppAdminConfig) { c.InstallationID = 0 },
		"unparsable config": func(c *GitHubAppAdminConfig) { c.GitHubConfigURL = "https://example.com" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			edit(&cfg)
			if _, err := NewAuditor(cfg); err == nil {
				t.Fatal("an incomplete credential must be refused")
			}
		})
	}
}

// The sentinel is URL-safe, because the client interpolates it into a query
// string without escaping it.
func TestTheListSentinelIsURLSafe(t *testing.T) {
	if url.QueryEscape(auditListName) != auditListName {
		t.Fatalf("the sentinel must survive interpolation: %q", auditListName)
	}
}
