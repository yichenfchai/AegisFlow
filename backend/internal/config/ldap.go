package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-ldap/ldap/v3"
)

// LDAP environment variable names. They are collected here so the partial
// configuration check and the tests cannot drift from the parser.
const (
	ldapURLVar                 = "AEGIS_LDAP_URL"
	ldapStartTLSVar            = "AEGIS_LDAP_STARTTLS"
	ldapBindDNVar              = "AEGIS_LDAP_BIND_DN"
	ldapBindPasswordVar        = "AEGIS_LDAP_BIND_PASSWORD"
	ldapBaseDNVar              = "AEGIS_LDAP_BASE_DN"
	ldapUserFilterVar          = "AEGIS_LDAP_USER_FILTER"
	ldapUsernameAttrVar        = "AEGIS_LDAP_USERNAME_ATTRIBUTE"
	ldapEmailAttrVar           = "AEGIS_LDAP_EMAIL_ATTRIBUTE"
	ldapDisplayNameAttrVar     = "AEGIS_LDAP_DISPLAY_NAME_ATTRIBUTE"
	ldapAutoProvisionVar       = "AEGIS_LDAP_AUTO_PROVISION"
	ldapLabelVar               = "AEGIS_LDAP_LABEL"
	ldapTimeoutVar             = "AEGIS_LDAP_TIMEOUT"
	ldapUsernamePlaceholder    = "{username}"
	defaultLDAPUserFilter      = "(uid={username})"
	defaultLDAPUsernameAttr    = "uid"
	defaultLDAPEmailAttr       = "mail"
	defaultLDAPDisplayNameAttr = "cn"
	defaultLDAPLabel           = "LDAP 登录"
)

