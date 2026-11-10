package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
)

// completionFlag is a flag offered by shell completion. takes marks a flag that
// consumes the next word; values, when set, is the fixed value set it offers.
type completionFlag struct {
	name   string
	takes  bool
	values []string
}

// completionPos describes one positional argument that offers fixed values.
// index counts positional arguments after the command path, starting at 1.
type completionPos struct {
	index  int
	label  string
	values []string
}

// completionCmd is a node of completionTable. A top-level command with subs is
// a group; every other command is a leaf that carries its own flags.
type completionCmd struct {
	name  string
	flags []completionFlag
	subs  []completionCmd
	pos   *completionPos
}

// completionLeaf is an executable command path and its flags.
type completionLeaf struct {
	path  []string
	flags []completionFlag
	pos   *completionPos
}

var (
	completionStatuses    = []string{"proposed", "accepted", "rejected", "superseded"}
	completionRecordKinds = []string{"note", "decision", "attempt", "handoff"}
)

func boolFlag(name string) completionFlag { return completionFlag{name: name} }

func valueFlag(name string) completionFlag { return completionFlag{name: name, takes: true} }

func choiceFlag(name string, values ...string) completionFlag {
	return completionFlag{name: name, takes: true, values: values}
}

func cmdLeaf(name string, flags ...completionFlag) completionCmd {
	return completionCmd{name: name, flags: flags}
}

func cmdGroup(name string, subs ...completionCmd) completionCmd {
	return completionCmd{name: name, subs: subs}
}

func (c completionCmd) withPos(index int, label string, values ...string) completionCmd {
	c.pos = &completionPos{index: index, label: label, values: values}
	return c
}

