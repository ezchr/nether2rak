package bridge

import (
	"math"
	"slices"
	"strings"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// The relay's own commands for players on a Network, handled before their server sees them:
//
//	/server [name]                 list the servers, or move to one
//	/broadcast [on|off|status]     list the world in the Friends tab or take it off (admins only)
//
// They are added to the command list every server sends, so they autocomplete everywhere. A
// /server name that isn't one of the relay's servers goes on to the server (Velocity has its own).

// geyserPlayerSubcommands are the /geyser subcommands every player may use (Geyser's permission
// defaults); geyserAdminSubcommands are added for the relay's admins.
var (
	geyserPlayerSubcommands = []string{"advancements", "statistics", "settings", "options", "tooltips", "quickactions", "offhand", "ping", "help"}
	geyserAdminSubcommands  = []string{"list", "reload", "dump", "version", "connectiontest", "gamerules", "nethernet", "extensions"}
)

// hiddenForPlayers are command names left out of non-admins' command lists: Velocity's LuckPerms
// command and its click-callback command, which players have no use for.
var hiddenForPlayers = map[string]bool{"lpv": true, "luckpermsvelocity": true, "velocity:callback": true}

// injectCommands adds the relay's commands to a server's command list.
func (s *switchSession) injectCommands(raw, payload []byte) []byte {
	var pk packet.AvailableCommands
	if decodeInto(&pk, payload) != nil {
		return raw
	}
	changed := false
	if !s.isAdmin() {
		// Proxy-internal commands Velocity adds for everyone; only admins need to see them.
		kept := pk.Commands[:0]
		for _, c := range pk.Commands {
			if hiddenForPlayers[c.Name] {
				changed = true
				continue
			}
			kept = append(kept, c)
		}
		pk.Commands = kept
	}
	have := map[string]bool{}
	for _, c := range pk.Commands {
		have[c.Name] = true
	}
	// /geyser reaches Bedrock without its subcommands (Geyser registers it on Velocity as a plain
	// command): give it the ones this player may use, so they are suggested.
	for i, c := range pk.Commands {
		if c.Name != "geyser" {
			continue
		}
		subs := geyserPlayerSubcommands
		if s.isAdmin() {
			subs = append(append([]string(nil), subs...), geyserAdminSubcommands...)
		}
		e := protocol.CommandEnum{Type: "N2RGeyser"}
		for _, v := range subs {
			idx := slices.Index(pk.EnumValues, v)
			if idx < 0 {
				idx = len(pk.EnumValues)
				pk.EnumValues = append(pk.EnumValues, v)
			}
			e.ValueIndices = append(e.ValueIndices, uint32(idx))
		}
		pk.Enums = append(pk.Enums, e)
		pk.Commands[i].Overloads = []protocol.CommandOverload{{Parameters: []protocol.CommandParameter{
			{Name: "subcommand", Type: protocol.CommandArgValid | protocol.CommandArgEnum | uint32(len(pk.Enums)-1), Optional: true},
		}}}
		changed = true
	}
	enum := func(name string, values []string) uint32 {
		e := protocol.CommandEnum{Type: name}
		for _, v := range values {
			i := slices.Index(pk.EnumValues, v)
			if i < 0 {
				i = len(pk.EnumValues)
				pk.EnumValues = append(pk.EnumValues, v)
			}
			e.ValueIndices = append(e.ValueIndices, uint32(i))
		}
		pk.Enums = append(pk.Enums, e)
		return uint32(len(pk.Enums) - 1)
	}
	added := false
	serverOverloads := func() []protocol.CommandOverload {
		servers := enum("N2RServer", s.n.ServerNames())
		return []protocol.CommandOverload{{Parameters: []protocol.CommandParameter{
			{Name: "server", Type: protocol.CommandArgValid | protocol.CommandArgEnum | servers, Optional: true},
		}}}
	}
	if !have["server"] {
		pk.Commands = append(pk.Commands, protocol.Command{
			Name: "server", Description: "Move to another server of this network", AliasesOffset: math.MaxUint32,
			Overloads: serverOverloads(),
		})
		added = true
	} else {
		// A server behind Velocity already lists Velocity's /server, whose server names Java
		// clients get as they type, which Bedrock can't: the relay answers /server itself, so its
		// list of servers replaces Velocity's options.
		for i, c := range pk.Commands {
			if c.Name == "server" {
				pk.Commands[i].Overloads = serverOverloads()
				added = true
			}
		}
	}
	if !have["broadcast"] && s.n.Listing != nil && s.isAdmin() {
		states := enum("N2RBroadcast", []string{"on", "off", "status"})
		pk.Commands = append(pk.Commands, protocol.Command{
			Name: "broadcast", Description: "Show or hide the world in the Friends tab", AliasesOffset: math.MaxUint32,
			Overloads: []protocol.CommandOverload{{Parameters: []protocol.CommandParameter{
				{Name: "state", Type: protocol.CommandArgValid | protocol.CommandArgEnum | states, Optional: true},
			}}},
		})
		added = true
	}
	if !added && !changed {
		return raw
	}
	return encodePacket(&pk)
}

func (s *switchSession) isAdmin() bool {
	return s.n.Admins[s.id.XUID] || s.n.Admins[strings.ToLower(s.id.DisplayName)]
}

// handleCommand runs a relay command and reports whether it was one (so it must not reach the server).
func (s *switchSession) handleCommand(line string) bool {
	fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "/"))
	if len(fields) == 0 {
		return false
	}
	switch strings.ToLower(fields[0]) {
	case "server":
		if len(fields) == 1 {
			cur := s.serverName()
			var names []string
			for _, n := range s.n.ServerNames() {
				if strings.EqualFold(n, cur) {
					n = "<green>" + n + "</green>"
				}
				names = append(names, n)
			}
			s.message("<yellow>You are on %s. Servers: %s. Use /server <name>.</yellow>", cur, strings.Join(names, ", "))
			return true
		}
		server := s.n.server(fields[1])
		if server == nil {
			return false // maybe the server's own /server (Velocity)
		}
		if err := s.requestSwitch(*server, ""); err != nil {
			s.message("<red>%s</red>", err)
		}
		return true
	case "broadcast":
		if s.n.Listing == nil {
			return false
		}
		if !s.isAdmin() {
			s.message("<red>You don't have permission to use this command.</red>")
			return true
		}
		arg := "status"
		if len(fields) > 1 {
			arg = strings.ToLower(fields[1])
		}
		switch arg {
		case "on", "off":
			if err := s.n.Listing.Set(arg == "on"); err != nil {
				s.message("<red>Could not switch the broadcast %s: %s</red>", arg, err)
				return true
			}
			s.n.Log.Info("network: broadcast switched by a player", "player", s.id.DisplayName, "on", arg == "on")
			if arg == "on" {
				s.message("<green>The world is now listed in the Friends tab.</green>")
			} else {
				s.message("<yellow>The world is no longer listed in the Friends tab. Players already in stay; direct IP still works.</yellow>")
			}
		case "status":
			if s.n.Listing.On() {
				s.message("<green>The world is listed in the Friends tab.</green> <grey>/broadcast off hides it.</grey>")
			} else {
				s.message("<yellow>The world is not listed in the Friends tab (direct IP only).</yellow> <grey>/broadcast on lists it.</grey>")
			}
		default:
			s.message("<red>Usage: /broadcast [on|off|status]</red>")
		}
		return true
	}
	return false
}
