package source_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/sandboxcontroller/model"
	"pdd/services/internal/sandboxcontroller/source"
)

var spec = model.Spec{Account: "alice smith", ProjectID: 7, RepoID: 3, HeadSHA: strings.Repeat("c", 40)}

func TestArchiveCallsInternalEndpoint(t *testing.T) {
	var gotPath, gotQuery, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotToken = r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Internal-Token")
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write([]byte("tarball"))
	}))
	defer srv.Close()

	body, err := source.New(srv.URL+"/", "tok", time.Second).Archive(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	_ = body.Close()

	if string(data) != "tarball" || gotPath != "/internal/v1/projects/7/repos/3/archive" || gotToken != "tok" {
		t.Errorf("data %q path %q token %q", data, gotPath, gotToken)
	}
	if gotQuery != "account=alice+smith&ref="+spec.HeadSHA {
		t.Errorf("query = %q", gotQuery)
	}
}

func TestArchiveReportsGatewayErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"repository not found"}}`))
	}))
	defer srv.Close()

	_, err := source.New(srv.URL, "tok", time.Second).Archive(context.Background(), spec)

	if !errors.Is(err, source.ErrUnavailable) || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "repository not found") {
		t.Errorf("err = %v", err)
	}
}

func TestArchiveToleratesNonJSONErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := source.New(srv.URL, "tok", time.Second).Archive(context.Background(), spec)

	if !errors.Is(err, source.ErrUnavailable) || !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v", err)
	}
}

func TestArchiveUnreachableGatewayDoesNotLeakAccount(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	_, err := source.New(url, "tok", time.Second).Archive(context.Background(), spec)

	if !errors.Is(err, source.ErrUnavailable) || strings.Contains(err.Error(), "alice") {
		t.Errorf("err = %v", err)
	}
}

func TestArchiveHonorsContextAndBadURL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.New("http://127.0.0.1:1", "t", time.Second).Archive(ctx, spec); !errors.Is(err, source.ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
	if _, err := source.New("http://bad host", "t", time.Second).Archive(context.Background(), spec); !errors.Is(err, source.ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
}