// completionTable is the single source of truth for every shell script. It is
// derived from the dispatch switch and printUsage in main.go and from the flag
// definitions of each run* function; TestCompletionTableMatchesHelp keeps it
// in step with the --help output of every command.
var completionTable = []completionCmd{
	cmdLeaf("init", choiceFlag("mode", "local", "git", "server"), valueFlag("data-dir"), valueFlag("server-url"), valueFlag("config")),
	cmdLeaf("doctor", boolFlag("json"), valueFlag("config")),
	cmdLeaf("open", valueFlag("project"), valueFlag("budget"), boolFlag("brief"), boolFlag("json"), valueFlag("config")),
	cmdGroup("project",
		cmdLeaf("list", boolFlag("json"), valueFlag("config")),
		cmdLeaf("show", boolFlag("json"), valueFlag("config")),
		cmdLeaf("set", valueFlag("name"), choiceFlag("kind", "app", "service", "repository", "infra", "other"),
			valueFlag("repo"), valueFlag("branch"), boolFlag("clear-paths"), boolFlag("clear-aliases"),
			valueFlag("path"), valueFlag("add-path"), valueFlag("alias"), boolFlag("json"), valueFlag("config")),
		cmdLeaf("members", choiceFlag("role", "viewer", "member", "maintainer"), boolFlag("json"), valueFlag("config")),
	),
	cmdLeaf("bootstrap", valueFlag("project"), valueFlag("budget"), boolFlag("json"), valueFlag("config")),
	cmdLeaf("context", valueFlag("project"), valueFlag("budget"), choiceFlag("kind", "decision", "attempt", "note"),
		valueFlag("since"), boolFlag("json"), valueFlag("config")),
	cmdLeaf("note", valueFlag("project"), valueFlag("title"), valueFlag("body"), valueFlag("tags"), boolFlag("create"), valueFlag("config")),
	cmdLeaf("decision", valueFlag("project"), valueFlag("title"), choiceFlag("status", completionStatuses...),
		valueFlag("context"), valueFlag("alternatives"), valueFlag("consequences"), valueFlag("evidence"), valueFlag("paths"), boolFlag("create"), valueFlag("config")),
	cmdLeaf("attempt", valueFlag("project"), valueFlag("title"), valueFlag("hypothesis"), valueFlag("result"), valueFlag("reason"), boolFlag("create"), valueFlag("config")),
	cmdLeaf("handoff", valueFlag("project"), valueFlag("summary"), valueFlag("next"), valueFlag("git-context"), boolFlag("no-git"), boolFlag("create"), valueFlag("config")),
	cmdLeaf("status", boolFlag("json"), valueFlag("config")),
	cmdLeaf("review", valueFlag("project"), valueFlag("since"), boolFlag("fix"), valueFlag("fix-kind"), boolFlag("json"), valueFlag("config")),
	cmdLeaf("promote", valueFlag("project"), choiceFlag("as", "decision", "attempt", "note"),
		choiceFlag("status", "proposed", "accepted"), valueFlag("title"), boolFlag("dismiss"), boolFlag("force"),
		boolFlag("yes"), boolFlag("dry-run"), boolFlag("json"), valueFlag("config")),
	cmdLeaf("sync", boolFlag("status"), boolFlag("push"), valueFlag("config")),
	cmdLeaf("index", boolFlag("rebuild"), valueFlag("config")),
	cmdLeaf("search", valueFlag("project"), valueFlag("limit"), choiceFlag("kind", "note", "decision", "attempt", "handoff", "dump", "item"),
		boolFlag("dumps"), valueFlag("since"), boolFlag("json"), valueFlag("config")),
	cmdLeaf("journal", valueFlag("project"), valueFlag("user"), valueFlag("event"), valueFlag("since"), valueFlag("limit"),
		boolFlag("json"), boolFlag("repair"), boolFlag("dry-run"), valueFlag("days"), valueFlag("config")),
	cmdGroup("event",
		cmdLeaf("commit", valueFlag("project"), valueFlag("repo"), boolFlag("quiet"), valueFlag("config")),
	),
	cmdGroup("hooks",
		cmdLeaf("install", valueFlag("repo"), boolFlag("force"), valueFlag("config")),
		cmdLeaf("uninstall", valueFlag("repo")),
		cmdLeaf("status", valueFlag("repo")),
	),
	cmdGroup("config", cmdLeaf("use"), cmdLeaf("show"), cmdLeaf("clear")),
	cmdLeaf("discover", valueFlag("root"), boolFlag("apply"), valueFlag("exclude"), valueFlag("config")),
	cmdLeaf("graphs", boolFlag("changed-only"), boolFlag("init"), boolFlag("dry-run"), valueFlag("timeout"), valueFlag("config")),
	cmdGroup("agents",
		cmdLeaf("sync", boolFlag("check"), valueFlag("source"), valueFlag("root"), valueFlag("home"), boolFlag("hooks"), valueFlag("config")),
	),
	cmdGroup("state",
		cmdLeaf("get", valueFlag("project"), boolFlag("json"), valueFlag("config")),
		cmdLeaf("set", valueFlag("project"), valueFlag("goal"), valueFlag("next"), valueFlag("fresh-for"),
			boolFlag("clear-active"), boolFlag("clear-blocked"), boolFlag("clear-questions"), boolFlag("json"),
			valueFlag("active"), valueFlag("blocked"), valueFlag("question"), valueFlag("config")),
	),
	cmdLeaf("catchup", valueFlag("since"), valueFlag("limit"), boolFlag("json"), valueFlag("config")),
	cmdLeaf("digest", valueFlag("since"), valueFlag("project"), valueFlag("source"), boolFlag("all"),
		valueFlag("limit"), valueFlag("rule"), boolFlag("json"), boolFlag("write"), boolFlag("dry-run"), valueFlag("config")),
	cmdGroup("record",
		cmdLeaf("list", valueFlag("project"), choiceFlag("kind", completionRecordKinds...), valueFlag("since"),
			choiceFlag("status", completionStatuses...), valueFlag("limit"), boolFlag("json"), valueFlag("config")),
		cmdLeaf("show", valueFlag("project"), boolFlag("json"), valueFlag("config")),
		cmdLeaf("status", valueFlag("project"), valueFlag("config")).withPos(2, "status", completionStatuses...),
		cmdLeaf("supersede", valueFlag("project"), valueFlag("by"), valueFlag("config")),
		cmdLeaf("trash", valueFlag("project"), boolFlag("yes"), valueFlag("config")),
		cmdLeaf("restore", valueFlag("project"), valueFlag("config")),
		cmdLeaf("trash-list", valueFlag("project"), boolFlag("json"), valueFlag("config")),
	),
	cmdLeaf("links", valueFlag("project"), boolFlag("broken"), boolFlag("json"), valueFlag("config")),
	cmdGroup("session",
		cmdLeaf("start", choiceFlag("hook", "claude", "codex", "opencode"), valueFlag("cwd"), valueFlag("config")),
		cmdLeaf("end", choiceFlag("hook", "claude", "codex", "opencode"), valueFlag("cwd"), valueFlag("config")),
	),
	cmdLeaf("mcp", valueFlag("config")),
	cmdGroup("connector",
		cmdLeaf("run", choiceFlag("name", connectorbuild.SnapshotProviders()...), valueFlag("project"), valueFlag("brain-project"), valueFlag("account"),
			boolFlag("all"), valueFlag("days"), valueFlag("since"), valueFlag("limit"), valueFlag("config")),
	),
	cmdGroup("connection",
		cmdLeaf("run", valueFlag("since"), valueFlag("days"), valueFlag("limit"), boolFlag("json"), valueFlag("config")),
		cmdLeaf("list", boolFlag("json"), valueFlag("config")),
		cmdLeaf("add", valueFlag("id"), valueFlag("account"), valueFlag("base-url"), valueFlag("project"), valueFlag("schedule"),
			choiceFlag("publish", "local-only", "shared"), choiceFlag("setting", connectionSettingChoices()...), boolFlag("json"), valueFlag("config")).withPos(1, "provider", connectorbuild.Builtin().Providers()...),
		cmdGroup("secret", cmdLeaf("set", valueFlag("config")).withPos(2, "field", connectionSecretFields()...)),
		cmdLeaf("remove", boolFlag("yes"), boolFlag("json"), valueFlag("config")),
		cmdLeaf("migrate", boolFlag("copy-legacy-secrets"), boolFlag("json"), valueFlag("config")),
		cmdLeaf("providers", boolFlag("json"), valueFlag("config")),
		cmdLeaf("auth", valueFlag("client-id"), valueFlag("scopes"), boolFlag("remove"), valueFlag("config")),
	),
	cmdLeaf("migrate", valueFlag("from"), valueFlag("data-dir"), boolFlag("dry-run"), valueFlag("rollback"), valueFlag("config")),
	cmdGroup("agent",
		cmdLeaf("run", valueFlag("parent-pid"), valueFlag("config")),
		cmdLeaf("status", boolFlag("json"), valueFlag("config")),
		cmdLeaf("stop", valueFlag("config")),
	),
	cmdGroup("secret", cmdLeaf("set"), cmdLeaf("list"), cmdLeaf("remove")),
	cmdLeaf("login", valueFlag("login"), boolFlag("password-stdin"), valueFlag("config")),
	cmdLeaf("logout", boolFlag("all"), valueFlag("config")),
	cmdLeaf("password", valueFlag("config")),
	cmdGroup("token",
		cmdLeaf("list", boolFlag("json"), valueFlag("config")),
		cmdLeaf("create", valueFlag("name"), choiceFlag("role", "admin", "member", "viewer"),
			choiceFlag("scope", "read", "write", "admin"), valueFlag("project"), valueFlag("expires"),
			valueFlag("expires-in-days"), boolFlag("json"), valueFlag("config")),
		cmdLeaf("revoke", valueFlag("config")),
	),
	cmdLeaf("serve", valueFlag("addr"), valueFlag("config")),
	cmdLeaf("update", boolFlag("check"), boolFlag("yes"), boolFlag("rollback"), choiceFlag("channel", "stable", "beta"), valueFlag("config")),
	cmdGroup("completion", cmdLeaf("bash"), cmdLeaf("zsh"), cmdLeaf("fish"), cmdLeaf("powershell")),
	cmdLeaf("help"),
	cmdLeaf("version"),
}

