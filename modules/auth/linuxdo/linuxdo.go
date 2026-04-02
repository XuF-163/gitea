// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package linuxdo implements the OAuth2 protocol for authenticating users through LinuxDo (connect.linux.do).
package linuxdo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/markbates/goth"
	"golang.org/x/oauth2"
)

const (
	// NOTE: new-api uses /oauth2/authorize
	authURL      string = "https://connect.linux.do/oauth2/authorize"
	tokenURL     string = "https://connect.linux.do/oauth2/token"
	userEndpoint string = "https://connect.linux.do/api/user"
)

// New creates a new LinuxDo provider and sets up important connection details.
func New(clientKey string, secret string, callbackURL string, scopes ...string) *Provider {
	p := &Provider{
		ClientKey:    clientKey,
		Secret:       secret,
		CallbackURL:  callbackURL,
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
		providerName: "linuxdo",
	}
	p.config = newConfig(p, scopes)
	return p
}

// Provider is the implementation of goth.Provider for accessing LinuxDo.
type Provider struct {
	ClientKey    string
	Secret       string
	CallbackURL  string
	HTTPClient   *http.Client
	config       *oauth2.Config
	providerName string
}

// Name gets the name used to retrieve this provider.
func (p *Provider) Name() string {
	return p.providerName
}

// SetName is to update the name of the provider (needed in case of multiple providers of 1 type)
func (p *Provider) SetName(name string) {
	p.providerName = name
}

// Client returns an HTTP client to be used for all requests to LinuxDo.
func (p *Provider) Client() *http.Client {
	return goth.HTTPClientWithFallBack(p.HTTPClient)
}

// Debug is no-op for the LinuxDo package.
func (p *Provider) Debug(debug bool) {}

// BeginAuth asks LinuxDo for an authentication end-point.
func (p *Provider) BeginAuth(state string) (goth.Session, error) {
	url := p.config.AuthCodeURL(state)
	s := &Session{
		AuthURL: url,
	}
	return s, nil
}

// FetchUser will go to LinuxDo and access basic info about the user.
func (p *Provider) FetchUser(session goth.Session) (goth.User, error) {
	s := session.(*Session)

	user := goth.User{
		AccessToken:  s.AccessToken,
		Provider:     p.Name(),
		RefreshToken: s.RefreshToken,
		ExpiresAt:    s.ExpiresAt,
	}

	if user.AccessToken == "" {
		return user, fmt.Errorf("%s cannot get user information without accessToken", p.providerName)
	}

	req, err := http.NewRequest("GET", userEndpoint, nil)
	if err != nil {
		return user, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	resp, err := p.Client().Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return user, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return user, fmt.Errorf("%s responded with a %d trying to fetch user information", p.providerName, resp.StatusCode)
	}

	bits, err := io.ReadAll(resp.Body)
	if err != nil {
		return user, err
	}

	err = json.NewDecoder(bytes.NewReader(bits)).Decode(&user.RawData)
	if err != nil {
		return user, err
	}

	err = userFromReader(bytes.NewReader(bits), &user)
	if err != nil {
		return user, err
	}

	return user, nil
}

func userFromReader(r io.Reader, user *goth.User) error {
	u := struct {
		ID         int    `json:"id"`
		Username   string `json:"username"`
		Name       string `json:"name"`
		Active     bool   `json:"active"`
		TrustLevel int    `json:"trust_level"`
		Silenced   bool   `json:"silenced"`
	}{}

	err := json.NewDecoder(r).Decode(&u)
	if err != nil {
		return err
	}

	user.UserID = fmt.Sprintf("%d", u.ID)
	user.NickName = u.Username
	user.Name = u.Name
	if user.Name == "" {
		user.Name = u.Username
	}

	return nil
}

func newConfig(p *Provider, scopes []string) *oauth2.Config {
	c := &oauth2.Config{
		ClientID:     p.ClientKey,
		ClientSecret: p.Secret,
		RedirectURL:  p.CallbackURL,
		Endpoint: oauth2.Endpoint{
			AuthURL:  authURL,
			TokenURL: tokenURL,
			// LinuxDo 要求使用 HTTP Basic Auth 进行 token 交换
			AuthStyle: oauth2.AuthStyleInHeader,
		},
		Scopes: []string{},
	}

	if len(scopes) > 0 {
		for _, scope := range scopes {
			c.Scopes = append(c.Scopes, scope)
		}
	}

	return c
}

// RefreshTokenAvailable refresh token is not provided by LinuxDo
func (p *Provider) RefreshTokenAvailable() bool {
	return false
}

// RefreshToken is not supported by LinuxDo
func (p *Provider) RefreshToken(refreshToken string) (*oauth2.Token, error) {
	return nil, nil
}
