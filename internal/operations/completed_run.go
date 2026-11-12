package operations

// RunIdentity identifies the recorded workflow run attempt whose demand is retired.
// Attempt zero denotes a broker row whose attempt is unknown.
type RunIdentity struct {
	Owner, Repository string
	RunID             int64
	Attempt           int
}
