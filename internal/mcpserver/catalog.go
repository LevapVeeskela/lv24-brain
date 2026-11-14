package mcpserver

// ToolSpec is the protocol metadata of one tool: everything tools/list reports
// plus the limits the catalogue documents. It never runs a tool.
type ToolSpec struct {
	Name         string         `json:"name"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	Annotations  Annotations    `json:"annotations"`
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`
	Limits       map[string]int `json:"limits,omitempty"`
}

// Catalog returns the metadata of every tool the server can expose, including
// the groups a particular backend would hide. It is what
// `brain mcp --list-tools --json` prints, so the contract can be inspected
// without a backend, a data directory or a running server. version is reported
// as serverInfo.version.
func Catalog(version string) []ToolSpec {
	s := &Server{version: version, catalog: true}
	s.tools = s.toolList()
	specs := make([]ToolSpec, 0, len(s.tools))
	for _, t := range s.tools {
		specs = append(specs, ToolSpec{
			Name:         t.Name,
			Title:        t.Annotations.Title,
			Description:  t.Description,
			Annotations:  t.Annotations,
			InputSchema:  t.InputSchema,
			OutputSchema: t.OutputSchema,
			Limits:       t.Limits,
		})
	}
	return specs
}
