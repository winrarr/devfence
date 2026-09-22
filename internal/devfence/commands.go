package devfence

import (
	"context"
	"os/exec"
	"strings"
)

var execCommand = exec.Command
var execCommandContext = exec.CommandContext

func commandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return execCommandContext(ctx, name, args...).Output()
}

func replaceEnv(environment []string, key, value string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			filtered = append(filtered, item)
		}
	}
	return append(filtered, prefix+value)
}
