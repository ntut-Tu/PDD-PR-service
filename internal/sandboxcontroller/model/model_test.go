package model_test

import (
	"strings"
	"testing"
	"time"

	"pdd/services/internal/sandboxcontroller/model"
)

func validSpec() model.Spec {
	return model.Spec{
		Account: "alice", ProjectID: 7, RepoID: 1, HeadSHA: strings.Repeat("a", 40),
		Files:       []model.File{{Path: "src/test/java/a/BTest.java", Content: "class BTest {}"}},
		TestClasses: []string{"a.BTest"},
	}
}

func TestValidateAcceptsValidSpec(t *testing.T) {
	spec := validSpec()
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*model.Spec){
		"account":              func(s *model.Spec) { s.Account = " " },
		"account ctl":          func(s *model.Spec) { s.Account = "a\nb" },
		"projectId":            func(s *model.Spec) { s.ProjectID = 0 },
		"repoId":               func(s *model.Spec) { s.RepoID = -1 },
		"headSha":              func(s *model.Spec) { s.HeadSHA = "main" },
		"upper sha":            func(s *model.Spec) { s.HeadSHA = strings.Repeat("A", 40) },
		"traversal":            func(s *model.Spec) { s.Files[0].Path = "src/test/../../pom.xml" },
		"outside":              func(s *model.Spec) { s.Files[0].Path = "src/main/java/A.java" },
		"absolute":             func(s *model.Spec) { s.Files[0].Path = "/src/test/A.java" },
		"empty path":           func(s *model.Spec) { s.Files[0].Path = "" },
		"backslash":            func(s *model.Spec) { s.Files[0].Path = "src\\test\\A.java" },
		"src/test is the file": func(s *model.Spec) { s.Files[0].Path = "src/test" },
		"too many files": func(s *model.Spec) {
			s.Files = make([]model.File, model.MaxFiles+1)
			for i := range s.Files {
				s.Files[i].Path = "src/test/A.java"
			}
		},
		"too large":   func(s *model.Spec) { s.Files[0].Content = strings.Repeat("x", model.MaxContentBytes+1) },
		"no classes":  func(s *model.Spec) { s.TestClasses = nil },
		"class shell": func(s *model.Spec) { s.TestClasses = []string{"a.B; rm -rf /"} },
		"class dots":  func(s *model.Spec) { s.TestClasses = []string{"a..B"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := validSpec()
			mutate(&spec)
			if err := spec.Validate(); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	spec := model.Spec{}
	err := spec.Validate()
	if err == nil {
		t.Fatal("accepted")
	}
	for _, f := range []string{"account", "projectId", "repoId", "headSha", "testClasses"} {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("missing %s in %v", f, err)
		}
	}
}

func TestCleanTestPath(t *testing.T) {
	good := map[string]string{
		"src/test/java/A.java":        "src/test/java/A.java",
		"./src/test//java/A.java":     "src/test/java/A.java",
		"module/src/test/java/A.java": "module/src/test/java/A.java",
	}
	for in, want := range good {
		got, err := model.CleanTestPath(in)
		if err != nil || got != want {
			t.Errorf("CleanTestPath(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := model.CleanTestPath("src/test/\x00"); err == nil {
		t.Error("NUL accepted")
	}
}

func TestPhaseTerminalAndID(t *testing.T) {
	for p, want := range map[model.Phase]bool{model.Pending: false, model.Preparing: false, model.Running: false,
		model.Succeeded: true, model.Failed: true, model.Error: true} {
		if p.Terminal() != want {
			t.Errorf("%s.Terminal() = %v", p, !want)
		}
	}
	if !model.ValidID("run_0123456789abcdef01234567") || model.ValidID("run_../x") || model.ValidID("") {
		t.Error("ValidID")
	}
}

func TestSetConditionReplacesSameType(t *testing.T) {
	var s model.Status
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.SetCondition("Prepared", false, at, "a")
	s.SetCondition("Started", true, at, "b")
	s.SetCondition("Prepared", true, at.Add(time.Second), "c")

	if len(s.Conditions) != 2 || !s.Conditions[0].Status || s.Conditions[0].Message != "c" {
		t.Errorf("conditions = %+v", s.Conditions)
	}
}

func TestNewIDIsValidAndUnique(t *testing.T) {
	a, b := model.NewID(), model.NewID()
	if !model.ValidID(a) || a == b {
		t.Errorf("ids %q %q", a, b)
	}
}
