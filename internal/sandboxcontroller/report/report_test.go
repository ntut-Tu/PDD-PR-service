package report_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"pdd/services/internal/sandboxcontroller/model"
	"pdd/services/internal/sandboxcontroller/report"
)

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func suitesOf(t *testing.T, name string) []report.Suite {
	t.Helper()
	suites, err := report.ParseSuites(strings.NewReader(readTestdata(t, name)))
	if err != nil {
		t.Fatal(err)
	}
	return suites
}

func TestParseSurefireReport(t *testing.T) {
	suites := suitesOf(t, "surefire-mixed.xml")

	want := report.Suite{Name: "a.BTest", Tests: 4, Failures: 1, Errors: 1, Skipped: 1}
	if len(suites) != 1 || suites[0] != want {
		t.Errorf("suites = %+v", suites)
	}
}

func TestParseGradleReport(t *testing.T) {
	suites := suitesOf(t, "gradle-failed.xml")

	want := report.Suite{Name: "a.BTest", Tests: 4, Failures: 1}
	if len(suites) != 1 || suites[0] != want {
		t.Errorf("suites = %+v", suites)
	}
}

func TestParseTestsuitesWrapper(t *testing.T) {
	xml := `<testsuites><testsuite name="x.A" tests="2" failures="0"/><testsuite name="x.B" tests="1" failures="1"/></testsuites>`

	suites, err := report.ParseSuites(strings.NewReader(xml))

	if err != nil || len(suites) != 2 || suites[1].Name != "x.B" || suites[1].Failures != 1 {
		t.Errorf("suites = %+v, err = %v", suites, err)
	}
}

func TestParseRejectsMalformedXML(t *testing.T) {
	if _, err := report.ParseSuites(strings.NewReader("<testsuite name='a'")); err == nil {
		t.Error("malformed xml accepted")
	}
}

func TestSummarizeAggregatesNestedClassesAndSkipsMissing(t *testing.T) {
	suites := []report.Suite{
		{Name: "a.B", Tests: 3, Failures: 1},
		{Name: "a.B$Inner", Tests: 2, Skipped: 1},
		{Name: "a.BTest", Tests: 5},
		{Name: "other.C", Tests: 9},
	}

	got := report.Summarize([]string{"a.B", "a.Missing"}, suites)

	want := []model.TestResult{{Class: "a.B", Passed: 3, Failed: 1, Skipped: 1}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("got %+v", got)
	}
}

