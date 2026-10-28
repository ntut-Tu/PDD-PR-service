package repos_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/repos"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
var ghRepo = model.Repo{Provider: model.GitHub}

type fakeProvider struct {
	content   []byte
	contentEr error
}

func (f *fakeProvider) Tree(context.Context, string, model.Repo, string) (provider.Tree, provider.Meta, error) {
	return provider.Tree{}, provider.Meta{}, f.contentEr
}
func (f *fakeProvider) File(context.Context, string, model.Repo, string, string) ([]byte, provider.Meta, error) {
	return f.content, provider.Meta{}, f.contentEr
}
func limited(p *fakeProvider, fileBytes int) *repos.Repos {
	return repos.New(map[model.Provider]repos.Provider{model.GitHub: p}, func() time.Time { return now }, fileBytes)
}
func TestFileText(t *testing.T) {
	p := &fakeProvider{content: []byte("héllo")}

	res, err := limited(p, 100).File(context.Background(), "tok", ghRepo, "abc", "a.txt")

	if err != nil || res.Content != "héllo" || res.Size != 6 || res.Binary || res.Truncated || res.Ref != "abc" || res.Path != "a.txt" {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestFileDetectsBinary(t *testing.T) {
	for name, content := range map[string][]byte{
		"nul byte":      []byte("PK\x00\x03text"),
		"invalid utf-8": {0xff, 0xfe, 'a'},
	} {
		res, err := limited(&fakeProvider{content: content}, 100).File(context.Background(), "tok", ghRepo, "abc", "x")

		if err != nil || !res.Binary || res.Content != "" || res.Size != int64(len(content)) || res.Truncated {
			t.Errorf("%s: result = %+v, %v", name, res, err)
		}
	}
}

func TestFileTruncatesOnCharacterBoundary(t *testing.T) {
	// "é" is two bytes; a cut at 5 would split the second one.
	p := &fakeProvider{content: []byte("aaéé")}

	res, err := limited(p, 5).File(context.Background(), "tok", ghRepo, "abc", "x")

	if err != nil || !res.Truncated || res.Content != "aaé" || res.Size != 6 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	long := &fakeProvider{content: []byte(strings.Repeat("x", 10))}
	if res, _ := limited(long, 10).File(context.Background(), "tok", ghRepo, "abc", "x"); res.Truncated {
		t.Error("file at the limit was truncated")
	}
}

func TestFileProviderError(t *testing.T) {
	p := &fakeProvider{contentEr: provider.ErrNotFound}

	if _, err := limited(p, 0).File(context.Background(), "tok", ghRepo, "abc", "x"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestMissingProvider(t *testing.T) {
	content := repos.New(nil, nil, 0)
	if _, err := content.Tree(context.Background(), "t", ghRepo, "x"); err == nil {
		t.Error("Tree without adapter did not fail")
	}
	if _, err := content.File(context.Background(), "t", ghRepo, "x", "p"); err == nil {
		t.Error("File without adapter did not fail")
	}
}
