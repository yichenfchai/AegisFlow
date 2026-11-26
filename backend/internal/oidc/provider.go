package oidc

import (
	"context"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

const (
	// discoveryPath is appended to the configured issuer; the issuer is kept
	// without a trailing slash so this yields a well-formed URL.
	discoveryPath = "/.well-known/openid-configuration"
	// maxProviderResponse bounds every provider HTTP response. Discovery and
	// token responses are tiny; an unbounded body would let a malicious or
	// compromised provider exhaust server memory.
	maxProviderResponse = 256 << 10
	// maxIDTokenSize bounds the token endpoint's id_token string.
	maxIDTokenSize = 64 << 10
	// clockLeeway is the accepted clock skew for exp/iat/nbf on the ID token.
	clockLeeway = 60 * time.Second
	// minRSAKeyBits refuses weak signing keys from the provider JWKS.
	minRSAKeyBits = 2048
	// maxRSAKeyBits refuses absurdly large keys that would burn CPU per login.
	maxRSAKeyBits = 8192
)

// Provider errors are internal sentinels; the Service maps them to the fixed
// browser-facing codes. None of them carries provider text.
var (
	ErrTokenExchange   = errors.New("OIDC token exchange failed")
	ErrInvalidToken    = errors.New("OIDC id_token is invalid")
	ErrEmailUnverified = errors.New("OIDC email is missing or not verified")
)

// Provider is the discovered OIDC provider: endpoints, the client registration
// and a lazily fetched, refresh-on-unknown-kid JWKS cache.
type Provider struct {
	issuer           string
	clientID         string
	clientSecret     string
	redirectURL      string
	scope            string
	authorizationURL string
	tokenURL         string
	jwksURL          string
	client           *http.Client
	timeout          time.Duration
	mu               sync.Mutex
	keys             map[string]*rsa.PublicKey
	keysLoaded       bool
}

// newProvider validates the options and performs discovery. Discovery is a
// startup step: a bad configuration or an unreachable/invalid provider means
// the server refuses to start instead of failing every login later.
func newProvider(ctx context.Context, options Options) (*Provider, error) {
	if options.Issuer == "" || options.ClientID == "" || options.RedirectURL == "" {
		return nil, errors.New("OIDC issuer, client id and redirect URL are required")
	}
	if options.DiscoveryTimeout <= 0 {
		return nil, errors.New("OIDC discovery timeout must be positive")
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: options.DiscoveryTimeout,
			// Never follow redirects on provider calls: discovery/JWKS/token
			// endpoints are exact configured resources, and following a 3xx
			// could downgrade to a host the operator did not approve.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	p := &Provider{
		issuer:       options.Issuer,
		clientID:     options.ClientID,
		clientSecret: options.ClientSecret,
		redirectURL:  options.RedirectURL,
		scope:        strings.Join(options.Scopes, " "),
		client:       client,
		timeout:      options.DiscoveryTimeout,
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, options.DiscoveryTimeout)
	defer cancel()
	if err := p.discover(discoveryCtx); err != nil {
		return nil, err
	}
	return p, nil
}

type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// discover fetches and validates {issuer}/.well-known/openid-configuration.
// The issuer value must match the configured issuer exactly (after trailing
// slash normalization), and every endpoint must satisfy the same https /
// loopback-http rule as the issuer itself.
func (p *Provider) discover(ctx context.Context) error {
	body, err := p.getJSON(ctx, p.issuer+discoveryPath)
	if err != nil {
		return fmt.Errorf("OIDC discovery failed: %w", err)
	}
	var document discoveryDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return errors.New("OIDC discovery document is not valid JSON")
	}
	if document.Issuer != p.issuer {
		return errors.New("OIDC discovery issuer does not match AEGIS_OIDC_ISSUER")
	}
	if document.AuthorizationEndpoint, err = validateProviderURL("authorization_endpoint", document.AuthorizationEndpoint); err != nil {
		return err
	}
	if document.TokenEndpoint, err = validateProviderURL("token_endpoint", document.TokenEndpoint); err != nil {
		return err
	}
	if document.JWKSURI, err = validateProviderURL("jwks_uri", document.JWKSURI); err != nil {
		return err
	}
	p.authorizationURL = document.AuthorizationEndpoint
	p.tokenURL = document.TokenEndpoint
	p.jwksURL = document.JWKSURI
	return nil
}

