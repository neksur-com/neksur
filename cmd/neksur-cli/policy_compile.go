// neksur-cli policy compile — operator subcommand for Plan 01-09 Task 3.
//
// Dogfoods the CEL compiler the L1 Catalog Gateway uses at runtime
// (`internal/policy/cel.Compiler` from Plan 01-05) so SecOps policy
// authors can validate CEL syntax BEFORE pushing policy text to the
// per-tenant graph. This is the Pitfall 7 mitigation per CONTEXT
// line 84: the same env (with manifest.has_column / manifest.has_partition
// / principal.role bindings) the gateway uses; the same compile
// errors a malformed customer commit would trigger.
//
// Flow:
//
//   1. Take one positional arg: path to a `.cel` file.
//   2. Read the file via os.ReadFile (no stdin in Phase 1; future
//      may add `-` for stdin).
//   3. Build cel.NewEnv() — process-singleton env from Plan 01-05.
//   4. Build cel.NewCompiler(env, 0) — 0 → defaultCacheSize 4096.
//   5. compiler.CompileOrGet("cli-compile-<file>", string(content)).
//   6. On error: print wrapped error to stderr, exit 1.
//   7. On success: print "Policy <file> compiles cleanly." to stdout,
//      exit 0.
//
// Usage:
//
//   neksur-cli policy compile /path/to/my-schema-policy.cel
//
// Exit codes:
//
//   0  Policy compiles cleanly.
//   1  CEL syntax error OR undeclared binding (wrapped in
//      cel.ErrCompileFailed).
//   2  Wrong usage (missing positional arg) OR file does not exist
//      / unreadable.
//
// See runbooks/policy-author.md §2.1 + §5 for the full operator
// workflow.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	cel "github.com/neksur-com/neksur/internal/policy/cel"
)

// runPolicyCompile implements `neksur-cli policy compile <file>`.
// Returns an int exit code (0 = success, 1 = compile failed, 2 =
// usage / missing file).
//
// ctx is unused today (the cel.Compiler doesn't accept ctx because
// compilation is a pure-CPU operation; future cel-go releases may
// thread ctx through compile-time custom-resolvers, at which point
// this signature is already ready).
func runPolicyCompile(_ context.Context, args []string) int {
	fs := flag.NewFlagSet("policy compile", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr,
			"Usage: neksur-cli policy compile <file.cel>")
		fmt.Fprintln(os.Stderr,
			"  Validates a CEL policy file against the same env the L1 gateway uses.")
		fmt.Fprintln(os.Stderr,
			"  Exit 0 on valid; exit 1 on CEL syntax/binding error; exit 2 on usage error.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	path := fs.Arg(0)

	// Read the policy file. os.ReadFile returns wrapped *PathError on
	// missing/unreadable; we surface it on stderr at exit 2 to
	// distinguish "you passed a bad path" from "CEL didn't parse".
	content, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "policy compile: read %s: %v\n", path, err)
		return 2
	}

	// Build the env + compiler — same shape the gateway uses at
	// runtime per cmd/neksur-server/main.go::runWithSaasAuth.
	// cel.NewEnv() is a process-singleton (sync.Once), so even when
	// the CLI is invoked multiple times in a single test process the
	// env is constructed at most once.
	env, err := cel.NewEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "policy compile: cel.NewEnv: %v\n", err)
		return 1
	}
	compiler, err := cel.NewCompiler(env, 0) // 0 → defaultCacheSize 4096
	if err != nil {
		fmt.Fprintf(os.Stderr, "policy compile: cel.NewCompiler: %v\n", err)
		return 1
	}

	// Compile. policyID is the file path prefixed with "cli-compile-"
	// so log scrapers can filter on the literal substring and
	// distinguish CLI compiles from production gateway compiles.
	policyID := "cli-compile-" + path
	if _, err := compiler.CompileOrGet(policyID, string(content)); err != nil {
		// errors.Is(err, cel.ErrCompileFailed) is true for any CEL
		// parse/compile failure (Plan 01-05 sentinel). We surface the
		// FULL wrapped message so operators see the cel-go ERROR
		// pointer-string + the underlying cause.
		fmt.Fprintf(os.Stderr, "policy compile: %v\n", err)
		// Annotate with the sentinel hint if it's the expected
		// compile-failure path (vs a runtime build error from
		// env.Program — unlikely but logged distinctly).
		if errors.Is(err, cel.ErrCompileFailed) {
			fmt.Fprintln(os.Stderr,
				"hint: policy text did not pass cel.ErrCompileFailed gate; see runbooks/policy-author.md §5 for common errors.")
		}
		return 1
	}

	fmt.Printf("Policy %s compiles cleanly.\n", path)
	return 0
}
