package cachegate

import (
	"fmt"
	"strings"
	"sync/atomic"
)

type artifactMetrics struct{ used, staged, objects, ttl, lru, refused atomic.Int64 }

func (m *artifactMetrics) render(w *strings.Builder) {
	for _, metric := range []struct {
		name  string
		value int64
	}{
		{"used_bytes", m.used.Load()}, {"staged_bytes", m.staged.Load()}, {"objects", m.objects.Load()},
		{"ttl_evictions_total", m.ttl.Load()}, {"lru_evictions_total", m.lru.Load()}, {"auth_refusals_total", m.refused.Load()},
	} {
		fmt.Fprintf(w, "fleet_artifacts_%s %d\n", metric.name, metric.value)
	}
}

// observe is a coherent snapshot taken under the store mutex. Failed queries
// preserve the last valid observation instead of publishing an empty store.
func (s *artifactStore) observe() {
	if s.metrics == nil {
		return
	}
	var used, staged, objects int64
	if s.db.QueryRow("SELECT COALESCE(SUM(length(data)),0) FROM blocks").Scan(&used) != nil {
		return
	}
	if s.db.QueryRow("SELECT COALESCE(SUM(length(data)),0) FROM blocks JOIN artifacts ON artifacts.id=blocks.artifact WHERE artifacts.final=0").Scan(&staged) != nil {
		return
	}
	if s.db.QueryRow("SELECT COUNT(*) FROM artifacts").Scan(&objects) != nil {
		return
	}
	s.metrics.used.Store(used)
	s.metrics.staged.Store(staged)
	s.metrics.objects.Store(objects)
}
