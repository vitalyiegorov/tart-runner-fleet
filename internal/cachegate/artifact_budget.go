package cachegate

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

var errArtifactSpace = errors.New("artifact write budget unavailable")

// writeBudget is shared by cache, artifact and npm writes on one filesystem.
// reserve includes a conservative journal/copy peak. Unknown space fails closed.
type writeBudget struct {
	mu              sync.Mutex
	floor, reserved int64
	probe           func() (int64, error)
	refused         atomic.Int64
}

func (b *writeBudget) reserve(size int64) (func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	free, err := b.probe()
	if size < 0 || err != nil || size > free || b.reserved > free-size || b.floor > free-size-b.reserved {
		b.refused.Add(1)
		return nil, errArtifactSpace
	}
	b.reserved += size
	return func() { b.mu.Lock(); b.reserved -= size; b.mu.Unlock() }, nil
}

// budgetWriter reserves before every write, retaining reservations until the
// entire publication finishes. It protects unknown-length npm streams too.
type budgetWriter struct {
	io.Writer
	budget   *writeBudget
	releases []func()
}

func (w *budgetWriter) Write(p []byte) (int, error) {
	release, err := w.budget.reserve(int64(len(p))*3 + 4096)
	if err != nil {
		return 0, err
	}
	w.releases = append(w.releases, release)
	return w.Writer.Write(p)
}
func (w *budgetWriter) close() {
	for _, release := range w.releases {
		release()
	}
}