// completionLeaves flattens completionTable into executable command paths.
func completionLeaves() []completionLeaf {
	var leaves []completionLeaf
	var walk func([]string, []completionCmd)
	walk = func(parent []string, commands []completionCmd) {
		for _, c := range commands {
			path := append(append([]string{}, parent...), c.name)
			if len(c.subs) == 0 {
				leaves = append(leaves, completionLeaf{path: path, flags: c.flags, pos: c.pos})
				continue
			}
			walk(path, c.subs)
		}
	}
	walk(nil, completionTable)
	return leaves
}

type completionGroup struct {
	path []string
	subs []completionCmd
}

func completionGroups() []completionGroup {
	var groups []completionGroup
	var walk func([]string, []completionCmd)
	walk = func(parent []string, commands []completionCmd) {
		for _, command := range commands {
			if len(command.subs) == 0 {
				continue
			}
			path := append(append([]string{}, parent...), command.name)
			groups = append(groups, completionGroup{path, command.subs})
			walk(path, command.subs)
		}
	}
	walk(nil, completionTable)
	return groups
}

func connectionSettingChoices() []string {
	var choices []string
	for _, descriptor := range connectorbuild.Builtin().Descriptors() {
		for _, field := range descriptor.ConfigFields {
			if !field.Secret {
				choices = append(choices, field.Key+"=")
			}
		}
	}
	return choices
}