// AuthorizationURL builds the authorization request. PKCE S256 and a nonce are
// always sent; state binds the request to the one-time state cookie.
func (p *Provider) AuthorizationURL(state, nonce, challenge string) string {
	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("client_id", p.clientID)
	values.Set("redirect_uri", p.redirectURL)
	values.Set("scope", p.scope)
	values.Set("state", state)
	values.Set("nonce", nonce)
	values.Set("code_challenge", challenge)
	values.Set("code_challenge_method", "S256")
	return p.authorizationURL + "?" + values.Encode()
}

// ExchangeCode redeems the authorization code at the token endpoint. A
// configured client secret uses client_secret_post; otherwise the public-client
// PKCE flow only sends client_id. Responses and errors are bounded and never
// surfaced verbatim.
func (p *Provider) ExchangeCode(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", p.redirectURL)
	form.Set("client_id", p.clientID)
	form.Set("code_verifier", verifier)
	if p.clientSecret != "" {
		form.Set("client_secret", p.clientSecret)
	}
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", ErrTokenExchange
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("%w: token endpoint unreachable", ErrTokenExchange)
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return "", fmt.Errorf("%w: unusable token response", ErrTokenExchange)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: token endpoint returned status %d", ErrTokenExchange, response.StatusCode)
	}
	var tokenResponse struct {
		IDToken string `json:"id_token"`
	}
	if err = json.Unmarshal(body, &tokenResponse); err != nil || tokenResponse.IDToken == "" {
		return "", fmt.Errorf("%w: token response has no id_token", ErrTokenExchange)
	}
	if len(tokenResponse.IDToken) > maxIDTokenSize {
		return "", fmt.Errorf("%w: id_token is too large", ErrTokenExchange)
	}
	return tokenResponse.IDToken, nil
}

