package session

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// branchStateWriter is the narrow store write for observed branch facts.
type branchStateWriter interface {
	SetSessionBranchState(ctx context.Context, id domain.SessionID, state domain.SessionBranchState) (bool, error)
}

// branchOwnershipWriter records the exact local branch tip the workspace is
// linked to. It is optional so older embedders that only persist branch state
// keep working; the SCM observer simply fails closed without ownership.
type branchOwnershipWriter interface {
	RecordSessionBranchTip(ctx context.Context, id domain.SessionID, repoKey, branch, headSHA, worktreePath string) (bool, error)
}

// branchStateRemote is the remote AO pushes session branches to.
const branchStateRemote = "origin"

type branchTarget struct {
	rec        domain.SessionRecord
	repoKey    string
	branch     string // current HEAD branch; empty on detached HEAD
	rootBranch string // AO's original session branch
	head       string // detached HEAD commit
	baseRefs   []string
	baseSHA    string
}

// ReconcileBranchStates refreshes every live session's persisted branch facts.
// It runs on the SCM observer's tick: one git for-each-ref per repository reads
// every session's branch, remote, and base tips, and git counts commits only
// for sessions whose tips moved since the last pass.
func (s *Service) ReconcileBranchStates(ctx context.Context) error {
	writer, ok := s.store.(branchStateWriter)
	if !ok {
		return nil
	}
	recs, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return err
	}
	return s.reconcileBranchStates(ctx, writer, recs, true)
}

// ReconcileSessionBranchState is the single-session pass a workspace watch
// runs when it sees a change.
func (s *Service) ReconcileSessionBranchState(ctx context.Context, id domain.SessionID) error {
	writer, ok := s.store.(branchStateWriter)
	if !ok {
		return nil
	}
	rec, found, err := s.store.GetSession(ctx, id)
	if err != nil || !found {
		return err
	}
	return s.reconcileBranchStates(ctx, writer, []domain.SessionRecord{rec}, false)
}

