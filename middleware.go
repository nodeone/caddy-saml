// based on https://github.com/crewjam/saml
package samlplugin

import (
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crewjam/saml"
	"github.com/davecgh/go-spew/spew"
	"github.com/dgrijalva/jwt-go"
	"github.com/mholt/caddy/caddyhttp/httpserver"
)

// SAMLPlugin implements middleware than allows a web application
// to support SAML.
//
// It implements http.Handler so that it can provide the metadata and ACS endpoints,
// typically /saml/metadata and /saml/acs, respectively.
//
// It also provides middleware RequireAccount which redirects users to
// the auth process if they do not have session credentials.
//
// When redirecting the user through the SAML auth flow, the middlware assigns
// a temporary cookie with a random name beginning with "saml_". The value of
// the cookie is a signed JSON Web Token containing the original URL requested
// and the SAML request ID. The random part of the name corresponds to the
// RelayState parameter passed through the SAML flow.
//
// When validating the SAML response, the RelayState is used to look up the
// correct cookie, validate that the SAML request ID, and redirect the user
// back to their original URL.
//
// Sessions are established by issuing a JSON Web Token (JWT) as a session
// cookie once the SAML flow has succeeded. The JWT token contains the
// authenticated attributes from the SAML assertion.
//
// When the middlware receives a request with a valid session JWT it extracts
// the SAML attributes and modifies the http.Request object adding a Context
// object to the request context that contains attributes from the initial
// SAML assertion.
//
// When issuing JSON Web Tokens, a signing key is required. Because the
// SAML service provider already has a private key, we borrow that key
// to sign the JWTs as well.
type SAMLPlugin struct {
	ServiceProvider   saml.ServiceProvider
	AllowIDPInitiated bool
	TokenMaxAge       time.Duration
	ClientState       ClientState
	ClientToken       ClientToken
	Map               map[string][]string
	next              httpserver.Handler
}

var jwtSigningMethod = jwt.SigningMethodHS256

func randomBytes(n int) []byte {
	rv := make([]byte, n)
	if _, err := saml.RandReader.Read(rv); err != nil {
		panic(err)
	}
	return rv
}

// ServeHTTP implements http.Handler and serves the SAML-specific HTTP endpoints
// on the URIs specified by m.ServiceProvider.MetadataURL and
// m.ServiceProvider.AcsURL.
func (s *SAMLPlugin) ServeHTTP(w http.ResponseWriter, r *http.Request) (int, error) {
	if r.URL.Path == s.ServiceProvider.MetadataURL.Path {
		md, err := s.GetEntityDescriptor()
		if err != nil {
			fmt.Printf("GetEntityDescriptor %#v", err)
			return 500, err
		}
		fmt.Fprintln(w, md)
		return 200, nil
	}

	if r.URL.Path == s.ServiceProvider.AcsURL.Path {
		r.ParseForm()
		assertion, err := s.ServiceProvider.ParseResponse(r, s.getPossibleRequestIDs(r))
		if err != nil {
			if parseErr, ok := err.(*saml.InvalidResponseError); ok {
				s.ServiceProvider.Logger.Printf("RESPONSE: ===\n%s\n===\nNOW: %s\nERROR: %s",
					parseErr.Response, parseErr.Now, parseErr.PrivateErr)
			}
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return s.next.ServeHTTP(w, r)
		}

		s.Authorize(w, r, assertion)
		return s.next.ServeHTTP(w, r)
	}

	for k, v := range s.Map {
		if strings.HasPrefix(r.URL.Path, k) {
			if token := s.GetAuthorizationToken(r); token != nil {
				r = r.WithContext(WithToken(r.Context(), token))
				if isAuthorized(v, token) {
					setHeaders(r, token)
					if dumpAttributes(v) {
						spew.Fdump(w, token)
						return 200, nil
					}
					return s.next.ServeHTTP(w, r)
				} else {
					return 403, nil
				}
			} else {
				s.RequireAccount(w, r)
			}
		}
	}
	return s.next.ServeHTTP(w, r)
}

