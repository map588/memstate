package main

import (
	"fmt"
	"strings"
)

// Shell completion. `memstate completions zsh` prints a static script; the
// script asks `memstate __complete KIND` for the values that live in the
// store (keypaths, project ids) or in this binary (config keys), so an
// installed copy of the script does not go stale when the store changes.

func cliCompletions(args []string) int {
	if len(args) != 1 || args[0] != "zsh" {
		return cliUsage("completions zsh")
	}
	fmt.Fprint(cliOut, zshCompletion)
	return 0
}

// cliComplete prints one candidate per line for the completion script. It
// always exits 0 and prints nothing on an error, because output on stderr
// or a failed command shows as noise on the command line at each Tab.
func cliComplete(args []string) int {
	if len(args) == 0 {
		return 0
	}
	kind := args[0]
	fs, o := newVerbFlags("__complete")
	if _, err := parseInterspersed(fs, args[1:]); err != nil {
		return 0
	}
	switch kind {
	case "config_keys":
		for _, s := range configSettings {
			fmt.Fprintln(cliOut, s.key)
		}
	case "projects":
		store, _, err := openStoreCLI(o.db)
		if err != nil {
			return 0
		}
		defer store.Close()
		ps, err := store.ListProjects()
		if err != nil {
			return 0
		}
		for _, p := range ps {
			// --shared reaches _shared; --project never takes a reserved id.
			if !strings.HasPrefix(p.ID, "_") {
				fmt.Fprintln(cliOut, p.ID)
			}
		}
	case "keypaths":
		project, err := resolveCLIProject(*o)
		if err != nil {
			return 0
		}
		store, _, err := openStoreCLI(o.db)
		if err != nil {
			return 0
		}
		defer store.Close()
		if deleted, err := store.ProjectDeleted(project); err != nil || deleted {
			return 0
		}
		mems, err := store.List(project, "")
		if err != nil {
			return 0
		}
		host := hostSlug()
		for _, m := range mems {
			if o.shared && isOtherHost(m.Keypath, host) {
				continue
			}
			fmt.Fprintln(cliOut, m.Keypath)
		}
	}
	return 0
}

// zshCompletion is the zsh completion script. Flags follow the verb, as
// cmdCLI requires. _multi_parts completes a keypath one dotted segment at a
// time.
const zshCompletion = `#compdef memstate

# zsh completion for memstate.
# Install: memstate completions zsh > "${fpath[1]}/_memstate"
# Or, in .zshrc after compinit: source <(memstate completions zsh)
# Keypaths, project ids and config keys come from "memstate __complete".

_memstate_projects() {
  local -a expl projects
  projects=(${(f)"$(_call_program projects $_memstate_cmd __complete projects 2>/dev/null)"})
  _wanted projects expl project compadd -a projects
}

_memstate_keypaths() {
  local -a expl scope kps
  (( $+opt_args[--shared] )) && scope+=(--shared)
  (( $+opt_args[--project] )) && scope+=(--project ${(q)${(Q)opt_args[--project]}})
  (( $+opt_args[--db] )) && scope+=(--db ${(q)${(Q)opt_args[--db]}})
  kps=(${(f)"$(_call_program keypaths $_memstate_cmd __complete keypaths $scope 2>/dev/null)"})
  _wanted keypaths expl keypath _multi_parts . kps
}

_memstate_config_keys() {
  local -a expl keys
  keys=(${(f)"$(_call_program config-keys $_memstate_cmd __complete config_keys 2>/dev/null)"})
  _wanted config-keys expl 'config key' compadd -a keys
}

_memstate() {
  local curcontext=$curcontext state state_descr line ret=1
  local _memstate_cmd=$words[1]
  typeset -A opt_args
  local -a expl verbs scope common categories
  categories=(decision config status note gotcha reference learning)
  scope=(
    '(--shared --all)--project=[a project other than the one of this repository]:project:_memstate_projects'
    '(--project --all)--shared[the reserved user scope]'
  )
  common=(
    '--json[print the raw JSON shape]'
    '--no-color[disable ANSI colors]'
    '--db=[SQLite file for reads]:SQLite file:_files'
    '--addr=[shared daemon address]:address: '
  )

  _arguments -C '1:verb:->verb' '*::arg:->args' && return 0
  case $state in
    verb)
      verbs=(
        'tree:keypath tree with metadata'
        'get:content at KEYPATH and below'
        'show:alias of get'
        'cat:alias of get'
        'history:every version of a keypath, newest first'
        'log:alias of history'
        'search:hybrid search through the daemon'
        'set:write one fact'
        'edit:open the current content in an editor'
        'rm:tombstone a keypath or a subtree'
        'delete:alias of rm'
        'projects:live projects with memory counts'
        'status:daemon health'
        'config:settings, their sources, and the running daemon'
        'completions:print a shell completion script'
        'help:show usage'
      )
      _describe -t verbs 'memstate verb' verbs && ret=0
      ;;
    args)
      curcontext=${curcontext%:*:*}:memstate-$words[1]:
      case $words[1] in
        tree)
          _arguments $scope $common '1::keypath:_memstate_keypaths' && ret=0 ;;
        get|show|cat)
          _arguments $scope $common '--raw[content only, no metadata]' \
            '1::keypath:_memstate_keypaths' && ret=0 ;;
        history|log)
          _arguments $scope $common '1:keypath:_memstate_keypaths' && ret=0 ;;
        search)
          _arguments $scope $common \
            '(--project --shared)--all[search every project]' \
            '--mode=[search mode]:mode:(hybrid fts semantic)' \
            '--limit=[maximum results]:count: ' \
            "--category=[only this category]:category:($categories)" \
            '--topics=[comma-separated topics, any of them]:topics: ' \
            '*:query: ' && ret=0 ;;
        set)
          _arguments $scope $common \
            "--category=[kind of memory]:category:($categories)" \
            '--topics=[comma-separated subject tags]:topics: ' \
            '--source=[provenance shown in history]:source: ' \
            '1:keypath:_memstate_keypaths' \
            '2:value (- reads stdin): ' && ret=0 ;;
        edit)
          _arguments $scope $common \
            "--category=[replace the category]:category:($categories)" \
            '--topics=[replace the topics]:topics: ' \
            '1:keypath:_memstate_keypaths' && ret=0 ;;
        rm|delete)
          _arguments $scope $common \
            '--recursive[also tombstone every keypath below]' \
            '--yes[skip the confirmation for --recursive]' \
            '1:keypath:_memstate_keypaths' && ret=0 ;;
        projects)
          _arguments '--db=[SQLite file]:SQLite file:_files' && ret=0 ;;
        status)
          _arguments '--addr=[shared daemon address]:address: ' && ret=0 ;;
        config)
          case $CURRENT in
            2) _wanted actions expl action compadd set unset path && ret=0 ;;
            3) [[ $words[2] == (set|unset) ]] && _memstate_config_keys && ret=0 ;;
          esac ;;
        completions)
          _arguments '1:shell:(zsh)' && ret=0 ;;
      esac
      ;;
  esac
  return ret
}

if [[ $funcstack[1] == _memstate ]]; then
  _memstate "$@"
else
  compdef _memstate memstate
fi
`
