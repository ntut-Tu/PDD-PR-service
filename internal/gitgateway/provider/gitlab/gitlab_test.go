package gitlab_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/provider/gitlab"
	"pdd/services/internal/gitgateway/provider/providertest"
	"pdd/services/pkg/gitprovider"
)

func newProvider(t *testing.T, batch, maxPages int) *gitlab.Provider {
	t.Helper()
	return gitlab.New(gitlab.Config{
		Client: providertest.NewClient(t), BatchSize: batch, MaxPages: maxPages,
		ListTTL: time.Minute, DetailTTL: time.Minute, ExpectedCost: 1,
	})
}

func repo(id int64, base, owner, name string) model.Repo {
	return model.Repo{ID: id, Provider: model.GitLab, BaseURL: base, Owner: owner, Name: name, URL: base + "/" + owner + "/" + name}
}

func mrNode(iid int, title, status string, draft bool) string {
	return fmt.Sprintf(`{"iid":"%d","title":%q,"draft":%t,"detailedMergeStatus":%q,"webUrl":"https://gl/mr/%d",
		"updatedAt":"2026-10-24T10:00:00Z","sourceBranch":"feat","targetBranch":"main","diffHeadSha":%q,
		"author":{"username":"alice","avatarUrl":"/uploads/alice.png"}}`, iid, title, draft, status, iid, headSHA(iid))
}

// headSHA is the head commit the fake provider reports for a merge request iid.
func headSHA(iid int) string { return fmt.Sprintf("%040x", iid) }

func mrPage(hasNext bool, cursor string, nodes ...string) string {
	return fmt.Sprintf(`{"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[%s]}`, hasNext, cursor, strings.Join(nodes, ","))
}

func project(path, mrs string) string {
	return fmt.Sprintf(`{"fullPath":%q,"mergeRequests":%s}`, path, mrs)
}

func TestEndpoint(t *testing.T) {
	if got := gitlab.Endpoint("https://gitlab.example.com:8443/"); got != "https://gitlab.example.com:8443/api/graphql" {
		t.Errorf("Endpoint = %q", got)
	}
}

func TestListPullsMapsAndBatchesProjects(t *testing.T) {
	srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) {
		providertest.JSON(w, `{"data":{"projects":{"nodes":[`+
			project("pdd/backend", mrPage(false, "", mrNode(1, "Fix", "MERGEABLE", false), mrNode(2, "WIP", "CONFLICTS", true)))+","+
			project("Group/Sub/Frontend", mrPage(false, "", mrNode(3, "Wait", "CHECKING", false)))+`]}}}`)
	})
	p := newProvider(t, 10, 3)

	res := p.ListPulls(context.Background(), "tok", []model.Repo{
		repo(1, srv.URL, "pdd", "backend"), repo(2, srv.URL, "group/sub", "frontend"),
	})

	reqs := srv.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/api/graphql" || reqs[0].Authorization != "Bearer tok" {
		t.Fatalf("requests = %+v", reqs)
	}
	if paths, _ := reqs[0].Variables["paths"].([]any); len(paths) != 2 || paths[0] != "pdd/backend" || paths[1] != "group/sub/frontend" {
		t.Errorf("paths = %v", reqs[0].Variables["paths"])
	}
	for _, want := range []string{"projects(fullPaths: $paths, first: 10)", "state: opened", "detailedMergeStatus"} {
		if !strings.Contains(reqs[0].Query, want) {
			t.Errorf("query lacks %q", want)
		}
	}
	if len(res) != 2 || res[0].Err != nil || res[1].Err != nil || len(res[0].Pulls) != 2 || len(res[1].Pulls) != 1 {
		t.Fatalf("results = %+v", res)
	}
	first := res[0].Pulls[0]
	if first.RepoID != 1 || first.Provider != model.GitLab || first.Number != 1 || first.State != "open" || first.Draft ||
		first.Mergeable != model.MergeableYes || first.Author.Login != "alice" ||
		first.Author.AvatarURL != srv.URL+"/uploads/alice.png" || first.SourceBranch != "feat" || first.TargetBranch != "main" ||
		first.URL != "https://gl/mr/1" || first.Repo.Owner != "pdd" || first.Repo.URL != srv.URL+"/pdd/backend" {
		t.Errorf("mapped pull = %+v", first)
	}
	if second := res[0].Pulls[1]; !second.Draft || second.Mergeable != model.MergeableConflict {
		t.Errorf("draft pull = %+v", second)
	}
	if res[1].Pulls[0].Mergeable != model.MergeableUnknown {
		t.Errorf("CHECKING must map to unknown: %+v", res[1].Pulls[0])
	}
}

