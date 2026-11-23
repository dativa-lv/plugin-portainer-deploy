// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLintStackFile(t *testing.T) {
	tests := []struct {
		name      string
		stack     string
		wantError string // empty: the stack follows the rules
	}{
		{
			name: "limits set",
			stack: `
services:
  api:
    image: example.com/team/api:1.0
    deploy:
      replicas: 2
      resources:
        limits:
          cpus: "0.5"
          memory: 256M
`,
		},
		{
			name: "limits set as variables",
			stack: `
services:
  api:
    image: example.com/team/api:1.0
    deploy:
      replicas: ${API_REPLICAS}
      resources:
        limits:
          cpus: ${API_CPUS}
          memory: ${API_MEMORY}
`,
		},
		{
			name: "no limits",
			stack: `
services:
  api:
    image: example.com/team/api:1.0
`,
			wantError: "2 stack rule violation(s)",
		},
		{
			name: "memory limit missing",
			stack: `
services:
  api:
    image: example.com/team/api:1.0
    deploy:
      resources:
        limits:
          cpus: 0.5
`,
			wantError: "1 stack rule violation(s)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"stack.yaml": test.stack})

			err := lintStackFile(filepath.Join(dir, "stack.yaml"))

			switch {
			case test.wantError == "" && err != nil:
				t.Errorf("want no error, got %v", err)
			case test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)):
				t.Errorf("want error containing %q, got %v", test.wantError, err)
			}
		})
	}
}

func TestRunsSingleReplica(t *testing.T) {
	tests := []struct {
		name   string
		deploy serviceDeploy
		want   bool
	}{
		{name: "replicas not set", deploy: serviceDeploy{}, want: true},
		{name: "one replica", deploy: serviceDeploy{Replicas: 1}, want: true},
		{name: "two replicas", deploy: serviceDeploy{Replicas: 2}, want: false},
		{name: "replicas from a variable", deploy: serviceDeploy{Replicas: "${REPLICAS}"}, want: false},
		{name: "global service", deploy: serviceDeploy{Mode: "global"}, want: false},
		{name: "job service", deploy: serviceDeploy{Mode: "replicated-job"}, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := runsSingleReplica(test.deploy); got != test.want {
				t.Errorf("got %v, want %v", got, test.want)
			}
		})
	}
}
