package domain

import "testing"

func exactPublicationEvidence() PRPublicationEvidence {
	const (
		url    = "https://github.com/acme/demo/pull/39"
		branch = "ao/worker-1/completion-percentage"
		head   = "3ecaf90aae882083d6b9a6a374b8b9725c3776d7"
	)
	return PRPublicationEvidence{
		SessionID:     "worker-1",
		InstanceID:    "inst-1",
		RepoKey:       "github|github.com|acme/demo",
		SessionPR:     true,
		PRURL:         url,
		SourceBranch:  branch,
		HeadSHA:       head,
		CI:            CIPassing,
		TipRegistered: true,
		Ownership: SessionPROwnership{
			PRURL:             url,
			SessionInstanceID: "inst-1",
			RepoKey:           "github|github.com|acme/demo",
			SourceBranch:      branch,
			HeadSHA:           head,
		},
	}
}

func TestPRPublicationEvidenceRequiresExactOwnedTip(t *testing.T) {
	if got := exactPublicationEvidence(); !got.Exact() || !got.Ready() {
		t.Fatalf("exact evidence = %+v, want exact and ready", got)
	}

	tests := []struct {
		name   string
		mutate func(*PRPublicationEvidence)
	}{
		{"foreign session PR", func(e *PRPublicationEvidence) { e.SessionPR = false }},
		{"foreign instance", func(e *PRPublicationEvidence) { e.Ownership.SessionInstanceID = "inst-2" }},
		{"foreign repository", func(e *PRPublicationEvidence) { e.Ownership.RepoKey = "github|github.com|acme/other" }},
		{"foreign branch", func(e *PRPublicationEvidence) { e.Ownership.SourceBranch = "ao/worker-1/root" }},
		{"foreign head", func(e *PRPublicationEvidence) { e.Ownership.HeadSHA = "head-other" }},
		{"unregistered tip", func(e *PRPublicationEvidence) { e.TipRegistered = false }},
		{"root branch prefix is not ownership", func(e *PRPublicationEvidence) {
			e.SourceBranch = "ao/worker-1/root"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evidence := exactPublicationEvidence()
			tt.mutate(&evidence)
			if evidence.Exact() || evidence.Ready() {
				t.Fatalf("evidence = %+v, want not exact and not ready", evidence)
			}
		})
	}
}

func TestPRPublicationEvidenceRequiresOpenPRAndGreenCI(t *testing.T) {
	for name, mutate := range map[string]func(*PRPublicationEvidence){
		"draft":      func(e *PRPublicationEvidence) { e.Draft = true },
		"merged":     func(e *PRPublicationEvidence) { e.Merged = true },
		"closed":     func(e *PRPublicationEvidence) { e.Closed = true },
		"ci pending": func(e *PRPublicationEvidence) { e.CI = CIPending },
		"ci failing": func(e *PRPublicationEvidence) { e.CI = CIFailing },
		"ci unknown": func(e *PRPublicationEvidence) { e.CI = "" },
	} {
		t.Run(name, func(t *testing.T) {
			evidence := exactPublicationEvidence()
			mutate(&evidence)
			if !evidence.Exact() || evidence.Ready() {
				t.Fatalf("evidence = %+v, want exact but not ready", evidence)
			}
		})
	}
}
