package main

import (
	"regexp"
	_ "unsafe" // go:linkname
)

// Featured servers put Marketplace packs on their ResourcePackStack without ever offering them in
// ResourcePacksInfo: a real client already owns them, so they are never sent. gophertunnel then
// fails the login with "texture pack (UUID=..., version=...) not downloaded" (confirmed
// 2026-09-30 against OneBlock) unless the pack is on its internal exemptedPacks list - the same
// list that holds the vanilla packs every client has. The scraper applies no packs at all, so
// exemptMissingPack adds whatever pack a failed login names to that list, and the next attempt
// gets through.

type exemptedResourcePack struct {
	uuid    string
	version string
}

//go:linkname gtExemptedPacks github.com/sandertv/gophertunnel/minecraft.exemptedPacks
var gtExemptedPacks []exemptedResourcePack

var missingPack = regexp.MustCompile(`pack \(UUID=([0-9a-fA-F-]+), version=([^)]+)\) not downloaded`)

// exemptMissingPack exempts the pack a "not downloaded" login error names, and reports whether it
// did (false for any other error, or a pack that was already exempt).
func exemptMissingPack(err error) bool {
	if err == nil {
		return false
	}
	m := missingPack.FindStringSubmatch(err.Error())
	if m == nil {
		return false
	}
	for _, p := range gtExemptedPacks {
		if p.uuid == m[1] && p.version == m[2] {
			return false
		}
	}
	gtExemptedPacks = append(gtExemptedPacks, exemptedResourcePack{uuid: m[1], version: m[2]})
	return true
}
