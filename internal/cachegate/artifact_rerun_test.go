package cachegate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// tokenRun mints a runtime token whose signed run_id names the GitHub run;
// the Actions.Results run segment is the attempt's backend run.
func (f *artifactFixture) tokenRun(repo, run, job, githubRun string) string {
	f.t.Helper()
	c := jwt.MapClaims{"iss": artifactIssuer, "exp": f.at.Add(time.Hour).Unix(), "repository_id": repo, "scp": "Actions.Results:" + run + ":" + job, "run_id": githubRun}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	token.Header["kid"] = "test"
	signed, err := token.SignedString(f.key)
	if err != nil {
		f.t.Fatal(err)
	}
	return signed
}

type listedArtifact struct {
	ID  string `json:"database_id"`
	Run string `json:"workflow_run_backend_id"`
	Job string `json:"workflow_job_run_backend_id"`
}

func (f *artifactFixture) listNamed(auth, run, job, name string) []listedArtifact {
	f.t.Helper()
	body := artifactBody(run, job, "")
	body["name_filter"] = name
	w := f.rpc(auth, "ListArtifacts", body)
	if w.Code != 200 {
		f.t.Fatalf("list status %d", w.Code)
	}
	var res struct {
		Artifacts []listedArtifact `json:"artifacts"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	return res.Artifacts
}

// download follows the toolkit: list by name, take the newest database ID,
// then sign with the producer's backend run and job.
func (f *artifactFixture) download(auth, run, job, name string) (string, int) {
	f.t.Helper()
	listed := f.listNamed(auth, run, job, name)
	if len(listed) == 0 {
		return "", 404
	}
	newest := listed[len(listed)-1]
	w := f.rpc(auth, "GetSignedArtifactURL", artifactBody(newest.Run, newest.Job, name))
	if w.Code != 200 {
		return "", w.Code
	}
	var got struct {
		URL string `json:"signed_url"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	return f.blob(got.URL, "GET", nil).Body.String(), 200
}

func TestArtifactLaterAttemptReadsEarlierAttempt(t *testing.T) {
	f := newArtifactFixture(t)
	f.upload(f.tokenRun("123", "attempt-1", "plan", "4242"), "attempt-1", "plan", "app", []byte("attempt one app"))
	rerun := f.tokenRun("123", "attempt-2", "e2e", "4242")
	if got, status := f.download(rerun, "attempt-2", "e2e", "app"); status != 200 || got != "attempt one app" {
		t.Fatalf("re-run of failed jobs could not read the earlier attempt: %d %q", status, got)
	}
}

func TestArtifactNewestAttemptWins(t *testing.T) {
	f := newArtifactFixture(t)
	f.upload(f.tokenRun("123", "attempt-1", "plan", "4242"), "attempt-1", "plan", "app", []byte("old"))
	f.upload(f.tokenRun("123", "attempt-2", "plan", "4242"), "attempt-2", "plan", "app", []byte("new"))
	if got, status := f.download(f.tokenRun("123", "attempt-3", "e2e", "4242"), "attempt-3", "e2e", "app"); status != 200 || got != "new" {
		t.Fatalf("newest copy not served: %d %q", status, got)
	}
}

func TestArtifactOtherRunsStayRefused(t *testing.T) {
	f := newArtifactFixture(t)
	f.upload(f.tokenRun("123", "attempt-1", "plan", "4242"), "attempt-1", "plan", "app", []byte("secret"))
	for name, auth := range map[string]string{
		"another GitHub run": f.tokenRun("123", "other", "e2e", "4243"),
		"no run_id claim":    f.token("123", "other", "e2e"),
		"malformed run_id":   f.tokenRun("123", "other", "e2e", "42 42"),
	} {
		t.Run(name, func(t *testing.T) {
			if listed := f.listNamed(auth, "other", "e2e", "app"); len(listed) != 0 {
				t.Fatalf("listed another run's artifact: %+v", listed)
			}
			if w := f.rpc(auth, "GetSignedArtifactURL", artifactBody("attempt-1", "plan", "app")); w.Code == 200 {
				t.Fatal("signed another run's artifact")
			}
		})
	}
	if w := f.rpc(f.tokenRun("456", "other", "e2e", "4242"), "ListArtifacts", artifactBody("other", "e2e", "")); w.Code == 200 && strings.Contains(w.Body.String(), "app") {
		t.Fatal("another repository listed the artifact")
	}
}

func TestArtifactNonStringRunIDCountsAsAbsent(t *testing.T) {
	f := newArtifactFixture(t)
	for _, claim := range []any{4242, true, map[string]any{"id": "4242"}} {
		c := jwt.MapClaims{"iss": artifactIssuer, "exp": f.at.Add(time.Hour).Unix(), "repository_id": "123", "scp": "Actions.Results:own:job", "run_id": claim}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
		token.Header["kid"] = "test"
		auth, err := token.SignedString(f.key)
		if err != nil {
			t.Fatal(err)
		}
		if w := f.rpc(auth, "ListArtifacts", artifactBody("own", "job", "")); w.Code != 200 {
			t.Fatalf("run_id %v refused the token's own run: %d", claim, w.Code)
		}
	}
}

func TestArtifactPreMigrationRowsStayAttemptScoped(t *testing.T) {
	f := newArtifactFixture(t)
	f.upload(f.token("123", "attempt-1", "plan"), "attempt-1", "plan", "app", []byte("legacy"))
	if _, status := f.download(f.tokenRun("123", "attempt-2", "e2e", "4242"), "attempt-2", "e2e", "app"); status == 200 {
		t.Fatal("a row without a GitHub run was read across attempts")
	}
}

func TestArtifactLaterAttemptCannotWriteEarlierAttempt(t *testing.T) {
	f := newArtifactFixture(t)
	f.upload(f.tokenRun("123", "attempt-1", "plan", "4242"), "attempt-1", "plan", "app", []byte("kept"))
	rerun := f.tokenRun("123", "attempt-2", "plan", "4242")
	for _, method := range []string{"CreateArtifact", "FinalizeArtifact", "DeleteArtifact"} {
		if w := f.rpc(rerun, method, artifactBody("attempt-1", "plan", "app")); w.Code == 200 {
			t.Fatalf("%s on the earlier attempt was allowed", method)
		}
	}
	if got, status := f.download(f.tokenRun("123", "attempt-1", "e2e", "4242"), "attempt-1", "e2e", "app"); status != 200 || got != "kept" {
		t.Fatalf("earlier attempt's artifact changed: %d %q", status, got)
	}
}

func TestArtifactStoreMigratesAnExistingDatabase(t *testing.T) {
	f := newArtifactFixture(t)
	f.upload(f.token("123", "attempt-1", "plan"), "attempt-1", "plan", "legacy", []byte("x"))
	if _, err := f.store.db.Exec("DROP INDEX artifacts_ghrun; ALTER TABLE artifacts DROP COLUMN ghrun"); err != nil {
		t.Fatal(err)
	}
	path := f.store.path
	f.store.db.Close()
	reopened, err := openArtifactStore(path, 32<<20, func() time.Time { return f.at }, f.budget)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	if a, err := reopened.named(artifactPrincipal{repo: "123", run: "attempt-1"}, "legacy"); err != nil || a.GitHubRun != "" {
		t.Fatalf("legacy row after migration: %+v %v", a, err)
	}
}