func TestListPullsSingleRepositoryMapsHeadSHA(t *testing.T) {
	srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) {
		providertest.JSON(w, `{"data":{"projects":{"nodes":[`+project("pdd/backend", mrPage(false, "", mrNode(4, "Only", "MERGEABLE", false)))+`]}}}`)
	})
	p := newProvider(t, 10, 3)

	res := p.ListPulls(context.Background(), "tok", []model.Repo{repo(3, srv.URL, "pdd", "backend")})

	reqs := srv.Requests()
	if paths, _ := reqs[0].Variables["paths"].([]any); len(reqs) != 1 || len(paths) != 1 || paths[0] != "pdd/backend" {
		t.Fatalf("requests = %+v", reqs)
	}
	if !strings.Contains(reqs[0].Query, "diffHeadSha") {
		t.Errorf("list query lacks diffHeadSha: %s", reqs[0].Query)
	}
	if len(res) != 1 || res[0].Err != nil || len(res[0].Pulls) != 1 || res[0].Pulls[0].HeadSHA != headSHA(4) {
		t.Fatalf("result = %+v", res)
	}
}

func TestListPullsGroupsRepositoriesByHost(t *testing.T) {
	answer := func(path string) func(providertest.GraphQLRequest, http.ResponseWriter) {
		return func(_ providertest.GraphQLRequest, w http.ResponseWriter) {
			providertest.JSON(w, `{"data":{"projects":{"nodes":[`+project(path, mrPage(false, "", mrNode(1, "t", "MERGEABLE", false)))+`]}}}`)
		}
	}
	srvA := providertest.NewServer(t, answer("a/one"))
	srvB := providertest.NewServer(t, answer("b/two"))
	p := newProvider(t, 10, 3)

	res := p.ListPulls(context.Background(), "tok", []model.Repo{repo(1, srvA.URL, "a", "one"), repo(2, srvB.URL, "b", "two")})

	if len(srvA.Requests()) != 1 || len(srvB.Requests()) != 1 {
		t.Fatalf("requests A=%d B=%d, want 1 each", len(srvA.Requests()), len(srvB.Requests()))
	}
	if res[0].Err != nil || res[1].Err != nil || res[0].Pulls[0].RepoID != 1 || res[1].Pulls[0].RepoID != 2 {
		t.Fatalf("results = %+v", res)
	}
}

func TestListPullsCapsBatchAtTenProjects(t *testing.T) {
	srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) {
		providertest.JSON(w, `{"data":{"projects":{"nodes":[]}}}`)
	})
	p := newProvider(t, 25, 3)
	var repos []model.Repo
	for i := 0; i < 11; i++ {
		repos = append(repos, repo(int64(i), srv.URL, "o", fmt.Sprintf("r%d", i)))
	}

	res := p.ListPulls(context.Background(), "tok", repos)

	if n := len(srv.Requests()); n != 2 {
		t.Fatalf("%d requests for 11 projects, want 2", n)
	}
	if len(res) != 11 || !errors.Is(res[10].Err, provider.ErrNotFound) {
		t.Fatalf("missing project must be reported: %+v", res[10])
	}
}

