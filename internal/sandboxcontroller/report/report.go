// Package report turns the output of a Maven or Gradle test run into a SandboxRun result: it
// reads the JUnit XML reports, decides between success, compile failure and test failure, and
// shortens the build output to the configured size.
package report

import (
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"pdd/services/internal/sandboxcontroller/model"
)

const (
	maxReportBytes = 8 << 20
	maxReportFiles = 2000
)

// Suite is one JUnit XML test suite, which is one test class.
type Suite struct {
	Name     string
	Tests    int
	Failures int
	Errors   int
	Skipped  int
}

// ParseSuites reads every testsuite element of a JUnit XML document (a single testsuite or a
// testsuites wrapper).
func ParseSuites(r io.Reader) ([]Suite, error) {
	dec := xml.NewDecoder(io.LimitReader(r, maxReportBytes))
	var suites []Suite
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return suites, nil
		}
		if err != nil {
			return nil, fmt.Errorf("report: parse xml: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "testsuite" {
			continue
		}
		var s Suite
		for _, a := range start.Attr {
			n, _ := strconv.Atoi(a.Value)
			switch a.Name.Local {
			case "name":
				s.Name = a.Value
			case "tests":
				s.Tests = n
			case "failures":
				s.Failures = n
			case "errors":
				s.Errors = n
			case "skipped":
				s.Skipped = n
			}
		}
		suites = append(suites, s)
	}
}

// Collect reads the Maven (surefire-reports) and Gradle (build/test-results) JUnit XML files
// below dir. Symbolic links are never followed because the tests wrote into this tree.
// Unreadable or malformed files are skipped.
func Collect(dir string) []Suite {
	var suites []Suite
	files := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !d.Type().IsRegular() || !strings.HasPrefix(name, "TEST-") || !strings.HasSuffix(name, ".xml") || !isReportDir(filepath.ToSlash(filepath.Dir(path))) {
			return nil
		}
		if files++; files > maxReportFiles {
			return filepath.SkipAll
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()
		if parsed, err := ParseSuites(f); err == nil {
			suites = append(suites, parsed...)
		}
		return nil
	})
	return suites
}

func isReportDir(dir string) bool {
	return strings.HasSuffix(dir, "/surefire-reports") || strings.Contains(dir, "/build/test-results/")
}

// Summarize totals the suites per requested class. Suites of nested classes (Outer$Inner) count
// for their outer class. A class without any suite has no entry.
func Summarize(classes []string, suites []Suite) []model.TestResult {
	var out []model.TestResult
	for _, class := range classes {
		var r model.TestResult
		found := false
		for _, s := range suites {
			if s.Name != class && !strings.HasPrefix(s.Name, class+"$") {
				continue
			}
			found = true
			r.Failed += s.Failures
			r.Errors += s.Errors
			r.Skipped += s.Skipped
			r.Passed += max(0, s.Tests-s.Failures-s.Errors-s.Skipped)
		}
		if found {
			r.Class = class
			out = append(out, r)
		}
	}
	return out
}

// Outcome is the verdict on a finished build.
type Outcome struct {
	Phase   model.Phase
	Reason  model.Reason
	Message string
	Result  model.Result
}

var (
	compileErrorPattern = regexp.MustCompile(`COMPILATION ERROR|Compilation failure|:compile\w*(Java|Kotlin|Groovy)'|\.java:(\[\d+,\d+\]|\d+): |cannot find symbol|package \S+ does not exist`)
	errorLinePattern    = regexp.MustCompile(`\[ERROR\]|\berror:|\bFAILED\b|What went wrong|Execution failed|^\s*> `)
)

// Evaluate decides the outcome of a build that exited with exitCode and printed output, given
// the test classes that were requested and the JUnit suites found in the workspace. The output
// in the result is shortened to maxBytes.
func Evaluate(classes []string, suites []Suite, exitCode int, output string, maxBytes int) Outcome {
	tests := Summarize(classes, suites)
	compiled := len(tests) > 0 || exitCode == 0
	res := model.Result{Compiled: compiled, Tests: tests}
	if res.Tests == nil {
		res.Tests = []model.TestResult{}
	}
	res.Output = Condense(output, maxBytes, !compiled)

	missing := missingClasses(classes, tests)
	failedTests, totalTests := 0, 0
	for _, t := range tests {
		failedTests += t.Failed + t.Errors
		totalTests += t.Passed + t.Failed + t.Errors + t.Skipped
	}
	switch {
	case !compiled && compileErrorPattern.MatchString(output):
		return Outcome{model.Failed, model.CompileFailed, "compilation failed", res}
	case !compiled:
		return Outcome{model.Failed, model.CompileFailed, fmt.Sprintf("build failed before any test ran (exit code %d)", exitCode), res}
	case failedTests > 0:
		return Outcome{model.Failed, model.TestsFailed, fmt.Sprintf("%d of %d tests failed", failedTests, totalTests), res}
	case len(missing) > 0:
		return Outcome{model.Failed, model.TestsFailed, "no test report for " + strings.Join(missing, ", "), res}
	case totalTests == 0:
		return Outcome{model.Failed, model.TestsFailed, "no test was executed", res}
	case exitCode != 0:
		return Outcome{model.Failed, model.TestsFailed, fmt.Sprintf("tests passed but the build failed (exit code %d)", exitCode), res}
	}
	return Outcome{model.Succeeded, "", fmt.Sprintf("%d tests passed", totalTests), res}
}

// Partial builds the result of a run that was stopped, from whatever reports exist so far.
func Partial(classes []string, suites []Suite, output string, maxBytes int) model.Result {
	tests := Summarize(classes, suites)
	if tests == nil {
		tests = []model.TestResult{}
	}
	return model.Result{Compiled: len(tests) > 0, Tests: tests, Output: Condense(output, maxBytes, false)}
}

func missingClasses(classes []string, tests []model.TestResult) []string {
	have := map[string]bool{}
	for _, t := range tests {
		have[t.Class] = true
	}
	var missing []string
	for _, c := range classes {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	return missing
}

// Condense shortens output to at most maxBytes. The end of the output is kept because build
// tools print their summary there. With keepErrors (nothing compiled) the error lines of the
// whole output are kept in front, so that early compiler messages survive.
func Condense(output string, maxBytes int, keepErrors bool) string {
	output = strings.ToValidUTF8(output, "?")
	if len(output) <= maxBytes {
		return output
	}
	head := ""
	if keepErrors {
		head = errorLines(output, maxBytes/2)
	}
	tailBytes := maxBytes - len(head)
	marker := fmt.Sprintf("[output truncated, %d bytes omitted]\n", len(output)-tailBytes)
	if tailBytes > len(marker) {
		tailBytes -= len(marker)
	} else {
		marker = ""
	}
	return head + marker + tail(output, tailBytes)
}

func errorLines(output string, limit int) string {
	var b strings.Builder
	for _, line := range strings.Split(output, "\n") {
		if !errorLinePattern.MatchString(line) {
			continue
		}
		if b.Len()+len(line)+1 > limit {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if b.Len() > 0 {
		b.WriteString("...\n")
	}
	return b.String()
}

// tail returns the last n bytes of s without splitting a UTF-8 sequence.
func tail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}
