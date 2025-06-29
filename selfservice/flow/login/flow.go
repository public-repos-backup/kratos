// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package login

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ory/kratos/x/redir"

	"github.com/ory/pop/v6"

	"github.com/tidwall/gjson"

	"github.com/ory/x/sqlxx"

	"github.com/ory/x/stringsx"

	hydraclientgo "github.com/ory/hydra-client-go/v2"

	"github.com/ory/kratos/driver/config"
	"github.com/ory/kratos/hydra"

	"github.com/ory/kratos/ui/container"

	"github.com/gofrs/uuid"
	"github.com/pkg/errors"

	"github.com/ory/x/urlx"

	"github.com/ory/kratos/identity"
	"github.com/ory/kratos/selfservice/flow"
	"github.com/ory/kratos/x"
)

// Login Flow
//
// This object represents a login flow. A login flow is initiated at the "Initiate Login API / Browser Flow"
// endpoint by a client.
//
// Once a login flow is completed successfully, a session cookie or session token will be issued.
//
// swagger:model loginFlow
type Flow struct {
	// ID represents the flow's unique ID. When performing the login flow, this
	// represents the id in the login UI's query parameter: http://<selfservice.flows.login.ui_url>/?flow=<flow_id>
	//
	// required: true
	ID             uuid.UUID     `json:"id" faker:"-" db:"id" rw:"r"`
	NID            uuid.UUID     `json:"-"  faker:"-" db:"nid"`
	OrganizationID uuid.NullUUID `json:"organization_id,omitempty"  faker:"-" db:"organization_id"`

	// Ory OAuth 2.0 Login Challenge.
	//
	// This value is set using the `login_challenge` query parameter of the registration and login endpoints.
	// If set will cooperate with Ory OAuth2 and OpenID to act as an OAuth2 server / OpenID Provider.
	OAuth2LoginChallenge sqlxx.NullString `json:"oauth2_login_challenge,omitempty" faker:"-" db:"oauth2_login_challenge_data"`

	// HydraLoginRequest is an optional field whose presence indicates that Kratos
	// is being used as an identity provider in a Hydra OAuth2 flow. Kratos
	// populates this field by retrieving its value from Hydra and it is used by
	// the login and consent UIs.
	HydraLoginRequest *hydraclientgo.OAuth2LoginRequest `json:"oauth2_login_request,omitempty" faker:"-" db:"-"`

	// Type represents the flow's type which can be either "api" or "browser", depending on the flow interaction.
	//
	// required: true
	Type flow.Type `json:"type" db:"type" faker:"flow_type"`

	// ExpiresAt is the time (UTC) when the flow expires. If the user still wishes to log in,
	// a new flow has to be initiated.
	//
	// required: true
	ExpiresAt time.Time `json:"expires_at" faker:"time_type" db:"expires_at"`

	// IssuedAt is the time (UTC) when the flow started.
	//
	// required: true
	IssuedAt time.Time `json:"issued_at" faker:"time_type" db:"issued_at"`

	// InternalContext stores internal context used by internals - for example MFA keys.
	InternalContext sqlxx.JSONRawMessage `db:"internal_context" json:"-" faker:"-"`

	// RequestURL is the initial URL that was requested from Ory Kratos. It can be used
	// to forward information contained in the URL's path or query for example.
	//
	// required: true
	RequestURL string `json:"request_url" db:"request_url"`

	// ReturnTo contains the requested return_to URL.
	ReturnTo string `json:"return_to,omitempty" db:"-"`

	// The active login method
	//
	// If set contains the login method used. If the flow is new, it is unset.
	Active identity.CredentialsType `json:"active,omitempty" db:"active_method"`

	// UI contains data which must be shown in the user interface.
	//
	// required: true
	UI *container.Container `json:"ui" db:"ui"`

	// CreatedAt is a helper struct field for gobuffalo.pop.
	CreatedAt time.Time `json:"created_at" db:"created_at"`

	// UpdatedAt is a helper struct field for gobuffalo.pop.
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`

	// CSRFToken contains the anti-csrf token associated with this flow. Only set for browser flows.
	CSRFToken string `json:"-" db:"csrf_token"`

	// Refresh stores whether this login flow should enforce re-authentication.
	Refresh bool `json:"refresh" db:"forced"`

	// RequestedAAL stores if the flow was requested to update the authenticator assurance level.
	//
	// This value can be one of "aal1", "aal2", "aal3".
	RequestedAAL identity.AuthenticatorAssuranceLevel `json:"requested_aal" faker:"len=4" db:"requested_aal"`

	// SessionTokenExchangeCode holds the secret code that the client can use to retrieve a session token after the login flow has been completed.
	// This is only set if the client has requested a session token exchange code, and if the flow is of type "api",
	// and only on creating the login flow.
	SessionTokenExchangeCode string `json:"session_token_exchange_code,omitempty" faker:"-" db:"-"`

	// State represents the state of this request:
	//
	// - choose_method: ask the user to choose a method to sign in with
	// - sent_email: the email has been sent to the user
	// - passed_challenge: the request was successful and the login challenge was passed.
	//
	// required: true
	State State `json:"state" faker:"-" db:"state"`

	// Only used internally
	IDToken string `json:"-" db:"-"`

	// Only used internally
	RawIDTokenNonce string `json:"-" db:"-"`

	// TransientPayload is used to pass data from the login to hooks and email templates
	//
	// required: false
	TransientPayload json.RawMessage `json:"transient_payload,omitempty" faker:"-" db:"-"`

	// Contains a list of actions, that could follow this flow
	//
	// It can, for example, contain a reference to the verification flow, created as part of the user's
	// registration.
	ContinueWithItems []flow.ContinueWith `json:"-" db:"-" faker:"-" `

	// ReturnToVerification contains the redirect URL for the verification flow.
	ReturnToVerification string `json:"-" db:"-"`

	isAccountLinkingFlow bool `json:"-" db:"-"`
}

var _ flow.Flow = new(Flow)

func NewFlow(conf *config.Config, exp time.Duration, csrf string, r *http.Request, flowType flow.Type) (*Flow, error) {
	now := time.Now().UTC()
	id := x.NewUUID()
	requestURL := x.RequestURL(r).String()

	// Pre-validate the return to URL which is contained in the HTTP request.
	_, err := redir.SecureRedirectTo(r,
		conf.SelfServiceBrowserDefaultReturnTo(r.Context()),
		redir.SecureRedirectUseSourceURL(requestURL),
		redir.SecureRedirectAllowURLs(conf.SelfServiceBrowserAllowedReturnToDomains(r.Context())),
		redir.SecureRedirectAllowSelfServiceURLs(conf.SelfPublicURL(r.Context())),
	)
	if err != nil {
		return nil, err
	}

	hydraLoginChallenge, err := hydra.GetLoginChallengeID(conf, r)
	if err != nil {
		return nil, err
	}

	refresh, _ := strconv.ParseBool(r.URL.Query().Get("refresh"))

	return &Flow{
		ID:                   id,
		OAuth2LoginChallenge: hydraLoginChallenge,
		ExpiresAt:            now.Add(exp),
		IssuedAt:             now,
		UI: &container.Container{
			Method: "POST",
			Action: flow.AppendFlowTo(urlx.AppendPaths(conf.SelfPublicURL(r.Context()), RouteSubmitFlow), id).String(),
		},
		RequestURL: requestURL,
		CSRFToken:  csrf,
		Type:       flowType,
		Refresh:    refresh,
		RequestedAAL: identity.AuthenticatorAssuranceLevel(strings.ToLower(stringsx.Coalesce(
			r.URL.Query().Get("aal"),
			string(identity.AuthenticatorAssuranceLevel1)))),
		InternalContext: []byte("{}"),
		State:           flow.StateChooseMethod,
	}, nil
}

func (f *Flow) GetType() flow.Type {
	return f.Type
}

func (f *Flow) GetRequestURL() string {
	return f.RequestURL
}

func (f Flow) TableName(ctx context.Context) string {
	return "selfservice_login_flows"
}

func (f Flow) WhereID(ctx context.Context, alias string) string {
	return fmt.Sprintf("%s.%s = ? AND %s.%s = ?", alias, "id", alias, "nid")
}

func (f *Flow) Valid() error {
	if f.ExpiresAt.Before(time.Now()) {
		return errors.WithStack(flow.NewFlowExpiredError(f.ExpiresAt))
	}
	return nil
}

func (f Flow) GetID() uuid.UUID {
	return f.ID
}

// IsRefresh returns true if the login flow was triggered to re-authenticate the user.
// This is the case if the refresh query parameter is set to true.
func (f *Flow) IsRefresh() bool {
	return f.Refresh
}

func (f *Flow) AppendTo(src *url.URL) *url.URL {
	return flow.AppendFlowTo(src, f.ID)
}

func (f Flow) GetNID() uuid.UUID {
	return f.NID
}

func (f *Flow) EnsureInternalContext() {
	if !gjson.ParseBytes(f.InternalContext).IsObject() {
		f.InternalContext = []byte("{}")
	}
}

func (f *Flow) GetInternalContext() sqlxx.JSONRawMessage {
	return f.InternalContext
}

func (f *Flow) SetInternalContext(bytes sqlxx.JSONRawMessage) {
	f.InternalContext = bytes
}

func (f Flow) MarshalJSON() ([]byte, error) {
	type local Flow
	f.SetReturnTo()
	return json.Marshal(local(f))
}

func (f *Flow) SetReturnTo() {
	// Return to is already set, do not overwrite it.
	if len(f.ReturnTo) > 0 {
		return
	}
	if u, err := url.Parse(f.RequestURL); err == nil {
		f.ReturnTo = u.Query().Get("return_to")
	}
}

func (f *Flow) AfterFind(*pop.Connection) error {
	f.SetReturnTo()
	return nil
}

func (f *Flow) AfterSave(*pop.Connection) error {
	f.SetReturnTo()
	return nil
}

func (f *Flow) GetUI() *container.Container {
	return f.UI
}

func (f *Flow) SecureRedirectToOpts(ctx context.Context, cfg config.Provider) (opts []redir.SecureRedirectOption) {
	return []redir.SecureRedirectOption{
		redir.SecureRedirectReturnTo(f.ReturnTo),
		redir.SecureRedirectUseSourceURL(f.RequestURL),
		redir.SecureRedirectAllowURLs(cfg.Config().SelfServiceBrowserAllowedReturnToDomains(ctx)),
		redir.SecureRedirectAllowSelfServiceURLs(cfg.Config().SelfPublicURL(ctx)),
		redir.SecureRedirectOverrideDefaultReturnTo(cfg.Config().SelfServiceFlowLoginReturnTo(ctx, f.Active.String())),
	}
}

func (f *Flow) GetState() flow.State {
	return flow.State(f.State)
}

func (f *Flow) GetFlowName() flow.FlowName {
	return flow.LoginFlow
}

func (f *Flow) SetState(state flow.State) {
	f.State = State(state)
}

func (t *Flow) GetTransientPayload() json.RawMessage {
	return t.TransientPayload
}

var _ flow.FlowWithContinueWith = new(Flow)

func (f *Flow) AddContinueWith(c flow.ContinueWith) {
	f.ContinueWithItems = append(f.ContinueWithItems, c)
}

func (f *Flow) ContinueWith() []flow.ContinueWith {
	return f.ContinueWithItems
}

func (f *Flow) SetReturnToVerification(to string) {
	f.ReturnToVerification = to
}

func (f *Flow) ToLoggerField() map[string]interface{} {
	if f == nil {
		return map[string]interface{}{}
	}
	return map[string]interface{}{
		"id":            f.ID.String(),
		"return_to":     f.ReturnTo,
		"request_url":   f.RequestURL,
		"active":        f.Active,
		"type":          f.Type,
		"nid":           f.NID,
		"state":         f.State,
		"refresh":       f.Refresh,
		"requested_aal": f.RequestedAAL,
	}
}

func (f *Flow) GetOAuth2LoginChallenge() sqlxx.NullString {
	return f.OAuth2LoginChallenge
}
