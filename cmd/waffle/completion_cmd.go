package main

import (
	"fmt"
	"io"
	"strings"
)

// top-level subcommands completed by shell completion scripts.
var completionSubcommands = []string{
	"setup",
	"chat",
	"serve",
	"status",
	"pair",
	"ws",
	"cron",
	"session",
	"forget",
	"usage",
	"pause",
	"resume",
	"secret",
	"provider",
	"backup",
	"restore",
	"doctor",
	"eval",
	"skills",
	"learn",
	"upgrade",
	"rollback",
	"completion",
	"help",
	"version",
}

// completionCmd prints shell completion scripts or installation help.
func completionCmd(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printCompletionHelp(stdout)
		return nil
	}

	switch args[0] {
	case "help", "-h", "--help":
		printCompletionHelp(stdout)
		return nil
	case "bash":
		_, err := io.WriteString(stdout, bashCompletionScript())
		return err
	case "zsh":
		_, err := io.WriteString(stdout, zshCompletionScript())
		return err
	case "fish":
		_, err := io.WriteString(stdout, fishCompletionScript())
		return err
	default:
		fmt.Fprintf(stderr, "unknown shell %q\n", args[0])
		printCompletionHelp(stderr)
		return errUsage
	}
}

func printCompletionHelp(w io.Writer) {
	fmt.Fprint(w, `Generate shell completion scripts for waffle.

Usage:
  waffle completion [bash|zsh|fish]

Install:

  # bash
  source <(waffle completion bash)

  # zsh
  source <(waffle completion zsh)

  # fish
  waffle completion fish | source

To load completions in every new shell, add the source line to your shell
profile (e.g. ~/.bashrc, ~/.zshrc) or install the fish script into
~/.config/fish/completions/waffle.fish.
`)
}

func bashCompletionScript() string {
	cmds := strings.Join(completionSubcommands, " ")
	return `# bash completion for waffle
_waffle_completions() {
	local cur prev
	COMPREPLY=()
	cur="${COMP_WORDS[COMP_CWORD]}"
	prev="${COMP_WORDS[COMP_CWORD-1]}"

	if [[ ${COMP_CWORD} -eq 1 ]]; then
		COMPREPLY=( $(compgen -W "` + cmds + `" -- "${cur}") )
		return 0
	fi

	case "${COMP_WORDS[1]}" in
	*)
		COMPREPLY=( $(compgen -W "--help -h --config -c" -- "${cur}") )
		;;
	esac
}

complete -F _waffle_completions waffle
`
}

func zshCompletionScript() string {
	cmds := strings.Join(completionSubcommands, " ")
	return `#compdef waffle
# zsh completion for waffle

_waffle() {
	local -a commands
	commands=(` + cmds + `)

	local state
	_arguments -C \
		'(-h --help)'{-h,--help}'[show help]' \
		'(-c --config)'{-c,--config}'[config path]:config file:_files' \
		'1: :->command' \
		'*::arg:->args'

	case $state in
	command)
		_describe -t commands 'waffle command' commands
		;;
	args)
		_arguments \
			'(-h --help)'{-h,--help}'[show help]' \
			'(-c --config)'{-c,--config}'[config path]:config file:_files'
		;;
	esac
}

compdef _waffle waffle
`
}

func fishCompletionScript() string {
	var b strings.Builder
	b.WriteString("# fish completion for waffle\n")
	b.WriteString("complete -c waffle -f\n")
	b.WriteString("complete -c waffle -s h -l help -d 'Show help'\n")
	b.WriteString("complete -c waffle -s c -l config -d 'Config path' -r\n")
	for _, cmd := range completionSubcommands {
		fmt.Fprintf(&b, "complete -c waffle -n '__fish_use_subcommand' -a %s -d '%s'\n", cmd, cmd)
	}
	return b.String()
}
