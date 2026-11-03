package githubscaleset

import "context"

// Retirer is the port `fleet scale-sets retire` holds against a scope the
// configuration no longer names: the auditor's listing and live counters, plus
// the one delete by id (issue #475).
//
// It embeds the Auditor and NOT a Provisioner, so it can read and delete but
// never create or repair a scale set at a scope nothing on this node binds.
type Retirer struct {
	*Auditor
	provisioner Provisioner
}

// NewRetirer builds the audit client and reaches the delete through that same
// official client, so both halves share one credential path.
func NewRetirer(c GitHubAppAdminConfig) (Retirer, error) {
	auditor, client, err := newAuditor(c)
	if err != nil {
		return Retirer{}, err
	}
	return Retirer{Auditor: auditor, provisioner: Provisioner{Client: client}}, nil
}

// Delete removes one runner scale set by id; see Provisioner.Delete.
func (r Retirer) Delete(ctx context.Context, id int) error {
	return r.provisioner.Delete(ctx, id)
}