func writeReport(t *testing.T, root, rel, name string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(readTestdata(t, name)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectFindsMavenAndGradleReports(t *testing.T) {
	root := t.TempDir()
	writeReport(t, root, "target/surefire-reports/TEST-a.BTest.xml", "surefire-pass.xml")
	writeReport(t, root, "mod/build/test-results/test/TEST-a.BTest.xml", "gradle-failed.xml")
	writeReport(t, root, "other/TEST-ignored.xml", "surefire-pass.xml")
	writeReport(t, root, "target/surefire-reports/summary.xml", "surefire-pass.xml")
	writeReport(t, root, ".git/target/surefire-reports/TEST-x.xml", "surefire-pass.xml")
	writeReport(t, root, "target/surefire-reports/TEST-bad.xml", "maven-compile-error.log")

	suites := report.Collect(root)

	if len(suites) != 2 {
		t.Fatalf("suites = %+v", suites)
	}
}

func TestCollectDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeReport(t, outside, "secret/TEST-leak.xml", "surefire-pass.xml")
	dir := filepath.Join(root, "target", "surefire-reports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret", "TEST-leak.xml"), filepath.Join(dir, "TEST-link.xml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}

	if suites := report.Collect(root); len(suites) != 0 {
		t.Errorf("followed a symlink: %+v", suites)
	}
}

func TestCollectOfMissingDirIsEmpty(t *testing.T) {
	if suites := report.Collect(filepath.Join(t.TempDir(), "nope")); len(suites) != 0 {
		t.Errorf("suites = %+v", suites)
	}
}

func TestEvaluate(t *testing.T) {
	classes := []string{"a.BTest"}
	pass := []report.Suite{{Name: "a.BTest", Tests: 2}}
	mixed := suitesOf(t, "surefire-mixed.xml")
	cases := []struct {
		name   string
		suites []report.Suite
		exit   int
		output string
		phase  model.Phase
		reason model.Reason
		msg    string
	}{
		{"all pass", pass, 0, "BUILD SUCCESS", model.Succeeded, "", "2 tests passed"},
		{"skipped counts as pass", []report.Suite{{Name: "a.BTest", Tests: 2, Skipped: 1}}, 0, "", model.Succeeded, "", "2 tests passed"},
		{"failures", mixed, 1, readTestdata(t, "maven-tests-failed.log"), model.Failed, model.TestsFailed, "2 of 4 tests failed"},
		{"compile error", nil, 1, readTestdata(t, "maven-compile-error.log"), model.Failed, model.CompileFailed, "compilation failed"},
		{"gradle compile error", nil, 1, "BTest.java:5: error: ';' expected", model.Failed, model.CompileFailed, "compilation failed"},
		{"build failed without compiler message", nil, 1, "Could not resolve dependencies", model.Failed, model.CompileFailed, "build failed before any test ran (exit code 1)"},
		{"class did not run", []report.Suite{{Name: "a.Other", Tests: 1}}, 0, "", model.Failed, model.TestsFailed, "no test report for a.BTest"},
		{"no tests", []report.Suite{{Name: "a.BTest"}}, 0, "", model.Failed, model.TestsFailed, "no test was executed"},
		{"passed but build failed", pass, 1, "", model.Failed, model.TestsFailed, "tests passed but the build failed (exit code 1)"},
		{"exit zero without reports", nil, 0, "", model.Failed, model.TestsFailed, "no test report for a.BTest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := report.Evaluate(classes, tc.suites, tc.exit, tc.output, 1<<20)

			if got.Phase != tc.phase || got.Reason != tc.reason || got.Message != tc.msg {
				t.Errorf("got %s/%s %q, want %s/%s %q", got.Phase, got.Reason, got.Message, tc.phase, tc.reason, tc.msg)
			}
			if got.Result.Tests == nil {
				t.Error("tests must be a list, not null")
			}
			if got.Result.Compiled != (tc.reason != model.CompileFailed) {
				t.Errorf("compiled = %v", got.Result.Compiled)
			}
		})
	}
}

func TestEvaluateKeepsCompilerErrorsWhenOutputIsTruncated(t *testing.T) {
	var b strings.Builder
	b.WriteString("[ERROR] /workspace/src/src/test/java/a/BTest.java:[5,57] ';' expected\n")
	for i := 0; i < 500; i++ {
		b.WriteString("[INFO] downloading something very long and chatty ........................\n")
	}
	b.WriteString("[ERROR] BUILD FAILURE tail\n")

	got := report.Evaluate([]string{"a.BTest"}, nil, 1, b.String(), 600)

	if len(got.Result.Output) > 600 {
		t.Errorf("output is %d bytes", len(got.Result.Output))
	}
	if !strings.Contains(got.Result.Output, "BTest.java:[5,57] ';' expected") || !strings.Contains(got.Result.Output, "BUILD FAILURE tail") {
		t.Errorf("output = %q", got.Result.Output)
	}
}

func TestCondense(t *testing.T) {
	long := strings.Repeat("0123456789", 100)

	if got := report.Condense("short", 100, false); got != "short" {
		t.Errorf("short = %q", got)
	}
	got := report.Condense(long+"END", 120, false)
	if len(got) > 120 || !strings.HasSuffix(got, "END") || !strings.Contains(got, "truncated") {
		t.Errorf("tail = %q", got)
	}
	if got := report.Condense(long, 10, false); len(got) > 10 || got == "" {
		t.Errorf("tiny limit = %q", got)
	}
	multi := strings.Repeat("é", 200)
	if got := report.Condense(multi, 101, false); !utf8.ValidString(got) || len(got) > 101 {
		t.Errorf("utf8 cut = %q", got)
	}
	if got := report.Condense("bad\xffbyte", 100, false); !utf8.ValidString(got) {
		t.Errorf("invalid utf8 kept: %q", got)
	}
	if got := report.Condense("x", 0, false); got != "" {
		// A zero limit is rejected by the configuration; the function still must not panic.
		t.Errorf("zero limit = %q", got)
	}
}

func TestPartial(t *testing.T) {
	res := report.Partial([]string{"a.BTest"}, []report.Suite{{Name: "a.BTest", Tests: 1}}, "out", 100)
	if !res.Compiled || len(res.Tests) != 1 || res.Output != "out" {
		t.Errorf("res = %+v", res)
	}
	res = report.Partial([]string{"a.BTest"}, nil, "out", 100)
	if res.Compiled || res.Tests == nil {
		t.Errorf("res = %+v", res)
	}
}
