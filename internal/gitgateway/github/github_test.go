package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/github"
	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/provider/providertest"
	"pdd/services/internal/gitgateway/upstream"
)

func newProvider(t *testing.T, srv *providertest.Server, batch, maxPages int) *github.Provider {
	t.Helper()
	return github.New(github.Config{
		APIURL: srv.URL, Client: providertest.NewClient(t), BatchSize: batch, MaxPages: maxPages,
		ListTTL: time.Minute, DetailTTL: time.Minute, ExpectedCost: 1,
	})
}

func repo(id int64, owner, name string) model.Repo {
	return model.Repo{ID: id, Provider: model.GitHub, BaseURL: "https://github.com", Owner: owner, Name: name, URL: "https://github.com/" + owner + "/" + name}
}

func pullNode(number int, title, mergeable string, draft bool) string {
	return fmt.Sprintf(`{"number":%d,"title":%q,"state":"OPEN","isDraft":%t,"mergeable":%q,"url":"https://github.com/o/n/pull/%d",
		"updatedAt":"2026-10-24T10:00:00Z","headRefName":"feature","baseRefName":"main","headRefOid":%q,
		"author":{"login":"alice","avatarUrl":"https://avatars/alice"}}`, number, title, draft, mergeable, number, headSHA(number))
}

// headSHA is the head commit the fake provider reports for a pull request number.
func headSHA(number int) string { return fmt.Sprintf("%040x", number) }

func page(hasNext bool, cursor string, nodes ...string) string {
	return fmt.Sprintf(`{"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[%s]}`, hasNext, cursor, strings.Join(nodes, ","))
}

func TestGraphQLEndpoint(t *testing.T) {
	tests := map[string]string{
		"https://api.github.com":         "https://api.github.com/graphql",
		"https://api.github.com/":        "https://api.github.com/graphql",
		"https://ghe.example.com/api/v3": "https://ghe.example.com/api/graphql",
		"http://127.0.0.1:9000":          "http://127.0.0.1:9000/graphql",
	}
	for in, want := range tests {
		if got := github.GraphQLEndpoint(in); got != want {
			t.Errorf("GraphQLEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListPullsMapsBatchedResponse(t *testing.T) {
	srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) {
		providertest.JSON(w, fmt.Sprintf(`{"data":{
			"r0":{"pullRequests":%s},
			"r1":{"pullRequests":%s},
			"rateLimit":{"cost":1,"remaining":4999,"resetAt":"2026-10-24T13:00:00Z","limit":5000}}}`,
			page(false, "", pullNode(1, "Fix", "MERGEABLE", false), pullNode(2, "WIP", "CONFLICTING", true)),
			page(false, "", pullNode(3, "Other", "UNKNOWN", false))))
	})
	p := newProvider(t, srv, 10, 3)

	res := p.ListPulls(context.Background(), "tok", []model.Repo{repo(1, "o", "n"), repo(2, "o", "m")})

	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d upstream requests for 2 repos, want 1", len(reqs))
	}
	if reqs[0].Authorization != "Bearer tok" || reqs[0].Path != "/graphql" {
		t.Errorf("request = %+v", reqs[0])
	}
	for _, want := range []string{"r0: repository(owner: $owner0, name: $name0)", "r1: repository", "states: OPEN", "rateLimit { cost remaining resetAt limit }"} {
		if !strings.Contains(reqs[0].Query, want) {
			t.Errorf("query lacks %q:\n%s", want, reqs[0].Query)
		}
	}
	if reqs[0].Variables["owner1"] != "o" || reqs[0].Variables["name1"] != "m" {
		t.Errorf("variables = %v", reqs[0].Variables)
	}
	if len(res) != 2 || res[0].Err != nil || res[1].Err != nil || len(res[0].Pulls) != 2 || len(res[1].Pulls) != 1 {
		t.Fatalf("results = %+v", res)
	}
	first := res[0].Pulls[0]
	if first.RepoID != 1 || first.Provider != model.GitHub || first.Number != 1 || first.Title != "Fix" || first.State != "open" ||
		first.Draft || first.Mergeable != model.MergeableYes || first.Author.Login != "alice" ||
		first.Author.AvatarURL != "https://avatars/alice" || first.SourceBranch != "feature" || first.TargetBranch != "main" ||
		first.URL != "https://github.com/o/n/pull/1" || first.Repo.Owner != "o" || first.Repo.Name != "n" ||
		first.Repo.URL != "https://github.com/o/n" || !first.UpdatedAt.Equal(time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("mapped pull = %+v", first)
	}
	second := res[0].Pulls[1]
	if !second.Draft || second.Mergeable != model.MergeableConflict {
		t.Errorf("draft/conflicting pull = %+v", second)
	}
	if res[1].Pulls[0].Mergeable != model.MergeableUnknown {
		t.Errorf("unknown pull = %+v", res[1].Pulls[0])
	}
}

