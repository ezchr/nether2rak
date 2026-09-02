package xbl

// This file mirrors the JSON shapes used by com.rtm516.mcxboxbroadcast.core.models.session.*
// (extracted from the real MCXboxBroadcastStandalone.jar). Field names/casing are copied
// verbatim since Xbox Live's session directory expects this exact shape.

type SessionRef struct {
	Scid         string `json:"scid"`
	TemplateName string `json:"templateName"`
	Name         string `json:"name"`
}

type MemberSubscription struct {
	ID          string   `json:"id"`
	ChangeTypes []string `json:"changeTypes"`
}

type MemberConstantsSystem struct {
	Xuid       string `json:"xuid"`
	Initialize bool   `json:"initialize"`
}

type MemberPropertiesSystem struct {
	Active       bool               `json:"active"`
	Connection   string             `json:"connection"`
	Subscription MemberSubscription `json:"subscription"`
}

type SessionMember struct {
	Constants  map[string]MemberConstantsSystem  `json:"constants"`
	Properties map[string]MemberPropertiesSystem `json:"properties"`
}

// SessionMemberResponse is the shape members come back as when reading an existing session
// (includes xuid nested the same way, used only for parsing). Properties/Connection is needed
// by RefreshNonces to tell a genuinely new (re)join apart from the same still-connected member -
// Xbox Live assigns a fresh "connection" value per member-slot join, so a change here (not just
// the XUID's mere presence) is what should trigger a new nonce. See RefreshNonces' doc comment.
type SessionMemberResponse struct {
	Constants  map[string]MemberConstantsSystem  `json:"constants"`
	Properties map[string]MemberPropertiesSystem `json:"properties"`
}

type Connection struct {
	ConnectionType int    `json:"ConnectionType"`
	HostIpAddress  string `json:"HostIpAddress"`
	HostPort       int    `json:"HostPort"`
	NetherNetId    uint64 `json:"NetherNetId"`
	PmsgId         string `json:"PmsgId"`
}

type SessionSystemProperties struct {
	JoinRestriction string `json:"joinRestriction"`
	ReadRestriction string `json:"readRestriction"`
	Closed          bool   `json:"closed"`
}

func DefaultSessionSystemProperties() SessionSystemProperties {
	// "followed" is the setting that makes the session visible to friends of anyone who is
	// (however briefly) a genuine member, i.e. the "friends of friends" mechanism.
	return SessionSystemProperties{JoinRestriction: "followed", ReadRestriction: "followed", Closed: false}
}

// SessionCustomProperties field shape and casing (including which fields are PascalCase vs
// camelCase) is verified verbatim against a live, confirmed-working MCXboxBroadcast Geyser
// extension session (captured via its own "mcxboxbroadcast dumpsession" command against real
// Xbox Live infrastructure on 2026-08-13) - not assumed or ported blind. Two real bugs were
// found this way: TitleId must be 0 (not a real Minecraft title ID - a prior guess had this
// wrong), and levelId must be present (it was missing entirely). Do not reintroduce either.
type SessionCustomProperties struct {
	BroadcastSetting        int               `json:"BroadcastSetting"`
	CrossPlayDisabled       bool              `json:"CrossPlayDisabled"`
	Joinability             string            `json:"Joinability"`
	LanGame                 bool              `json:"LanGame"`
	MaxMemberCount          int               `json:"MaxMemberCount"`
	MemberCount             int               `json:"MemberCount"`
	OnlineCrossPlatformGame bool              `json:"OnlineCrossPlatformGame"`
	SupportedConnections    []Connection      `json:"SupportedConnections"`
	TitleId                 int               `json:"TitleId"`
	TransportLayer          int               `json:"TransportLayer"`
	LevelId                 string            `json:"levelId"`
	HostName                string            `json:"hostName"`
	OwnerId                 string            `json:"ownerId"`
	RakNetGUID              string            `json:"rakNetGUID"`
	WorldName               string            `json:"worldName"`
	WorldType               string            `json:"worldType"`
	Protocol                int               `json:"protocol"`
	Version                 string            `json:"version"`
	IsEditorWorld           bool              `json:"isEditorWorld"`
	IsHardcore              bool              `json:"isHardcore"`
	Nonces                  map[string]string `json:"nonces"`
}

type SessionProperties struct {
	System SessionSystemProperties `json:"system"`
	Custom SessionCustomProperties `json:"custom"`
}

// CreateSessionRequest is PUT to
// https://sessiondirectory.xboxlive.com/serviceconfigs/{scid}/sessionTemplates/MinecraftLobby/sessions/{sessionId}
type CreateSessionRequest struct {
	Members    map[string]SessionMember `json:"members"`
	Properties SessionProperties        `json:"properties"`
}

// CreateSessionResponse is the (partial) response shape we care about - just enough to read
// back the current member list for nonce bookkeeping.
type CreateSessionResponse struct {
	Members map[string]SessionMemberResponse `json:"members"`
}

// CreateHandleRequest is POSTed to https://sessiondirectory.xboxlive.com/handles
type CreateHandleRequest struct {
	Version    int        `json:"version"`
	Type       string     `json:"type"`
	SessionRef SessionRef `json:"sessionRef"`
}

type CreateHandleResponse struct {
	Id string `json:"id"`
}
