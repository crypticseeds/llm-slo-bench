package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunRejectsUnknownCommand(t *testing.T) {
	err := run([]string{"surprise"})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("run() error = %v, want unknown command error", err)
	}
}

func TestRunMockRejectsUnknownProfile(t *testing.T) {
	err := runMock(context.Background(), []string{"--profile", "surprise"})
	if err == nil || !strings.Contains(err.Error(), "unknown latency profile") {
		t.Fatalf("runMock() error = %v, want profile error", err)
	}
}

func TestRunHelpIsSuccessful(t *testing.T) {
	if err := run([]string{"probe", "--help"}); err != nil {
		t.Fatalf("run(probe --help) error = %v", err)
	}
}