// RequireAccount is HTTP middleware that requires that each request be
// associated with a valid session. If the request is not associated with a valid
// session, then rather than serve the request, the middlware redirects the user
// to start the SAML auth flow.
func (s *SAMLPlugin) RequireAccount(w http.ResponseWriter, r *http.Request) {
	// If we try to redirect when the original request is the ACS URL we'll
	// end up in a loop. This is a programming error, so we panic here. In
	// general this means a 500 to the user, which is preferable to a
	// redirect loop.
	if r.URL.Path == s.ServiceProvider.AcsURL.Path {
		panic("don't wrap SAMLPlugin with RequireAccount")
	}

	binding := saml.HTTPRedirectBinding
	bindingLocation := s.ServiceProvider.GetSSOBindingLocation(binding)
	if bindingLocation == "" {
		binding = saml.HTTPPostBinding
		bindingLocation = s.ServiceProvider.GetSSOBindingLocation(binding)
	}

	req, err := s.ServiceProvider.MakeAuthenticationRequest(bindingLocation)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// relayState is limited to 80 bytes but also must be integrety protected.
	// this means that we cannot use a JWT because it is way to long. Instead
	// we set a cookie that corresponds to the state
	relayState := base64.URLEncoding.EncodeToString(randomBytes(42))

	secretBlock := x509.MarshalPKCS1PrivateKey(s.ServiceProvider.Key)
	state := jwt.New(jwtSigningMethod)
	claims := state.Claims.(jwt.MapClaims)
	claims["id"] = req.ID
	claims["uri"] = r.URL.String()
	signedState, err := state.SignedString(secretBlock)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.ClientState.SetState(w, r, relayState, signedState)
	if binding == saml.HTTPRedirectBinding {
		redirectURL := req.Redirect(relayState)
		w.Header().Add("Location", redirectURL.String())
		w.WriteHeader(http.StatusFound)
		return
	}
	if binding == saml.HTTPPostBinding {
		w.Header().Add("Content-Security-Policy", ""+
			"default-src; "+
			"script-src 'sha256-AjPdJSbZmeWHnEc5ykvJFay8FTWeTeRbs9dutfZ0HqE='; "+
			"reflected-xss block; referrer no-referrer;")
		w.Header().Add("Content-type", "text/html")
		w.Write([]byte(`<!DOCTYPE html><html><body>`))
		w.Write(req.Post(relayState))
		w.Write([]byte(`</body></html>`))
		return
	}
	panic("not reached")
}

func (s *SAMLPlugin) getPossibleRequestIDs(r *http.Request) []string {
	rv := []string{}
	for _, value := range s.ClientState.GetStates(r) {
		jwtParser := jwt.Parser{
			ValidMethods: []string{jwtSigningMethod.Name},
		}
		token, err := jwtParser.Parse(value, func(t *jwt.Token) (interface{}, error) {
			secretBlock := x509.MarshalPKCS1PrivateKey(s.ServiceProvider.Key)
			return secretBlock, nil
		})
		if err != nil || !token.Valid {
			s.ServiceProvider.Logger.Printf("... invalid token %s", err)
			continue
		}
		claims := token.Claims.(jwt.MapClaims)
		rv = append(rv, claims["id"].(string))
	}

	// If IDP initiated requests are allowed, then we can expect an empty response ID.
	if s.AllowIDPInitiated {
		rv = append(rv, "")
	}

	return rv
}

