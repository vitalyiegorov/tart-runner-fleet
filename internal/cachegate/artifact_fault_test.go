package cachegate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

// These tests inject I/O failures at the SQLite boundary, not successful empty
// responses. Production always uses the normal SQLite driver.
var artifactFaultSerial atomic.Int64
var errArtifactIO = errors.New("injected artifact database I/O failure")

type artifactFaultDriver struct {
	match string
	write bool
}

func (d *artifactFaultDriver) Open(path string) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(path)
	if err != nil {
		return nil, err
	}
	return &artifactFaultConnection{Conn: conn, fail: d}, nil
}

type artifactFaultConnection struct {
	driver.Conn
	fail *artifactFaultDriver
}

func (c *artifactFaultConnection) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if c.fail.write && strings.Contains(q, c.fail.match) {
		return nil, errArtifactIO
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (c *artifactFaultConnection) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if !c.fail.write && strings.Contains(q, c.fail.match) {
		return nil, errArtifactIO
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func artifactInjectFailure(t *testing.T, f *artifactFixture, match string, write bool) {
	t.Helper()
	f.store.db.Close()
	name := fmt.Sprintf("artifact-fault-%d", artifactFaultSerial.Add(1))
	sql.Register(name, &artifactFaultDriver{match, write})
	db, err := sql.Open(name, f.store.path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	f.store.db = db
}

func TestArtifactStorageIOErrorsNeverBecomeSuccess(t *testing.T) {
	tests := []struct {
		name, match string
		write       bool
		op          func(*artifactFixture, artifactRecord) error
	}{
		{"expiry selection", "SELECT id FROM artifacts WHERE expires", false, func(f *artifactFixture, _ artifactRecord) error { return f.store.makeRoom(0, 0) }},
		{"staged byte accounting", "SUM(length(data))", false, func(f *artifactFixture, _ artifactRecord) error { return f.store.makeRoom(0, 0) }},
		{"object accounting", "COUNT(*) FROM artifacts", false, func(f *artifactFixture, _ artifactRecord) error { return f.store.makeRoom(0, 0) }},
		{"LRU selection", "SELECT id FROM artifacts WHERE final", false, func(f *artifactFixture, _ artifactRecord) error { f.store.cap = 1; return f.store.makeRoom(5, 0) }},
		{"LRU removal", "DELETE FROM artifacts", true, func(f *artifactFixture, _ artifactRecord) error { f.store.cap = 1; return f.store.makeRoom(5, 0) }},
		{"expiry removal", "DELETE FROM artifacts", true, func(f *artifactFixture, _ artifactRecord) error {
			f.at = f.at.Add(73 * time.Hour)
			return f.store.makeRoom(0, 0)
		}},
		{"create publication", "INSERT INTO artifacts", true, func(f *artifactFixture, _ artifactRecord) error {
			_, err := f.store.create(artifactPrincipal{repo: "123", run: "r", job: "j"}, "new", time.Time{})
			return err
		}},
		{"create lookup", "WHERE repo=?", false, func(f *artifactFixture, _ artifactRecord) error {
			_, err := f.store.create(artifactPrincipal{repo: "123", run: "r", job: "j"}, "new", time.Time{})
			return err
		}},
		{"block previous size", "SELECT length(data)", false, func(f *artifactFixture, a artifactRecord) error {
			a.Final = false
			a.Blocks = "[]"
			return f.store.putBlock(a, "a2V5", []byte("x"))
		}},
		{"block count", "COUNT(*) FROM blocks", false, func(f *artifactFixture, a artifactRecord) error {
			a.Final = false
			a.Blocks = "[]"
			return f.store.putBlock(a, "a2V5", []byte("x"))
		}},
		{"block persistence", "INSERT INTO blocks", true, func(f *artifactFixture, a artifactRecord) error {
			a.Final = false
			a.Blocks = "[]"
			return f.store.putBlock(a, "a2V5", []byte("x"))
		}},
		{"block activity", "UPDATE artifacts SET access", true, func(f *artifactFixture, a artifactRecord) error {
			a.Final = false
			a.Blocks = "[]"
			return f.store.putBlock(a, "a2V5", []byte("x"))
		}},
		{"block existence", "SELECT EXISTS", false, func(f *artifactFixture, a artifactRecord) error {
			a.Final = false
			return f.store.commitBlocks(a, []string{"YmxvY2stMQ=="})
		}},
		{"block-list publication", "UPDATE artifacts SET blocks", true, func(f *artifactFixture, a artifactRecord) error {
			a.Final = false
			return f.store.commitBlocks(a, []string{"YmxvY2stMQ=="})
		}},
		{"download bytes", "SELECT data", false, func(f *artifactFixture, a artifactRecord) error { _, err := f.store.stream(a, io.Discard); return err }},
		{"final publication", "UPDATE artifacts SET final", true, func(f *artifactFixture, a artifactRecord) error {
			a.Final = false
			_, err := f.store.finalize(a, artifactRequest{Size: "1"})
			return err
		}},
		{"download recency", "UPDATE artifacts SET access", true, func(f *artifactFixture, _ artifactRecord) error {
			_, err := f.store.rpc(artifactPrincipal{repo: "123", run: "r", job: "j"}, "GetSignedArtifactURL", artifactRequest{Name: "a", Job: "j"}, f.origin)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newArtifactFixture(t)
			f.upload(f.token("123", "r", "j"), "r", "j", "a", []byte("x"))
			a, err := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "a")
			if err != nil {
				t.Fatal(err)
			}
			artifactInjectFailure(t, f, test.match, test.write)
			if err = test.op(f, a); err == nil {
				t.Fatal("I/O failure acknowledged as success")
			}
		})
	}
}

type artifactFailedWriter struct{ short bool }

func (w artifactFailedWriter) Write([]byte) (int, error) {
	if w.short {
		return 0, nil
	}
	return 0, io.ErrClosedPipe
}
func TestArtifactCorruptIndexAndInterruptedStream(t *testing.T) {
	f := newArtifactFixture(t)
	token := f.token("123", "r", "j")
	f.upload(token, "r", "j", "a", []byte("x"))
	a, _ := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "a")
	for _, w := range []io.Writer{artifactFailedWriter{}, artifactFailedWriter{short: true}} {
		if _, err := f.store.stream(a, w); err == nil {
			t.Fatal("broken stream reported success")
		}
	}
	a.Blocks = "not json"
	if _, err := f.store.stream(a, io.Discard); err == nil {
		t.Fatal("corrupt block-list became empty bytes")
	}
	if err := f.store.putBlock(a, "key", nil); err == nil {
		t.Fatal("final blob changed")
	}
	a.Final = false
	a.Blocks = "[]"
	f.store.cap = 0
	if err := f.store.putBlock(a, "key", []byte("large")); err == nil {
		t.Fatal("oversize staged write allowed")
	}
	f.store.cap = 1024
	for _, keys := range [][]string{nil, {"YmxvY2stMQ==", "YmxvY2stMQ=="}} {
		if f.store.commitBlocks(a, keys) == nil {
			t.Fatal("bad block-list accepted")
		}
	}
	a.Blocks = `["other"]`
	if f.store.commitBlocks(a, []string{"YmxvY2stMQ=="}) == nil {
		t.Fatal("changed committed block-list allowed")
	}
	if _, err := f.store.finalize(a, artifactRequest{Size: "invalid"}); err == nil {
		t.Fatal("invalid size accepted")
	}
	f.store.db.Exec("UPDATE artifacts SET created='corrupt' WHERE name='a'")
	if _, err := f.store.list(artifactPrincipal{repo: "123", run: "r"}); err == nil {
		t.Fatal("corrupt listing became empty")
	}
	f.store.metrics = &artifactMetrics{}
	artifactInjectFailure(t, f, "WHERE artifacts.final=0", false)
	f.store.observe() // keep prior metrics on unreadable staging
}

func TestArtifactPrivateStorageAndInvalidRPCs(t *testing.T) {
	f := newArtifactFixture(t)
	token := f.token("123", "r", "j")
	if f.rpc(token, "CreateArtifact", artifactBody("r", "j", "")).Code != http.StatusBadRequest {
		t.Fatal("empty artifact name accepted")
	}
	body := artifactBody("r", "j", "a")
	body["version"] = 3
	if f.rpc(token, "CreateArtifact", body).Code != http.StatusBadRequest {
		t.Fatal("old client accepted")
	}
	f.create(token, "r", "j", "a")
	f.create(token, "r", "j", "a") // retry creates a fresh capability for the same object
	if _, err := f.store.create(artifactPrincipal{repo: "123", run: "r", job: "other"}, "a", time.Time{}); err == nil {
		t.Fatal("another producer replaced unfinished upload")
	}
	f.store.db.Close()
	if _, err := f.store.create(artifactPrincipal{}, "a", time.Time{}); err == nil {
		t.Fatal("unavailable cleanup allowed create")
	}
	os.Remove(f.store.path)
	if _, err := f.store.reserveWrite(); err == nil {
		t.Fatal("missing storage allowed reservation")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0755)
	if _, err := openArtifactStore(filepath.Join(dir, "artifacts.db"), 1024, func() time.Time { return f.at }, f.budget); err == nil {
		t.Fatal("public artifact directory allowed")
	}
	os.Chmod(dir, 0700)
	path := filepath.Join(dir, "artifacts.db")
	os.Mkdir(path, 0700)
	if _, err := openArtifactStore(path, 1024, func() time.Time { return f.at }, f.budget); err == nil {
		t.Fatal("directory used as database")
	}
}

func TestArtifactMetadataAndBlockCountBounds(t *testing.T) {
	f := newArtifactFixture(t)
	f.create(f.token("123", "r", "j"), "r", "j", "a")
	a, _ := f.store.named(artifactPrincipal{repo: "123", run: "r"}, "a")
	if _, err := f.store.db.Exec(`WITH RECURSIVE ids(n) AS(SELECT 1 UNION ALL SELECT n+1 FROM ids WHERE n<10000) INSERT INTO blocks(artifact,key,data) SELECT ?, 'block'||n, X'' FROM ids`, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.putBlock(a, "another", nil); !errors.Is(err, errArtifactSpace) {
		t.Fatal("block count abuse allowed")
	}
	if _, err := f.store.candidates("SELECT 'corrupt-id'"); err == nil {
		t.Fatal("corrupt candidate silently ignored")
	}
	private := t.TempDir()
	os.Chmod(private, 0700)
	path := filepath.Join(private, "index")
	os.WriteFile(path, nil, 0644)
	if _, err := openArtifactStore(path, 1024, func() time.Time { return f.at }, f.budget); err == nil {
		t.Fatal("public index allowed")
	}
	f.store.metrics = &artifactMetrics{}
	artifactInjectFailure(t, f, "COUNT(*) FROM artifacts", false)
	f.store.observe()
}

func TestArtifactOwnerRefusesDifferentStorageFilesystem(t *testing.T) {
	root, err := os.MkdirTemp("/dev/shm", "artifact-509-test-")
	if err != nil {
		return
	} // Darwin may have no second filesystem; the byte predicate is portable.
	t.Cleanup(func() { os.RemoveAll(root) })
	local := t.TempDir()
	if sameArtifactFilesystem(local, root) {
		return
	}
	c := &artifactConfig{dir: root, origin: "http://gate.test", repos: "123", budgetPath: local, cap: 1024, floor: 1, cachePeak: 1}
	if _, _, _, err := c.open(nil, &Counters{}); err == nil {
		t.Fatal("owner wrote outside guarded filesystem")
	}
}

func TestArtifactOwnerChecksFilesystemBeforeCreatingDatabase(t *testing.T) {
	root, err := os.MkdirTemp("/dev/shm", "artifact-509-test-")
	if err != nil {
		return
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	local := t.TempDir()
	if sameArtifactFilesystem(local, root) {
		return
	}
	c := &artifactConfig{dir: root, origin: "http://gate.test", repos: "123", budgetPath: local, cap: 1024, floor: 1, cachePeak: 1}
	if _, _, _, err := c.open(nil, &Counters{}); err == nil {
		t.Fatal("owner allowed foreign filesystem")
	}
	if _, err := os.Stat(filepath.Join(root, "artifacts.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unguarded database created on foreign filesystem before rejection")
	}
}

func TestArtifactReconnectKeepsStorageConstraints(t *testing.T) {
	f := newArtifactFixture(t)
	f.store.db.SetMaxIdleConns(0)
	for _, query := range []string{"PRAGMA foreign_keys", "PRAGMA max_page_count"} {
		var actual int64
		if err := f.store.db.QueryRow(query).Scan(&actual); err != nil {
			t.Fatal(err)
		}
		expected := int64(1)
		if strings.Contains(query, "max_page_count") {
			expected = (f.store.cap + (8 << 20)) / 4096
		}
		if actual != expected {
			t.Fatalf("reconnected SQLite lost storage constraint %s", query)
		}
	}
}
