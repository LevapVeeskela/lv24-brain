# Embedded tree-sitter reactor — provenance

`ts.wasm.gz` is the only non-Go code the native indexer executes. It is a
standalone WASI reactor compiled ahead of time from the pinned sources below.
The indexer never downloads, compiles or links C at run time; `zig` is required
only to rebuild this asset during release engineering.

## Asset

| Property | Value |
| --- | --- |
| file | `internal/adapters/codeindex/wasm/ts.wasm.gz` |
| gzip size | 444,436 B |
| raw wasm size | 3,851,887 B |
| gzip SHA-256 | `03129d0835e23176c6a238cc577a41b28e2483708a6ec080c143c11dfea1561d` |
| raw SHA-256 | `9b1adf861ecc25fb12bc1b0392b106ddbed37d88a292927dd0bd7b564edff76e` |
| compression | gzip, BestCompression, header mtime 0, OS 255 |

`decodeTSWasm` in `ts_runtime.go` verifies both sizes and both checksums before
the runtime is created. The runtime also checks each grammar's ABI version
(TypeScript/TSX 14, JavaScript 15) and compiles every query.

## Sources

| Component | Tag | Commit | License |
| --- | --- | --- | --- |
| tree-sitter core | `v0.27.2` | `3fb7be49145acfb07e33b61ed5689e2b6771b2e9` | MIT |
| tree-sitter-typescript (`typescript/`) | `v0.23.2` | `f975a621f4e7f532fe322e13c4f79495e0a7b2e7` | MIT |
| tree-sitter-typescript (`tsx/`) | `v0.23.2` | same repository | MIT |
| tree-sitter-javascript | `v0.25.0` | `44c892e0be055ac465d5eeddae6d3e194424e7de` | MIT |
| tree-sitter-python (built into the reactor, not queried by the extractor) | `v0.25.0` | `293fdc02038ee2bf0e2e206711b69c90ac0d413f` | MIT |
| zig (build tool only) | `0.13.0` | windows-x86_64 distribution | MIT |
| wazero (runtime, not vendored here) | `v1.12.0` | Go module dependency | Apache-2.0 |

The reactor exports `tree_sitter_python` because the pinned spike build included
the Python grammar. The native extractor does not use it yet: Python extraction
stays pending until its queries and golden tests land. The extra grammar is
dead weight in the asset, not a claimed language.

## Build recipe

Run from a directory containing the clones `ts-core/`, `ts-typescript/` and
`ts-javascript/`, with `zig.exe` on `PATH` (or adapt the first line). This is a
build-time recipe; it is not part of `go build`.

```powershell
$ErrorActionPreference = 'Stop'
$zig = '.\zig\zig-windows-x86_64-0.13.0\zig.exe'
$exports = @(
  'malloc','free','strlen',
  'ts_parser_new','ts_parser_delete','ts_parser_set_language','ts_parser_parse_string',
  'ts_language_abi_version','ts_language_name','ts_tree_delete','ts_tree_root_node',
  'ts_node_type','ts_node_string','ts_node_child_count','ts_node_named_child_count',
  'ts_node_child','ts_node_named_child','ts_node_child_by_field_name',
  'ts_node_start_byte','ts_node_end_byte','ts_node_start_point','ts_node_is_named',
  'ts_node_is_error','ts_node_has_error','ts_node_parent',
  'ts_query_new','ts_query_delete','ts_query_capture_name_for_id','ts_query_capture_count',
  'ts_query_cursor_new','ts_query_cursor_delete','ts_query_cursor_exec','ts_query_cursor_next_match',
  'ts_query_cursor_next_capture',
  'tree_sitter_typescript','tree_sitter_tsx','tree_sitter_javascript','tree_sitter_python'
)
$argsList = @(
  'cc','--target=wasm32-wasi-musl','-mexec-model=reactor',
  '-I','ts-core/lib/include','-I','ts-core/lib/src',
  '-I','ts-typescript/typescript/src','-I','ts-typescript/tsx/src',
  '-I','ts-javascript/src','-I','ts-python/src',
  'ts-core/lib/src/lib.c',
  'ts-typescript/typescript/src/parser.c','ts-typescript/typescript/src/scanner.c',
  'ts-typescript/tsx/src/parser.c','ts-typescript/tsx/src/scanner.c',
  'ts-javascript/src/parser.c','ts-javascript/src/scanner.c',
  'ts-python/src/parser.c','ts-python/src/scanner.c',
  '-o','ts.wasm','-Oz','-fPIC','-Wl,--no-entry','-Wl,-z,stack-size=1048576','-Wl,--strip-debug'
)
foreach ($e in $exports) { $argsList += "-Wl,--export=$e" }
& $zig @argsList
if ($LASTEXITCODE -ne 0) { throw "zig cc failed: $LASTEXITCODE" }
```

The gzip step is Go's `compress/gzip` at `BestCompression` with `ModTime` set to
the Unix epoch and `OS` set to 255, which makes the asset byte-reproducible.

The only wasm imports are `wasi_snapshot_preview1.fd_write`, `fd_seek`,
`fd_close` and `random_get`; wazero instantiates it with no filesystem and no
preopens.

## Licenses

### tree-sitter core

The MIT License (MIT)

Copyright (c) 2018 Max Brunsfeld

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

### tree-sitter-typescript

The MIT License (MIT)

Copyright (c) 2017 Max Brunsfeld

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

### tree-sitter-javascript

The MIT License (MIT)

Copyright (c) 2014 Max Brunsfeld

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

### tree-sitter-python

The MIT License (MIT)

Copyright (c) 2016 Max Brunsfeld

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

### zig

MIT. See <https://github.com/ziglang/zig/blob/0.13.0/LICENSE>.
