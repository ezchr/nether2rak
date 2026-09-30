package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"

	"github.com/gameparrot/netherconnect/session"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

// Featured servers ("experiences", minecraft://joinExperience?experienceId=...) only let in a
// player the gatherings service has assigned to one of their instances. The game does that by
// posting the experience id to gatherings' join/experience AS THE PLAYER, then connecting to the
// address it answers with; a connection from an account that skipped the request is turned away
// with "Player '<playfab id>' is not assigned to the server" (ServiceRuntime.PlayerNotAllowed,
// confirmed 2026-09-30 against OneBlock). experienceAddress makes that same request with this
// account's own Minecraft services token, so the scrape that follows is the assigned player.

const discoveryURL = "https://client.discovery.minecraft-services.net/api/v1.0/discovery/MinecraftPE/builds/"

// gatheringsURI looks the gatherings service up through service discovery, since Mojang moves
// services between hosts and the game resolves them per build the same way.
func gatheringsURI(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL+protocol.CurrentVersion, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("service discovery: %w", err)
	}
	defer resp.Body.Close()
	var d struct {
		Result struct {
			ServiceEnvironments map[string]map[string]struct {
				ServiceURI string `json:"serviceUri"`
			} `json:"serviceEnvironments"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return "", fmt.Errorf("service discovery: %w", err)
	}
	uri := d.Result.ServiceEnvironments["gatherings"]["prod"].ServiceURI
	if uri == "" {
		return "", fmt.Errorf("service discovery: no gatherings service for build %s", protocol.CurrentVersion)
	}
	return uri, nil
}

// experienceAddress asks gatherings to place this account in the experience and returns the
// host:port of the instance it was assigned to.
func experienceAddress(ctx context.Context, authSession *session.Session, experienceID string) (string, error) {
	uri, err := gatheringsURI(ctx)
	if err != nil {
		return "", err
	}
	tok, err := authSession.MCToken(ctx)
	if err != nil {
		return "", fmt.Errorf("minecraft services token: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"experienceId": experienceID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uri+"/api/v2.0/join/experience", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", tok.AuthorizationHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("join experience: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("join experience: status %d: %s", resp.StatusCode, raw)
	}
	var r struct {
		Result struct {
			IPv4Address string `json:"ipV4Address"`
			Port        int    `json:"port"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Result.IPv4Address == "" || r.Result.Port == 0 {
		return "", fmt.Errorf("join experience: no address in reply: %s", raw)
	}
	return net.JoinHostPort(r.Result.IPv4Address, strconv.Itoa(r.Result.Port)), nil
}
