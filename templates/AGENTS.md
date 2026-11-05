# {{ .ProjectName }}

This project is tracked by LV24 Brain. Shared knowledge lives in the private
brain data repository; secrets never do.

## Working agreement

- Read `state.md` before starting work.
- Record durable choices with `brain decision`.
- Record failed approaches with `brain attempt` so they are not repeated.
- End a session with `brain handoff` including the next action.
- Run `brain doctor` before editing data after an engine update.
- Never write tokens, passwords, mail bodies, or machine-local paths into the
  brain data repository.

## Project

- ID: `{{ .ProjectID }}`
- Kind: `{{ .ProjectKind }}`
{{- if .RepoURL }}
- Repository: {{ .RepoURL }}
{{- end }}