// ldapAttributePattern is the RFC 4512 attribute description shape accepted for
// the three identity attributes: an ASCII letter followed by letters, digits or
// hyphens, optionally followed by one or more ";option" segments.
var ldapAttributePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*(?:;[A-Za-z0-9-]+)*$`)

// loadLDAP parses and validates the optional LDAP directory settings. It never
// reaches the network: a directory that is unreachable at runtime answers
// 503 on the login endpoint instead of preventing the server from starting.
func loadLDAP(c *Config) error {
	rawURL := strings.TrimSpace(os.Getenv(ldapURLVar))
	if rawURL == "" {
		// Fail closed on partial configuration instead of silently starting a
		// server whose operator believes directory login is enabled.
		for _, key := range []string{
			ldapStartTLSVar, ldapBindDNVar, ldapBindPasswordVar, ldapBaseDNVar,
			ldapUserFilterVar, ldapUsernameAttrVar, ldapEmailAttrVar,
			ldapDisplayNameAttrVar, ldapAutoProvisionVar, ldapLabelVar, ldapTimeoutVar,
		} {
			if os.Getenv(key) != "" {
				return fmt.Errorf("%s requires %s; LDAP directory authentication is disabled when the URL is unset", key, ldapURLVar)
			}
		}
		return nil
	}
	rawURL = strings.TrimRight(rawURL, "/")
	if len(rawURL) > 2048 || !utf8.ValidString(rawURL) || strings.IndexFunc(rawURL, unicode.IsControl) >= 0 || strings.ContainsAny(rawURL, " \t") {
		return fmt.Errorf("%s must be a single-line ldap:// or ldaps:// URL of at most 2048 bytes", ldapURLVar)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return fmt.Errorf("%s must be an absolute ldap:// or ldaps:// URL with a host", ldapURLVar)
	}
	if parsed.Scheme != "ldap" && parsed.Scheme != "ldaps" {
		return fmt.Errorf("%s must use ldap:// or ldaps://, got scheme %q", ldapURLVar, parsed.Scheme)
	}
	if parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.Path != "" {
		return fmt.Errorf("%s must not contain userinfo, a path, a query or a fragment", ldapURLVar)
	}
	if port := parsed.Port(); port != "" {
		value, convErr := strconv.Atoi(port)
		if convErr != nil || value < 1 || value > 65535 {
			return fmt.Errorf("%s must use a numeric port between 1 and 65535", ldapURLVar)
		}
	}
	c.LDAP.URL = rawURL
	startTLS, err := parseBool(ldapStartTLSVar, false)
	if err != nil {
		return err
	}
	if startTLS && parsed.Scheme == "ldaps" {
		return errors.New("AEGIS_LDAP_STARTTLS must not be combined with an ldaps:// URL, which is already encrypted")
	}
	// A plain connection sends the service bind and the user password in
	// cleartext, so it is only acceptable over loopback (local test/dev) or
	// with StartTLS. This mirrors the review/approval webhook rule.
	if parsed.Scheme == "ldap" && !startTLS && !isLoopbackHost(parsed.Hostname()) {
		return errors.New("AEGIS_LDAP_URL must use ldaps:// or AEGIS_LDAP_STARTTLS=true unless the host is loopback")
	}
	c.LDAP.StartTLS = startTLS
	bindDN := os.Getenv(ldapBindDNVar)
	bindPassword := os.Getenv(ldapBindPasswordVar)
	if (bindDN == "") != (bindPassword == "") {
		return errors.New("AEGIS_LDAP_BIND_DN and AEGIS_LDAP_BIND_PASSWORD must be configured together, or both left empty for anonymous search")
	}
	if bindDN != "" {
		if !validLDAPDN(bindDN) {
			return errors.New("AEGIS_LDAP_BIND_DN must be a valid single-line LDAP distinguished name of at most 1024 bytes")
		}
		if !utf8.ValidString(bindPassword) || len(bindPassword) > 1024 || strings.IndexFunc(bindPassword, unicode.IsControl) >= 0 {
			return errors.New("AEGIS_LDAP_BIND_PASSWORD must be valid UTF-8 of at most 1024 bytes without control characters")
		}
		c.LDAP.BindDN = bindDN
		c.LDAP.BindPassword = bindPassword
	}
	baseDN := strings.TrimSpace(os.Getenv(ldapBaseDNVar))
	if !validLDAPDN(baseDN) {
		return errors.New("AEGIS_LDAP_BASE_DN is required and must be a valid single-line LDAP distinguished name of at most 1024 bytes")
	}
	c.LDAP.BaseDN = baseDN
	filter := strings.TrimSpace(env(ldapUserFilterVar, defaultLDAPUserFilter))
	if len(filter) > 1024 || strings.IndexFunc(filter, unicode.IsControl) >= 0 || strings.Count(filter, ldapUsernamePlaceholder) != 1 {
		return errors.New("AEGIS_LDAP_USER_FILTER must contain exactly one {username} placeholder and be at most 1024 bytes")
	}
	// Validate the filter grammar with an inert stand-in for the placeholder:
	// escaping happens per login, so only the literal template can be checked.
	if _, err = ldap.CompileFilter(strings.ReplaceAll(filter, ldapUsernamePlaceholder, "aegisflow-placeholder")); err != nil {
		return errors.New("AEGIS_LDAP_USER_FILTER is not a valid RFC 4515 search filter")
	}
	c.LDAP.UserFilter = filter
	if c.LDAP.UsernameAttribute, err = ldapAttribute(ldapUsernameAttrVar, defaultLDAPUsernameAttr); err != nil {
		return err
	}
	if c.LDAP.EmailAttribute, err = ldapAttribute(ldapEmailAttrVar, defaultLDAPEmailAttr); err != nil {
		return err
	}
	if c.LDAP.DisplayNameAttribute, err = ldapAttribute(ldapDisplayNameAttrVar, defaultLDAPDisplayNameAttr); err != nil {
		return err
	}
	c.LDAP.AutoProvision, err = parseBool(ldapAutoProvisionVar, false)
	if err != nil {
		return err
	}
	c.LDAP.Label = strings.TrimSpace(env(ldapLabelVar, defaultLDAPLabel))
	if !validOIDCText(c.LDAP.Label, 1, 128) {
		return errors.New("AEGIS_LDAP_LABEL must be a non-empty single line of at most 128 bytes")
	}
	c.LDAP.Timeout, err = duration(ldapTimeoutVar, "5s")
	if err != nil {
		return err
	}
	if c.LDAP.Timeout > 30*time.Second {
		return errors.New("AEGIS_LDAP_TIMEOUT must be at most 30s")
	}
	return nil
}

// ldapAttribute resolves one identity attribute name from the environment with
// its documented default and validates the RFC 4512 attribute description.
func ldapAttribute(key, fallback string) (string, error) {
	value := strings.TrimSpace(env(key, fallback))
	if len(value) > 64 || !ldapAttributePattern.MatchString(value) {
		return "", fmt.Errorf("%s must be an LDAP attribute name of at most 64 bytes (letters, digits and inner hyphens)", key)
	}
	return value, nil
}

// validLDAPDN accepts a non-empty single-line distinguished name at most 1024
// bytes long that the LDAP client can parse, so a malformed DN fails startup
// instead of the first login attempt.
func validLDAPDN(raw string) bool {
	if raw == "" || len(raw) > 1024 || !utf8.ValidString(raw) || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return false
	}
	_, err := ldap.ParseDN(raw)
	return err == nil
}
