package chat

import (
	"fmt"
	"strings"
	"unicode"
)

// Name is the canonical name of a chat command, without its leading slash.
type Name string

const (
	CommandHelp        Name = "help"
	CommandExit        Name = "exit"
	CommandModel       Name = "model"
	CommandModels      Name = "models"
	CommandNew         Name = "new"
	CommandSessions    Name = "sessions"
	CommandResume      Name = "resume"
	CommandStatus      Name = "status"
	CommandUsage       Name = "usage"
	CommandPermissions Name = "permissions"
	CommandSkill       Name = "skill"
	CommandRepo        Name = "repo"
	CommandWorkset     Name = "workset"
)

// Command describes one canonical chat command and its completion/help
// metadata. Aliases never appear as separate registry entries.
type Command struct {
	Name        Name     `json:"name"`
	Usage       string   `json:"usage"`
	Aliases     []string `json:"aliases"`
	Description string   `json:"description"`
}

// ParsedCommand contains a canonical command name and its trimmed arguments.
type ParsedCommand struct {
	Name Name   `json:"name"`
	Args string `json:"args"`
}

var commandRegistry = [...]Command{
	{Name: CommandHelp, Usage: "/help", Description: "show commands and keys"},
	{Name: CommandExit, Usage: "/exit", Aliases: []string{"quit"}, Description: "finish and close chat"},
	{Name: CommandModel, Usage: "/model [alias]", Description: "choose the session model"},
	{Name: CommandModels, Usage: "/models", Description: "list configured models"},
	{Name: CommandNew, Usage: "/new", Aliases: []string{"reset", "clear"}, Description: "start a new session"},
	{Name: CommandSessions, Usage: "/sessions", Description: "list recent sessions"},
	{Name: CommandResume, Usage: "/resume [session]", Description: "resume a session"},
	{Name: CommandStatus, Usage: "/status", Description: "show current runtime status"},
	{Name: CommandUsage, Usage: "/usage", Description: "show token and request usage"},
	{Name: CommandPermissions, Usage: "/permissions", Description: "show effective sandbox and tool policy"},
	{Name: CommandSkill, Usage: "/skill <name> [args]", Description: "invoke a skill"},
	{Name: CommandRepo, Usage: "/repo <owner/repo>", Description: "open a repository workspace"},
	{Name: CommandWorkset, Usage: "/workset [list|replace <id> <text>|drop <id>|clear]", Description: "inspect or correct the working set"},
}

// Commands returns a deep copy of the canonical command registry in stable
// display order.
func Commands() []Command {
	commands := make([]Command, len(commandRegistry))
	for i, command := range commandRegistry {
		commands[i] = cloneCommand(command)
	}
	return commands
}

// Complete returns canonical commands whose slash-prefixed names start with
// prefix, preserving registry order.
func Complete(prefix string) []Command {
	var commands []Command
	for _, command := range commandRegistry {
		if strings.HasPrefix("/"+string(command.Name), prefix) {
			commands = append(commands, cloneCommand(command))
		}
	}
	return commands
}

// ParseInput recognizes an exact slash-command first token. Unknown commands
// and ordinary model messages return ok=false. Recognized commands return
// ok=true even when validation supplies a usage error.
func ParseInput(input string) (parsed ParsedCommand, ok bool, err error) {
	if !strings.HasPrefix(input, "/") {
		return ParsedCommand{}, false, nil
	}

	token, args := splitFirstToken(input)
	for _, command := range commandRegistry {
		if token != "/"+string(command.Name) && !matchesAlias(token, command.Aliases) {
			continue
		}

		parsed = ParsedCommand{Name: command.Name, Args: args}
		if args == "" && (command.Name == CommandSkill || command.Name == CommandRepo) {
			return parsed, true, fmt.Errorf("usage: %s", command.Usage)
		}
		return parsed, true, nil
	}
	return ParsedCommand{}, false, nil
}

func splitFirstToken(input string) (string, string) {
	separator := strings.IndexFunc(input, unicode.IsSpace)
	if separator < 0 {
		return input, ""
	}
	return input[:separator], strings.TrimSpace(input[separator:])
}

func matchesAlias(token string, aliases []string) bool {
	for _, alias := range aliases {
		if token == "/"+alias {
			return true
		}
	}
	return false
}

func cloneCommand(command Command) Command {
	command.Aliases = append([]string(nil), command.Aliases...)
	return command
}
