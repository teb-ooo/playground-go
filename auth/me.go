package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// MeBody is the response of GET /auth/me.
type MeBody struct {
	Subject   string   `json:"subject" doc:"Stable identity id (OIDC sub)."`
	Email     string   `json:"email" doc:"Email address."`
	Username  string   `json:"username" doc:"Username (preferred_username)."`
	Picture   string   `json:"picture" doc:"Avatar URL, empty if none."`
	Groups    []string `json:"groups" doc:"Roles from the identity's groups claim."`
	IsAdmin   bool     `json:"is_admin" doc:"True when groups contains admin."`
	IsOwner   bool     `json:"is_owner" doc:"True when the email equals the app owner (APP_OWNER), compared case-insensitively."`
	Anonymous bool     `json:"anonymous,omitempty" doc:"True only in the answer to ?optional=1 when nobody is signed in; every other field is then empty."`
}

// MeInput is the query of GET /auth/me.
type MeInput struct {
	Optional bool `query:"optional" doc:"Answer 200 with anonymous=true instead of 401 when nobody is signed in (the platform shell uses it so a signed-out page logs no failed request)."`
}

// MeOutput is the Huma output type of get-current-user.
type MeOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         MeBody
}

func registerMe(api huma.API, owner string) {
	huma.Register(api, huma.Operation{
		OperationID: "get-current-user",
		Method:      http.MethodGet,
		Path:        "/auth/me",
		Summary:     "Get the signed-in user",
		Description: "Returns the signed-in user's subject, email, username, picture, groups and admin flag and owner flag, or 401 when nobody is signed in (with ?optional=1: 200 and anonymous=true instead). Read by the web package; hidden from MCP.",
		Tags:        []string{"auth"},
		Hidden:      true,
		Security:    []map[string][]string{{"session": {}}, {"bearer": {}}},
	}, func(ctx context.Context, in *MeInput) (*MeOutput, error) {
		if _, ok := FromContext(ctx); !ok && in.Optional {
			return &MeOutput{CacheControl: "no-store", Body: MeBody{Groups: []string{}, Anonymous: true}}, nil
		}
		u, err := Require(ctx)
		if err != nil {
			return nil, err
		}
		groups := u.Groups
		if groups == nil {
			groups = []string{}
		}
		return &MeOutput{CacheControl: "no-store", Body: MeBody{
			Subject: u.Subject, Email: u.Email, Username: u.Username, Picture: u.Picture,
			Groups: groups, IsAdmin: u.IsAdmin(),
			IsOwner: owner != "" && strings.EqualFold(strings.TrimSpace(u.Email), owner),
		}}, nil
	})
}

// AddSecuritySchemes declares the "session" (cookie) and "bearer" security
// schemes that operations reference in their Security field.
func AddSecuritySchemes(o *huma.OpenAPI) {
	if o.Components == nil {
		o.Components = &huma.Components{}
	}
	if o.Components.SecuritySchemes == nil {
		o.Components.SecuritySchemes = map[string]*huma.SecurityScheme{}
	}
	o.Components.SecuritySchemes["session"] = &huma.SecurityScheme{Type: "apiKey", In: "cookie", Name: CookieName,
		Description: "Session cookie set by /auth/callback."}
	o.Components.SecuritySchemes["bearer"] = &huma.SecurityScheme{Type: "http", Scheme: "bearer", BearerFormat: "JWT",
		Description: "Access token issued by the identity service (it must carry a scope for this app), or a platform API key (pk_...)."}
}
