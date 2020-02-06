package jounce

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"gopkg.in/irc.v3"
)

const (
	rpl_localusers   = "265"
	rpl_globalusers  = "266"
	rpl_topicwhotime = "333"
)

type modeSet string

func (ms modeSet) Has(c byte) bool {
	return strings.IndexByte(string(ms), c) >= 0
}

func (ms *modeSet) Add(c byte) {
	if !ms.Has(c) {
		*ms += modeSet(c)
	}
}

func (ms *modeSet) Del(c byte) {
	i := strings.IndexByte(string(*ms), c)
	if i >= 0 {
		*ms = (*ms)[:i] + (*ms)[i+1:]
	}
}

func (ms *modeSet) Apply(s string) error {
	var plusMinus byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '+', '-':
			plusMinus = c
		default:
			switch plusMinus {
			case '+':
				ms.Add(c)
			case '-':
				ms.Del(c)
			default:
				return fmt.Errorf("malformed modestring %q: missing plus/minus", s)
			}
		}
	}
	return nil
}

type channelStatus byte

const (
	channelPublic  channelStatus = '='
	channelSecret  channelStatus = '@'
	channelPrivate channelStatus = '*'
)

func parseChannelStatus(s string) (channelStatus, error) {
	if len(s) > 1 {
		return 0, fmt.Errorf("invalid channel status %q: more than one character", s)
	}
	switch cs := channelStatus(s[0]); cs {
	case channelPublic, channelSecret, channelPrivate:
		return cs, nil
	default:
		return 0, fmt.Errorf("invalid channel status %q: unknown status", s)
	}
}

type membership byte

const (
	membershipFounder   membership = '~'
	membershipProtected membership = '&'
	membershipOperator  membership = '@'
	membershipHalfOp    membership = '%'
	membershipVoice     membership = '+'
)

const stdMembershipPrefixes = "~&@%+"

func parseMembershipPrefix(s string) (prefix membership, nick string) {
	// TODO: any prefix from PREFIX RPL_ISUPPORT
	if strings.IndexByte(stdMembershipPrefixes, s[0]) >= 0 {
		return membership(s[0]), s[1:]
	} else {
		return 0, s
	}
}

type upstreamChannel struct {
	Name      string
	Topic     string
	TopicWho  string
	TopicTime time.Time
	Status    channelStatus
	Members   map[string]membership
}

type upstreamConn struct {
	upstream *Upstream
	net      net.Conn
	irc      *irc.Conn
	srv      *Server

	serverName            string
	availableUserModes    string
	availableChannelModes string
	channelModesWithParam string

	registered bool
	modes      modeSet
	channels   map[string]*upstreamChannel
}

func (c *upstreamConn) getChannel(name string) (*upstreamChannel, error) {
	ch, ok := c.channels[name]
	if !ok {
		return nil, fmt.Errorf("unknown channel %q", name)
	}
	return ch, nil
}