func connectionSecretFields() []string {
	var choices []string
	for _, descriptor := range connectorbuild.Builtin().Descriptors() {
		for _, field := range descriptor.ConfigFields {
			if field.Secret && !containsString(choices, field.Key) {
				choices = append(choices, field.Key)
			}
		}
	}
	return choices
}

func (l completionLeaf) key() string { return strings.Join(l.path, " ") }

func topLevelNames() []string {
	names := make([]string, 0, len(completionTable))
	for _, c := range completionTable {
		names = append(names, c.name)
	}
	return names
}

func subNames(c completionCmd) []string {
	names := make([]string, 0, len(c.subs))
	for _, s := range c.subs {
		names = append(names, s.name)
	}
	return names
}

// flagWords returns every flag of a leaf in --name form.
func flagWords(flags []completionFlag) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		out = append(out, "--"+f.name)
	}
	return out
}

// valueFlagWords returns the flags that consume the next word.
func valueFlagWords(flags []completionFlag) []string {
	var out []string
	for _, f := range flags {
		if f.takes {
			out = append(out, "--"+f.name)
		}
	}
	return out
}

// plainValueFlagWords returns value-taking flags without a fixed value set.
func plainValueFlagWords(flags []completionFlag) []string {
	var out []string
	for _, f := range flags {
		if f.takes && len(f.values) == 0 {
			out = append(out, "--"+f.name)
		}
	}
	return out
}

// scriptWriter accumulates an indented script, four spaces per level.
type scriptWriter struct {
	b strings.Builder
}

func (w *scriptWriter) line(indent int, s string) {
	w.b.WriteString(strings.Repeat("    ", indent))
	w.b.WriteString(s)
	w.b.WriteByte('\n')
}

func (w *scriptWriter) String() string { return w.b.String() }

