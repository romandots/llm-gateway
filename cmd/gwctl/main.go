// Command gwctl is the control plane of the LLM gateway: it reconciles the
// declarative configuration in config/ with a running LiteLLM proxy through
// its Admin API, and manages consumer keys, spend reports and health checks.
//
// It is deliberately a separate static binary rather than a script inside the
// proxy image: upgrading the data plane must never break key management.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
