// Package ldap authenticates a user against an external LDAP directory. The
// directory is only a credential source: this package verifies the password by
// binding as the single matched entry and returns the identity attributes; the
// auth package owns local account linkage and session issuance and this package
// never touches the database. It does not depend on Gin, and all configuration
// syntax checks already ran in internal/config.
//
// Security properties:
//   - the whole attempt (connect, optional StartTLS, service bind, search, user
//     bind) shares one deadline; a cancelled request closes the connection;
//   - the login name is escaped with RFC 4515 before it is substituted into the
//     configured filter, so directory metacharacters cannot change the filter;
//   - search has a size limit of two and a hard parse limit, so an ambiguous
//     directory match is detected without reading an unbounded result set;
//   - attribute values are capped and control characters are rejected before
//     they can reach the local account layer;
//   - passwords, bind credentials and entry DNs are never logged, and directory
//     errors only reach callers through the fixed sentinels below.
package ldap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	gldap "github.com/go-ldap/ldap/v3"
)

var (
	// ErrInvalidCredentials covers a wrong password, an unknown directory user
	// and a directory entry with an unusable username. Callers must answer 401
	// without revealing which case happened.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrAmbiguous reports more than one directory entry matching the filter.
	// It is answered as a generic 401 (a directory data problem, not an
	// authorization decision) and logged with attribute counts only.
	ErrAmbiguous = errors.New("ambiguous directory match")
	// ErrUnavailable reports an unreachable directory, a timeout, a failed
	// StartTLS handshake or a failed service-account bind. Callers answer 503
	// with a generic message and never expose the endpoint or directory error.
	ErrUnavailable = errors.New("ldap directory unavailable")
	// ErrInvalidEntry reports an unusable identity attribute (oversized or
	// containing control characters). It is answered as a generic 401 as well.
	ErrInvalidEntry = errors.New("invalid directory entry")
)

const (
	// MaxAttributeBytes bounds one identity attribute value read from the
	// directory; anything longer is not a usable username/email/display name.
	MaxAttributeBytes = 1024
	// MaxUsernameBytes and MaxPasswordBytes mirror the HTTP request bounds so
	// the client rejects oversized credentials before dialing.
	MaxUsernameBytes = 256
	MaxPasswordBytes = 1024
	// maxTimeLimitSeconds is the search time limit sent to the directory; the
	// local deadline is authoritative and always shorter.
	maxTimeLimitSeconds = 30
)

// Identity is the verified identity of one directory user. Email may be empty
// when the directory has no usable address; local account provisioning then
// refuses to create an account (see auth.LoginLDAP).
type Identity struct {
	Username    string
	Email       string
	DisplayName string
	// DN is used for the user bind and never leaves this package or the auth
	// package's internal control flow; it is never logged or persisted.
	DN string
}

// Options is the validated directory configuration. The zero value is not
// usable: callers only build a Client when the feature is enabled.
type Options struct {
	URL                  string
	StartTLS             bool
	BindDN               string
	BindPassword         string
	BaseDN               string
	UserFilter           string
	UsernameAttribute    string
	EmailAttribute       string
	DisplayNameAttribute string
	Label                string
	Timeout              time.Duration
	// TLSConfig overrides the client TLS configuration. Production leaves it
	// nil, which means the system roots and TLS 1.2+; tests use it to trust a
	// throwaway certificate for a loopback directory.
	TLSConfig *tls.Config
	// Logger receives sanitized operational warnings; nil uses slog.Default().
	Logger *slog.Logger
}

// Client is one immutable directory configuration. It is safe for concurrent
// use: every authentication opens its own connection.
type Client struct {
	options Options
	logger  *slog.Logger
	host    string
}