func renderBash() string {
	w := &scriptWriter{}
	w.line(0, "# bash completion for brain. Generated by `brain completion bash`.")
	w.line(0, "# Install: brain completion bash > ~/.local/share/bash-completion/completions/brain")
	w.line(0, "_brain_complete() {")
	w.line(1, "local cur prev key n i word")
	w.line(1, "COMPREPLY=()")
	w.line(1, `cur="${COMP_WORDS[COMP_CWORD]}"`)
	w.line(1, `prev="${COMP_WORDS[COMP_CWORD-1]}"`)
	w.line(1, `if [ "$COMP_CWORD" -eq 1 ]; then`)
	w.line(2, `COMPREPLY=( $(compgen -W "`+strings.Join(topLevelNames(), " ")+`" -- "$cur") )`)
	w.line(2, "return 0")
	w.line(1, "fi")
	w.line(1, `key="${COMP_WORDS[1]}"`)
	for _, group := range completionGroups() {
		w.line(1, `if [ "$key" = "`+strings.Join(group.path, " ")+`" ]; then`)
		w.line(2, fmt.Sprintf(`if [ "$COMP_CWORD" -eq %d ]; then`, len(group.path)+1))
		w.line(3, `COMPREPLY=( $(compgen -W "`+strings.Join(subNames(completionCmd{subs: group.subs}), " ")+`" -- "$cur") )`)
		w.line(3, "return 0")
		w.line(2, "fi")
		w.line(2, fmt.Sprintf(`key="$key ${COMP_WORDS[%d]}"`, len(group.path)+1))
		w.line(1, "fi")
	}
	w.line(1, `case "$key" in`)
	for _, l := range completionLeaves() {
		if len(l.flags) == 0 && l.pos == nil {
			continue
		}
		w.line(2, `"`+l.key()+`")`)
		w.line(3, `case "$prev" in`)
		for _, f := range l.flags {
			if len(f.values) > 0 {
				w.line(4, "--"+f.name+`) COMPREPLY=( $(compgen -W "`+strings.Join(f.values, " ")+`" -- "$cur") ); return 0 ;;`)
			}
		}
		if plain := plainValueFlagWords(l.flags); len(plain) > 0 {
			w.line(4, strings.Join(plain, "|")+") return 0 ;;")
		}
		w.line(3, "esac")
		if l.pos != nil {
			w.line(3, `if [[ "$cur" != -* ]]; then`)
			w.line(4, "n=0")
			w.line(4, fmt.Sprintf("i=%d", len(l.path)+1))
			w.line(4, `while [ "$i" -lt "$COMP_CWORD" ]; do`)
			w.line(5, `word="${COMP_WORDS[i]}"`)
			w.line(5, `case "$word" in`)
			if takes := valueFlagWords(l.flags); len(takes) > 0 {
				w.line(6, strings.Join(takes, "|")+") i=$((i+2)); continue ;;")
			}
			w.line(6, "-*) ;;")
			w.line(6, "*) n=$((n+1)) ;;")
			w.line(5, "esac")
			w.line(5, "i=$((i+1))")
			w.line(4, "done")
			w.line(4, fmt.Sprintf(`if [ "$n" -eq %d ]; then`, l.pos.index-1))
			w.line(5, `COMPREPLY=( $(compgen -W "`+strings.Join(l.pos.values, " ")+`" -- "$cur") )`)
			w.line(5, "return 0")
			w.line(4, "fi")
			w.line(3, "fi")
		}
		w.line(3, `COMPREPLY=( $(compgen -W "`+strings.Join(flagWords(l.flags), " ")+`" -- "$cur") )`)
		w.line(3, "return 0")
		w.line(2, ";;")
	}
	w.line(1, "esac")
	w.line(1, "return 0")
	w.line(0, "}")
	w.line(0, "complete -F _brain_complete brain")
	return w.String()
}