func TestListPullsSingleRepositoryMapsHeadSHA(t *testing.T) {
	srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) {
		providertest.JSON(w, fmt.Sprintf(`{"data":{"r0":{"pullRequests":%s},
			"rateLimit":{"cost":1,"remaining":4999,"resetAt":"2026-10-24T13:00:00Z","limit":5000}}}`,
			page(false, "", pullNode(7, "Only", "MERGEABLE", false))))
	})
	p := newProvider(t, srv, 10, 3)

	res := p.ListPulls(context.Background(), "tok", []model.Repo{repo(9, "o", "n")})

	reqs := srv.Requests()
	if len(reqs) != 1 || reqs[0].Variables["owner0"] != "o" || reqs[0].Variables["name0"] != "n" || reqs[0].Variables["owner1"] != nil {
		t.Fatalf("requests = %+v", reqs)
	}
	if !strings.Contains(reqs[0].Query, "headRefOid") {
		t.Errorf("list query lacks headRefOid: %s", reqs[0].Query)
	}
	if len(res) != 1 || res[0].Err != nil || len(res[0].Pulls) != 1 || res[0].Pulls[0].HeadSHA != headSHA(7) || res[0].Pulls[0].RepoID != 9 {
		t.Fatalf("result = %+v", res)
	}
}

func TestListPullsSplitsBatchesAndServesSecondCallFromCache(t *testing.T) {
	srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) {
		providertest.JSON(w, `{"data":{"r0":{"pullRequests":`+page(false, "")+`},"r1":null,"rateLimit":{"cost":1,"remaining":4000,"resetAt":"2099-01-01T00:00:00Z","limit":5000}}}`)
	})
	p := newProvider(t, srv, 2, 3)
	repos := []model.Repo{repo(1, "o", "a"), repo(2, "o", "b"), repo(3, "o", "c")}

	p.ListPulls(context.Background(), "tok", repos)
	p.ListPulls(context.Background(), "tok", repos)

	if n := len(srv.Requests()); n != 2 {
		t.Fatalf("%d upstream requests, want 2 (two batches, second call cached)", n)
	}
}

func TestListPullsMissingRepositoryFailsOnlyThatRepo(t *testing.T) {
	srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) {
		providertest.JSON(w, `{"data":{"r0":{"pullRequests":`+page(false, "", pullNode(1, "A", "MERGEABLE", false))+`},"r1":null},
			"errors":[{"type":"NOT_FOUND","path":["r1"],"message":"Could not resolve to a Repository with the name 'o/gone'."}]}`)
	})
	p := newProvider(t, srv, 10, 3)

	res := p.ListPulls(context.Background(), "tok", []model.Repo{repo(1, "o", "a"), repo(2, "o", "gone")})

	if res[0].Err != nil || len(res[0].Pulls) != 1 {
		t.Errorf("healthy repo = %+v", res[0])
	}
	if !errors.Is(res[1].Err, provider.ErrNotFound) {
		t.Errorf("missing repo error = %v, want ErrNotFound", res[1].Err)
	}
}

