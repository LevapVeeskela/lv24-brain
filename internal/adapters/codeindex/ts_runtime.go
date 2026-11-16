package codeindex

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// tsWasmGzip is the pinned reactor described by wasm/PROVENANCE.md.
//
//go:embed wasm/ts.wasm.gz
var tsWasmGzip []byte

// The embedded reactor is a single WASI module built from pinned tree-sitter
// core and grammar sources. It is the only non-Go code the native indexer
// executes, and it is built ahead of time: no CGO, no compiler, no download at
// run time. wasm/PROVENANCE.md records the exact sources, licenses and build
// recipe.
const (
	wasmAssetPath = "wasm/ts.wasm.gz"

	// tsWasmRawBytes and tsWasmGzipBytes make a truncated or replaced asset
	// fail before a parser exists.
	tsWasmRawBytes  = 3851887
	tsWasmGzipBytes = 444436

	// tsWasmRawSHA256 and tsWasmGzipSHA256 pin the exact bytes. A rebuild of
	// the asset updates PROVENANCE.md and these two constants together.
	tsWasmRawSHA256  = "9b1adf861ecc25fb12bc1b0392b106ddbed37d88a292927dd0bd7b564edff76e"
	tsWasmGzipSHA256 = "03129d0835e23176c6a238cc577a41b28e2483708a6ec080c143c11dfea1561d"

	// tsNodeSize is the sizeof(TSNode) of tree-sitter 0.27: four 32-bit
	// fields (context[4] is 16 bytes) plus id and tree.
	tsNodeSize = 24
	// tsQueryMatchSize is sizeof(TSQueryMatch): id u32, pattern u16, count
	// u16, captures pointer u32.
	tsQueryMatchSize = 12
	// tsQueryCaptureSize is sizeof(TSQueryCapture): TSNode plus capture
	// index.
	tsQueryCaptureSize = 28
	// maxAncestorDepth bounds a parent walk over a malformed tree.
	maxAncestorDepth = 80
)

// tsEngine owns one wazero runtime, the instantiated reactor and one compiled
// query set per language. It is created lazily on the first polyglot write and
// closed with the index.
type tsEngine struct {
	mu        sync.Mutex
	ctx       context.Context
	rt        wazero.Runtime
	mod       api.Module
	languages map[string]*tsLanguage
	fields    map[string]uint32
	closed    bool
}

// tsLanguage is one grammar and its compiled extraction query. One query pass
// collects declarations, imports, calls and references; four separate queries
// would traverse the whole tree four times.
type tsLanguage struct {
	ptr uint64
	all *tsQuery
}

// tsQuery owns a compiled query and its cursor. Queries are not safe for
// concurrent use; tsEngine.mu serializes all calls.
type tsQuery struct {
	engine   *tsEngine
	query    uint64
	cursor   uint64
	matchBuf uint32
	names    map[uint32]string
}

// tsNode is a pointer to a 24-byte TSNode in wasm memory. Every node returned
// by a helper is malloc'ed and must be freed by the caller.
type tsNode uint32

// newTSEngine verifies the embedded asset and compiles every declared query.
// Any failure is fatal for the polyglot extractor: a partially working
// grammar would produce symbols the caller cannot trust.
func newTSEngine() (*tsEngine, error) {
	raw, err := decodeTSWasm()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	engine := &tsEngine{ctx: ctx, rt: runtime, languages: map[string]*tsLanguage{}, fields: map[string]uint32{}}
	fail := func(err error) (*tsEngine, error) {
		_ = runtime.Close(ctx)
		return nil, err
	}
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, runtime); err != nil {
		return fail(fmt.Errorf("codeindex: start the tree-sitter wasm runtime: %w", err))
	}
	compiled, err := runtime.CompileModule(ctx, raw)
	if err != nil {
		return fail(fmt.Errorf("codeindex: compile %s: %w", wasmAssetPath, err))
	}
	module, err := runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig())
	if err != nil {
		return fail(fmt.Errorf("codeindex: instantiate %s: %w", wasmAssetPath, err))
	}
	engine.mod = module
	if init := module.ExportedFunction("_initialize"); init != nil {
		if _, err := init.Call(ctx); err != nil {
			return fail(fmt.Errorf("codeindex: initialize %s: %w", wasmAssetPath, err))
		}
	}
	for _, spec := range polyglotSpecs {
		if _, ok := engine.languages[spec.Language]; ok {
			continue
		}
		language, err := engine.loadLanguage(spec)
		if err != nil {
			return fail(err)
		}
		engine.languages[spec.Language] = language
	}
	return engine, nil
}