func renderZsh() string {
	w := &scriptWriter{}
	w.line(0, "#compdef brain")
	w.line(0, "# zsh completion for brain. Generated by `brain completion zsh`.")
	w.line(0, "# Save as _brain in a directory on $fpath (for example ~/.zfunc), then start a new shell.")
	w.line(0, "_brain() {")
	w.line(1, "local key")
	w.line(1, "if (( CURRENT == 2 )); then")
	w.line(2, "_values 'brain command' "+strings.Join(topLevelNames(), " "))
	w.line(2, "return")
	w.line(1, "fi")
	w.line(1, "key=$words[2]")
	for _, group := range completionGroups() {
		path := strings.Join(group.path, " ")
		w.line(1, "if [[ $key == '"+path+"' ]]; then")
		w.line(2, fmt.Sprintf("if (( CURRENT == %d )); then", len(group.path)+2))
		w.line(3, "_values '"+path+" subcommand' "+strings.Join(subNames(completionCmd{subs: group.subs}), " "))
		w.line(3, "return")
		w.line(2, "fi")
		w.line(2, fmt.Sprintf(`key="$key $words[%d]"`, len(group.path)+2))
		w.line(1, "fi")
	}
	w.line(1, "case $key in")
	for _, l := range completionLeaves() {
		if len(l.flags) == 0 && l.pos == nil {
			continue
		}
		var specs []string
		for _, f := range l.flags {
			switch {
			case len(f.values) > 0:
				specs = append(specs, "'--"+f.name+"=:"+f.name+":("+strings.Join(f.values, " ")+")'")
			case f.takes:
				specs = append(specs, "'--"+f.name+"=:"+f.name+":'")
			default:
				specs = append(specs, "'--"+f.name+"'")
			}
		}
		if l.pos != nil {
			// _arguments sees the full words array, including the command path.
			for i := 1; i < len(l.path)+l.pos.index; i++ {
				specs = append(specs, fmt.Sprintf("'%d:argument:'", i))
			}
			specs = append(specs, fmt.Sprintf("'%d:%s:(%s)'", len(l.path)+l.pos.index, l.pos.label, strings.Join(l.pos.values, " ")))
		}
		w.line(2, "'"+l.key()+"')")
		w.line(3, "_arguments \\")
		for i, spec := range specs {
			end := " \\"
			if i == len(specs)-1 {
				end = " ;;"
			}
			w.line(4, spec+end)
		}
	}
	w.line(1, "esac")
	w.line(0, "}")
	w.line(0, `if [[ $zsh_eval_context[-1] == loadautofunc ]]; then`)
	w.line(1, `_brain "$@"`)
	w.line(0, "else")
	w.line(1, "compdef _brain brain")
	w.line(0, "fi")
	return w.String()
}

