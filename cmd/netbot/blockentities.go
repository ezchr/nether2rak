package main

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

// chunkBlockEntities reads the block entities at the end of a network-encoded LevelChunk payload
// (n sub-chunks, then 24 biome storages, a border-block count, then NBT compounds until the end),
// as a client does. It returns each block entity's id and position, and an error if a compound is
// malformed: a Bedrock client drops the whole chunk then.
func chunkBlockEntities(data []byte, n int) ([]string, error) {
	buf := bytes.NewBuffer(data)
	skipStorage := func(biome bool) error {
		head, err := buf.ReadByte()
		if err != nil {
			return err
		}
		bits := int(head >> 1)
		if bits == 0x7f {
			return nil
		}
		words := 0
		if bits != 0 {
			words = 4096 / (32 / bits)
			if bits == 3 || bits == 5 || bits == 6 {
				words++
			}
		}
		if len(buf.Next(words*4)) != words*4 {
			return errors.New("short storage")
		}
		count := int32(1)
		if bits != 0 {
			if err := protocol.Varint32(buf, &count); err != nil {
				return err
			}
		}
		for i := int32(0); i < count; i++ {
			var v int32
			if err := protocol.Varint32(buf, &v); err != nil {
				return err
			}
		}
		return nil
	}
	for s := 0; s < n; s++ {
		ver, err := buf.ReadByte()
		if err != nil {
			return nil, err
		}
		storages := byte(1)
		if ver == 8 || ver == 9 {
			storages, _ = buf.ReadByte()
			if ver == 9 {
				_, _ = buf.ReadByte()
			}
		}
		for i := byte(0); i < storages; i++ {
			if err := skipStorage(false); err != nil {
				return nil, fmt.Errorf("sub-chunk %d: %w", s, err)
			}
		}
	}
	for i := 0; i < 24; i++ {
		if err := skipStorage(true); err != nil {
			return nil, fmt.Errorf("biome %d: %w", i, err)
		}
	}
	if _, err := buf.ReadByte(); err != nil { // border blocks
		return nil, nil
	}
	var out []string
	dec := nbt.NewDecoderWithEncoding(buf, nbt.NetworkLittleEndian)
	for buf.Len() > 0 {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return out, fmt.Errorf("block entity %d: %w", len(out), err)
		}
		out = append(out, fmt.Sprintf("%v@%v,%v,%v", m["id"], m["x"], m["y"], m["z"]))
	}
	return out, nil
}
