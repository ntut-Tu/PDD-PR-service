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
	"pdd/services/pkg/gitprovider"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

type fakeProvider struct {
	list      func(repos []model.Repo) []provider.RepoPulls
	detail    model.PullDetail
	detailMet provider.Meta
	detailErr error
	gotRepos  []model.Repo

	files     provider.ChangedFiles
	tree      provider.Tree
	treeMeta  provider.Meta
	content   []byte
	contentEr error
	gotRef    string
}

func (f *fakeProvider) PullFiles(context.Context, string, model.Repo, int) (provider.ChangedFiles, provider.Meta, error) {
	return f.files, provider.Meta{}, nil
}

func (f *fakeProvider) Tree(_ context.Context, _ string, _ model.Repo, ref string) (provider.Tree, provider.Meta, error) {
	f.gotRef = ref
	return f.tree, f.treeMeta, nil
}

func (f *fakeProvider) File(context.Context, string, model.Repo, string, string) ([]byte, provider.Meta, error) {
	return f.content, provider.Meta{}, f.contentEr
}

func (f *fakeProvider) ListPulls(_ context.Context, _ string, repos []model.Repo) []provider.RepoPulls {
	f.gotRepos = repos
	return f.list(repos)
}

func (f *fakeProvider) PullDetail(context.Context, string, model.Repo, int) (model.PullDetail, provider.Meta, error) {
	return f.detail, f.detailMet, f.detailErr
}

func pull(repoID int64, number int, updated time.Duration) model.Pull {
	return model.Pull{RepoID: repoID, Number: number, UpdatedAt: now.Add(updated)}
}

func newService(gh, gl provider.Provider) *service.Service {
	return service.New(map[model.Provider]provider.Provider{model.GitHub: gh, model.GitLab: gl}, func() time.Time { return now }, service.Limits{})
}

func TestListPullsMergesProvidersNewestFirst(t *testing.T) {
	gh := &fakeProvider{list: func(repos []model.Repo) []provider.RepoPulls {
		return []provider.RepoPulls{{Repo: repos[0], Pulls: []model.Pull{pull(1, 1, -2*time.Hour)}, Meta: provider.Meta{FetchedAt: now.Add(-time.Minute)}}}
	}}
	gl := &fakeProvider{list: func(repos []model.Repo) []provider.RepoPulls {
		return []provider.RepoPulls{{Repo: repos[0], Pulls: []model.Pull{pull(2, 5, -time.Hour), pull(2, 6, -3*time.Hour)}, Meta: provider.Meta{FetchedAt: now}}}
	}}
	svc := newService(gh, gl)

	res := svc.ListPulls(context.Background(), "tok", []model.Repo{
		{ID: 1, Provider: model.GitHub, Owner: "o", Name: "a"}, {ID: 2, Provider: model.GitLab, Owner: "g", Name: "b"},
	})

	var order []int
	for _, p := range res.Pulls {
		order = append(order, p.Number)
	}
	if len(order) != 3 || order[0] != 5 || order[1] != 1 || order[2] != 6 {
		t.Errorf("order = %v, want [5 1 6]", order)
	}
	if res.Stale || len(res.Errors) != 0 || !res.FetchedAt.Equal(now.Add(-time.Minute)) {
		t.Errorf("result = %+v", res)
	}
	if len(gh.gotRepos) != 1 || gh.gotRepos[0].ID != 1 || len(gl.gotRepos) != 1 || gl.gotRepos[0].ID != 2 {
		t.Errorf("repos were not routed by provider: gh=%v gl=%v", gh.gotRepos, gl.gotRepos)
	}
}

func TestListPullsSingleRepositoryDoesNotAskOtherProviders(t *testing.T) {
	gh := &fakeProvider{list: func(repos []model.Repo) []provider.RepoPulls {
		return []provider.RepoPulls{{Repo: repos[0], Pulls: []model.Pull{pull(repos[0].ID, 1, 0)}}}
	}}
	gl := &fakeProvider{}
	svc := newService(gh, gl)

	res := svc.ListPulls(context.Background(), "tok", []model.Repo{{ID: 4, Provider: model.GitHub}})

	if len(res.Pulls) != 1 || res.Pulls[0].RepoID != 4 || len(gh.gotRepos) != 1 || gl.gotRepos != nil {
		t.Fatalf("result = %+v, github got %+v, gitlab got %+v", res, gh.gotRepos, gl.gotRepos)
	}
}

func TestListPullsEmptyProjectHasNonNilCollections(t *testing.T) {
	res := newService(nil, nil).ListPulls(context.Background(), "tok", nil)

	if res.Pulls == nil || res.Errors == nil || res.Stale || !res.FetchedAt.Equal(now) {
		t.Fatalf("result = %+v", res)
	}
}

