package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{name: "no args", args: nil, wantCode: ExitUsage, wantStderr: "Usage: dalforge"},
		{name: "help", args: []string{"-h"}, wantCode: ExitOK, wantStderr: "generate"},
		{name: "unknown command", args: []string{"frobnicate"}, wantCode: ExitUsage, wantStderr: `unknown command "frobnicate"`},
		{name: "unknown flag", args: []string{"-nope"}, wantCode: ExitUsage, wantStderr: "flag provided but not defined"},
		{name: "generate", args: []string{"generate"}, wantCode: ExitError, wantStderr: "dalforge generate: not implemented"},
		{name: "lint", args: []string{"lint"}, wantCode: ExitError, wantStderr: "dalforge lint: not implemented"},
		{name: "migrate", args: []string{"migrate"}, wantCode: ExitError, wantStderr: "dalforge migrate: not implemented"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := Run(tt.args, &stderr); got != tt.wantCode {
				t.Errorf("Run(%q) = %d, want %d", tt.args, got, tt.wantCode)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("Run(%q) stderr = %q, want it to contain %q", tt.args, stderr.String(), tt.wantStderr)
			}
		})
	}
}
