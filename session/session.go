package session

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/df-mc/go-playfab"
	"github.com/df-mc/go-playfab/title"
	"github.com/df-mc/go-xsapi"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/service"
	"golang.org/x/oauth2"
	"golang.org/x/text/language"
)

type Session struct {
	cache           *auth.XBLTokenCache
	env             service.AuthorizationEnvironment
	playfabIdentity *playfab.Identity
	mcToken         *service.Token
	src             oauth2.TokenSource
	config          auth.Config
	conf            service.TokenConfig
	tok             *oauth2.Token
}

// SessionFromTokenSource creates a session from an XBOX token source and returns it.
func SessionFromTokenSource(src oauth2.TokenSource, config auth.Config, ctx context.Context) (s *Session, err error) {
	s = &Session{src: src, config: config}
	if err := s.login(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Session) login(ctx context.Context) error {
	_, err := s.Token()
	if err != nil {
		return fmt.Errorf("request token: %w", err)
	}
	err = s.initDiscovery(ctx)
	if err != nil {
		return fmt.Errorf("init discovery: %w", err)
	}

	region, _ := language.English.Region()
	s.conf = service.TokenConfig{
		Device: service.DeviceConfig{
			ApplicationType: service.ApplicationTypeMinecraftPE,
			Capabilities:    []string{service.CapabilityRayTracing},
			GameVersion:     protocol.CurrentVersion,
			ID:              uuid.NewString(),
			Memory:          strconv.FormatUint(rand.Uint64(), 10),
			Platform:        service.PlatformWindows10,
			PlayFabTitleID:  s.env.PlayFabTitleID,
			StorePlatform:   service.StorePlatformUWPStore,
			Type:            service.DeviceTypeWindows10,
		},
		User: service.UserConfig{
			Language:     language.English.String(),
			LanguageCode: language.AmericanEnglish,
			RegionCode:   region.String(),
			TokenType:    service.TokenTypePlayFab,
		},
	}

	s.cache = s.config.NewTokenCache()

	if err = s.loginWithPlayfab(ctx); err != nil {
		return err
	}

	return s.obtainMcToken(ctx)
}

func (s *Session) initDiscovery(ctx context.Context) error {
	discovery, err := service.Discover(ctx, service.ApplicationTypeMinecraftPE, protocol.CurrentVersion)
	if err != nil {
		return fmt.Errorf("discover: %w", err)
	}

	if err := discovery.Environment(&s.env); err != nil {
		return fmt.Errorf("decode environment: %w", err)
	}

	return nil
}

func (s *Session) loginWithPlayfab(ctx context.Context) (err error) {
	identityProvider := playfab.XBLIdentityProvider{
		TokenSource: &xblTokenSource{
			TokenSource:  s,
			relyingParty: playfab.RelyingParty,
			ctx:          auth.WithXBLTokenCache(ctx, s.cache),
		},
	}

	s.playfabIdentity, err = identityProvider.Login(playfab.LoginConfig{
		Title:         title.Title(s.env.PlayFabTitleID),
		CreateAccount: true,
	})
	if err != nil {
		return fmt.Errorf("error logging in to playfab: %w", err)
	}

	return nil
}

func (s *Session) obtainMcToken(ctx context.Context) (err error) {
	playfabIdentity, err := s.PlayfabIdentity(ctx)
	if err != nil {
		return err
	}
	s.conf.User.Token = playfabIdentity.SessionTicket
	// Assign to a local first, not directly to s.mcToken: env.Token returns (nil, err) on
	// failure, and s.mcToken must keep holding the last known-good token on a failed refresh
	// rather than being wiped to nil - a nil s.mcToken crashes every later MCToken() call (which
	// dereferences s.mcToken.ValidUntil unconditionally) with no panic recovery anywhere in the
	// process. Confirmed 2026-08-21: ForceRefreshMCToken hit Xbox Live's own refresh rate limit
	// ("Token cannot be exchanged within the limited timeframe"), which nil'd s.mcToken via this
	// exact assignment, and the next bridge.Listen call's MCToken() panicked and killed nether2rak
	// entirely.
	tok, err := s.env.Token(ctx, s.conf)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	s.mcToken = tok
	return nil
}

// Obtainer returns the Xbox token obtainer, which contains the device token
func (s *Session) RequestXBLToken(ctx context.Context, relyingParty string) (*auth.XBLToken, error) {
	tok, err := s.Token()
	if err != nil {
		return nil, fmt.Errorf("obtain live token: %w", err)
	}
	return auth.RequestXBLToken(auth.WithXBLTokenCache(ctx, s.cache), tok, relyingParty)
}

// PlayfabIdentity returns the user's Playfab identity, which includes the session ticket.
func (s *Session) PlayfabIdentity(ctx context.Context) (*playfab.Identity, error) {
	if pastExpirationTime(s.playfabIdentity.EntityToken.Expiration) {
		if err := s.loginWithPlayfab(ctx); err != nil {
			return nil, err
		}
	}
	return s.playfabIdentity, nil
}

// MCToken returns the session token, or refreshes it if it has expired.
func (s *Session) MCToken(ctx context.Context) (*service.Token, error) {
	// s.mcToken is nil before the very first successful obtainMcToken call - guard explicitly
	// rather than dereferencing .ValidUntil on it, which panics.
	if s.mcToken == nil || pastExpirationTime(s.mcToken.ValidUntil) {
		if err := s.obtainMcToken(ctx); err != nil {
			return nil, err
		}
	}
	return s.mcToken, nil
}

// ForceRefreshMCToken re-obtains the session token unconditionally, ignoring its cached
// ValidUntil. Xbox Live has been observed rejecting session/activity-handle operations with a
// 403 well before a cached token's own ValidUntil passed (confirmed 2026-08-20: ~14 minutes of
// "the owner isn't active in the referenced session" errors, all against the same
// not-yet-expired cached token, self-resolving only once that token's real expiry forced a
// fresh MCToken call) - so callers that see repeated session-creation failures should call this
// directly rather than waiting on MCToken's lazy expiry check, which has no way to know the
// cached token stopped actually working server-side.
func (s *Session) ForceRefreshMCToken(ctx context.Context) (*service.Token, error) {
	if err := s.obtainMcToken(ctx); err != nil {
		return nil, err
	}
	return s.mcToken, nil
}

// LegacyMultiplayerXBL requests an XBL token for the old multiplayer endpoint.
func (s *Session) LegacyMultiplayerXBL(ctx context.Context) (tok *auth.XBLToken, err error) {
	return s.RequestXBLToken(ctx, "https://multiplayer.minecraft.net/")
}

// MultiplayerToken requests a multiplayer token from Microsoft. The token can be reused, but is not
// reused by the vanilla client.
func (s *Session) MultiplayerToken(ctx context.Context, key *ecdsa.PublicKey) (jwt string, err error) {
	mcToken, err := s.MCToken(ctx)
	if err != nil {
		return "", fmt.Errorf("obtain MCToken: %w", err)
	}
	return s.env.MultiplayerToken(ctx, &mcTokenSource{mcToken: mcToken}, key)
}

func (s *Session) Token() (*oauth2.Token, error) {
	if s.tok.Valid() {
		return s.tok, nil
	}
	tok, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	s.tok = tok
	return s.tok, nil
}

const expirationTimeDelta = time.Minute

func pastExpirationTime(expirationTime time.Time) bool {
	return time.Now().After(expirationTime.Add(-expirationTimeDelta))
}

type mcTokenSource struct {
	mcToken *service.Token
}

func (m *mcTokenSource) Token() (*service.Token, error) {
	return m.mcToken, nil
}

// xblTokenSource is an implementation of [xsapi.TokenSource].
type xblTokenSource struct {
	oauth2.TokenSource
	relyingParty string
	ctx          context.Context
}

// Token requests an XSTS token that relies on the party specified in xblTokenSource.relyingParty.
// It uses the underlying [oauth2.TokenSource] to request Windows Live tokens.
func (x xblTokenSource) Token() (xsapi.Token, error) {
	token, err := x.TokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("request live token: %w", err)
	}
	ctx := x.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	xsts, err := auth.RequestXBLToken(ctx, token, x.relyingParty)
	if err != nil {
		return nil, fmt.Errorf("request xsts token for %q: %w", x.relyingParty, err)
	}
	return &xstsToken{xsts}, nil
}

// xstsToken wraps an [auth.XBLToken] for use in the xsapi package.
type xstsToken struct {
	*auth.XBLToken
}

// DisplayClaims returns [xsapi.DisplayClaims] from the user info claimed by the token.
func (t *xstsToken) DisplayClaims() xsapi.DisplayClaims {
	return xsapi.DisplayClaims{
		GamerTag: t.AuthorizationToken.DisplayClaims.UserInfo[0].GamerTag,
		XUID:     t.AuthorizationToken.DisplayClaims.UserInfo[0].XUID,
		UserHash: t.AuthorizationToken.DisplayClaims.UserInfo[0].UserHash,
	}
}

// String returns a string representation of the XSTS token in the same format used for Authorization headers.
func (t *xstsToken) String() string {
	return fmt.Sprintf("XBL3.0 x=%s;%s", t.AuthorizationToken.DisplayClaims.UserInfo[0].UserHash, t.AuthorizationToken.Token)
}
