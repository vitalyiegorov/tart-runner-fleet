package provision

import (
	"context"
	"errors"
	"testing"

	"github.com/actions/scaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type fakeRecreater struct {
	*fakeClient
	deleted   []int
	deleteErr error
	nextID    int
}

func (f *fakeRecreater) Delete(_ context.Context, id int) error {
	f.deleted = append(f.deleted, id)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	// GitHub stops holding the object, so the replacement is a new object with a
	// new id -- which is the whole point: a queued job is routed to an id.
	f.plans = map[string]githubscaleset.ScaleSetPlan{}
	return nil
}

func recreaterFor(t *testing.T, nextID int) *fakeRecreater {
	t.Helper()
	client := &fakeRecreater{fakeClient: &fakeClient{plans: map[string]githubscaleset.ScaleSetPlan{
		"repo-large": {Action: githubscaleset.ScaleSetReuse, ID: 17}}}, nextID: nextID}
	client.ensure = func(spec githubscaleset.ScaleSetSpec) (scaleset.RunnerScaleSet, error) {
		return scaleset.RunnerScaleSet{ID: client.nextID, Name: spec.Name}, nil
	}
	return client
}

func recreateConfig() config.Config {
	cfg := provisionConfig()
	for index, set := range cfg.GitHub.Scopes[0].ScaleSets {
		if set.Name == "repo-large" {
			cfg.GitHub.Scopes[0].ScaleSets[index].ID = 17
		}
	}
	return cfg
}

func recreateRequest(client Recreater, name string) RecreateRequest {
	return RecreateRequest{Config: recreateConfig(), Name: name,
		LoadKey: func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
			return githubscaleset.NewPrivateKeySecret("pem"), nil
		},
		Open: func(githubscaleset.GitHubAppAdminConfig) (Recreater, error) { return client, nil }}
}

// TestRecreateDeletesTheSetAndPersistsTheReplacementID is the remedy issue #336
// needed three times in one day, and which an operator had to perform with a
// one-off program built from the SDK: GitHub's counters for the set are stale,
// the object cannot be repaired in place, and only a NEW id starts receiving
// work again.
func TestRecreateDeletesTheSetAndPersistsTheReplacementID(t *testing.T) {
	client := recreaterFor(t, 19)

	result, err := Recreate(context.Background(), recreateRequest(client, "repo-large"))
	if err != nil {
		t.Fatal(err)
	}

	if len(client.deleted) != 1 || client.deleted[0] != 17 {
		t.Fatalf("the stranded object must be deleted exactly once: %v", client.deleted)
	}
	if len(client.created) != 1 || client.created[0] != "repo-large" {
		t.Fatalf("the replacement carries the same name: %v", client.created)
	}
	if result.OldID != 17 || result.NewID != 19 || result.Scope != "repo" || result.Profile != "large" {
		t.Fatalf("old id, new id, scope and profile must be reported: %#v", result)
	}
	var persisted int
	for _, set := range result.Config.GitHub.Scopes[0].ScaleSets {
		if set.Name == "repo-large" {
			persisted = set.ID
		}
	}
	if persisted != 19 {
		t.Fatalf("the new id must be written into the returned configuration: %d", persisted)
	}
	// Nothing else moved: a recreate is one set, not a provisioning run.
	for _, set := range result.Config.GitHub.Scopes[0].ScaleSets {
		if set.Name != "repo-large" && set.ID != 0 {
			t.Fatalf("an untouched set must stay untouched: %#v", set)
		}
	}
}

// A set the configuration does not name is refused: the command may only
// delete an object this node declares it serves.
func TestRecreateRefusesASetTheConfigurationDoesNotName(t *testing.T) {
	client := recreaterFor(t, 19)

	_, err := Recreate(context.Background(), recreateRequest(client, "repo-unknown"))

	if !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("an unknown set is not found: %v", err)
	}
	if len(client.deleted) != 0 {
		t.Fatalf("nothing may be deleted on a refusal: %v", client.deleted)
	}
}

