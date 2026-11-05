---
project_id: {{ .ProjectID }}
kind: decision
updated_at: {{ .UpdatedAt }}
schema_version: {{ .SchemaVersion }}
permalink: {{ .Permalink }}
---

# {{ .Title }}

Status: {{ .Status }}

## Context

{{ .Context }}

## Alternatives

{{ range .Alternatives }}- {{ . }}
{{ else }}- None recorded.
{{ end }}
## Consequences

{{ range .Consequences }}- {{ . }}
{{ else }}- None recorded.
{{ end }}
## Evidence

{{ range .Evidence }}- {{ . }}
{{ else }}- None recorded.
{{ end }}