// New validates the structural invariants the configuration layer already
// enforces (defense in depth for programmatic callers) and returns a client.
func New(options Options) (*Client, error) {
	if options.URL == "" {
		return nil, errors.New("ldap: URL is required")
	}
	if options.Timeout < time.Second {
		return nil, errors.New("ldap: a timeout of at least one second is required")
	}
	if options.BaseDN == "" {
		return nil, errors.New("ldap: search base DN is required")
	}
	if strings.Count(options.UserFilter, "{username}") != 1 {
		return nil, errors.New("ldap: user filter must contain exactly one {username} placeholder")
	}
	parsed, err := url.Parse(options.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "ldap" && parsed.Scheme != "ldaps") {
		return nil, errors.New("ldap: URL must be an absolute ldap:// or ldaps:// URL")
	}
	if options.StartTLS && parsed.Scheme == "ldaps" {
		return nil, errors.New("ldap: StartTLS cannot be combined with ldaps://")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{options: options, logger: logger, host: parsed.Hostname()}, nil
}

// Label is the login-page entry text; never empty when a client exists.
func (c *Client) Label() string { return c.options.Label }

// Authenticate verifies username/password against the directory and returns the
// canonical identity. A nil password never reaches the directory (an
// unauthenticated simple bind would succeed as anonymous). All failure modes
// are represented by the package sentinels; the underlying error is preserved
// for server-side logging but callers must not expose it.
func (c *Client) Authenticate(ctx context.Context, username, password string) (*Identity, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > MaxUsernameBytes || !utf8.ValidString(username) ||
		strings.IndexFunc(username, unicode.IsControl) >= 0 {
		return nil, ErrInvalidCredentials
	}
	if password == "" || len(password) > MaxPasswordBytes {
		return nil, ErrInvalidCredentials
	}
	// One deadline for connect, StartTLS, service bind, search and the user
	// bind. A caller cancellation closes the connection so a blocked read
	// cannot outlive the HTTP request.
	opCtx, cancel := context.WithTimeout(ctx, c.options.Timeout)
	defer cancel()
	dialer := &net.Dialer{Timeout: c.options.Timeout}
	conn, err := gldap.DialURL(c.options.URL, gldap.DialWithDialer(dialer), gldap.DialWithTLSConfig(c.tlsConfig()))
	if err != nil {
		return nil, fmt.Errorf("%w: dial: %v", ErrUnavailable, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(opCtx, func() { _ = conn.Close() })
	defer stop()
	setDeadline := func() bool {
		remaining := time.Until(deadlineOf(opCtx))
		if remaining <= 0 {
			return false
		}
		conn.SetTimeout(remaining)
		return true
	}
	if !setDeadline() {
		return nil, ErrUnavailable
	}
	if c.options.StartTLS {
		// StartTLS only ever runs over a plaintext ldap:// connection (rejected
		// at configuration time otherwise) and before any credential is sent.
		if err = conn.StartTLS(c.tlsConfig()); err != nil {
			return nil, fmt.Errorf("%w: starttls: %v", ErrUnavailable, err)
		}
	}
	if c.options.BindDN != "" {
		// A failed service bind is an operator problem (wrong credentials or an
		// unreachable directory), not a user credential failure: answer 503
		// instead of letting every login look like a normal wrong password.
		if !setDeadline() {
			return nil, ErrUnavailable
		}
		if err = conn.Bind(c.options.BindDN, c.options.BindPassword); err != nil {
			return nil, fmt.Errorf("%w: service bind: %v", ErrUnavailable, err)
		}
	}
	if !setDeadline() {
		return nil, ErrUnavailable
	}
	filter := strings.Replace(c.options.UserFilter, "{username}", gldap.EscapeFilter(username), 1)
	attributes := []string{c.options.UsernameAttribute, c.options.EmailAttribute, c.options.DisplayNameAttribute}
	request := gldap.NewSearchRequest(
		c.options.BaseDN,
		gldap.ScopeWholeSubtree,
		gldap.NeverDerefAliases,
		2, // detecting an ambiguous match needs at most two entries
		searchTimeLimit(opCtx),
		false,
		filter,
		attributes,
		nil,
	)
	// Never read more than the size limit even if a broken server ignores it.
	request.EnforceSizeLimit = true
	result, err := conn.Search(request)
	if err != nil {
		if result != nil && len(result.Entries) > 1 {
			c.logAmbiguous(len(result.Entries))
			return nil, ErrAmbiguous
		}
		if isSizeLimitExceeded(err) {
			c.logAmbiguous(len(result.Entries))
			return nil, ErrAmbiguous
		}
		return nil, fmt.Errorf("%w: search: %v", ErrUnavailable, err)
	}
	switch len(result.Entries) {
	case 0:
		return nil, ErrInvalidCredentials
	case 1:
	default:
		c.logAmbiguous(len(result.Entries))
		return nil, ErrAmbiguous
	}
	entry := result.Entries[0]
	identifier, ok := c.readAttribute(entry, c.options.UsernameAttribute)
	if !ok {
		c.logInvalidEntry("username")
		return nil, ErrInvalidEntry
	}
	if identifier == "" {
		// The configured attribute may be absent from an otherwise matching
		// entry; the submitted login name is the fallback local identifier.
		identifier = username
	}
	email, ok := c.readAttribute(entry, c.options.EmailAttribute)
	if !ok {
		// An unusable email is treated as absent: it can never link or create
		// an account, but a username-based local match may still succeed.
		email = ""
	}
	displayName, ok := c.readAttribute(entry, c.options.DisplayNameAttribute)
	if !ok {
		displayName = ""
	}
	// The password is verified last, with the user's own DN, so a directory
	// entry is never trusted as an identity before its user credential passed.
	if !setDeadline() {
		return nil, ErrUnavailable
	}
	if err = conn.Bind(entry.DN, password); err != nil {
		if gldap.IsErrorWithCode(err, gldap.LDAPResultInvalidCredentials) {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("%w: user bind: %v", ErrUnavailable, err)
	}
	return &Identity{Username: identifier, Email: email, DisplayName: displayName, DN: entry.DN}, nil
}

// readAttribute returns the first value of one attribute. ok=false reports an
// invalid value (oversized or containing control characters); an absent or
// empty attribute returns ("", true) so the caller can apply its fallback.
func (c *Client) readAttribute(entry *gldap.Entry, attribute string) (string, bool) {
	values := entry.GetAttributeValues(attribute)
	if len(values) == 0 {
		return "", true
	}
	value := values[0]
	if len(value) > MaxAttributeBytes || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", false
	}
	return strings.TrimSpace(value), true
}

// logAmbiguous records a directory-data warning without the login name, the
// filter value or any entry DN. Only the search base and the match count help
// an operator diagnose a misconfigured directory.
func (c *Client) logAmbiguous(entries int) {
	c.logger.Warn("ldap search matched multiple entries", "baseDn", c.options.BaseDN, "entries", entries)
}

// logInvalidEntry records an unusable identity attribute without its value.
func (c *Client) logInvalidEntry(attribute string) {
	c.logger.Warn("ldap entry attribute is unusable", "attribute", attribute)
}

// tlsConfig builds the client TLS configuration. The override exists for tests
// only; production uses the system roots with TLS 1.2 as the floor. The server
// name is always the configured host so certificate validation is host-bound.
func (c *Client) tlsConfig() *tls.Config {
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.host}
	if c.options.TLSConfig != nil {
		config = c.options.TLSConfig.Clone()
		if config.ServerName == "" {
			config.ServerName = c.host
		}
		if config.MinVersion == 0 {
			config.MinVersion = tls.VersionTLS12
		}
	}
	return config
}

// deadlineOf returns the deadline of a context; contexts created here always
// have one. The zero time means no deadline, which callers treat as expired.
func deadlineOf(ctx context.Context) time.Time {
	deadline, ok := ctx.Deadline()
	if !ok {
		return time.Time{}
	}
	return deadline
}

// searchTimeLimit converts the remaining budget into the LDAP timeLimit field,
// rounded up and bounded so a directory cannot hold the connection past the
// local deadline.
func searchTimeLimit(ctx context.Context) int {
	remaining := time.Until(deadlineOf(ctx))
	if remaining <= 0 {
		return 1
	}
	seconds := int((remaining + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	if seconds > maxTimeLimitSeconds {
		seconds = maxTimeLimitSeconds
	}
	return seconds
}

// isSizeLimitExceeded reports the directory's size-limit result code, which is
// how a real server announces that more entries matched than were requested.
func isSizeLimitExceeded(err error) bool {
	return gldap.IsErrorWithCode(err, gldap.LDAPResultSizeLimitExceeded) || errors.Is(err, gldap.ErrSizeLimitExceeded)
}
