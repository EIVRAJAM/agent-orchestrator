package scm

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func ownedSessionRepo(id, instance, branch string, tips ...string) sessionRepo {
	index := map[string]map[string]struct{}{}
	if branch != "" {
		index[branch] = map[string]struct{}{}
		for _, tip := range tips {
			index[branch][tip] = struct{}{}
		}
	}
	return sessionRepo{
		session:    domain.SessionRecord{ID: domain.SessionID(id), InstanceID: instance},
		branch:     branch,
		instanceID: instance,
		branchTips: index,
	}
}

func TestMatchSession_RequiresInstanceBranchAndHead(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     string
		head       string
		candidates []sessionRepo
		want       domain.SessionID
	}{
		{"exact registered tip", "ao/p-1/root", "sha1", []sessionRepo{ownedSessionRepo("p-1", "inst-1", "ao/p-1/root", "sha1")}, "p-1"},
		{"registered stacked tip", "ao/p-1/topic", "sha2", []sessionRepo{ownedSessionRepo("p-1", "inst-1", "ao/p-1/topic", "sha2")}, "p-1"},
		{"unregistered prefix", "ao/p-1/topic", "sha2", []sessionRepo{ownedSessionRepo("p-1", "inst-1", "ao/p-1/root", "sha1")}, ""},
		{"unregistered hyphen", "ao/p-1-topic", "sha2", []sessionRepo{ownedSessionRepo("p-1", "inst-1", "ao/p-1/root", "sha1")}, ""},
		{"wrong head", "ao/p-1/topic", "other", []sessionRepo{ownedSessionRepo("p-1", "inst-1", "ao/p-1/topic", "sha2")}, ""},
		{"empty head", "ao/p-1/topic", "", []sessionRepo{ownedSessionRepo("p-1", "inst-1", "ao/p-1/topic", "sha2")}, ""},
		{"ambiguous instances", "ao/p-1/topic", "sha2", []sessionRepo{
			ownedSessionRepo("p-1", "inst-1", "ao/p-1/topic", "sha2"),
			ownedSessionRepo("p-1", "inst-2", "ao/p-1/topic", "sha2"),
		}, ""},
		{"duplicate same instance", "ao/p-1/topic", "sha2", []sessionRepo{
			ownedSessionRepo("p-1", "inst-1", "ao/p-1/topic", "sha2"),
			ownedSessionRepo("p-1", "inst-1", "ao/p-1/topic", "sha2"),
		}, "p-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchSession(tc.candidates, tc.source, tc.head)
			if !ok {
				if tc.want != "" {
					t.Fatalf("no match, want %s", tc.want)
				}
				return
			}
			if got.session.ID != tc.want {
				t.Fatalf("matched %q, want %q", got.session.ID, tc.want)
			}
		})
	}
}