func renderFish() string {
	w := &scriptWriter{}
	w.line(0, "# fish completion for brain. Generated by `brain completion fish`.")
	w.line(0, "# Install: brain completion fish > ~/.config/fish/completions/brain.fish")
	w.line(0, "complete -c brain -f")
	w.line(0, "function __brain_at")
	w.line(1, "set -l w (commandline -opc)")
	w.line(1, "test (count $w) -ge 1; and set -e w[1]")
	w.line(1, "set -l n (count $argv)")
	w.line(1, "test (count $w) -ge $n; or return 1")
	w.line(1, `test "$w[1..$n]" = "$argv"`)
	w.line(0, "end")
	w.line(0, "function __brain_sub")
	w.line(1, "set -l w (commandline -opc)")
	w.line(1, `test (count $w) -eq (math (count $argv) + 1); and __brain_at $argv`)
	w.line(0, "end")
	w.line(0, "function __brain_nwords")
	w.line(1, "set -l w (commandline -opc)")
	w.line(1, "math (count $w) - 1")
	w.line(0, "end")
	w.line(0, "complete -c brain -n 'test (__brain_nwords) -eq 0' -a '"+strings.Join(topLevelNames(), " ")+"'")
	for _, group := range completionGroups() {
		w.line(0, "complete -c brain -n '__brain_sub "+strings.Join(group.path, " ")+"' -a '"+strings.Join(subNames(completionCmd{subs: group.subs}), " ")+"'")
	}
	for _, l := range completionLeaves() {
		if l.pos != nil {
			fn := "__brain_pos_" + strings.ReplaceAll(l.key(), " ", "_")
			w.line(0, "function "+fn)
			w.line(1, "set -l w (commandline -opc)")
			w.line(1, "set -l n 0")
			w.line(1, fmt.Sprintf("set -l i %d", len(l.path)+2))
			w.line(1, "while test $i -le (count $w)")
			w.line(2, "switch $w[$i]")
			if takes := valueFlagWords(l.flags); len(takes) > 0 {
				w.line(3, "case '"+strings.Join(takes, "' '")+"'")
				w.line(4, "set i (math $i + 1)")
			}
			w.line(3, "case '-*'")
			w.line(4, "true")
			w.line(3, "case '*'")
			w.line(4, "set n (math $n + 1)")
			w.line(2, "end")
			w.line(2, "set i (math $i + 1)")
			w.line(1, "end")
			w.line(1, "echo $n")
			w.line(0, "end")
			w.line(0, fmt.Sprintf("complete -c brain -n '__brain_at %s; and test (%s) -eq %d' -a '%s'",
				l.key(), fn, l.pos.index-1, strings.Join(l.pos.values, " ")))
		}
		for _, f := range l.flags {
			line := "complete -c brain -n '__brain_at " + l.key() + "' -l " + f.name
			if f.takes {
				line += " -x"
			}
			if len(f.values) > 0 {
				line += " -a '" + strings.Join(f.values, " ") + "'"
			}
			w.line(0, line)
		}
	}
	return w.String()
}

// psQuoteList renders words as a PowerShell array body: 'a', 'b'.
func psQuoteList(words []string) string {
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, "'"+word+"'")
	}
	return strings.Join(quoted, ", ")
}

