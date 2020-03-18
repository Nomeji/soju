package soju

import (
	"fmt"
	"strings"

	"github.com/google/shlex"
	"gopkg.in/irc.v3"
)

const serviceNick = "BouncerServ"

type serviceCommand struct {
	usage  string
	desc   string
	handle func(dc *downstreamConn, params []string) error
}

func sendServicePRIVMSG(dc *downstreamConn, text string) {
	dc.SendMessage(&irc.Message{
		Prefix:  &irc.Prefix{Name: serviceNick},
		Command: "PRIVMSG",
		Params:  []string{dc.nick, text},
	})
}

func handleServicePRIVMSG(dc *downstreamConn, text string) {
	words, err := shlex.Split(text)
	if err != nil {
		sendServicePRIVMSG(dc, fmt.Sprintf("error: failed to parse command: %v", err))
		return
	}

	var name string
	var params []string
	if len(words) > 0 {
		name = strings.ToLower(words[0])
		params = words[1:]
	}

	cmd, ok := serviceCommands[name]
	if !ok {
		sendServicePRIVMSG(dc, fmt.Sprintf(`error: unknown command %q (type "help" for a list of commands)`, name))
		return
	}

	if err := cmd.handle(dc, params); err != nil {
		sendServicePRIVMSG(dc, fmt.Sprintf("error: %v", err))
	}
}

var serviceCommands map[string]serviceCommand

func init() {
	serviceCommands = map[string]serviceCommand{
		"help": {
			usage:  "[command]",
			desc:   "print help message",
			handle: handleServiceHelp,
		},
	}
}

func handleServiceHelp(dc *downstreamConn, params []string) error {
	if len(params) > 0 {
		name := strings.ToLower(params[0])
		cmd, ok := serviceCommands[name]
		if !ok {
			return fmt.Errorf("unknown command %q", name)
		}

		text := name
		if cmd.usage != "" {
			text += " " + cmd.usage
		}
		text += ": " + cmd.desc

		sendServicePRIVMSG(dc, text)
	} else {
		var l []string
		for name := range serviceCommands {
			l = append(l, name)
		}
		sendServicePRIVMSG(dc, "available commands: "+strings.Join(l, ", "))
	}
	return nil
}