// An ambiguous name is refused rather than guessed: two scopes may each hold a
// set of the same name, and deleting the wrong one is unrecoverable.
func TestRecreateRefusesAnAmbiguousName(t *testing.T) {
	client := recreaterFor(t, 19)
	request := recreateRequest(client, "repo-large")
	second := request.Config.GitHub.Scopes[0]
	second.Name = "other"
	second.ConfigURL = "https://github.com/owner/other"
	second.Targets = []string{"owner/other"}
	second.ScaleSets = append([]config.ScaleSet(nil), second.ScaleSets...)
	request.Config.GitHub.Scopes = append(request.Config.GitHub.Scopes, second)
	request.Config.Targets = append(request.Config.Targets, config.Target{Type: "repo", Slug: "owner/other", MaxActive: 4})

	if _, err := Recreate(context.Background(), request); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("an ambiguous name is a conflict: %v", err)
	}
	if len(client.deleted) != 0 {
		t.Fatalf("nothing may be deleted on a refusal: %v", client.deleted)
	}
}

// GitHub answering with the SAME id is not a replacement. It means the delete
// did not take, and reporting it as a recreation would send the operator back
// to a set that is still stranded.
func TestRecreateRefusesAReplacementThatIsTheSameObject(t *testing.T) {
	client := recreaterFor(t, 17)

	if _, err := Recreate(context.Background(), recreateRequest(client, "repo-large")); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("the same id back is uncertain: %v", err)
	}
}

// A delete GitHub refused stops the operation before anything is provisioned
// or written: the configuration must never name a set that was not created.
func TestRecreateStopsWhenTheDeleteFails(t *testing.T) {
	client := recreaterFor(t, 19)
	client.deleteErr = errors.New("GitHub refused")

	if _, err := Recreate(context.Background(), recreateRequest(client, "repo-large")); err == nil {
		t.Fatal("a refused delete must surface")
	}
	if len(client.created) != 0 {
		t.Fatalf("nothing may be provisioned after a failed delete: %v", client.created)
	}
}

