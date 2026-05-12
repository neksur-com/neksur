// neksur-worker — L3 Detection Worker entry point.
//
// Phase 0 stub. M3 fills in the async worker that periodically scans
// catalog snapshots for post-commit policy violations (regex PII classifier
// + sampling per docs/phase-0-stack.md §2.1 row "L3 Detection Worker").
// Sits on top of internal/detection/ once that package lands.
package main

import "fmt"

func main() {
	fmt.Println("Neksur Worker (placeholder — Phase 0 stub; M3 will wire up the L3 Detection scanner).")
}
