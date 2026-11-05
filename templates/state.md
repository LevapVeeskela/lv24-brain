---
project_id: {{ .ProjectID }}
kind: state
updated_at: {{ .UpdatedAt }}
schema_version: {{ .SchemaVersion }}
permalink: {{ .Permalink }}
---

# {{ .ProjectName }} — State

## Goal

{{ .Goal }}

## Active

{{ range .Active }}- {{ . }}
{{ else }}- Nothing active.
{{ end }}
## Blocked

{{ range .Blocked }}- {{ . }}
{{ else }}- Nothing blocked.
{{ end }}
## Next action

{{ .NextAction }}

## Open questions

{{ range .OpenQuestions }}- {{ . }}
{{ else }}- None.
{{ end }}
