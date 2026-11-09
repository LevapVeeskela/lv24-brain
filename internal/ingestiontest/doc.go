// Package ingestiontest provides a reusable contract test suite for
// item-level sources (ports.Source).
//
// The suite drives a source through an in-memory host that follows the host
// contract from internal/ports/ingestion.go exactly: Read with the persisted
// checkpoints, discard the whole batch on error, validate every item (after
// WithHash) and persist it, and only then persist the batch checkpoints.
//
// A connector package plugs in by building a Harness around its own fake
// transport (fake HTTP server, scripted IMAP fakes, stub MCP client, ...).
// The fake must support adding an item, updating an item, deleting an item
// and adding N items; the harness exposes those mutations as closures so the
// suite can change the upstream between reads. Capability expectations are
// taken from the Source's own descriptor: a case is skipped with a clear
// message when the descriptor does not declare the capability it needs
// (for example Deletes).
//
// Example (inside the connector's own _test.go file):
//
//	func TestMySourceContract(t *testing.T) {
//		fake := newFakeUpstream() // the connector's own fake transport
//		conn := domain.Connection{ID: "my-conn", Provider: "myprovider", Account: "tester"}
//		bad := conn
//		bad.Account = "bad" // the fake rejects this credential
//		h := ingestiontest.Harness{
//			Connection: conn,
//			NewSource: func(c domain.Connection) ports.Source {
//				return NewMySource(c, fake)
//			},
//			AddItem:    func() { fake.AddDoc() },
//			UpdateItem: func() { fake.UpdateFirstDoc() },
//			DeleteItem: func() { fake.DeleteFirstDoc() },
//			AddMany:    func(n int) { fake.AddDocs(n) },
//			BadConnection: &bad, // nil when the fake has no bad-credential variant
//			PageSize:      4,    // Limit used for the paging and replay cases
//		}
//		ingestiontest.RunSourceSuite(t, h)
//	}
//
// The in-memory host (Host, MemoryItemStore, MemoryCheckpointStore, Budget,
// Drive) is a test double, not production code: it exists so connector tests
// need no database or filesystem.
package ingestiontest