func TestListPullsFollowsPagesUpToCap(t *testing.T) {
	srv := providertest.NewServer(t, func(req providertest.GraphQLRequest, w http.ResponseWriter) {
		switch req.Variables["after"] {
		case nil:
			providertest.JSON(w, `{"data":{"projects":{"nodes":[`+project("o/n", mrPage(true, "c1", mrNode(1, "p1", "MERGEABLE", false)))+`]}}}`)
		case "c1":
			providertest.JSON(w, `{"data":{"project":{"mergeRequests":`+mrPage(true, "c2", mrNode(2, "p2", "MERGEABLE", false))+`}}}`)
		default:
			providertest.JSON(w, `{"data":{"project":{"mergeRequests":`+mrPage(false, "", mrNode(3, "p3", "MERGEABLE", false))+`}}}`)
		}
	})
	p := newProvider(t, 10, 2)

	res := p.ListPulls(context.Background(), "tok", []model.Repo{repo(1, srv.URL, "o", "n")})

	if res[0].Err != nil || len(res[0].Pulls) != 2 || len(srv.Requests()) != 2 {
		t.Fatalf("result = %+v after %d requests, want 2 pulls from 2 pages", res[0], len(srv.Requests()))
	}
	if got := srv.Requests()[1].Variables; got["path"] != "o/n" || got["after"] != "c1" {
		t.Errorf("page 2 variables = %v", got)
	}
}

func TestListPullsLaterPageFailures(t *testing.T) {
	tests := map[string]string{
		"project vanished": `{"data":{"project":null}}`,
		"http failure":     "",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			srv := providertest.NewServer(t, func(req providertest.GraphQLRequest, w http.ResponseWriter) {
				if req.Variables["after"] == nil {
					providertest.JSON(w, `{"data":{"projects":{"nodes":[`+project("o/n", mrPage(true, "c1", mrNode(1, "p1", "MERGEABLE", false)))+`]}}}`)
				} else if body == "" {
					w.WriteHeader(http.StatusBadRequest)
				} else {
					providertest.JSON(w, body)
				}
			})
			p := newProvider(t, 10, 3)

			res := p.ListPulls(context.Background(), "tok", []model.Repo{repo(1, srv.URL, "o", "n")})

			if res[0].Err == nil || len(res[0].Pulls) != 0 {
				t.Fatalf("result = %+v", res[0])
			}
		})
	}
}

func TestListPullsProviderFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		check  func(error) bool
	}{
		{"unauthorized", http.StatusUnauthorized, func(err error) bool { return errors.Is(err, gitprovider.ErrUnauthorized) }},
		{"server error", http.StatusInternalServerError, func(err error) bool { return errors.Is(err, gitprovider.ErrMaxRetry) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) { w.WriteHeader(tt.status) })
			p := newProvider(t, 10, 3)

			res := p.ListPulls(context.Background(), "tok", []model.Repo{repo(1, srv.URL, "o", "a"), repo(2, srv.URL, "o", "b")})

			if !tt.check(res[0].Err) || !tt.check(res[1].Err) {
				t.Fatalf("errors = %v, %v", res[0].Err, res[1].Err)
			}
		})
	}
}

