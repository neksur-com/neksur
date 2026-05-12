// neksur-server — main backend binary entry point.
//
// Phase 0 stub. M1 wires up the REST API skeleton + Iceberg REST proxy
// foundation; M2 adds the MCP server + policy CRUD; M3 adds the pgwire
// SQL proxy + L1 Catalog Gateway full validation; M4 adds the Spark
// write-path integration. See docs/phase-0-stack.md §5 for the milestone
// breakdown, and §6 for the planned internal/ package layout this binary
// will compose.
package main

import "fmt"

func main() {
	fmt.Println("Neksur Server (placeholder — Phase 0 stub; M1 will wire up REST API, MCP server, SQL proxy).")
}