func renderPowerShell() string {
	w := &scriptWriter{}
	w.line(0, "# PowerShell completion for brain. Generated by `brain completion powershell`.")
	w.line(0, "# Install: add this line to $PROFILE:  brain completion powershell | Out-String | Invoke-Expression")
	w.line(0, "Register-ArgumentCompleter -Native -CommandName brain,brain.exe -ScriptBlock {")
	w.line(1, "param($wordToComplete, $commandAst, $cursorPosition)")
	w.line(1, "$top = @("+psQuoteList(topLevelNames())+")")
	w.line(1, "$subs = @{")
	for _, group := range completionGroups() {
		w.line(2, "'"+strings.Join(group.path, " ")+"' = @("+psQuoteList(subNames(completionCmd{subs: group.subs}))+")")
	}
	w.line(1, "}")
	w.line(1, "$flags = @{")
	for _, l := range completionLeaves() {
		if len(l.flags) == 0 {
			continue
		}
		var names []string
		for _, f := range l.flags {
			if f.takes {
				names = append(names, "--"+f.name+"=")
			} else {
				names = append(names, "--"+f.name)
			}
		}
		w.line(2, "'"+l.key()+"' = @("+psQuoteList(names)+")")
	}
	w.line(1, "}")
	w.line(1, "$values = @{")
	for _, l := range completionLeaves() {
		for _, f := range l.flags {
			if len(f.values) > 0 {
				w.line(2, "'"+l.key()+" --"+f.name+"' = @("+psQuoteList(f.values)+")")
			}
		}
	}
	w.line(1, "}")
	w.line(1, "$positional = @{")
	for _, l := range completionLeaves() {
		if l.pos != nil {
			w.line(2, fmt.Sprintf("'%s' = @{ index = %d; values = @(%s) }", l.key(), l.pos.index, psQuoteList(l.pos.values)))
		}
	}
	w.line(1, "}")
	w.line(1, "$words = @($commandAst.CommandElements | Select-Object -Skip 1 | ForEach-Object { $_.ToString() })")
	w.line(1, "if ($wordToComplete -ne '' -and $words.Count -gt 0) { $words = @($words | Select-Object -SkipLast 1) }")
	w.line(1, "$items = @()")
	w.line(1, "$key = ''; $pathLen = 0")
	w.line(1, "foreach ($word in $words) {")
	w.line(2, "if ($pathLen -eq 0) { $key = $word; $pathLen = 1; continue }")
	w.line(2, "if (-not $subs.ContainsKey($key)) { break }")
	w.line(2, "$key += ' ' + $word; $pathLen++")
	w.line(1, "}")
	w.line(1, "if ($words.Count -eq 0) {")
	w.line(2, "$items = $top")
	w.line(1, "} elseif ($subs.ContainsKey($key) -and $words.Count -eq $pathLen) {")
	w.line(2, "$items = $subs[$key]")
	w.line(1, "} else {")
	w.line(2, "$prev = $words[-1]")
	w.line(2, "if ($values.ContainsKey($key + ' ' + $prev)) {")
	w.line(3, "$items = $values[$key + ' ' + $prev]")
	w.line(2, "} elseif ($flags.ContainsKey($key) -and ($flags[$key] -contains ($prev + '='))) {")
	w.line(3, "$items = @()")
	w.line(2, "} else {")
	w.line(3, "if ($flags.ContainsKey($key)) { $items = @($flags[$key] | ForEach-Object { $_.TrimEnd('=') }) }")
	w.line(3, "if ($positional.ContainsKey($key) -and -not $wordToComplete.StartsWith('-')) {")
	w.line(4, "$n = 0")
	w.line(4, "for ($i = $pathLen; $i -lt $words.Count; $i++) {")
	w.line(5, "$t = $words[$i]")
	w.line(5, "if ($t.StartsWith('-')) {")
	w.line(6, "if ($flags.ContainsKey($key) -and ($flags[$key] -contains ($t + '='))) { $i++ }")
	w.line(5, "} else { $n++ }")
	w.line(4, "}")
	w.line(4, "if ($n -eq ($positional[$key].index - 1)) { $items += $positional[$key].values }")
	w.line(3, "}")
	w.line(2, "}")
	w.line(1, "}")
	w.line(1, "$items | Where-Object { $_ -like \"$wordToComplete*\" } | ForEach-Object {")
	w.line(2, "[System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)")
	w.line(1, "}")
	w.line(0, "}")
	return w.String()
}

func printCompletionUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: brain completion bash|zsh|fish|powershell

Print a shell completion script for brain to stdout. Install:
  bash        brain completion bash > ~/.local/share/bash-completion/completions/brain
  zsh         brain completion zsh > <a directory on $fpath>/_brain
  fish        brain completion fish > ~/.config/fish/completions/brain.fish
  powershell  brain completion powershell | Out-String | Invoke-Expression   (add to $PROFILE)
`)
}

// runCompletion prints the completion script for one shell. Completion
// scripts are static: they never run brain to look up project ids or values.
func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, "brain completion: missing shell (bash, zsh, fish or powershell)\n\n")
		printCompletionUsage(stderr)
		return 2
	}
	if wantsHelp(args) || args[0] == "help" {
		printCompletionUsage(stdout)
		return 0
	}
	if len(args) != 1 {
		fmt.Fprint(stderr, "brain completion: expected exactly one shell\n\n")
		printCompletionUsage(stderr)
		return 2
	}
	var script string
	switch args[0] {
	case "bash":
		script = renderBash()
	case "zsh":
		script = renderZsh()
	case "fish":
		script = renderFish()
	case "powershell":
		script = renderPowerShell()
	default:
		fmt.Fprintf(stderr, "brain completion: unknown shell %q (want bash, zsh, fish or powershell)\n\n", args[0])
		printCompletionUsage(stderr)
		return 2
	}
	fmt.Fprint(stdout, script)
	return 0
}
