package cachegate

import "time"

var artifactSweepInterval = time.Minute

// Expiry is checked on every lookup; this sweep reclaims idle staging and
// expired records even when no new uploads arrive. Stop joins before DB close.
func artifactMaintenance(s *artifactStore) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(artifactSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.prune()
			}
		}
	}()
	return func() { close(stop); <-done }
}
func (s *artifactStore) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := s.reserveWrite()
	if err != nil {
		return
	}
	defer release()
	_ = s.makeRoom(0, 0)
	s.observe()
}
