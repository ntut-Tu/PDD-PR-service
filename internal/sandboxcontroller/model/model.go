// Package model defines the SandboxRun resource and its validation.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Phase is the lifecycle stage of a run.
type Phase string

// Phases of a SandboxRun. Succeeded, Failed and Error are terminal.
const (
	Pending   Phase = "Pending"
	Preparing Phase = "Preparing"
	Running   Phase = "Running"
	Succeeded Phase = "Succeeded"
	Failed    Phase = "Failed"
	Error     Phase = "Error"
)

// Terminal reports whether the run has finished.
func (p Phase) Terminal() bool { return p == Succeeded || p == Failed || p == Error }

// Reason explains a Failed or Error phase.
type Reason string

// Reasons of a finished run. The first three come with Failed, the rest with Error.
const (
	TestsFailed        Reason = "tests_failed"
	CompileFailed      Reason = "compile_failed"
	Timeout            Reason = "timeout"
	SandboxUnavailable Reason = "sandbox_unavailable"
	UnsupportedProject Reason = "unsupported_project"
	SourceUnavailable  Reason = "source_unavailable"
	Internal           Reason = "internal"
)

// BuildTool is the detected build system of the project.
type BuildTool string

// Supported build tools.
const (
	Maven  BuildTool = "maven"
	Gradle BuildTool = "gradle"
)

// File is a generated test file written into the workspace.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Spec is the immutable desired state of a run.
type Spec struct {
	Account     string   `json:"account"`
	ProjectID   int64    `json:"projectId"`
	RepoID      int64    `json:"repoId"`
	HeadSHA     string   `json:"headSha"`
	Files       []File   `json:"files"`
	TestClasses []string `json:"testClasses"`
}

// TestResult is the outcome of one test class.
type TestResult struct {
	Class   string `json:"class"`
	Passed  int    `json:"passed"`
	Failed  int    `json:"failed"`
	Errors  int    `json:"errors"`
	Skipped int    `json:"skipped"`
}

// Result holds what a finished run produced.
type Result struct {
	Compiled bool         `json:"compiled"`
	Tests    []TestResult `json:"tests"`
	Output   string       `json:"output"`
}

// Condition records one completed step of a run.
type Condition struct {
	Type    string    `json:"type"`
	Status  bool      `json:"status"`
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

// Status is the observed state of a run.
type Status struct {
	Phase      Phase       `json:"phase"`
	Reason     Reason      `json:"reason,omitempty"`
	Message    string      `json:"message"`
	BuildTool  BuildTool   `json:"buildTool,omitempty"`
	Result     *Result     `json:"result,omitempty"`
	Conditions []Condition `json:"conditions"`
	CreatedAt  time.Time   `json:"createdAt"`
	StartedAt  *time.Time  `json:"startedAt,omitempty"`
	FinishedAt *time.Time  `json:"finishedAt,omitempty"`
}

// Run is a SandboxRun record.
type Run struct {
	ID                string `json:"id"`
	Spec              Spec   `json:"spec"`
	Status            Status `json:"status"`
	DeletionRequested bool   `json:"deletionRequested"`
}

// SetCondition adds or replaces the condition of the given type.
func (s *Status) SetCondition(typ string, ok bool, at time.Time, message string) {
	c := Condition{Type: typ, Status: ok, Time: at.UTC(), Message: message}
	for i := range s.Conditions {
		if s.Conditions[i].Type == typ {
			s.Conditions[i] = c
			return
		}
	}
	s.Conditions = append(s.Conditions, c)
}

// NewID returns a random run id such as run_0123456789abcdef01234567.
func NewID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("model: random id: %v", err))
	}
	return "run_" + hex.EncodeToString(b[:])
}