func TestListPullsReportsErrorsPerRepo(t *testing.T) {
	quota := &gitprovider.QuotaReserveError{Remaining: 3, Reserve: 10, ResetAt: now.Add(time.Hour)}
	gh := &fakeProvider{list: func(repos []model.Repo) []provider.RepoPulls {
		return []provider.RepoPulls{
			{Repo: repos[0], Err: &gitprovider.StatusError{StatusCode: 401}},
			{Repo: repos[1], Err: &gitprovider.MaxRetryError{Attempts: 3, Err: errors.New("boom")}},
			{Repo: repos[2], Err: quota},
			{Repo: repos[3], Pulls: []model.Pull{pull(4, 1, 0)}, Meta: provider.Meta{FetchedAt: now}},
		}
	}}
	svc := newService(gh, nil)
	repos := []model.Repo{
		{ID: 1, Provider: model.GitHub, Owner: "o", Name: "a"}, {ID: 2, Provider: model.GitHub, Owner: "o", Name: "b"},
		{ID: 3, Provider: model.GitHub, Owner: "o", Name: "c"}, {ID: 4, Provider: model.GitHub, Owner: "o", Name: "d"},
	}

	res := svc.ListPulls(context.Background(), "tok", repos)

	if len(res.Pulls) != 1 || len(res.Errors) != 3 || res.Stale {
		t.Fatalf("result = %+v", res)
	}
	wantCodes := []string{service.CodeProviderUnauthorized, service.CodeUpstreamFailed, service.CodeQuotaReserve}
	for i, want := range wantCodes {
		got := res.Errors[i]
		if got.Code != want || got.RepoID != int64(i+1) || got.Repo == "" || got.Message == "" {
			t.Errorf("error %d = %+v, want code %s", i, got, want)
		}
		if strings.Contains(got.Message, "gitprovider") {
			t.Errorf("error %d leaks internal detail: %q", i, got.Message)
		}
	}
	if res.Errors[0].Repo != "o/a" {
		t.Errorf("repo label = %q", res.Errors[0].Repo)
	}
}

func TestListPullsMarksStaleDataWithReason(t *testing.T) {
	fetched := now.Add(-30 * time.Minute)
	gh := &fakeProvider{list: func(repos []model.Repo) []provider.RepoPulls {
		return []provider.RepoPulls{
			{Repo: repos[0], Pulls: []model.Pull{pull(1, 1, 0)}, Meta: provider.Meta{FetchedAt: fetched, Stale: true, StaleReason: &gitprovider.QuotaReserveError{}}},
			{Repo: repos[1], Pulls: []model.Pull{pull(2, 1, 0)}, Meta: provider.Meta{FetchedAt: now}},
			{Repo: repos[2], Pulls: []model.Pull{pull(3, 1, 0)}, Meta: provider.Meta{FetchedAt: fetched, Stale: true}},
		}
	}}
	repos := []model.Repo{
		{ID: 1, Provider: model.GitHub, Owner: "o", Name: "a"}, {ID: 2, Provider: model.GitHub, Owner: "o", Name: "b"},
		{ID: 3, Provider: model.GitHub, Owner: "o", Name: "c"},
	}

	res := newService(gh, nil).ListPulls(context.Background(), "tok", repos)

	if !res.Stale || !res.FetchedAt.Equal(fetched) || len(res.Pulls) != 3 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Errors) != 2 || res.Errors[0].Code != service.CodeQuotaReserve || res.Errors[1].Code != service.CodeUpstreamFailed {
		t.Fatalf("errors = %+v", res.Errors)
	}
}

func TestPullDetail(t *testing.T) {
	fetched := now.Add(-time.Minute)
	gl := &fakeProvider{
		detail:    model.PullDetail{Description: "d"},
		detailMet: provider.Meta{FetchedAt: fetched, Stale: true},
	}
	svc := newService(nil, gl)

	res, err := svc.PullDetail(context.Background(), "tok", model.Repo{Provider: model.GitLab}, 3)

	if err != nil || res.Pull.Description != "d" || !res.Stale || !res.FetchedAt.Equal(fetched) {
		t.Fatalf("result = %+v, %v", res, err)
	}

	gl.detailErr = provider.ErrNotFound
	if _, err := svc.PullDetail(context.Background(), "tok", model.Repo{Provider: model.GitLab}, 3); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
	if _, err := svc.PullDetail(context.Background(), "tok", model.Repo{Provider: model.GitHub}, 3); err == nil {
		t.Fatal("missing adapter did not fail")
	}
}

func TestCode(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{&gitprovider.QuotaReserveError{}, service.CodeQuotaReserve},
		{&gitprovider.StatusError{StatusCode: 401}, service.CodeProviderUnauthorized},
		{&gitprovider.MaxRetryError{Err: errors.New("x")}, service.CodeUpstreamFailed},
		{errors.New("other"), service.CodeUpstreamFailed},
	}
	for _, tt := range tests {
		if got := service.Code(tt.err); got != tt.want {
			t.Errorf("Code(%v) = %s, want %s", tt.err, got, tt.want)
		}
	}
}