// decodeTSWasm decompresses and verifies the embedded reactor.
func decodeTSWasm() ([]byte, error) {
	gzipSum := sha256.Sum256(tsWasmGzip)
	if hex.EncodeToString(gzipSum[:]) != tsWasmGzipSHA256 {
		return nil, fmt.Errorf("codeindex: %s checksum mismatch; the embedded asset is not the pinned build", wasmAssetPath)
	}
	if len(tsWasmGzip) != tsWasmGzipBytes {
		return nil, fmt.Errorf("codeindex: %s is %d bytes, want %d", wasmAssetPath, len(tsWasmGzip), tsWasmGzipBytes)
	}
	reader, err := gzip.NewReader(bytes.NewReader(tsWasmGzip))
	if err != nil {
		return nil, fmt.Errorf("codeindex: decompress %s: %w", wasmAssetPath, err)
	}
	defer func() { _ = reader.Close() }()
	raw, err := io.ReadAll(io.LimitReader(reader, tsWasmRawBytes+1))
	if err != nil {
		return nil, fmt.Errorf("codeindex: decompress %s: %w", wasmAssetPath, err)
	}
	if len(raw) != tsWasmRawBytes {
		return nil, fmt.Errorf("codeindex: %s decompresses to %d bytes, want %d", wasmAssetPath, len(raw), tsWasmRawBytes)
	}
	rawSum := sha256.Sum256(raw)
	if hex.EncodeToString(rawSum[:]) != tsWasmRawSHA256 {
		return nil, fmt.Errorf("codeindex: %s content checksum mismatch", wasmAssetPath)
	}
	return raw, nil
}

// loadLanguage resolves one grammar and compiles its four queries.
func (e *tsEngine) loadLanguage(spec polyglotSpec) (*tsLanguage, error) {
	pointer, err := e.call(spec.Grammar)
	if err != nil {
		return nil, fmt.Errorf("codeindex: resolve grammar %s: %w", spec.Grammar, err)
	}
	if pointer == 0 {
		return nil, fmt.Errorf("codeindex: grammar %s is missing from %s", spec.Grammar, wasmAssetPath)
	}
	abi, err := e.call("ts_language_abi_version", pointer)
	if err != nil {
		return nil, fmt.Errorf("codeindex: read %s ABI: %w", spec.Grammar, err)
	}
	if uint32(abi) != spec.ABI {
		return nil, fmt.Errorf("codeindex: grammar %s reports ABI %d, the extractor was written for %d", spec.Grammar, abi, spec.ABI)
	}
	language := &tsLanguage{ptr: pointer}
	combined := strings.Join([]string{spec.Declarations, spec.Imports, spec.Calls, spec.References}, "\n")
	compiled, err := e.newQuery(pointer, combined)
	if err != nil {
		return nil, fmt.Errorf("codeindex: compile the %s extraction query: %w", spec.Language, err)
	}
	language.all = compiled
	return language, nil
}

// close releases the runtime and every module allocation.
func (e *tsEngine) close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.rt != nil {
		return e.rt.Close(e.ctx)
	}
	return nil
}