func TestPullDetailMapsCommitsAndDropsSystemNotes(t *testing.T) {
	srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) {
		providertest.JSON(w, `{"data":{"project":{"mergeRequest":`+
			`{"iid":"7","title":"Add x","draft":false,"detailedMergeStatus":"MERGEABLE","webUrl":"https://gl/mr/7",`+
			`"updatedAt":"2026-10-24T10:00:00Z","sourceBranch":"feat","targetBranch":"main","diffHeadSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","diffRefs":{"baseSha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},`+
			`"author":{"username":"alice","avatarUrl":"https://secure.gravatar.com/a"},"description":"Details",`+
			`"commits":{"nodes":[{"sha":"abc","title":"first","message":"first\n\nbody","authorName":"Alice A","committedDate":"2026-10-23T09:00:00Z","webUrl":"https://gl/c/abc"}]},`+
			`"notes":{"nodes":[`+
			`{"id":"gid://gitlab/Note/1","body":"assigned to bob","createdAt":"2026-10-24T07:00:00Z","system":true,"author":{"username":"alice","avatarUrl":""}},`+
			`{"id":"gid://gitlab/Note/2","body":"LGTM","createdAt":"2026-10-24T08:00:00Z","system":false,"author":{"username":"bob","avatarUrl":"/b.png"}},`+
			`{"id":"gid://gitlab/Note/3","body":"anon","createdAt":"2026-10-24T09:00:00Z","system":false,"author":null}]}}}}}`)
	})
	p := newProvider(t, 10, 3)

	detail, meta, err := p.PullDetail(context.Background(), "tok", repo(9, srv.URL, "pdd", "backend"), 7)

	if err != nil {
		t.Fatalf("PullDetail: %v", err)
	}
	if v := srv.Requests()[0].Variables; v["path"] != "pdd/backend" || v["iid"] != "7" {
		t.Errorf("variables = %v", v)
	}
	if detail.HeadSHA != strings.Repeat("a", 40) || detail.BaseSHA != strings.Repeat("b", 40) {
		t.Errorf("shas = %q / %q", detail.HeadSHA, detail.BaseSHA)
	}
	if q := srv.Requests()[0].Query; !strings.Contains(q, "diffRefs { baseSha }") || !strings.Contains(q, "diffHeadSha") {
		t.Errorf("detail query lacks the sha fields: %s", q)
	}
	if meta.Stale || detail.RepoID != 9 || detail.Number != 7 || detail.Description != "Details" ||
		detail.Author.AvatarURL != "https://secure.gravatar.com/a" {
		t.Errorf("detail = %+v meta = %+v", detail, meta)
	}
	if len(detail.Commits) != 1 || detail.Commits[0].SHA != "abc" || detail.Commits[0].Author != "Alice A" ||
		detail.Commits[0].Message != "first\n\nbody" || detail.Commits[0].URL != "https://gl/c/abc" {
		t.Errorf("commits = %+v", detail.Commits)
	}
	if len(detail.Comments) != 2 || detail.Comments[0].ID != "gid://gitlab/Note/2" || detail.Comments[0].Author.AvatarURL != srv.URL+"/b.png" ||
		detail.Comments[1].Author.Login != "" {
		t.Errorf("comments = %+v", detail.Comments)
	}
}

func TestPullDetailWithoutDiffRefsHasEmptyBaseSHA(t *testing.T) {
	srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) {
		providertest.JSON(w, `{"data":{"project":{"mergeRequest":`+mrNode(8, "No refs", "UNCHECKED", false)+`}}}`)
	})
	p := newProvider(t, 10, 3)

	detail, _, err := p.PullDetail(context.Background(), "tok", repo(9, srv.URL, "pdd", "backend"), 8)

	if err != nil || detail.BaseSHA != "" || detail.HeadSHA != headSHA(8) {
		t.Fatalf("detail = %+v, err = %v", detail, err)
	}
}

func TestPullDetailNotFoundAndErrors(t *testing.T) {
	notFound := map[string]string{
		"null merge request": `{"data":{"project":{"mergeRequest":null}}}`,
		"null project":       `{"data":{"project":null}}`,
	}
	for name, body := range notFound {
		t.Run(name, func(t *testing.T) {
			srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) { providertest.JSON(w, body) })
			p := newProvider(t, 10, 3)

			if _, _, err := p.PullDetail(context.Background(), "tok", repo(1, srv.URL, "o", "n"), 1); !errors.Is(err, provider.ErrNotFound) {
				t.Fatalf("error = %v, want ErrNotFound", err)
			}
		})
	}

	srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) })
	p := newProvider(t, 10, 3)
	if _, _, err := p.PullDetail(context.Background(), "tok", repo(1, srv.URL, "o", "n"), 1); !errors.Is(err, gitprovider.ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}
