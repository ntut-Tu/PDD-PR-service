package pulls_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/pulls"
	"pdd/services/internal/gitgateway/repos"
)

func limited(p *fakeProvider, patchBytes int64) *pulls.Pulls {
	content := repos.New(map[model.Provider]repos.Provider{model.GitHub: p}, func() time.Time { return now }, 0)
	return pulls.New(map[model.Provider]pulls.Provider{model.GitHub: p}, content, func() time.Time { return now }, patchBytes)
}

var ghRepo = model.Repo{Provider: model.GitHub}

func TestPullFilesAddsCommitsAndDropsPatchesOverBudget(t *testing.T) {
	p := &fakeProvider{
		detail: model.PullDetail{Pull: model.Pull{HeadSHA: "head"}, BaseSHA: "base"},
		files: provider.ChangedFiles{Files: []model.ChangedFile{
			{Path: "a", Patch: "12345"}, {Path: "b", Patch: "123456"}, {Path: "c", Patch: "12"}, {Path: "d"},
		}},
	}

	res, err := limited(p, 8).PullFiles(context.Background(), "tok", ghRepo, 1)

	if err != nil || res.HeadSHA != "head" || res.BaseSHA != "base" || !res.Truncated {
		t.Fatalf("result = %+v, %v", res, err)
	}
	patches := []string{res.Files[0].Patch, res.Files[1].Patch, res.Files[2].Patch, res.Files[3].Patch}
	if patches[0] != "12345" || patches[1] != "" || patches[2] != "12" || patches[3] != "" {
		t.Errorf("patches = %q", patches)
	}
}

func TestPullFilesWithinBudgetIsNotTruncated(t *testing.T) {
	p := &fakeProvider{files: provider.ChangedFiles{Files: []model.ChangedFile{{Path: "a", Patch: "1234"}}}}

	res, err := limited(p, 4).PullFiles(context.Background(), "tok", ghRepo, 1)

	if err != nil || res.Truncated || res.Files[0].Patch != "1234" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	p.files.Truncated = true
	if res, _ := limited(p, 0).PullFiles(context.Background(), "tok", ghRepo, 1); !res.Truncated {
		t.Error("provider truncation was not passed through")
	}
}

func TestPullFilesAndTreeFailures(t *testing.T) {
	p := &fakeProvider{detailErr: provider.ErrNotFound}
	svc := limited(p, 0)

	if _, err := svc.PullFiles(context.Background(), "t", ghRepo, 1); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("PullFiles error = %v", err)
	}
	if _, err := svc.PullTree(context.Background(), "t", ghRepo, 1); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("PullTree error = %v", err)
	}
	missing := pulls.New(nil, nil, nil, 0)
	if _, err := missing.PullFiles(context.Background(), "t", ghRepo, 1); err == nil {
		t.Error("PullFiles without adapter did not fail")
	}
	if _, err := missing.PullTree(context.Background(), "t", ghRepo, 1); err == nil {
		t.Error("PullTree without adapter did not fail")
	}

}

func TestPullTreeUsesHeadCommit(t *testing.T) {
	p := &fakeProvider{
		detail:   model.PullDetail{Pull: model.Pull{HeadSHA: "headsha"}},
		tree:     provider.Tree{Entries: []model.TreeEntry{{Path: "a", Type: "blob", Size: 1}}, Truncated: true},
		treeMeta: provider.Meta{FetchedAt: now.Add(-time.Minute), Stale: true},
	}

	res, err := limited(p, 0).PullTree(context.Background(), "tok", ghRepo, 3)

	if err != nil || p.gotRef != "headsha" || res.Ref != "headsha" || !res.Truncated || !res.Stale || len(res.Entries) != 1 ||
		!res.FetchedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("result = %+v (ref %q), %v", res, p.gotRef, err)
	}
}

func TestPullTreeWithoutHeadCommitFails(t *testing.T) {
	if _, err := limited(&fakeProvider{}, 0).PullTree(context.Background(), "tok", ghRepo, 3); err == nil {
		t.Fatal("empty head commit accepted")
	}
}

func TestPullFilesReportsOldestFetchTimeAndStaleness(t *testing.T) {
	p := &fakeProvider{detailMet: provider.Meta{FetchedAt: now.Add(-time.Hour), Stale: true}}

	res, err := limited(p, 0).PullFiles(context.Background(), "tok", ghRepo, 1)

	if err != nil || !res.Stale || !res.FetchedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("result = %+v, %v", res, err)
	}
}
