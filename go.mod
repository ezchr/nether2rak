module github.com/gameparrot/netherconnect

go 1.24.0

toolchain go1.24.4

require (
	github.com/coder/websocket v1.8.14
	github.com/coreos/go-oidc/v3 v3.17.0
	github.com/creachadair/jrpc2 v1.3.0
	github.com/df-mc/go-nethernet v1.0.11
	github.com/df-mc/go-playfab v1.0.0
	github.com/df-mc/go-xsapi v1.0.1
	github.com/go-jose/go-jose/v4 v4.1.4
	github.com/google/uuid v1.6.0
	github.com/pion/webrtc/v4 v4.2.10-0.20260224155637-aa3b95c72dd2
	github.com/sandertv/go-raknet v1.15.0
	github.com/sandertv/gophertunnel v1.56.2
	golang.org/x/oauth2 v0.35.0
	golang.org/x/text v0.34.0
)

require (
	github.com/creachadair/mds v0.21.4 // indirect
	github.com/df-mc/jsonc v1.0.5 // indirect
	github.com/go-gl/mathgl v1.1.0 // indirect
	github.com/klauspost/compress v1.18.1 // indirect
	github.com/pion/datachannel v1.6.0 // indirect
	github.com/pion/dtls/v3 v3.1.2 // indirect
	github.com/pion/ice/v4 v4.2.1 // indirect
	github.com/pion/interceptor v0.1.44 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.16 // indirect
	github.com/pion/rtp v1.10.1 // indirect
	github.com/pion/sctp v1.9.2 // indirect
	github.com/pion/sdp/v3 v3.0.18 // indirect
	github.com/pion/srtp/v3 v3.0.10 // indirect
	github.com/pion/stun/v3 v3.1.1 // indirect
	github.com/pion/transport/v4 v4.0.1 // indirect
	github.com/pion/turn/v4 v4.1.4 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	golang.org/x/crypto v0.48.0 // indirect
	golang.org/x/image v0.21.0 // indirect
	golang.org/x/net v0.50.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/time v0.10.0 // indirect
)

replace golang.org/x/oauth2 => github.com/golang/oauth2 v0.35.0

replace golang.org/x/text => github.com/golang/text v0.34.0

replace golang.org/x/image => github.com/golang/image v0.9.0

replace golang.org/x/tools => github.com/golang/tools v0.9.3

replace golang.org/x/sync => github.com/golang/sync v0.9.0

replace golang.org/x/net => github.com/golang/net v0.9.0

replace golang.org/x/time => github.com/golang/time v0.9.0

replace golang.org/x/crypto => github.com/golang/crypto v0.48.0

replace golang.org/x/sys => github.com/golang/sys v0.9.0

replace golang.org/x/term => github.com/golang/term v0.8.0

replace golang.org/x/mod => github.com/golang/mod v0.10.0

replace golang.org/x/xerrors => github.com/golang/xerrors v0.0.0-20220907171357-04be3eba64a2

replace github.com/df-mc/go-nethernet => ./vendor-go-nethernet