// idTokenClaims is the subset of the OIDC ID token this deployment relies on.
// The embedded registered claims carry iss/aud/exp/iat/nbf/sub validation.
type idTokenClaims struct {
	Nonce             string `json:"nonce"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
	AZP               string `json:"azp"`
	jwt.RegisteredClaims
}

// Identity is the verified external identity used for local account linkage.
type Identity struct {
	Email             string
	PreferredUsername string
	Name              string
}

// VerifyIDToken verifies RS256 signature, JWKS key selection, issuer,
// audience, expiry/issued-at/not-before with 60s skew, nonce, azp (when
// present) and the verified-email requirement. Anything other than a
// fully-valid token returns ErrInvalidToken / ErrEmailUnverified; there is no
// partial-trust path and no "decode without verify" fallback.
func (p *Provider) VerifyIDToken(ctx context.Context, raw, expectedNonce string) (*Identity, error) {
	claims := &idTokenClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodRS256 {
			return nil, ErrInvalidToken
		}
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, ErrInvalidToken
		}
		return p.signingKey(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(p.issuer),
		jwt.WithAudience(p.clientID),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(clockLeeway),
	)
	if err != nil || token == nil || !token.Valid {
		return nil, ErrInvalidToken
	}
	// The nonce is the id_token replay guard bound to the one-time state row.
	if claims.Nonce == "" || expectedNonce == "" || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(expectedNonce)) != 1 {
		return nil, ErrInvalidToken
	}
	// azp is only checked when present (the task's contract); a provider that
	// emits it must name this client.
	if claims.AZP != "" && claims.AZP != p.clientID {
		return nil, ErrInvalidToken
	}
	if !claims.EmailVerified || strings.TrimSpace(claims.Email) == "" {
		return nil, ErrEmailUnverified
	}
	if _, err = auth.NormalizeEmail(claims.Email); err != nil {
		return nil, ErrEmailUnverified
	}
	return &Identity{
		Email:             claims.Email,
		PreferredUsername: truncateUTF8(claims.PreferredUsername, 256),
		Name:              truncateUTF8(claims.Name, 1024),
	}, nil
}

// signingKey returns the RS256 key for kid, refreshing the JWKS once when the
// kid is unknown. Key rotation is therefore picked up without a restart. The
// mutex serializes refreshes; every login attempt is bounded by the shared
// login rate limiter, so an unknown-kid request cannot drive unbounded refreshes.
func (p *Provider) signingKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.keysLoaded {
		if key, ok := p.keys[kid]; ok {
			return key, nil
		}
	}
	if err := p.refreshKeysLocked(ctx); err != nil {
		return nil, err
	}
	if key, ok := p.keys[kid]; ok {
		return key, nil
	}
	return nil, ErrInvalidToken
}

type jsonWebKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// refreshKeysLocked fetches and replaces the signing-key cache. Callers hold
// p.mu. Only RSA keys usable for RS256 signatures are accepted; anything else
// is ignored (and an empty usable set is an error so a broken JWKS cannot make
// verification silently succeed).
func (p *Provider) refreshKeysLocked(ctx context.Context) error {
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	body, err := p.getJSON(callCtx, p.jwksURL)
	if err != nil {
		return fmt.Errorf("OIDC JWKS fetch failed: %w", err)
	}
	var document struct {
		Keys []jsonWebKey `json:"keys"`
	}
	if err = json.Unmarshal(body, &document); err != nil {
		return errors.New("OIDC JWKS document is not valid JSON")
	}
	keys := make(map[string]*rsa.PublicKey, len(document.Keys))
	for _, key := range document.Keys {
		if key.Kty != "RSA" || key.Kid == "" {
			continue
		}
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		if key.Alg != "" && key.Alg != "RS256" {
			continue
		}
		publicKey, parseErr := parseRSAPublicKey(key.N, key.E)
		if parseErr != nil {
			continue
		}
		keys[key.Kid] = publicKey
	}
	if len(keys) == 0 {
		return errors.New("OIDC JWKS contains no usable RSA signing key")
	}
	p.keys = keys
	p.keysLoaded = true
	return nil
}

// parseRSAPublicKey converts JWK n/e members into an RSA public key and
// enforces the key-size bounds.
func parseRSAPublicKey(modulus, exponent string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(modulus)
	if err != nil || len(nBytes) == 0 {
		return nil, errors.New("invalid JWK modulus")
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(exponent)
	if err != nil || len(eBytes) == 0 || len(eBytes) > 4 {
		return nil, errors.New("invalid JWK exponent")
	}
	exponentValue := 0
	for _, b := range eBytes {
		exponentValue = exponentValue<<8 | int(b)
	}
	if exponentValue < 3 || exponentValue%2 == 0 {
		return nil, errors.New("invalid JWK exponent")
	}
	modulusValue := new(big.Int).SetBytes(nBytes)
	bits := modulusValue.BitLen()
	if bits < minRSAKeyBits || bits > maxRSAKeyBits {
		return nil, fmt.Errorf("unsupported RSA key size %d", bits)
	}
	return &rsa.PublicKey{N: modulusValue, E: exponentValue}, nil
}

// getJSON performs a bounded GET and returns the body of a 200 response. All
// provider calls go through here so the size cap and status rule cannot drift.
func (p *Provider) getJSON(ctx context.Context, rawURL string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errors.New("invalid provider endpoint")
	}
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return nil, errors.New("provider endpoint unreachable")
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider endpoint returned status %d", response.StatusCode)
	}
	return body, nil
}

// readBounded reads at most maxProviderResponse bytes and fails when the body
// is larger, instead of trusting a Content-Length header.
func readBounded(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxProviderResponse+1))
	if err != nil {
		return nil, errors.New("provider response could not be read")
	}
	if len(data) > maxProviderResponse {
		return nil, errors.New("provider response exceeds the size limit")
	}
	return data, nil
}

// validateProviderURL applies the endpoint safety rules from the discovery
// document: absolute http(s), no userinfo/query/fragment, https unless the host
// is loopback. A local test provider may use plain HTTP; production must not.
func validateProviderURL(name, raw string) (string, error) {
	if raw == "" || len(raw) > 2048 {
		return "", fmt.Errorf("OIDC discovery document is missing a valid %s", name)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return "", fmt.Errorf("OIDC discovery %s must be an absolute http(s) URL without userinfo, query or fragment", name)
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !loopbackHost(parsed.Hostname()) {
			return "", fmt.Errorf("OIDC discovery %s must use https unless the host is loopback", name)
		}
	default:
		return "", fmt.Errorf("OIDC discovery %s must be an http(s) URL", name)
	}
	return raw, nil
}

// loopbackHost reports whether host is a literal loopback address or the
// localhost name, matching the config package's endpoint rule.
func loopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// truncateUTF8 truncates on a rune boundary so a display claim can never store
// an invalid UTF-8 prefix.
func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes || !utf8.ValidString(value) {
		return value
	}
	for len(value) > maxBytes {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}