func TestListPullsFollowsPagesUpToCap(t *testing.T) {
	srv := providertest.NewServer(t, func(req providertest.GraphQLRequest, w http.ResponseWriter) {
		after, _ := req.Variables["after"].(string)
		switch after {
		case "":
			providertest.JSON(w, `{"data":{"r0":{"pullRequests":`+page(true, "c1", pullNode(1, "p1", "MERGEABLE", false))+`}}}`)
		case "c1":
			providertest.JSON(w, `{"data":{"repository":{"pullRequests":`+page(true, "c2", pullNode(2, "p2", "MERGEABLE", false))+`}}}`)
		default:
			providertest.JSON(w, `{"data":{"repository":{"pullRequests":`+page(true, "c3", pullNode(3, "p3", "MERGEABLE", false))+`}}}`)
		}
	})
	p := newProvider(t, srv, 10, 2)

	res := p.ListPulls(context.Background(), "tok", []model.Repo{repo(1, "o", "n")})

	if res[0].Err != nil || len(res[0].Pulls) != 2 {
		t.Fatalf("result = %+v, want 2 pulls from 2 pages", res[0])
	}
	if n := len(srv.Requests()); n != 2 {
		t.Fatalf("%d requests, want 2 (cap)", n)
	}
	if got := srv.Requests()[1].Variables; got["after"] != "c1" || got["owner"] != "o" || got["name"] != "n" {
		t.Errorf("page 2 variables = %v", got)
	}
}