func (s *Service) reconcileBranchStates(ctx context.Context, writer branchStateWriter, recs []domain.SessionRecord, full bool) error {
	// Serialized so an older read can never persist over a newer one when the
	// tick and a workspace change reconcile the same session at once.
	s.branchStateMu.Lock()
	defer s.branchStateMu.Unlock()
	if s.branchTips == nil {
		s.branchTips = map[domain.SessionID]string{}
	}

	type projectLookup struct {
		rec domain.ProjectRecord
		ok  bool
	}
	projects := map[domain.ProjectID]projectLookup{}
	repos := map[string][]branchTarget{}
	live := map[domain.SessionID]bool{}
	for _, rec := range recs {
		if rec.IsTerminated || rec.Metadata.WorkspacePath == "" || isStandaloneScratchWorkspace(rec) {
			continue
		}
		live[rec.ID] = true
		project, seen := projects[rec.ProjectID]
		if !seen {
			p, ok, err := s.sessionProject(ctx, rec)
			if err != nil {
				return err
			}
			project = projectLookup{rec: p, ok: ok}
			projects[rec.ProjectID] = project
		}
		if project.ok && project.rec.Kind.WithDefault() == domain.ProjectKindWorkspace {
			if err := s.reconcileWorkspaceOwnership(ctx, writer, rec); err != nil {
				return err
			}
			continue
		}
		commonDir, branch, head, ok := readGitHead(rec.Metadata.WorkspacePath)
		if !ok {
			continue
		}
		repoKey := ""
		origin := project.rec.RepoOriginURL
		if canonical := strings.TrimSpace(project.rec.Config.CanonicalRepoURL); canonical != "" {
			origin = canonical
		}
		if identity, err := domain.ParseRepositoryIdentity(origin); err == nil && identity.Provider != "" && identity.Host != "" {
			repoKey = identity.Key()
		}
		repos[commonDir] = append(repos[commonDir], branchTarget{
			rec:        rec,
			repoKey:    repoKey,
			branch:     branch,
			rootBranch: strings.TrimSpace(rec.Metadata.Branch),
			head:       head,
			baseRefs:   branchBaseRefs(rec.Metadata.DiffBaseRef, defaultBranchForProject(project.rec, project.ok)),
			baseSHA:    strings.TrimSpace(rec.Metadata.DiffBaseSHA),
		})
	}
	if full {
		for id := range s.branchTips {
			if !live[id] {
				delete(s.branchTips, id)
			}
		}
	}

	for _, targets := range repos {
		tips, ok := readRefTips(ctx, targets)
		if !ok {
			continue
		}
		for _, target := range targets {
			if err := s.reconcileBranchTarget(ctx, writer, target, tips); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) workspaceRepoKey(ctx context.Context, rec domain.SessionRecord, repoName string) string {
	project, ok, err := s.sessionProject(ctx, rec)
	if err != nil || !ok {
		return ""
	}
	origin := project.RepoOriginURL
	if repoName != domain.RootWorkspaceRepoName {
		repos, err := s.store.ListWorkspaceRepos(ctx, string(rec.ProjectID))
		if err != nil {
			return ""
		}
		for _, repo := range repos {
			if repo.Name == repoName {
				origin = repo.RepoOriginURL
				break
			}
		}
	}
	identity, err := domain.ParseRepositoryIdentity(origin)
	if err != nil || identity.Provider == "" || identity.Host == "" {
		return ""
	}
	return identity.Key()
}

// reconcileWorkspaceOwnership records exact branch tips for each materialized
// child worktree in a workspace project. Workspace projects skip the
// single-repo branch-state path because their root may not be a Git checkout,
// but their PRs still need durable instance ownership.
func (s *Service) reconcileWorkspaceOwnership(ctx context.Context, writer branchStateWriter, rec domain.SessionRecord) error {
	owner, ok := writer.(branchOwnershipWriter)
	if !ok {
		return nil
	}
	rows, err := s.store.ListSessionWorktrees(ctx, rec.ID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Branch == "" || row.WorktreePath == "" {
			continue
		}
		repoKey := row.RepoKey
		if repoKey == "" {
			repoKey = s.workspaceRepoKey(ctx, rec, row.RepoName)
		}
		if repoKey == "" {
			continue
		}
		_, branch, _, ok := readGitHead(row.WorktreePath)
		if !ok {
			continue
		}
		if row.RepoKey != "" && branch != "" {
			if raw, err := gitWorkspaceOutput(ctx, row.WorktreePath, "rev-parse", "refs/heads/"+branch); err == nil {
				if headSHA := strings.TrimSpace(raw); headSHA != "" {
					if _, err := owner.RecordSessionBranchTip(ctx, rec.ID, row.RepoKey, branch, headSHA, row.WorktreePath); err != nil {
						return err
					}
				}
			}
		}
		if row.Branch != branch {
			if raw, err := gitWorkspaceOutput(ctx, row.WorktreePath, "rev-parse", "refs/heads/"+row.Branch); err == nil {
				if headSHA := strings.TrimSpace(raw); headSHA != "" {
					if _, err := owner.RecordSessionBranchTip(ctx, rec.ID, repoKey, row.Branch, headSHA, row.WorktreePath); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (s *Service) reconcileBranchTarget(ctx context.Context, writer branchStateWriter, target branchTarget, tips map[string]string) error {
	local := target.head
	remote := ""
	if target.branch != "" {
		local = tips["refs/heads/"+target.branch]
		remote = tips["refs/remotes/"+branchStateRemote+"/"+target.branch]
	}
	if local == "" {
		return nil // unborn branch
	}
	base := target.baseSHA
	for _, ref := range target.baseRefs {
		if sha := tips[ref]; sha != "" {
			base = sha
			break
		}
	}
	if owner, ok := writer.(branchOwnershipWriter); ok {
		// Only local branch refs are evidence that this workspace owns a tip.
		// origin/<branch> may be a stale remote left by another session/data dir;
		// it is useful for BranchState, not for ownership.
		if target.repoKey != "" && target.branch != "" {
			if local := tips["refs/heads/"+target.branch]; local != "" {
				if _, err := owner.RecordSessionBranchTip(ctx, target.rec.ID, target.repoKey, target.branch, local, target.rec.Metadata.WorkspacePath); err != nil {
					return err
				}
			}
		}
		if target.repoKey != "" && target.rootBranch != "" {
			if rootLocal := tips["refs/heads/"+target.rootBranch]; rootLocal != "" {
				if _, err := owner.RecordSessionBranchTip(ctx, target.rec.ID, target.repoKey, target.rootBranch, rootLocal, target.rec.Metadata.WorkspacePath); err != nil {
					return err
				}
			}
		}
	}
	rec := target.rec
	fingerprint := local + " " + remote + " " + base
	if rec.BranchState != nil && s.branchTips[rec.ID] == fingerprint {
		return nil
	}

	root := rec.Metadata.WorkspacePath
	state := domain.SessionBranchState{Branch: target.branch, HeadSHA: local}
	if base != "" {
		commits, ok := gitRevListCount(ctx, root, base+".."+local)
		if !ok {
			// A failed read is not evidence that nothing was committed.
			return nil
		}
		state.Commits = commits
	}
	switch remote {
	case "":
		state.Unpushed = state.Commits
	case local:
		state.RemoteBranch = branchStateRemote + "/" + target.branch
	default:
		unpushed, ok := gitRevListCount(ctx, root, remote+".."+local)
		if !ok {
			return nil
		}
		state.RemoteBranch = branchStateRemote + "/" + target.branch
		state.Unpushed = unpushed
	}
	if rec.BranchState == nil || *rec.BranchState != state {
		if _, err := writer.SetSessionBranchState(ctx, rec.ID, state); err != nil {
			return err
		}
	}
	s.branchTips[rec.ID] = fingerprint
	return nil
}

// readRefTips lists the branch, remote, and base tips of every target in one
// repository with a single git process.
func readRefTips(ctx context.Context, targets []branchTarget) (map[string]string, bool) {
	args := []string{"for-each-ref", "--format=%(objectname) %(refname)"}
	seen := map[string]bool{}
	add := func(ref string) {
		if !seen[ref] {
			seen[ref] = true
			args = append(args, ref)
		}
	}
	for _, target := range targets {
		if target.branch != "" {
			add("refs/heads/" + target.branch)
			add("refs/remotes/" + branchStateRemote + "/" + target.branch)
		}
		if target.rootBranch != "" && target.rootBranch != target.branch {
			add("refs/heads/" + target.rootBranch)
			add("refs/remotes/" + branchStateRemote + "/" + target.rootBranch)
		}
		for _, ref := range target.baseRefs {
			add(ref)
		}
	}
	out, err := gitWorkspaceOutput(ctx, targets[0].rec.Metadata.WorkspacePath, args...)
	if err != nil {
		return nil, false
	}
	tips := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if sha, ref, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			tips[ref] = sha
		}
	}
	return tips, true
}

// branchBaseRefs returns the full ref names the session's diff base resolves
// from, in the same preference order the Files tab uses.
func branchBaseRefs(recordedRef, defaultBranch string) []string {
	ref := strings.TrimSpace(recordedRef)
	if ref == "" || ref == "HEAD" {
		ref = defaultBranch
	}
	candidates := workspaceBaseRefCandidates(ref)
	refs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		switch {
		case strings.HasPrefix(candidate, "refs/"):
		case strings.HasPrefix(candidate, "origin/"):
			candidate = "refs/remotes/" + candidate
		default:
			candidate = "refs/heads/" + candidate
		}
		refs = append(refs, candidate)
	}
	return refs
}

// readGitHead reads a worktree's common git dir and HEAD from the files git
// itself reads, so finding them costs no git process.
func readGitHead(root string) (commonDir, branch, head string, ok bool) {
	gitDir := filepath.Join(root, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return "", "", "", false
	}
	if !info.IsDir() {
		raw, err := os.ReadFile(gitDir)
		if err != nil {
			return "", "", "", false
		}
		path, found := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
		if !found {
			return "", "", "", false
		}
		gitDir = filepath.FromSlash(strings.TrimSpace(path))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
	}
	commonDir = gitDir
	if raw, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		commonDir = filepath.FromSlash(strings.TrimSpace(string(raw)))
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(gitDir, commonDir)
		}
	}
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", "", "", false
	}
	text := strings.TrimSpace(string(raw))
	if ref, symbolic := strings.CutPrefix(text, "ref: "); symbolic {
		branch, ok = strings.CutPrefix(ref, "refs/heads/")
		return filepath.Clean(commonDir), branch, "", ok
	}
	return filepath.Clean(commonDir), "", text, text != ""
}

func gitRevListCount(ctx context.Context, root, rangeSpec string) (int, bool) {
	out, err := gitWorkspaceOutput(ctx, root, "rev-list", "--count", rangeSpec)
	if err != nil {
		return 0, false
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	return count, err == nil
}