func (c *upstreamConn) handleMessage(msg *irc.Message) error {
	switch msg.Command {
	case "PING":
		// TODO: handle params
		return c.irc.WriteMessage(&irc.Message{
			Command: "PONG",
			Params:  []string{c.srv.Hostname},
		})
	case "MODE":
		if len(msg.Params) < 2 {
			return newNeedMoreParamsError(msg.Command)
		}
		if nick := msg.Params[0]; nick != c.upstream.Nick {
			return fmt.Errorf("received MODE message for unknow nick %q", nick)
		}
		return c.modes.Apply(msg.Params[1])
	case "NOTICE":
		c.srv.Logger.Printf("%q: %v", c.upstream.Addr, msg)
	case irc.RPL_WELCOME:
		c.registered = true
		c.srv.Logger.Printf("Connection to %q registered", c.upstream.Addr)

		for _, ch := range c.upstream.Channels {
			err := c.irc.WriteMessage(&irc.Message{
				Command: "JOIN",
				Params:  []string{ch},
			})
			if err != nil {
				return err
			}
		}
	case irc.RPL_MYINFO:
		if len(msg.Params) < 5 {
			return newNeedMoreParamsError(msg.Command)
		}
		c.serverName = msg.Params[1]
		c.availableUserModes = msg.Params[3]
		c.availableChannelModes = msg.Params[4]
		if len(msg.Params) > 5 {
			c.channelModesWithParam = msg.Params[5]
		}
	case "JOIN":
		if len(msg.Params) < 1 {
			return newNeedMoreParamsError(msg.Command)
		}
		for _, ch := range strings.Split(msg.Params[0], ",") {
			c.srv.Logger.Printf("Joined channel %q", ch)
			c.channels[ch] = &upstreamChannel{
				Name:    ch,
				Members: make(map[string]membership),
			}
		}
	case irc.RPL_TOPIC, irc.RPL_NOTOPIC:
		if len(msg.Params) < 3 {
			return newNeedMoreParamsError(msg.Command)
		}
		ch, err := c.getChannel(msg.Params[1])
		if err != nil {
			return err
		}
		if msg.Command == irc.RPL_TOPIC {
			ch.Topic = msg.Params[2]
		} else {
			ch.Topic = ""
		}
	case "TOPIC":
		if len(msg.Params) < 1 {
			return newNeedMoreParamsError(msg.Command)
		}
		ch, err := c.getChannel(msg.Params[0])
		if err != nil {
			return err
		}
		if len(msg.Params) > 1 {
			ch.Topic = msg.Params[1]
		} else {
			ch.Topic = ""
		}
	case rpl_topicwhotime:
		if len(msg.Params) < 4 {
			return newNeedMoreParamsError(msg.Command)
		}
		ch, err := c.getChannel(msg.Params[1])
		if err != nil {
			return err
		}
		ch.TopicWho = msg.Params[2]
		sec, err := strconv.ParseInt(msg.Params[3], 10, 64)
		if err != nil {
			return fmt.Errorf("failed to parse topic time: %v", err)
		}
		ch.TopicTime = time.Unix(sec, 0)
	case irc.RPL_NAMREPLY:
		if len(msg.Params) < 4 {
			return newNeedMoreParamsError(msg.Command)
		}
		ch, err := c.getChannel(msg.Params[2])
		if err != nil {
			return err
		}

		status, err := parseChannelStatus(msg.Params[1])
		if err != nil {
			return err
		}
		ch.Status = status

		for _, s := range strings.Split(msg.Params[3], " ") {
			membership, nick := parseMembershipPrefix(s)
			ch.Members[nick] = membership
		}
	case irc.RPL_ENDOFNAMES:
		// TODO
	case irc.RPL_YOURHOST, irc.RPL_CREATED:
		// Ignore
	case irc.RPL_LUSERCLIENT, irc.RPL_LUSEROP, irc.RPL_LUSERUNKNOWN, irc.RPL_LUSERCHANNELS, irc.RPL_LUSERME:
		// Ignore
	case irc.RPL_MOTDSTART, irc.RPL_MOTD, irc.RPL_ENDOFMOTD:
		// Ignore
	case rpl_localusers, rpl_globalusers:
		// Ignore
	case irc.RPL_STATSVLINE, irc.RPL_STATSPING, irc.RPL_STATSBLINE, irc.RPL_STATSDLINE:
		// Ignore
	default:
		c.srv.Logger.Printf("Unhandled upstream message: %v", msg)
	}
	return nil
}

func connect(s *Server, upstream *Upstream) error {
	s.Logger.Printf("Connecting to %v", upstream.Addr)

	netConn, err := tls.Dial("tcp", upstream.Addr, nil)
	if err != nil {
		return fmt.Errorf("failed to dial %q: %v", upstream.Addr, err)
	}

	c := upstreamConn{
		upstream: upstream,
		net:      netConn,
		irc:      irc.NewConn(netConn),
		srv:      s,
		channels: make(map[string]*upstreamChannel),
	}
	defer netConn.Close()

	err = c.irc.WriteMessage(&irc.Message{
		Command: "NICK",
		Params:  []string{upstream.Nick},
	})
	if err != nil {
		return err
	}

	err = c.irc.WriteMessage(&irc.Message{
		Command: "USER",
		Params:  []string{upstream.Username, "0", "*", upstream.Realname},
	})
	if err != nil {
		return err
	}

	for {
		msg, err := c.irc.ReadMessage()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("failed to read IRC command: %v", err)
		}

		if err := c.handleMessage(msg); err != nil {
			c.srv.Logger.Printf("Failed to handle message %q from %q: %v", msg, upstream.Addr, err)
		}
	}

	return netConn.Close()
}
