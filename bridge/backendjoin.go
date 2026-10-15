package bridge

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/ezchr/nether2rak/proxy"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// backendSpawn is what a backend sent while the relay itself logged a player into it, up to and
// including the spawn. The relay does this (rather than the player's client) when it moves a
// player to another server: the client is already in a world and must not see a second login.
type backendSpawn struct {
	StartGame    *packet.StartGame
	ItemRegistry *packet.ItemRegistry
	Actors       *packet.AvailableActorIdentifiers
	Biomes       *packet.BiomeDefinitionList
	PacksInfo    *packet.ResourcePacksInfo
	PackStack    *packet.ResourcePackStack

	// After holds every other packet the backend sent after StartGame, raw and in order (chunks,
	// inventory, commands, the spawn PlayStatus...). They are passed to the client once the
	// switch has prepared it.
	After [][]byte
	// Counts is how many packets of each id were seen, for the probe.
	Counts map[uint32]int
}

// joinBackend plays the client's part of the join on a backend the relay has just logged in to
// (ForwardIdentity returned): it accepts the resource packs without downloading them (the player's
// client already has the packs it was given at its first login), asks for chunks and waits for
// the spawn. chunkRadius is the radius the player's client asked for.
func joinBackend(c *proxy.ProxyConn, chunkRadius int32, timeout time.Duration) (*backendSpawn, error) {
	sp := &backendSpawn{Counts: map[uint32]int{}}
	deadline := time.Now().Add(timeout)
	if d, ok := c.Conn().(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(deadline)
		defer d.SetReadDeadline(time.Time{})
	}
	for {
		pks, err := c.ReadPackets()
		if err != nil {
			return sp, fmt.Errorf("read: %w", err)
		}
		for _, raw := range pks {
			id, payload, err := splitPacket(raw)
			if err != nil {
				return sp, err
			}
			sp.Counts[id]++
			switch id {
			case packet.IDPlayStatus:
				var pk packet.PlayStatus
				if err := decodeInto(&pk, payload); err != nil {
					return sp, err
				}
				switch pk.Status {
				case packet.PlayStatusLoginSuccess:
					continue
				case packet.PlayStatusPlayerSpawn:
					if sp.StartGame == nil {
						return sp, errors.New("spawned before StartGame")
					}
					sp.After = append(sp.After, raw)
					return sp, nil
				default:
					return sp, fmt.Errorf("play status %d", pk.Status)
				}
			case packet.IDDisconnect:
				var pk packet.Disconnect
				_ = decodeInto(&pk, payload)
				return sp, fmt.Errorf("disconnected: %s", pk.Message)
			case packet.IDResourcePacksInfo:
				sp.PacksInfo = &packet.ResourcePacksInfo{}
				if err := decodeInto(sp.PacksInfo, payload); err != nil {
					return sp, err
				}
				if err := c.WritePacket(&packet.ResourcePackClientResponse{Response: packet.PackResponseAllPacksDownloaded}); err != nil {
					return sp, err
				}
			case packet.IDResourcePackStack:
				sp.PackStack = &packet.ResourcePackStack{}
				if err := decodeInto(sp.PackStack, payload); err != nil {
					return sp, err
				}
				if err := c.WritePacket(&packet.ResourcePackClientResponse{Response: packet.PackResponseCompleted}); err != nil {
					return sp, err
				}
			case packet.IDStartGame:
				sp.StartGame = &packet.StartGame{}
				if err := decodeInto(sp.StartGame, payload); err != nil {
					return sp, err
				}
				if err := c.WritePacket(&packet.RequestChunkRadius{ChunkRadius: chunkRadius, MaxChunkRadius: uint8(min(chunkRadius, 255))}); err != nil {
					return sp, err
				}
			case packet.IDItemRegistry:
				sp.ItemRegistry = &packet.ItemRegistry{}
				if err := decodeInto(sp.ItemRegistry, payload); err != nil {
					return sp, err
				}
			case packet.IDAvailableActorIdentifiers:
				sp.Actors = &packet.AvailableActorIdentifiers{}
				if err := decodeInto(sp.Actors, payload); err != nil {
					return sp, err
				}
			case packet.IDBiomeDefinitionList:
				sp.Biomes = &packet.BiomeDefinitionList{}
				if err := decodeInto(sp.Biomes, payload); err != nil {
					return sp, err
				}
			default:
				if sp.StartGame != nil {
					sp.After = append(sp.After, raw)
				}
			}
		}
	}
}

// splitPacket returns a raw packet's id and its payload.
func splitPacket(raw []byte) (uint32, []byte, error) {
	buf := bytes.NewBuffer(raw)
	var h packet.Header
	if err := h.Read(buf); err != nil {
		return 0, nil, fmt.Errorf("read packet header: %w", err)
	}
	return h.PacketID, buf.Bytes(), nil
}

// decodeInto decodes a packet payload, turning the decoder's panics into errors.
func decodeInto(pk packet.Packet, payload []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("decode %T: %v", pk, r)
		}
	}()
	pk.Marshal(protocol.NewReader(bytes.NewBuffer(payload), 0, false))
	return nil
}

// encodePacket encodes a packet with its header, as the pumps pass packets on.
func encodePacket(pk packet.Packet) []byte {
	b := bytes.NewBuffer(nil)
	h := packet.Header{PacketID: pk.ID()}
	_ = h.Write(b)
	pk.Marshal(protocol.NewWriter(b, 0))
	return b.Bytes()
}