// call invokes one wasm export. An export with no result yields zero.
func (e *tsEngine) call(name string, args ...uint64) (uint64, error) {
	function := e.mod.ExportedFunction(name)
	if function == nil {
		return 0, fmt.Errorf("codeindex: %s does not export %s", wasmAssetPath, name)
	}
	out, err := function.Call(e.ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if len(out) == 0 {
		return 0, nil
	}
	return out[0], nil
}

// malloc allocates memory in the wasm heap.
func (e *tsEngine) malloc(size uint32) (uint32, error) {
	pointer, err := e.call("malloc", uint64(size))
	if err != nil {
		return 0, err
	}
	if pointer == 0 && size > 0 {
		return 0, fmt.Errorf("codeindex: tree-sitter wasm is out of memory for %d bytes", size)
	}
	return uint32(pointer), nil
}

// free releases one wasm allocation.
func (e *tsEngine) free(pointer uint32) {
	if pointer != 0 {
		_, _ = e.call("free", uint64(pointer))
	}
}

// write copies bytes into wasm memory.
func (e *tsEngine) write(pointer uint32, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if !e.mod.Memory().Write(pointer, data) {
		return fmt.Errorf("codeindex: write %d bytes outside the wasm heap at %d", len(data), pointer)
	}
	return nil
}

// cstr reads a NUL-terminated string from wasm memory.
func (e *tsEngine) cstr(pointer uint32) (string, error) {
	if pointer == 0 {
		return "", nil
	}
	const chunk = 64
	var out strings.Builder
	for offset := uint32(0); offset < 1<<20; offset += chunk {
		data, ok := e.mod.Memory().Read(pointer+offset, chunk)
		if !ok {
			return "", fmt.Errorf("codeindex: read a string outside the wasm heap at %d", pointer+offset)
		}
		if index := bytes.IndexByte(data, 0); index >= 0 {
			out.Write(data[:index])
			return out.String(), nil
		}
		out.Write(data)
	}
	return "", fmt.Errorf("codeindex: string at %d is not terminated", pointer)
}

// field returns the cached wasm pointer of a field name.
func (e *tsEngine) field(name string) (uint32, uint32, error) {
	if pointer, ok := e.fields[name]; ok {
		return pointer, uint32(len(name)), nil
	}
	pointer, err := e.malloc(uint32(len(name)))
	if err != nil {
		return 0, 0, err
	}
	if err := e.write(pointer, []byte(name)); err != nil {
		e.free(pointer)
		return 0, 0, err
	}
	e.fields[name] = pointer
	return pointer, uint32(len(name)), nil
}

// newNode allocates one TSNode buffer.
func (e *tsEngine) newNode() (tsNode, error) {
	pointer, err := e.malloc(tsNodeSize)
	return tsNode(pointer), err
}

// newNodeFilled reads the parent of node into a fresh buffer.
func (e *tsEngine) parent(node tsNode) (tsNode, error) {
	out, err := e.newNode()
	if err != nil {
		return 0, err
	}
	if _, err := e.call("ts_node_parent", uint64(out), uint64(node)); err != nil {
		e.free(uint32(out))
		return 0, err
	}
	return out, nil
}

// childByField reads one named field into a fresh buffer.
func (e *tsEngine) childByField(node tsNode, name string) (tsNode, error) {
	out, err := e.newNode()
	if err != nil {
		return 0, err
	}
	pointer, length, err := e.field(name)
	if err != nil {
		e.free(uint32(out))
		return 0, err
	}
	if _, err := e.call("ts_node_child_by_field_name", uint64(out), uint64(node), uint64(pointer), uint64(length)); err != nil {
		e.free(uint32(out))
		return 0, err
	}
	return out, nil
}

// namedChild reads one named child into a fresh buffer.
func (e *tsEngine) namedChild(node tsNode, index uint32) (tsNode, error) {
	out, err := e.newNode()
	if err != nil {
		return 0, err
	}
	if _, err := e.call("ts_node_named_child", uint64(out), uint64(node), uint64(index)); err != nil {
		e.free(uint32(out))
		return 0, err
	}
	return out, nil
}

// zero reports whether a node buffer holds the null TSNode.
func (e *tsEngine) zero(node tsNode) bool {
	data, ok := e.mod.Memory().Read(uint32(node), tsNodeSize)
	if !ok {
		return true
	}
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

// nodeType reads the grammar node type.
func (e *tsEngine) nodeType(node tsNode) (string, error) {
	pointer, err := e.call("ts_node_type", uint64(node))
	if err != nil {
		return "", err
	}
	return e.cstr(uint32(pointer))
}

// byteRange reads the half-open byte range of a node.
func (e *tsEngine) byteRange(node tsNode) (uint32, uint32, error) {
	start, err := e.call("ts_node_start_byte", uint64(node))
	if err != nil {
		return 0, 0, err
	}
	end, err := e.call("ts_node_end_byte", uint64(node))
	if err != nil {
		return 0, 0, err
	}
	return uint32(start), uint32(end), err
}

// hasError reports whether node or a descendant contains a parse error.
func (e *tsEngine) hasError(node tsNode) bool {
	value, err := e.call("ts_node_has_error", uint64(node))
	return err == nil && value != 0
}

// isError reports whether node itself is an ERROR node.
func (e *tsEngine) isError(node tsNode) bool {
	value, err := e.call("ts_node_is_error", uint64(node))
	return err == nil && value != 0
}

// nodeText returns the source slice of a node, bounded by the source length.
func (e *tsEngine) nodeText(source []byte, node tsNode) string {
	start, end, err := e.byteRange(node)
	if err != nil || end > uint32(len(source)) || start > end {
		return ""
	}
	return string(source[start:end])
}

// newQuery compiles one query text and creates its cursor.
func (e *tsEngine) newQuery(language uint64, text string) (*tsQuery, error) {
	queryPtr, err := e.malloc(uint32(len(text)))
	if err != nil {
		return nil, err
	}
	defer e.free(queryPtr)
	if err := e.write(queryPtr, []byte(text)); err != nil {
		return nil, err
	}
	errorOffset, err := e.malloc(4)
	if err != nil {
		return nil, err
	}
	defer e.free(errorOffset)
	errorType, err := e.malloc(4)
	if err != nil {
		return nil, err
	}
	defer e.free(errorType)
	compiled, err := e.call("ts_query_new", language, uint64(queryPtr), uint64(len(text)), uint64(errorOffset), uint64(errorType))
	if err != nil {
		return nil, err
	}
	if compiled == 0 {
		offset, _ := e.mod.Memory().ReadUint32Le(errorOffset)
		kind, _ := e.mod.Memory().ReadUint32Le(errorType)
		return nil, fmt.Errorf("query error type=%d offset=%d near %q", kind, offset, snippetAt(text, int(offset), 60))
	}
	cursor, err := e.call("ts_query_cursor_new")
	if err != nil {
		_, _ = e.call("ts_query_delete", compiled)
		return nil, err
	}
	matchBuf, err := e.malloc(tsQueryMatchSize)
	if err != nil {
		_, _ = e.call("ts_query_cursor_delete", cursor)
		_, _ = e.call("ts_query_delete", compiled)
		return nil, err
	}
	return &tsQuery{engine: e, query: compiled, cursor: cursor, matchBuf: matchBuf, names: map[uint32]string{}}, nil
}

// snippetAt renders a bounded excerpt around a query error offset.
func snippetAt(text string, offset, length int) string {
	if offset < 0 || offset >= len(text) {
		return text
	}
	end := offset + length
	if end > len(text) {
		end = len(text)
	}
	return text[offset:end]
}

// exec starts a query over root.
func (q *tsQuery) exec(root uint64) error {
	_, err := q.engine.call("ts_query_cursor_exec", q.cursor, q.query, root)
	return err
}

// captureName resolves and caches one capture id.
func (q *tsQuery) captureName(id uint32) (string, error) {
	if name, ok := q.names[id]; ok {
		return name, nil
	}
	lengthPtr, err := q.engine.malloc(4)
	if err != nil {
		return "", err
	}
	defer q.engine.free(lengthPtr)
	pointer, err := q.engine.call("ts_query_capture_name_for_id", q.query, uint64(id), uint64(lengthPtr))
	if err != nil {
		return "", err
	}
	name, err := q.engine.cstr(uint32(pointer))
	if err != nil {
		return "", err
	}
	q.names[id] = name
	return name, nil
}

// next returns the next match's captures. The returned nodes are pointers to
// the cursor-owned capture array, valid until the next call.
func (q *tsQuery) next() ([]tsCapture, bool, error) {
	found, err := q.engine.call("ts_query_cursor_next_match", q.cursor, uint64(q.matchBuf))
	if err != nil || found == 0 {
		return nil, false, err
	}
	capturesPointer, ok := q.engine.mod.Memory().ReadUint32Le(q.matchBuf + 8)
	if !ok {
		return nil, false, fmt.Errorf("codeindex: query match captures outside wasm memory")
	}
	count, ok := q.engine.mod.Memory().ReadUint16Le(q.matchBuf + 6)
	if !ok {
		return nil, false, fmt.Errorf("codeindex: query match capture count outside wasm memory")
	}
	captures := make([]tsCapture, 0, count)
	for index := uint32(0); index < uint32(count); index++ {
		base := capturesPointer + index*tsQueryCaptureSize
		captureID, ok := q.engine.mod.Memory().ReadUint32Le(base + 24)
		if !ok {
			return nil, false, fmt.Errorf("codeindex: query capture outside wasm memory")
		}
		name, err := q.captureName(captureID)
		if err != nil {
			return nil, false, err
		}
		captures = append(captures, tsCapture{name: name, node: tsNode(base)})
	}
	return captures, true, nil
}

// tsParse is one parsed source and its reusable state.
type tsParse struct {
	engine     *tsEngine
	parser     uint64
	tree       uint64
	root       tsNode
	source     []byte
	lineStarts []int
	hasError   bool
}

// parse builds a tree for source. A tree with syntax errors is still returned:
// clean declarations are indexed and the file is marked partial.
func (e *tsEngine) parse(language *tsLanguage, source []byte) (*tsParse, error) {
	parser, err := e.call("ts_parser_new")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*tsParse, error) {
		_, _ = e.call("ts_parser_delete", parser)
		return nil, err
	}
	set, err := e.call("ts_parser_set_language", parser, language.ptr)
	if err != nil {
		return fail(err)
	}
	if set == 0 {
		return fail(fmt.Errorf("codeindex: the tree-sitter grammar rejected the parser"))
	}
	sourcePtr, err := e.malloc(uint32(len(source) + 1))
	if err != nil {
		return fail(err)
	}
	if err := e.write(sourcePtr, append(append([]byte(nil), source...), 0)); err != nil {
		e.free(sourcePtr)
		return fail(err)
	}
	tree, err := e.call("ts_parser_parse_string", parser, 0, uint64(sourcePtr), uint64(len(source)))
	e.free(sourcePtr)
	if err != nil {
		return fail(err)
	}
	if tree == 0 {
		return fail(fmt.Errorf("codeindex: parse returned no tree"))
	}
	root, err := e.newNode()
	if err != nil {
		_, _ = e.call("ts_tree_delete", tree)
		return fail(err)
	}
	if _, err := e.call("ts_tree_root_node", uint64(root), tree); err != nil {
		e.free(uint32(root))
		_, _ = e.call("ts_tree_delete", tree)
		return fail(err)
	}
	lineStarts := []int{0}
	for index, value := range source {
		if value == '\n' {
			lineStarts = append(lineStarts, index+1)
		}
	}
	return &tsParse{
		engine:     e,
		parser:     parser,
		tree:       tree,
		root:       root,
		source:     source,
		lineStarts: lineStarts,
		hasError:   e.hasError(root),
	}, nil
}

// close releases the parser, tree and root node.
func (p *tsParse) close() {
	p.engine.free(uint32(p.root))
	_, _ = p.engine.call("ts_tree_delete", p.tree)
	_, _ = p.engine.call("ts_parser_delete", p.parser)
}

// lineAt converts a byte offset to a 1-based line number.
func (p *tsParse) lineAt(offset uint32) int {
	index := sort.Search(len(p.lineStarts), func(index int) bool {
		return p.lineStarts[index] > int(offset)
	}) - 1
	if index < 0 {
		index = 0
	}
	return index + 1
}

// tsCapture is one named capture of a query match.
type tsCapture struct {
	name string
	node tsNode
}