// Authorize is invoked by ServeHTTP when we have a new, valid SAML assertion.
// It sets a cookie that contains a signed JWT containing the assertion attributes.
// It then redirects the user's browser to the original URL contained in RelayState.
func (s *SAMLPlugin) Authorize(w http.ResponseWriter, r *http.Request, assertion *saml.Assertion) {
	secretBlock := x509.MarshalPKCS1PrivateKey(s.ServiceProvider.Key)

	redirectURI := "/"
	if relayState := r.Form.Get("RelayState"); relayState != "" {
		stateValue := s.ClientState.GetState(r, relayState)
		if stateValue == "" {
			s.ServiceProvider.Logger.Printf("cannot find corresponding state: %s", relayState)
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}

		jwtParser := jwt.Parser{
			ValidMethods: []string{jwtSigningMethod.Name},
		}
		state, err := jwtParser.Parse(stateValue, func(t *jwt.Token) (interface{}, error) {
			return secretBlock, nil
		})
		if err != nil || !state.Valid {
			s.ServiceProvider.Logger.Printf("Cannot decode state JWT: %s (%s)", err, stateValue)
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		claims := state.Claims.(jwt.MapClaims)
		redirectURI = claims["uri"].(string)

		// delete the cookie
		s.ClientState.DeleteState(w, r, relayState)
	}

	now := saml.TimeNow()
	claims := AuthorizationToken{}
	claims.Audience = s.ServiceProvider.Metadata().EntityID
	claims.IssuedAt = now.Unix()
	claims.ExpiresAt = now.Add(s.TokenMaxAge).Unix()
	claims.NotBefore = now.Unix()
	if sub := assertion.Subject; sub != nil {
		if nameID := sub.NameID; nameID != nil {
			claims.StandardClaims.Subject = nameID.Value
		}
	}
	for _, attributeStatement := range assertion.AttributeStatements {
		claims.Attributes = map[string][]string{}
		for _, attr := range attributeStatement.Attributes {
			claimName := attr.FriendlyName
			if claimName == "" {
				claimName = attr.Name
			}
			for _, value := range attr.Values {
				claims.Attributes[claimName] = append(claims.Attributes[claimName], value.Value)
			}
		}
	}
	signedToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256,
		claims).SignedString(secretBlock)
	if err != nil {
		panic(err)
	}

	s.ClientToken.SetToken(w, r, signedToken, s.TokenMaxAge)
	http.Redirect(w, r, redirectURI, http.StatusFound)
}

// IsAuthorized returns true if the request has already been authorized.
//
// Note: This function is retained for compatability. Use GetAuthorizationToken in new code
// instead.
func (s *SAMLPlugin) IsAuthorized(r *http.Request) bool {
	return s.GetAuthorizationToken(r) != nil
}

// GetAuthorizationToken is invoked by RequireAccount to determine if the request
// is already authorized or if the user's browser should be redirected to the
// SAML login flow. If the request is authorized, then the request context is
// ammended with a Context object.
func (s *SAMLPlugin) GetAuthorizationToken(r *http.Request) *AuthorizationToken {
	tokenStr := s.ClientToken.GetToken(r)
	if tokenStr == "" {
		return nil
	}

	tokenClaims := AuthorizationToken{}
	token, err := jwt.ParseWithClaims(tokenStr, &tokenClaims, func(t *jwt.Token) (interface{}, error) {
		secretBlock := x509.MarshalPKCS1PrivateKey(s.ServiceProvider.Key)
		return secretBlock, nil
	})
	if err != nil || !token.Valid {
		s.ServiceProvider.Logger.Printf("ERROR: invalid token: %s", err)
		return nil
	}
	if err := tokenClaims.StandardClaims.Valid(); err != nil {
		s.ServiceProvider.Logger.Printf("ERROR: invalid token claims: %s", err)
		return nil
	}
	if tokenClaims.Audience != s.ServiceProvider.Metadata().EntityID {
		s.ServiceProvider.Logger.Printf("ERROR: invalid audience: %s", err)
		return nil
	}

	return &tokenClaims
}

// RequireAttribute returns a middleware function that requires that the
// SAML attribute `name` be set to `value`. This can be used to require
// that a remote user be a member of a group. It relies on the Claims assigned
// to to the context in RequireAccount.
//
// For example:
//
//     goji.Use(m.RequireAccount)
//     goji.Use(RequireAttributeSAMLPlugin("eduPersonAffiliation", "Staff"))
//
func RequireAttribute(name, value string) func(http.Handler) http.Handler {
	return func(handler http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			if claims := Token(r.Context()); claims != nil {
				for _, actualValue := range claims.Attributes[name] {
					if actualValue == value {
						handler.ServeHTTP(w, r)
						return
					}
				}
			}
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		}
		return http.HandlerFunc(fn)
	}
}