// The unwired forms are invalid rather than best-effort.
func TestRecreateRefusesAnUnwiredRequest(t *testing.T) {
	client := recreaterFor(t, 19)
	for name, edit := range map[string]func(*RecreateRequest){
		"no opener": func(r *RecreateRequest) { r.Open = nil },
		"no loader": func(r *RecreateRequest) { r.LoadKey = nil },
		"no name":   func(r *RecreateRequest) { r.Name = "" },
		"no key": func(r *RecreateRequest) {
			r.LoadKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
				return nil, nil
			}
		},
	} {
		request := recreateRequest(client, "repo-large")
		edit(&request)
		if _, err := Recreate(context.Background(), request); !errors.Is(err, operations.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Every way the request can fail before GitHub is touched, and the two ways it
// can fail after: none of them may report a recreation.
func TestRecreateSurfacesEachFailureAsItself(t *testing.T) {
	opened := errors.New("no installation")
	ensureFailed := errors.New("GitHub refused the create")
	tests := map[string]struct {
		edit func(*RecreateRequest, *fakeRecreater)
		want error
	}{
		"no scope configured": {edit: func(r *RecreateRequest, _ *fakeRecreater) {
			r.Config.GitHub.Scopes = nil
		}, want: operations.ErrInvalid},
		"key refused": {edit: func(r *RecreateRequest, _ *fakeRecreater) {
			r.LoadKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
				return nil, errors.New("keychain refused")
			}
		}},
		"client refused": {edit: func(r *RecreateRequest, _ *fakeRecreater) {
			r.Open = func(githubscaleset.GitHubAppAdminConfig) (Recreater, error) { return nil, opened }
		}, want: opened},
		"create refused": {edit: func(_ *RecreateRequest, client *fakeRecreater) {
			client.ensure = func(githubscaleset.ScaleSetSpec) (scaleset.RunnerScaleSet, error) {
				return scaleset.RunnerScaleSet{}, ensureFailed
			}
		}, want: ensureFailed},
		"no id back": {edit: func(_ *RecreateRequest, client *fakeRecreater) {
			client.ensure = func(githubscaleset.ScaleSetSpec) (scaleset.RunnerScaleSet, error) {
				return scaleset.RunnerScaleSet{}, nil
			}
		}, want: operations.ErrUncertain},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			client := recreaterFor(t, 19)
			request := recreateRequest(client, "repo-large")
			tt.edit(&request, client)
			_, err := Recreate(context.Background(), request)
			if err == nil {
				t.Fatal("the failure must surface")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// --scope is how an operator resolves a name two scopes carry, and it must
// select the scope they named rather than the first match.
func TestRecreateSelectsTheNamedScope(t *testing.T) {
	client := recreaterFor(t, 19)
	request := recreateRequest(client, "repo-large")
	second := request.Config.GitHub.Scopes[0]
	second.Name = "other"
	second.ConfigURL = "https://github.com/owner/other"
	second.Targets = []string{"owner/other"}
	second.ScaleSets = append([]config.ScaleSet(nil), second.ScaleSets...)
	request.Config.GitHub.Scopes = append(request.Config.GitHub.Scopes, second)
	request.Config.Targets = append(request.Config.Targets, config.Target{Type: "repo", Slug: "owner/other", MaxActive: 4})
	request.Scope = "other"

	result, err := Recreate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scope != "other" {
		t.Fatalf("the named scope must be the one recreated: %#v", result)
	}
	if result.Config.GitHub.Scopes[0].ScaleSets[2].ID == 19 {
		t.Fatal("the other scope's set must be untouched")
	}
}

// TestRecreateResumesAfterAnInterruptedRun is the operational hole CodeRabbit
// found on #341: the delete succeeds, the create or the write then fails, and
// the operator runs the command again. The second run must not fail on a delete
// GitHub can no longer perform -- that leaves the node polling a set that no
// longer exists, which is a worse state than the one being repaired.
func TestRecreateResumesAfterAnInterruptedRun(t *testing.T) {
	// The old object is gone and nothing carries its name: the delete took, the
	// create did not. The retry provisions and reports, deleting nothing.
	client := recreaterFor(t, 19)
	client.plans = map[string]githubscaleset.ScaleSetPlan{}

	result, err := Recreate(context.Background(), recreateRequest(client, "repo-large"))
	if err != nil {
		t.Fatal(err)
	}
	if len(client.deleted) != 0 {
		t.Fatalf("a set GitHub no longer holds must not be deleted again: %v", client.deleted)
	}
	if result.OldID != 17 || result.NewID != 19 {
		t.Fatalf("the substitution is still reported: %#v", result)
	}

	// The replacement exists under the same name with a new id: the create took
	// and the write did not. The retry adopts it rather than deleting it.
	adopting := recreaterFor(t, 21)
	adopting.plans = map[string]githubscaleset.ScaleSetPlan{
		"repo-large": {Action: githubscaleset.ScaleSetReuse, ID: 19}}
	adopting.ensure = func(githubscaleset.ScaleSetSpec) (scaleset.RunnerScaleSet, error) {
		return scaleset.RunnerScaleSet{ID: 19, Name: "repo-large"}, nil
	}

	result, err = Recreate(context.Background(), recreateRequest(adopting, "repo-large"))
	if err != nil {
		t.Fatal(err)
	}
	if len(adopting.deleted) != 0 {
		t.Fatalf("a replacement already created must never be deleted: %v", adopting.deleted)
	}
	if result.NewID != 19 {
		t.Fatalf("the replacement's id must be reported so the operator can persist it: %#v", result)
	}
}

// A set the configuration carries no id for is refused: there is no object to
// replace, `Ensure` would adopt or create one, and reporting that as a
// recreation would tell an operator a stranded set had been repaired when it
// had not been touched.
func TestRecreateRefusesASetWithNoConfiguredID(t *testing.T) {
	client := recreaterFor(t, 19)
	request := recreateRequest(client, "repo-large")
	for index, set := range request.Config.GitHub.Scopes[0].ScaleSets {
		if set.Name == "repo-large" {
			request.Config.GitHub.Scopes[0].ScaleSets[index].ID = 0
		}
	}

	if _, err := Recreate(context.Background(), request); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("a set with no configured id is a conflict: %v", err)
	}
	if len(client.deleted) != 0 || len(client.created) != 0 {
		t.Fatalf("nothing may be touched on a refusal: %v %v", client.deleted, client.created)
	}
}

// An inspection GitHub would not answer stops the run before the delete.
func TestRecreateStopsWhenTheInspectionFails(t *testing.T) {
	client := recreaterFor(t, 19)
	client.inspect = func(githubscaleset.ScaleSetSpec) (githubscaleset.ScaleSetPlan, error) {
		return githubscaleset.ScaleSetPlan{}, errors.New("GitHub is unreachable")
	}

	if _, err := Recreate(context.Background(), recreateRequest(client, "repo-large")); err == nil {
		t.Fatal("an unanswered inspection must surface")
	}
	if len(client.deleted) != 0 {
		t.Fatalf("nothing may be deleted on an unread plan: %v", client.deleted)
	}
}
