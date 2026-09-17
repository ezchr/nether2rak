package xbl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// InviteRequest is POSTed to https://sessiondirectory.xboxlive.com/handles to send a real Xbox
// Live game invite. This is a completely different handle "type" from the "activity" handle
// Create() already sends, not a variant of it.
//
// Shape matches github.com/df-mc/go-xsapi/mpsd's own Session.Invite (v1.0.1, the version already
// pinned in this project's go.mod for session/session.go) exactly, field for field - that is a
// real, independently-working implementation of the same endpoint, and it deliberately differs
// from Microsoft's own GDK REST doc example in one important way (see InviteAttrs).
//
// Deliberately has no "id" field: the docs' own example shows one, but that's the ID Xbox Live
// assigns and returns in the response, not something a caller supplies on the request. Sending
// one is rejected outright - confirmed live 2026-09-16: "status 400: Invalid handle provided.
// The request body must not specify the handle 'id' field."
type InviteRequest struct {
	Version     int         `json:"version"`
	Type        string      `json:"type"`
	SessionRef  SessionRef  `json:"sessionRef"`
	InviteAttrs InviteAttrs `json:"inviteAttributes"`
	InvitedXuid string      `json:"invitedXuid"`
}

// InviteAttrs is context shown in the invite notification. Xbox Live uses TitleId to resolve
// which game to show the icon/name for, and - this turned out to be the actual bug - the real
// client appears to need it to be a genuine, existing title ID to render an invite at all.
//
// InviteTitleId (below) is the correct value, NOT the package-level TitleId=0 used everywhere
// else in this codebase for session/world listing. Confirmed against
// github.com/HashimTheArab/go-mcxboxbroadcast (a real, independently-working MCXboxBroadcast
// reimplementation) 2026-09-16: it defines two SEPARATE constants for this. Its own status.go
// spells this out directly: "Minecraft friend-list sessions use TitleId=0 in MPSD custom
// properties. The package TitleID constant is still used for Xbox invite handles" - and its
// package TitleID constant is 896928775 (a real, registered Minecraft Bedrock title ID), used
// only for the invite path (broadcaster.go's Invite method), never for session creation.
//
// This project had been using TitleId=0 for both, on the assumption (from an earlier, unrelated
// capture of session CREATION, not an invite) that 0 was safe everywhere. It is safe for session
// creation - MCXboxBroadcast does the same - but not for invites: Xbox Live still accepts a
// title-0 invite request and returns a normal-looking invite handle (confirmed live 2026-09-16:
// full handle body with a genuine id, expiration, senderXuid), but nothing ever surfaced as a
// notification on the real recipient device, because there is no real game "0" for the
// client-side invite renderer to resolve into anything displayable.
//
// TitleId is a STRING on the wire ("896928775"), not a bare int - go-xsapi's own working
// Session.Invite (github.com/df-mc/go-xsapi/mpsd, already the pinned dependency this project's
// session/session.go itself uses) renders it via strconv.FormatInt into a map[string]any, i.e.
// {"titleId":"896928775"}, not {"titleId":896928775}. Match that exactly, not the unquoted int
// Microsoft's own GDK REST doc example happens to show.
type InviteAttrs struct {
	TitleId string `json:"titleId"`
}

// InviteTitleId is the real Minecraft Bedrock title ID Xbox Live needs to render an invite
// notification - see InviteAttrs' doc comment for how this was found and confirmed to differ
// from the package's own TitleId=0 (which stays correct for session creation, unchanged).
const InviteTitleId = 896928775

// SendInvite sends a real Xbox Live game invite to invitedXuid, into this Session's own live
// world. Only the account that owns the session can do this - Xbox Live checks the caller's
// membership/rights against the session referenced, so this must be called on the Session
// belonging to the account actually hosting the world, never on behalf of a different account.
//
// This is a one-shot fire: Xbox Live delivers the invite as a push notification (and an item in
// the recipient's Xbox app/console "invites" list) regardless of whether they're online right
// now. There is no separate "accept" step this code needs to handle - tapping the notification on
// their end is what joins them to the session referenced.
func (s *Session) SendInvite(ctx context.Context, invitedXUID string) error {
	auth, err := s.authHeader(ctx)
	if err != nil {
		return fmt.Errorf("invite %s: %w", invitedXUID, err)
	}

	body := InviteRequest{
		Version: 1,
		Type:    "invite",
		SessionRef: SessionRef{
			Scid:         ServiceConfigID,
			TemplateName: TemplateName,
			Name:         s.sessionID,
		},
		InviteAttrs: InviteAttrs{TitleId: strconv.Itoa(InviteTitleId)},
		InvitedXuid: invitedXUID,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal invite request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, createHandleURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-xbl-contract-version", xblContractVersion)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send invite to %s: %w", invitedXUID, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("send invite to %s: status %d: %s", invitedXUID, resp.StatusCode, string(respBody))
	}
	// The docs describe a successful response body as "an invite handle" - a real handle ID here
	// is proof Xbox Live actually created something, not just accepted the request, which
	// matters for diagnosing "the invite never arrived" (a real handle can still fail to notify
	// the recipient's device for other reasons this comment doesn't yet know about).
	s.log.Info("sent xbox live game invite", "invitedXuid", invitedXUID, "handleResponse", string(respBody))
	return nil
}
