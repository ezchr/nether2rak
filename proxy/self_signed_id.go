package proxy

import "github.com/google/uuid"

// selfSignedIDNamespace is a fixed namespace used to derive a deterministic
// SelfSignedID from a player's real XUID via UUID v5 (SHA-1).
//
// Native BDS keys a self-signed connection's persistent player-data record by
// SelfSignedID rather than by XUID (XUID itself is discarded for
// AuthenticationType 2 / self-signed chains). Left unset, gophertunnel's
// EncodeOffline lets the field default per-connection, so BDS mints a fresh
// identity - and therefore fresh, empty save data - every single reconnect.
// Deriving SelfSignedID from the player's real XUID instead makes it the same
// value every time that XUID connects, so BDS resolves the same on-disk
// player record instead of creating a new one.
var selfSignedIDNamespace = uuid.MustParse("a3e78ee7-823a-4cb5-9fe0-532b54ccc20d")

// selfSignedIDFromXUID returns a deterministic SelfSignedID for a given XUID.
func selfSignedIDFromXUID(xuid string) string {
	return uuid.NewSHA1(selfSignedIDNamespace, []byte(xuid)).String()
}
