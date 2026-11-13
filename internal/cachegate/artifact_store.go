package cachegate

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // the artifact database is separate from fleet.db
)

var errArtifactMissing = errors.New("artifact not found")
var errArtifactInput = errors.New("invalid artifact request")

// Blocks and the publication record share SQLite's FULL synchronous transaction.
// No filesystem rename/index split or second recovery mechanism is needed.
type artifactStore struct {
	mu      sync.Mutex
	db      *sql.DB
	path    string
	cap     int64
	clock   func() time.Time
	budget  *writeBudget
	secret  [32]byte
	leases  map[int64]int
	metrics *artifactMetrics
}
type artifactRecord struct {
	ID                       int64
	Repo, Run, Job, Name     string
	Created, Expires, Access int64
	Size                     int64
	Hash                     string
	Blocks                   string
	Final                    bool
}

const artifactColumns = "id,repo,run,job,name,created,expires,access,size,hash,blocks,final"

func openArtifactStore(path string, capBytes int64, clock func() time.Time, budget *writeBudget) (*artifactStore, error) {
	if capBytes <= 0 || capBytes > 1<<50 {
		return nil, errArtifactInput
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		return nil, errArtifactInput
	}
	// Restrict the database before SQLite creates a journal beside it.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- private path from operator configuration.
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || info.Mode().Perm()&0o077 != 0 {
		return nil, errArtifactInput
	}
	// Driver DSN pragmas apply to every replacement connection too.
	pragmas := url.Values{"_pragma": []string{"foreign_keys(1)", "synchronous(FULL)", "busy_timeout(5000)", fmt.Sprintf("max_page_count(%d)", (capBytes+(8<<20))/4096)}}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: pragmas.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &artifactStore{db: db, path: path, cap: capBytes, clock: clock, budget: budget, leases: map[int64]int{}}
	_, _ = rand.Read(s.secret[:])
	for _, query := range []string{
		"PRAGMA journal_mode=DELETE",
		`CREATE TABLE IF NOT EXISTS artifacts(id INTEGER PRIMARY KEY AUTOINCREMENT CHECK(id<=9007199254740991),repo TEXT NOT NULL,run TEXT NOT NULL,job TEXT NOT NULL,name TEXT NOT NULL,created INTEGER NOT NULL,expires INTEGER NOT NULL,access INTEGER NOT NULL,size INTEGER NOT NULL DEFAULT 0,hash TEXT NOT NULL DEFAULT '',blocks TEXT NOT NULL DEFAULT '[]',final INTEGER NOT NULL DEFAULT 0,UNIQUE(repo,run,name))`,
		`CREATE TABLE IF NOT EXISTS blocks(artifact INTEGER NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,key TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(artifact,key))`,
	} {
		if _, err = db.Exec(query); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return s, nil
}

func scanArtifact(row interface{ Scan(...any) error }) (artifactRecord, error) {
	var a artifactRecord
	err := row.Scan(&a.ID, &a.Repo, &a.Run, &a.Job, &a.Name, &a.Created, &a.Expires, &a.Access, &a.Size, &a.Hash, &a.Blocks, &a.Final)
	if errors.Is(err, sql.ErrNoRows) {
		err = errArtifactMissing
	}
	return a, err
}
func (s *artifactStore) record(id int64) (artifactRecord, error) {
	return scanArtifact(s.db.QueryRow("SELECT "+artifactColumns+" FROM artifacts WHERE id=?", id))
}
func (s *artifactStore) named(p artifactPrincipal, name string) (artifactRecord, error) {
	return scanArtifact(s.db.QueryRow("SELECT "+artifactColumns+" FROM artifacts WHERE repo=? AND run=? AND name=?", p.repo, p.run, name))
}

// makeRoom evicts expired entries first, then finalized entries by LRU. The
// caller holds mu, including across downloads, so an active transfer is leased.
func (s *artifactStore) makeRoom(extra, keep int64) error {
	at := s.clock().Unix()
	ids, err := s.candidates("SELECT id FROM artifacts WHERE expires<=? OR (final=0 AND access<=?) ORDER BY id", at, at-int64(time.Hour/time.Second))
	if err != nil {
		return err
	}
	for _, id := range ids {
		if s.leases[id] == 0 {
			if _, err = s.db.Exec("DELETE FROM artifacts WHERE id=?", id); err != nil {
				return err
			}
			if s.metrics != nil {
				s.metrics.ttl.Add(1)
			}
		}
	}
	for {
		var used, count int64
		if err := s.db.QueryRow("SELECT COALESCE(SUM(length(data)),0) FROM blocks").Scan(&used); err != nil {
			return err
		}
		if err := s.db.QueryRow("SELECT COUNT(*) FROM artifacts").Scan(&count); err != nil {
			return err
		}
		if extra <= s.cap && used <= s.cap-extra && count < 10000 {
			return nil
		}
		ids, err := s.candidates("SELECT id FROM artifacts WHERE final=1 AND id<>? ORDER BY access,id", keep)
		if err != nil {
			return err
		}
		var victim int64
		for _, id := range ids {
			if s.leases[id] == 0 {
				victim = id
				break
			}
		}
		if victim == 0 {
			return errArtifactSpace
		}
		if _, err := s.db.Exec("DELETE FROM artifacts WHERE id=?", victim); err != nil {
			return err
		}
		if s.metrics != nil {
			s.metrics.lru.Add(1)
		}
	}
}
func (s *artifactStore) create(p artifactPrincipal, name string, expires time.Time) (artifactRecord, error) {
	if err := s.makeRoom(0, 0); err != nil {
		return artifactRecord{}, err
	}
	if a, err := s.named(p, name); err == nil {
		if a.Job != p.job || a.Final {
			return artifactRecord{}, errArtifactInput
		}
		return a, nil
	} else if !errors.Is(err, errArtifactMissing) {
		return a, err
	}
	at := s.clock()
	limit := at.Add(72 * time.Hour)
	if expires.IsZero() || expires.After(limit) {
		expires = limit
	}
	if !expires.After(at) {
		return artifactRecord{}, errArtifactInput
	}
	result, err := s.db.Exec("INSERT INTO artifacts(repo,run,job,name,created,expires,access) VALUES(?,?,?,?,?,?,?)", p.repo, p.run, p.job, name, at.Unix(), expires.Unix(), at.Unix())
	if err != nil {
		return artifactRecord{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return artifactRecord{}, err
	}
	return s.record(id)
}
func (s *artifactStore) putBlock(a artifactRecord, key string, data []byte) error {
	if a.Final || a.Blocks != "[]" {
		return errArtifactInput
	}
	var old int64
	err := s.db.QueryRow("SELECT length(data) FROM blocks WHERE artifact=? AND key=?", a.ID, key).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := s.makeRoom(max(0, int64(len(data))-old), a.ID); err != nil {
		return err
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM blocks WHERE artifact=?", a.ID).Scan(&count); err != nil {
		return err
	}
	if count >= 10000 {
		return errArtifactSpace
	}
	_, err = s.db.Exec("INSERT INTO blocks(artifact,key,data) VALUES(?,?,?) ON CONFLICT(artifact,key) DO UPDATE SET data=excluded.data", a.ID, key, data)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE artifacts SET access=? WHERE id=?", s.clock().Unix(), a.ID)
	return err
}
func (s *artifactStore) commitBlocks(a artifactRecord, keys []string) error {
	if a.Final || len(keys) == 0 || len(keys) > 10000 {
		return errArtifactInput
	}
	unique := map[string]bool{}
	for _, key := range keys {
		var exists bool
		if unique[key] {
			return errArtifactInput
		}
		unique[key] = true
		if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM blocks WHERE artifact=? AND key=?)", a.ID, key).Scan(&exists); err != nil {
			return err
		} else if !exists {
			return errArtifactInput
		}
	}
	encoded, _ := json.Marshal(keys)
	if a.Blocks != "[]" && a.Blocks != string(encoded) {
		return errArtifactInput
	}
	_, err := s.db.Exec("UPDATE artifacts SET blocks=? WHERE id=?", string(encoded), a.ID)
	return err
}
func (s *artifactStore) stream(a artifactRecord, w io.Writer) (int64, error) {
	var keys []string
	if json.Unmarshal([]byte(a.Blocks), &keys) != nil || len(keys) == 0 {
		return 0, errArtifactInput
	}
	var total int64
	for _, key := range keys {
		var data []byte
		if err := s.db.QueryRow("SELECT data FROM blocks WHERE artifact=? AND key=?", a.ID, key).Scan(&data); err != nil {
			return total, err
		}
		n, err := w.Write(data)
		total += int64(n)
		if err != nil {
			return total, err
		}
		if n != len(data) {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}
func (s *artifactStore) list(p artifactPrincipal) ([]artifactRecord, error) {
	rows, err := s.db.Query("SELECT "+artifactColumns+" FROM artifacts WHERE repo=? AND run=? AND final=1 AND expires>? ORDER BY id", p.repo, p.run, s.clock().Unix())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	all := []artifactRecord{}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, a)
	}
	return all, rows.Err()
}

// DELETE journaling can copy existing pages before a transaction overwrites
// them. Reserve the entire current database plus a bounded block's growth,
// rather than assuming a tiny XML commit costs only its request length.
func (s *artifactStore) reserveWrite() (func(), error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return nil, err
	}
	return s.budget.reserve(info.Size()*2 + artifactBlockLimit*3 + (1 << 20))
}

func (s *artifactStore) candidates(query string, args ...any) ([]int64, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
