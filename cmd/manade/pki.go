package main

import (
	"fmt"
	"io"
	"time"

	"github.com/cplieger/manade/internal/pki"
)

const pkiUsage = "usage: manade pki <dir> <host>..."

// runPKI issues the pool's TLS files under args[0] for the hosts args[1:]
// names, and names each directory it made and where it belongs.
func runPKI(args []string, stderr io.Writer) int {
	if len(args) < 1 || args[0] == "" {
		_, _ = fmt.Fprintln(stderr, pkiUsage)
		return 2
	}
	root := args[0]
	made, err := pki.Issue(root, args[1:], time.Now())
	for _, dir := range made {
		_, _ = fmt.Fprintln(stderr, "manade pki wrote", dir)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "Mount %s/%s as the orchestrator's TLS_DIR and each %s/<host> as that host's; %s/%s holds the CA key, which only manade pki reads, so keep it off every machine.\n",
		root, pki.OrchestratorDir, root, root, pki.CADir)
	return 0
}
