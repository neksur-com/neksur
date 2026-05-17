// BSL 1.1 — Copyright 2024 Neksur, Inc.
// See LICENSE file for terms.

// license-gen is a stand-alone CLI that generates signed Neksur license manifests.
//
// Usage:
//
//	license-gen [flags]
//
// Run with -h for full flag list and usage.
//
// Production private key ceremony: see Plan 03-15 operator runbook.
// The private key MUST NEVER be committed to the repository.

package main

import (
	"io"
	"os"

	"github.com/neksur-com/neksur/internal/licensegen"
)

func main() {
	os.Exit(licensegen.Run(os.Args[1:], os.Stdout, os.Stderr))
}

// Run is a thin shim exported from package main for cmd/license-gen/main_test.go.
// Integration tests use internal/licensegen.Run directly (importable non-main package).
func Run(args []string, stdout, stderr io.Writer) int {
	return licensegen.Run(args, stdout, stderr)
}
