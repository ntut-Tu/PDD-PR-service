// Package build detects the build tool of a checkout and describes how to run the generated
// tests with it inside a sandbox: the command line, the environment, and the configuration
// files that point Maven and Gradle at the allow-list proxy.
package build

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"pdd/services/internal/sandboxcontroller/model"
)

// ErrUnsupported means the checkout is not a Maven or Gradle project.
var ErrUnsupported = errors.New("unsupported project")

// Container paths. The run directory is mounted at WorkspaceMount and the repository cache at
// CacheMount; HOME points into the cache so that .m2 and .gradle are cached per repository.
const (
	WorkspaceMount = "/workspace"
	CacheMount     = "/cache"
	SrcPath        = WorkspaceMount + "/src"
	InitPath       = WorkspaceMount + "/.pdd"
	HomePath       = CacheMount + "/home"
)

// Plan is how to run the tests of one project.
type Plan struct {
	Tool       model.BuildTool
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
}

// Proxy is where the sandbox reaches the allow-list proxy.
type Proxy struct {
	Host string
	Port string
}

// Detect finds the build tool in the root of srcDir and whether the project's wrapper script
// can be used. Maven wins when both build files exist.
func Detect(srcDir string) (tool model.BuildTool, wrapper bool, err error) {
	switch {
	case exists(srcDir, "pom.xml"):
		return model.Maven, exists(srcDir, "mvnw") && exists(srcDir, ".mvn/wrapper/maven-wrapper.properties"), nil
	case exists(srcDir, "build.gradle"), exists(srcDir, "build.gradle.kts"):
		return model.Gradle, exists(srcDir, "gradlew") && exists(srcDir, "gradle/wrapper/gradle-wrapper.properties") &&
			exists(srcDir, "gradle/wrapper/gradle-wrapper.jar"), nil
	}
	return "", false, fmt.Errorf("%w: neither pom.xml nor build.gradle(.kts) found at the repository root", ErrUnsupported)
}

func exists(dir, rel string) bool {
	info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil && info.Mode().IsRegular()
}

// NewPlan builds the command that compiles the project and runs only the given test classes.
// No shell is involved: the class names are validated and passed as separate arguments.
func NewPlan(tool model.BuildTool, wrapper bool, classes []string, proxy Proxy) Plan {
	p := Plan{Tool: tool, WorkingDir: SrcPath}
	javaProxy := javaProxyFlags(proxy)
	switch tool {
	case model.Maven:
		p.Entrypoint = []string{"mvn"}
		if wrapper {
			p.Entrypoint = []string{"sh", "./mvnw"}
		}
		p.Cmd = []string{"-B", "-ntp", "-s", InitPath + "/settings.xml", "test",
			"-Dtest=" + strings.Join(classes, ","), "-Dsurefire.failIfNoSpecifiedTests=false", "-DfailIfNoTests=false"}
		p.Env = []string{
			"HOME=" + HomePath,
			"MAVEN_USER_HOME=" + HomePath + "/.m2",
			"MAVEN_CONFIG=",
			"MAVEN_OPTS=" + strings.Join(append([]string{"-Duser.home=" + HomePath}, javaProxy...), " "),
		}
	default:
		p.Entrypoint = []string{"gradle"}
		if wrapper {
			p.Entrypoint = []string{"sh", "./gradlew"}
		}
		p.Cmd = []string{"test", "--no-daemon", "--console=plain", "-I", InitPath + "/init.gradle"}
		for _, c := range classes {
			p.Cmd = append(p.Cmd, "--tests", c)
		}
		p.Env = []string{
			"HOME=" + HomePath,
			"GRADLE_USER_HOME=" + HomePath + "/.gradle",
			"GRADLE_OPTS=" + strings.Join(append([]string{"-Duser.home=" + HomePath}, javaProxy...), " "),
		}
	}
	return p
}

func javaProxyFlags(p Proxy) []string {
	var flags []string
	for _, scheme := range []string{"http", "https"} {
		flags = append(flags, "-D"+scheme+".proxyHost="+p.Host, "-D"+scheme+".proxyPort="+p.Port)
	}
	return append(flags, "-Dhttp.nonProxyHosts=")
}

// WriteInit writes the Maven settings and the Gradle init script into dir (the mounted init
// directory). Maven gets the proxy through settings.xml, which newer Maven versions require;
// the Gradle init script lets --tests match nothing in modules that do not have the class.
func WriteInit(dir string, proxy Proxy) error {
	port, err := strconv.Atoi(proxy.Port)
	if err != nil {
		return fmt.Errorf("build: proxy port %q: %w", proxy.Port, err)
	}
	settings := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<settings xmlns="http://maven.apache.org/SETTINGS/1.2.0">
  <proxies>
    <proxy>
      <id>pdd-sandbox</id>
      <active>true</active>
      <protocol>http</protocol>
      <host>%s</host>
      <port>%d</port>
    </proxy>
  </proxies>
</settings>
`, xmlEscape(proxy.Host), port)
	const gradleInit = `allprojects {
    tasks.withType(Test).configureEach {
        filter.failOnNoMatchingTests = false
    }
}
`
	for name, content := range map[string]string{"settings.xml": settings, "init.gradle": gradleInit} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return fmt.Errorf("build: write %s: %w", name, err)
		}
	}
	return nil
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
