package provider_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/provider/providertest"
	"pdd/services/pkg/gitprovider"
	"pdd/services/pkg/gitprovider/client"
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func TestMetaMerge(t *testing.T) {
	fresh := provider.Meta{FetchedAt: t0}
	older := provider.Meta{FetchedAt: t0.Add(-time.Hour), Stale: true, StaleReason: gitprovider.ErrQuotaReserve}

	if got := (provider.Meta{}).Merge(fresh); got != fresh {
		t.Errorf("zero.Merge(x) = %+v", got)
	}
	if got := fresh.Merge(provider.Meta{}); got != fresh {
		t.Errorf("x.Merge(zero) = %+v", got)
	}
	got := fresh.Merge(older)
	if !got.FetchedAt.Equal(older.FetchedAt) || !got.Stale || !errors.Is(got.StaleReason, gitprovider.ErrQuotaReserve) {
		t.Errorf("fresh.Merge(older stale) = %+v", got)
	}
	if got := older.Merge(fresh); !got.Stale || !got.FetchedAt.Equal(older.FetchedAt) {
		t.Errorf("older.Merge(fresh) = %+v", got)
	}
}

func TestGraphQLErrors(t *testing.T) {
	notFound := provider.GraphQLErrors{{Message: "a", Type: "NOT_FOUND"}, {Message: "b"}}
	other := provider.GraphQLErrors{{Message: "boom"}}

	if !errors.Is(notFound, provider.ErrNotFound) || errors.Is(other, provider.ErrNotFound) {
		t.Error("ErrNotFound matching is wrong")
	}
	if errors.Is(notFound, gitprovider.ErrMaxRetry) {
		t.Error("matched an unrelated error")
	}
	if got := notFound.Error(); got != "graphql: a; b" {
		t.Errorf("Error() = %q", got)
	}
}

func TestAbsoluteURL(t *testing.T) {
	if got := provider.AbsoluteURL("https://gl.example.com/", "/uploads/a.png"); got != "https://gl.example.com/uploads/a.png" {
		t.Errorf("relative = %q", got)
	}
	if got := provider.AbsoluteURL("https://gl.example.com", "https://secure.gravatar.com/a"); got != "https://secure.gravatar.com/a" {
		t.Errorf("absolute = %q", got)
	}
}

func listRequest(srv *providertest.RESTServer) func(int) client.Request {
	return func(page int) client.Request {
		return client.Request{URL: srv.URL + "/list?page=" + string(rune('0'+page)), Token: "tok"}
	}
}

func TestPagesStopsAtShortPageOrCap(t *testing.T) {
	srv := providertest.NewRESTServer(t, func(req providertest.RESTRequest, w http.ResponseWriter) {
		if strings.HasSuffix(req.Query, "page=3") {
			providertest.JSON(w, `[3]`)
			return
		}
		providertest.JSON(w, `[1,2]`)
	})
	c := providertest.NewClient(t)

	items, truncated, meta, err := provider.Pages[int](context.Background(), c, 2, 5, listRequest(srv))
	if err != nil || truncated || len(items) != 5 || meta.FetchedAt.IsZero() {
		t.Fatalf("all pages = %v truncated %v meta %+v err %v", items, truncated, meta, err)
	}

	items, truncated, _, err = provider.Pages[int](context.Background(), providertest.NewClient(t), 2, 2, listRequest(srv))
	if err != nil || !truncated || len(items) != 4 {
		t.Fatalf("capped = %v truncated %v err %v", items, truncated, err)
	}
}

func TestPagesAndGetReportFailures(t *testing.T) {
	status := http.StatusOK
	srv := providertest.NewRESTServer(t, func(_ providertest.RESTRequest, w http.ResponseWriter) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"not":"a list"`))
	})

	if _, _, _, err := provider.Pages[int](context.Background(), providertest.NewClient(t), 2, 2, listRequest(srv)); err == nil {
		t.Error("malformed page accepted")
	}
	status = http.StatusNotFound
	if _, _, err := provider.Get(context.Background(), providertest.NewClient(t), client.Request{URL: srv.URL + "/x"}); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("404 error = %v", err)
	}
	status = http.StatusTeapot
	if _, _, err := provider.Get(context.Background(), providertest.NewClient(t), client.Request{URL: srv.URL + "/x"}); err == nil || errors.Is(err, provider.ErrNotFound) {
		t.Errorf("418 error = %v", err)
	}
	if _, _, err := provider.Get(context.Background(), providertest.NewClient(t), client.Request{URL: "::bad"}); err == nil {
		t.Error("invalid URL accepted")
	}
	var v struct{}
	if err := provider.Decode([]byte("{"), &v); err == nil {
		t.Error("Decode accepted malformed JSON")
	}
}
