package auth

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// MeBody is the response of GET /auth/me.
type MeBody struct {
	Subject  string   `json:"subject" doc:"Stable identity id (OIDC sub)."`
	Email    string   `json:"email" doc:"Email address."`
	Username string   `json:"username" doc:"Username (preferred_username)."`
	Picture  string   `json:"picture" doc:"Avatar URL, empty if none."`
	Groups   []string `json:"groups" doc:"Roles from the identity's groups claim."`
	IsAdmin  bool     `json:"is_admin" doc:"True when groups contains admin."`
}

// MeOutput is the Huma output type of get-current-user.
type MeOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         MeBody
}

func registerMe(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-current-user",
		Method:      http.MethodGet,
		Path:        "/auth/me",
		Summary:     "Get the signed-in user",
		Description: "Returns the signed-in user's subject, email, username, picture, groups and admin flag, or 401 when nobody is signed in. Read by the web package; hidden from MCP.",
		Tags:        []string{"auth"},
		Hidden:      true,
		Security:    []map[string][]string{{"session": {}}, {"bearer": {}}},
	}, func(ctx context.Context, _ *struct{}) (*MeOutput, error) {
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
		Description: "Access token issued by the identity service."}
}