func TestListPullsLaterPageFailureReportsRepoError(t *testing.T) {
	srv := providertest.NewServer(t, func(req providertest.GraphQLRequest, w http.ResponseWriter) {
		if req.Variables["after"] == nil {
			providertest.JSON(w, `{"data":{"r0":{"pullRequests":`+page(true, "c1", pullNode(1, "p1", "MERGEABLE", false))+`}}}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	})
	p := newProvider(t, srv, 10, 3)

	res := p.ListPulls(context.Background(), "tok", []model.Repo{repo(1, "o", "n")})

	var status *upstream.StatusError
	if !errors.As(res[0].Err, &status) || status.StatusCode != http.StatusBadRequest || len(res[0].Pulls) != 0 {
		t.Fatalf("result = %+v", res[0])
	}
}

func TestListPullsProviderFailures(t *testing.T) {
	tests := []struct {
		name    string
		respond func(w http.ResponseWriter)
		check   func(error) bool
	}{
		{"unauthorized", func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) },
			func(err error) bool { return errors.Is(err, upstream.ErrUnauthorized) }},
		{"server error after retries", func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
			func(err error) bool { return errors.Is(err, upstream.ErrMaxRetry) }},
		{"bad request", func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadRequest) },
			func(err error) bool {
				var s *upstream.StatusError
				return errors.As(err, &s) && s.StatusCode == 400
			}},
		{"graphql errors without data", func(w http.ResponseWriter) {
			providertest.JSON(w, `{"errors":[{"message":"boom"}]}`)
		}, func(err error) bool { var g provider.GraphQLErrors; return errors.As(err, &g) }},
		{"malformed body", func(w http.ResponseWriter) { providertest.JSON(w, `{`) },
			func(err error) bool { return err != nil }},
		{"no data", func(w http.ResponseWriter) { providertest.JSON(w, `{}`) },
			func(err error) bool { return err != nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) { tt.respond(w) })
			p := newProvider(t, srv, 10, 3)

			res := p.ListPulls(context.Background(), "tok", []model.Repo{repo(1, "o", "a"), repo(2, "o", "b")})

			for _, r := range res {
				if !tt.check(r.Err) {
					t.Errorf("error of %s = %v", r.Repo.Name, r.Err)
				}
			}
		})
	}
}

func TestPullDetailMapsCommitsAndComments(t *testing.T) {
	srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) {
		providertest.JSON(w, `{"data":{"repository":{"pullRequest":`+
			`{"number":42,"title":"Add x","state":"OPEN","isDraft":false,"mergeable":"MERGEABLE","url":"https://github.com/o/n/pull/42",`+
			`"updatedAt":"2026-10-24T10:00:00Z","headRefName":"feat","baseRefName":"main","headRefOid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","baseRefOid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","author":{"login":"alice","avatarUrl":"a"},`+
			`"body":"## Description\nHello",`+
			`"commits":{"totalCount":2,"nodes":[`+
			`{"commit":{"oid":"abc","messageHeadline":"first","message":"first\n\nbody","committedDate":"2026-10-23T09:00:00Z","url":"https://github.com/o/n/commit/abc","author":{"name":"Alice A"}}},`+
			`{"commit":{"oid":"def","message":"second","committedDate":"2026-10-23T10:00:00Z","url":"u","author":null}}]},`+
			`"comments":{"totalCount":2,"nodes":[`+
			`{"id":"IC_1","body":"LGTM","createdAt":"2026-10-24T08:00:00Z","author":{"login":"bob","avatarUrl":"b"}},`+
			`{"id":"IC_2","body":"deleted user","createdAt":"2026-10-24T09:00:00Z","author":null}]}}},`+
			`"rateLimit":{"cost":1,"remaining":4990,"resetAt":"2099-01-01T00:00:00Z","limit":5000}}}`)
	})
	p := newProvider(t, srv, 10, 3)

	detail, meta, err := p.PullDetail(context.Background(), "tok", repo(5, "o", "n"), 42)

	if err != nil {
		t.Fatalf("PullDetail: %v", err)
	}
	if meta.Stale || meta.FetchedAt.IsZero() {
		t.Errorf("meta = %+v", meta)
	}
	if v := srv.Requests()[0].Variables; v["owner"] != "o" || v["name"] != "n" || v["number"] != float64(42) {
		t.Errorf("variables = %v", v)
	}
	if detail.HeadSHA != strings.Repeat("a", 40) || detail.BaseSHA != strings.Repeat("b", 40) {
		t.Errorf("shas = %q / %q", detail.HeadSHA, detail.BaseSHA)
	}
	if q := srv.Requests()[0].Query; !strings.Contains(q, "headRefOid") || !strings.Contains(q, "baseRefOid") {
		t.Errorf("detail query lacks the sha fields: %s", q)
	}
	if detail.RepoID != 5 || detail.Number != 42 || detail.Title != "Add x" || detail.Description != "## Description\nHello" {
		t.Errorf("detail = %+v", detail)
	}
	if len(detail.Commits) != 2 || detail.Commits[0].SHA != "abc" || detail.Commits[0].Message != "first\n\nbody" ||
		detail.Commits[0].Author != "Alice A" || detail.Commits[0].URL != "https://github.com/o/n/commit/abc" ||
		detail.Commits[0].CommittedAt.IsZero() || detail.Commits[1].Author != "" {
		t.Errorf("commits = %+v", detail.Commits)
	}
	if len(detail.Comments) != 2 || detail.Comments[0].ID != "IC_1" || detail.Comments[0].Author.Login != "bob" ||
		detail.Comments[0].Body != "LGTM" || detail.Comments[1].Author.Login != "ghost" {
		t.Errorf("comments = %+v", detail.Comments)
	}
}

func TestPullDetailNotFound(t *testing.T) {
	tests := map[string]string{
		"null pull request": `{"data":{"repository":{"pullRequest":null}}}`,
		"null repository":   `{"data":{"repository":null}}`,
		"graphql not found": `{"data":{"repository":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a PullRequest"}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) { providertest.JSON(w, body) })
			p := newProvider(t, srv, 10, 3)

			_, _, err := p.PullDetail(context.Background(), "tok", repo(5, "o", "n"), 1)

			if !errors.Is(err, provider.ErrNotFound) {
				t.Fatalf("error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestPullDetailPropagatesProviderErrors(t *testing.T) {
	srv := providertest.NewServer(t, func(_ providertest.GraphQLRequest, w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) })
	p := newProvider(t, srv, 10, 3)

	_, _, err := p.PullDetail(context.Background(), "bad", repo(5, "o", "n"), 1)

	if !errors.Is(err, upstream.ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}
