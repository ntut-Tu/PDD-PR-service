package build_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pdd/services/internal/sandboxcontroller/build"
	"pdd/services/internal/sandboxcontroller/model"
)

func project(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		path := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name    string
		files   []string
		tool    model.BuildTool
		wrapper bool
	}{
		{"maven", []string{"pom.xml"}, model.Maven, false},
		{"maven wrapper", []string{"pom.xml", "mvnw", ".mvn/wrapper/maven-wrapper.properties"}, model.Maven, true},
		{"mvnw without properties", []string{"pom.xml", "mvnw"}, model.Maven, false},
		{"gradle groovy", []string{"build.gradle"}, model.Gradle, false},
		{"gradle kotlin", []string{"build.gradle.kts", "settings.gradle.kts"}, model.Gradle, false},
		{"gradle wrapper", []string{"build.gradle", "gradlew", "gradle/wrapper/gradle-wrapper.properties", "gradle/wrapper/gradle-wrapper.jar"}, model.Gradle, true},
		{"gradlew without jar", []string{"build.gradle", "gradlew", "gradle/wrapper/gradle-wrapper.properties"}, model.Gradle, false},
		{"maven preferred", []string{"pom.xml", "build.gradle"}, model.Maven, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, wrapper, err := build.Detect(project(t, tc.files...))
			if err != nil || tool != tc.tool || wrapper != tc.wrapper {
				t.Errorf("got %s %v %v", tool, wrapper, err)
			}
		})
	}
}

func TestDetectUnsupported(t *testing.T) {
	for name, files := range map[string][]string{
		"empty":    nil,
		"npm":      {"package.json"},
		"nested":   {"sub/pom.xml"},
		"dir only": {"pom.xml/x"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := build.Detect(project(t, files...)); !errors.Is(err, build.ErrUnsupported) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestDetectIgnoresSymlinkedBuildFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "pom.xml")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := build.Detect(dir); !errors.Is(err, build.ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
}

var proxy = build.Proxy{Host: "172.20.0.2", Port: "3128"}

func TestMavenPlan(t *testing.T) {
	p := build.NewPlan(model.Maven, false, []string{"a.BTest", "a.CTest"}, proxy)

	if strings.Join(p.Entrypoint, " ") != "mvn" || p.WorkingDir != "/workspace/src" {
		t.Errorf("plan = %+v", p)
	}
	cmd := strings.Join(p.Cmd, " ")
	for _, want := range []string{"-B", "test", "-Dtest=a.BTest,a.CTest", "-Dsurefire.failIfNoSpecifiedTests=false", "-s /workspace/.pdd/settings.xml"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("cmd %q lacks %q", cmd, want)
		}
	}
	env := strings.Join(p.Env, "\n")
	for _, want := range []string{"HOME=/cache/home", "-Dhttps.proxyHost=172.20.0.2", "-Dhttp.proxyPort=3128", "-Duser.home=/cache/home"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}
}

func TestWrapperPlansUseTheScripts(t *testing.T) {
	m := build.NewPlan(model.Maven, true, []string{"a.B"}, proxy)
	g := build.NewPlan(model.Gradle, true, []string{"a.B"}, proxy)

	if strings.Join(m.Entrypoint, " ") != "sh ./mvnw" || strings.Join(g.Entrypoint, " ") != "sh ./gradlew" {
		t.Errorf("entrypoints = %v %v", m.Entrypoint, g.Entrypoint)
	}
}

func TestGradlePlan(t *testing.T) {
	p := build.NewPlan(model.Gradle, false, []string{"a.BTest", "a.CTest"}, proxy)

	cmd := strings.Join(p.Cmd, " ")
	for _, want := range []string{"test", "--tests a.BTest --tests a.CTest", "--no-daemon", "-I /workspace/.pdd/init.gradle"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("cmd %q lacks %q", cmd, want)
		}
	}
	env := strings.Join(p.Env, "\n")
	for _, want := range []string{"GRADLE_USER_HOME=/cache/home/.gradle", "GRADLE_OPTS=", "-Dhttps.proxyHost=172.20.0.2"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}
}

func TestWriteInit(t *testing.T) {
	dir := t.TempDir()

	if err := build.WriteInit(dir, build.Proxy{Host: "a&b", Port: "3128"}); err != nil {
		t.Fatal(err)
	}

	settings, _ := os.ReadFile(filepath.Join(dir, "settings.xml"))
	if !strings.Contains(string(settings), "<host>a&amp;b</host>") || !strings.Contains(string(settings), "<port>3128</port>") {
		t.Errorf("settings = %s", settings)
	}
	gradle, _ := os.ReadFile(filepath.Join(dir, "init.gradle"))
	if !strings.Contains(string(gradle), "failOnNoMatchingTests = false") {
		t.Errorf("init.gradle = %s", gradle)
	}
}

func TestWriteInitErrors(t *testing.T) {
	if err := build.WriteInit(t.TempDir(), build.Proxy{Host: "h", Port: "x"}); err == nil {
		t.Error("bad port accepted")
	}
	if err := build.WriteInit(filepath.Join(t.TempDir(), "missing"), proxy); err == nil {
		t.Error("missing dir accepted")
	}
}
