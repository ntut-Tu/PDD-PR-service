package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/service"
)

func limited(p provider.Provider, limits service.Limits) *service.Service {
	return service.New(map[model.Provider]provider.Provider{model.GitHub: p}, func() time.Time { return now }, limits)
}

var ghRepo = model.Repo{Provider: model.GitHub}

func TestPullFilesAddsCommitsAndDropsPatchesOverBudget(t *testing.T) {
	p := &fakeProvider{
		detail: model.PullDetail{Pull: model.Pull{HeadSHA: "head"}, BaseSHA: "base"},
		files: provider.ChangedFiles{Files: []model.ChangedFile{
			{Path: "a", Patch: "12345"}, {Path: "b", Patch: "123456"}, {Path: "c", Patch: "12"}, {Path: "d"},
		}},
	}

	res, err := limited(p, service.Limits{PatchBytes: 8}).PullFiles(context.Background(), "tok", ghRepo, 1)

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

	res, err := limited(p, service.Limits{PatchBytes: 4}).PullFiles(context.Background(), "tok", ghRepo, 1)

	if err != nil || res.Truncated || res.Files[0].Patch != "1234" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	p.files.Truncated = true
	if res, _ := limited(p, service.Limits{}).PullFiles(context.Background(), "tok", ghRepo, 1); !res.Truncated {
		t.Error("provider truncation was not passed through")
	}
}

func TestPullFilesAndTreeFailures(t *testing.T) {
	p := &fakeProvider{detailErr: provider.ErrNotFound}
	svc := limited(p, service.Limits{})

	if _, err := svc.PullFiles(context.Background(), "t", ghRepo, 1); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("PullFiles error = %v", err)
	}
	if _, err := svc.PullTree(context.Background(), "t", ghRepo, 1); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("PullTree error = %v", err)
	}
	missing := service.New(nil, nil, service.Limits{})
	if _, err := missing.PullFiles(context.Background(), "t", ghRepo, 1); err == nil {
		t.Error("PullFiles without adapter did not fail")
	}
	if _, err := missing.PullTree(context.Background(), "t", ghRepo, 1); err == nil {
		t.Error("PullTree without adapter did not fail")
	}
	if _, err := missing.Tree(context.Background(), "t", ghRepo, "x"); err == nil {
		t.Error("Tree without adapter did not fail")
	}
	if _, err := missing.File(context.Background(), "t", ghRepo, "x", "p"); err == nil {
		t.Error("File without adapter did not fail")
	}
}

func TestPullTreeUsesHeadCommit(t *testing.T) {
	p := &fakeProvider{
		detail:   model.PullDetail{Pull: model.Pull{HeadSHA: "headsha"}},
		tree:     provider.Tree{Entries: []model.TreeEntry{{Path: "a", Type: "blob", Size: 1}}, Truncated: true},
		treeMeta: provider.Meta{FetchedAt: now.Add(-time.Minute), Stale: true},
	}

	res, err := limited(p, service.Limits{}).PullTree(context.Background(), "tok", ghRepo, 3)

	if err != nil || p.gotRef != "headsha" || res.Ref != "headsha" || !res.Truncated || !res.Stale || len(res.Entries) != 1 ||
		!res.FetchedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("result = %+v (ref %q), %v", res, p.gotRef, err)
	}
}

func TestPullTreeWithoutHeadCommitFails(t *testing.T) {
	if _, err := limited(&fakeProvider{}, service.Limits{}).PullTree(context.Background(), "tok", ghRepo, 3); err == nil {
		t.Fatal("empty head commit accepted")
	}
}

func TestFileText(t *testing.T) {
	p := &fakeProvider{content: []byte("héllo")}

	res, err := limited(p, service.Limits{FileBytes: 100}).File(context.Background(), "tok", ghRepo, "abc", "a.txt")

	if err != nil || res.Content != "héllo" || res.Size != 6 || res.Binary || res.Truncated || res.Ref != "abc" || res.Path != "a.txt" {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestFileDetectsBinary(t *testing.T) {
	for name, content := range map[string][]byte{
		"nul byte":      []byte("PK\x00\x03text"),
		"invalid utf-8": {0xff, 0xfe, 'a'},
	} {
		res, err := limited(&fakeProvider{content: content}, service.Limits{FileBytes: 100}).File(context.Background(), "tok", ghRepo, "abc", "x")

		if err != nil || !res.Binary || res.Content != "" || res.Size != int64(len(content)) || res.Truncated {
			t.Errorf("%s: result = %+v, %v", name, res, err)
		}
	}
}

func TestFileTruncatesOnCharacterBoundary(t *testing.T) {
	// "é" is two bytes; a cut at 5 would split the second one.
	p := &fakeProvider{content: []byte("aaéé")}

	res, err := limited(p, service.Limits{FileBytes: 5}).File(context.Background(), "tok", ghRepo, "abc", "x")

	if err != nil || !res.Truncated || res.Content != "aaé" || res.Size != 6 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	long := &fakeProvider{content: []byte(strings.Repeat("x", 10))}
	if res, _ := limited(long, service.Limits{FileBytes: 10}).File(context.Background(), "tok", ghRepo, "abc", "x"); res.Truncated {
		t.Error("file at the limit was truncated")
	}
}

func TestFileProviderError(t *testing.T) {
	p := &fakeProvider{contentEr: provider.ErrNotFound}

	if _, err := limited(p, service.Limits{}).File(context.Background(), "tok", ghRepo, "abc", "x"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestPullFilesReportsOldestFetchTimeAndStaleness(t *testing.T) {
	p := &fakeProvider{detailMet: provider.Meta{FetchedAt: now.Add(-time.Hour), Stale: true}}

	res, err := limited(p, service.Limits{}).PullFiles(context.Background(), "tok", ghRepo, 1)

	if err != nil || !res.Stale || !res.FetchedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("result = %+v, %v", res, err)
	}
}
