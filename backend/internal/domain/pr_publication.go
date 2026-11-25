package domain

// PRPublicationEvidence is the exact chain required before an issue-backed
// session's PR may be treated as published for review. It deliberately does
// not use branch prefixes, session root branches, or the local remote-tracking
// branch name: the durable ownership row and the registered branch tip must
// name the PR's own source branch and head.
type PRPublicationEvidence struct {
	SessionID     SessionID
	InstanceID    string
	RepoKey       string
	SessionPR     bool
	PRURL         string
	SourceBranch  string
	HeadSHA       string
	Draft         bool
	Merged        bool
	Closed        bool
	CI            CIState
	Ownership     SessionPROwnership
	TipRegistered bool
}

// Exact reports whether the PR is owned by this concrete session instance,
// repository, source branch, and head, with that exact branch tip registered.
func (e PRPublicationEvidence) Exact() bool {
	if e.SessionID == "" || e.InstanceID == "" || e.RepoKey == "" || !e.SessionPR {
		return false
	}
	if e.PRURL == "" || e.SourceBranch == "" || e.HeadSHA == "" || !e.TipRegistered {
		return false
	}
	return e.Ownership.PRURL == e.PRURL &&
		e.Ownership.SessionInstanceID == e.InstanceID &&
		e.Ownership.RepoKey == e.RepoKey &&
		e.Ownership.SourceBranch == e.SourceBranch &&
		e.Ownership.HeadSHA == e.HeadSHA
}

// Ready reports whether the exact, owned PR is open and has green CI. Push
// observation may be unknown or attempted-but-unconfirmed: an open PR at the
// exact owned head is itself durable remote publication evidence.
func (e PRPublicationEvidence) Ready() bool {
	return e.Exact() && !e.Draft && !e.Merged && !e.Closed && e.CI == CIPassing
}
