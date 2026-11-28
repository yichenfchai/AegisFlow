// Package ldaptest provides an embedded, real LDAP v3 server for integration
// tests of the directory authentication client. It speaks the wire protocol over
// TCP (BER via the same github.com/go-asn1-ber/asn1-ber library the client
// uses), so tests exercise actual bind and search requests instead of a mocked
// interface: anonymous and service-account binds, subtree search, RFC 4511
// filters, size limits, StartTLS and LDAPS.
//
// The package is test support only: it listens on loopback, keeps entries in
// memory and is never imported by production code.
package ldaptest

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
)

// LDAP protocol constants used by the test server.
const (
	classUniversal                 = ber.ClassUniversal
	classApplication               = ber.ClassApplication
	classContext                   = ber.ClassContext
	typePrimitive                  = ber.TypePrimitive
	typeConstructed                = ber.TypeConstructed
	tagEnumerated                  = ber.TagEnumerated
	tagInteger                     = ber.TagInteger
	tagOctetString                 = ber.TagOctetString
	tagBoolean                     = ber.TagBoolean
	tagSequence                    = ber.TagSequence
	tagSet                         = ber.TagSet
	applicationBind                = 0
	applicationUnbind              = 2
	applicationSearch              = 3
	applicationEntry               = 4
	applicationDone                = 5
	applicationExtended            = 23
	applicationExtResp             = 24
	resultSuccess                  = 0
	resultProtocolError            = 2
	resultSizeLimitExceeded        = 4
	resultInvalidCredentials       = 49
	resultInsufficientAccessRights = 50
	resultUnwillingToPerform       = 53
	scopeBaseObject                = 0
	scopeSingleLevel               = 1
	scopeWholeSubtree              = 2
	filterAnd                      = 0
	filterOr                       = 1
	filterNot                      = 2
	filterEqualityMatch            = 3
	filterSubstrings               = 4
	filterPresent                  = 7
	startTLSOID                    = "1.3.6.1.4.1.1466.20037"
)

// Entry is one in-memory directory entry. Attributes are returned for search
// results; UserPassword is compared for simple binds and never returned unless
// a client explicitly requests it. All keys and DN comparisons are
// case-insensitive.
type Entry struct {
	DN           string
	Attributes   map[string][]string
	UserPassword []string
}

// Option configures the embedded server.
type Option func(*Server)

// WithTLS serves LDAPS: every accepted connection is a TLS connection.
func WithTLS(config *tls.Config) Option {
	return func(s *Server) { s.ldapsConfig = config }
}

// WithStartTLS accepts the StartTLS extended operation and upgrades that
// connection after an LDAP-layer success response.
func WithStartTLS(config *tls.Config) Option {
	return func(s *Server) { s.startTLSConfig = config }
}

// WithRequireBind rejects searches on connections that have not completed a
// successful bind, which proves the service-account bind path is really used.
func WithRequireBind() Option {
	return func(s *Server) { s.requireBind = true }
}

// WithHang accepts connections but never answers them, which exercises the
// client's request-level deadline.
func WithHang() Option {
	return func(s *Server) { s.hang = true }
}

// Server is a running embedded directory. Entries may be added or removed while
// the server is running; the tests usually add all entries before dialing.
type Server struct {
	url            string
	listener       net.Listener
	mu             sync.RWMutex
	entries        map[string]*Entry
	conns          map[net.Conn]struct{}
	closed         bool
	ldapsConfig    *tls.Config
	startTLSConfig *tls.Config
	requireBind    bool
	hang           bool
	wg             sync.WaitGroup
}

// Start listens on a random loopback port and registers a cleanup that stops
// the server when the test ends.
func Start(t testing.TB, options ...Option) *Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start embedded LDAP server: %v", err)
	}
	s := &Server{
		listener: listener,
		entries:  map[string]*Entry{},
		conns:    map[net.Conn]struct{}{},
	}
	for _, option := range options {
		option(s)
	}
	scheme := "ldap"
	if s.ldapsConfig != nil {
		scheme = "ldaps"
		s.listener = tls.NewListener(listener, s.ldapsConfig)
	}
	s.url = fmt.Sprintf("%s://%s", scheme, listener.Addr().String())
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.Stop)
	return s
}

// URL is the configured ldap:// or ldaps:// endpoint.
func (s *Server) URL() string { return s.url }

// AddEntry inserts or replaces one entry. UserPassword values are compared for
// binds only; Attributes are what searches can return.
func (s *Server) AddEntry(entry Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := Entry{DN: entry.DN, Attributes: map[string][]string{}, UserPassword: append([]string(nil), entry.UserPassword...)}
	for name, values := range entry.Attributes {
		copied.Attributes[name] = append([]string(nil), values...)
	}
	s.entries[strings.ToLower(strings.TrimSpace(entry.DN))] = &copied
}

// AddUser adds a person entry under ou=people with uid/mail/cn attributes and
// the bind password, returning its DN.
func (s *Server) AddUser(uid, mail, cn, password string) string {
	dn := fmt.Sprintf("uid=%s,ou=people,dc=example,dc=com", uid)
	attributes := map[string][]string{
		"objectClass": {"inetOrgPerson"},
		"uid":         {uid},
		"cn":          {cn},
		"sn":          {cn},
	}
	if mail != "" {
		attributes["mail"] = []string{mail}
	}
	s.AddEntry(Entry{DN: dn, Attributes: attributes, UserPassword: []string{password}})
	return dn
}

// AddMailUser adds a user whose username attribute and email differ, which the
// account-linkage tests need.
func (s *Server) AddMailUser(uid, mail, cn, password string) string {
	return s.AddUser(uid, mail, cn, password)
}

// Stop closes the listener and every live connection. It is idempotent.
func (s *Server) Stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	listener := s.listener
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	_ = listener.Close()
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.RLock()
			closed := s.closed
			s.mu.RUnlock()
			if closed {
				return
			}
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
			_ = conn.Close()
		}()
	}
}

// serveConn reads one LDAPMessage at a time. Requests are handled sequentially
// per connection; the client only issues one operation at a time, and this
// keeps the TLS upgrade after StartTLS race-free.
func (s *Server) serveConn(conn net.Conn) {
	if s.hang {
		// Read and discard forever so the client's own deadline is what stops
		// the operation.
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	authenticated := false
	for {
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		packet, err := ber.ReadPacket(conn)
		if err != nil {
			return
		}
		if len(packet.Children) < 2 {
			return
		}
		messageID, ok := packet.Children[0].Value.(int64)
		if !ok {
			return
		}
		operation := packet.Children[1]
		if operation.ClassType != classApplication {
			return
		}
		switch operation.Tag {
		case applicationBind:
			result := s.bind(operation)
			if result == resultSuccess {
				authenticated = true
			}
			s.writeBindResponse(conn, messageID, result)
		case applicationUnbind:
			return
		case applicationSearch:
			if s.requireBind && !authenticated {
				s.writeSearchDone(conn, messageID, resultInsufficientAccessRights)
				continue
			}
			s.handleSearch(conn, messageID, operation)
		case applicationExtended:
			upgraded, keep := s.handleExtended(conn, messageID, operation)
			if !keep {
				return
			}
			conn = upgraded
		default:
			return
		}
	}
}

// bind checks the simple bind credentials and reports the LDAP result code.
func (s *Server) bind(request *ber.Packet) int64 {
	if len(request.Children) < 3 {
		return resultProtocolError
	}
	name, _ := request.Children[1].Value.(string)
	auth := request.Children[2]
	if auth.ClassType != classContext || auth.Tag != 0 {
		// Only simple authentication is supported; SASL is not used by tests.
		return resultInvalidCredentials
	}
	password := string(auth.Data.Bytes())
	if strings.TrimSpace(name) == "" {
		// Anonymous simple bind is permitted only with an empty password.
		if password == "" {
			return resultSuccess
		}
		return resultInvalidCredentials
	}
	s.mu.RLock()
	entry := s.entries[strings.ToLower(strings.TrimSpace(name))]
	s.mu.RUnlock()
	if entry == nil {
		return resultInvalidCredentials
	}
	for _, candidate := range entry.UserPassword {
		if candidate == password {
			return resultSuccess
		}
	}
	return resultInvalidCredentials
}

// handleExtended implements the StartTLS extended operation. It returns the
// (possibly upgraded) connection and whether the session should continue.
func (s *Server) handleExtended(conn net.Conn, messageID int64, request *ber.Packet) (net.Conn, bool) {
	if len(request.Children) == 0 {
		s.writeExtendedResponse(conn, messageID, resultProtocolError, "")
		return conn, true
	}
	name, _ := request.Children[0].Value.(string)
	if name == "" {
		name = string(request.Children[0].Data.Bytes())
	}
	if name != startTLSOID {
		s.writeExtendedResponse(conn, messageID, resultProtocolError, "")
		return conn, true
	}
	if s.startTLSConfig == nil || s.ldapsConfig != nil {
		s.writeExtendedResponse(conn, messageID, resultUnwillingToPerform, "")
		return conn, true
	}
	// The success response is written before the TLS handshake, as RFC 4511
	// requires; the client then starts one TLS handshake on the same socket.
	s.writeExtendedResponse(conn, messageID, resultSuccess, "")
	tlsConn := tls.Server(conn, s.startTLSConfig)
	if err := tlsConn.Handshake(); err != nil {
		return conn, false
	}
	return tlsConn, true
}

// handleSearch evaluates the request against the in-memory entries. A size
// limit is honoured like a real directory: at most That many entries are
// returned and the result code is sizeLimitExceeded when more matched.
func (s *Server) handleSearch(conn net.Conn, messageID int64, request *ber.Packet) {
	if len(request.Children) < 8 {
		s.writeSearchDone(conn, messageID, resultProtocolError)
		return
	}
	base, _ := request.Children[0].Value.(string)
	scope, _ := request.Children[1].Value.(int64)
	sizeLimit, _ := request.Children[3].Value.(int64)
	filter := request.Children[6]
	attributes := requestedAttributes(request.Children[7])

	s.mu.RLock()
	entries := make([]*Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		entries = append(entries, entry)
	}
	s.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].DN < entries[j].DN })

	matched := make([]*Entry, 0, len(entries))
	for _, entry := range entries {
		if !inScope(entry.DN, base, int(scope)) {
			continue
		}
		if !matchesFilter(filter, entry.Attributes) {
			continue
		}
		matched = append(matched, entry)
	}
	truncated := sizeLimit > 0 && int64(len(matched)) > sizeLimit
	if truncated {
		matched = matched[:sizeLimit]
	}
	for _, entry := range matched {
		s.writeSearchEntry(conn, messageID, entry, attributes)
	}
	if truncated {
		s.writeSearchDone(conn, messageID, resultSizeLimitExceeded)
		return
	}
	s.writeSearchDone(conn, messageID, resultSuccess)
}

// requestedAttributes resolves the requested attribute descriptions; an empty
// list (or "*") returns every attribute.
func requestedAttributes(sequence *ber.Packet) []string {
	if sequence == nil {
		return nil
	}
	attributes := make([]string, 0, len(sequence.Children))
	for _, child := range sequence.Children {
		value, ok := child.Value.(string)
		if !ok || value == "*" {
			return nil
		}
		attributes = append(attributes, value)
	}
	return attributes
}

// inScope implements base, one-level and subtree scope with a case-insensitive
// DN comparison.
func inScope(dn, base string, scope int) bool {
	dn = strings.ToLower(strings.TrimSpace(dn))
	base = strings.ToLower(strings.TrimSpace(base))
	switch scope {
	case scopeBaseObject:
		return dn == base
	case scopeSingleLevel:
		if !strings.HasSuffix(dn, ","+base) {
			return false
		}
		relative := strings.TrimSuffix(dn, ","+base)
		return !strings.Contains(relative, ",")
	default:
		return dn == base || strings.HasSuffix(dn, ","+base)
	}
}

// matchesFilter evaluates the subset of RFC 4511 filters the tests need:
// and/or/not, equality, presence and substrings. Anything else does not match.
func matchesFilter(filter *ber.Packet, attributes map[string][]string) bool {
	if filter == nil || filter.ClassType != classContext {
		return false
	}
	switch filter.Tag {
	case filterAnd:
		for _, child := range filter.Children {
			if !matchesFilter(child, attributes) {
				return false
			}
		}
		return true
	case filterOr:
		for _, child := range filter.Children {
			if matchesFilter(child, attributes) {
				return true
			}
		}
		return false
	case filterNot:
		if len(filter.Children) != 1 {
			return false
		}
		return !matchesFilter(filter.Children[0], attributes)
	case filterEqualityMatch:
		if len(filter.Children) < 2 {
			return false
		}
		name, _ := filter.Children[0].Value.(string)
		assertion, _ := filter.Children[1].Value.(string)
		for _, value := range attributeValues(attributes, name) {
			if strings.EqualFold(value, assertion) {
				return true
			}
		}
		return false
	case filterPresent:
		name := string(filter.Data.Bytes())
		return len(attributeValues(attributes, name)) > 0
	case filterSubstrings:
		return matchesSubstrings(filter, attributes)
	}
	return false
}

// matchesSubstrings handles the initial/any/final substring components.
func matchesSubstrings(filter *ber.Packet, attributes map[string][]string) bool {
	if len(filter.Children) < 2 {
		return false
	}
	name, _ := filter.Children[0].Value.(string)
	sequence := filter.Children[1]
	var initial, final string
	var any []string
	for _, child := range sequence.Children {
		value := string(child.Data.Bytes())
		switch child.Tag {
		case 0:
			initial = value
		case 1:
			any = append(any, value)
		case 2:
			final = value
		}
	}
	for _, value := range attributeValues(attributes, name) {
		value = strings.ToLower(value)
		if initial != "" && !strings.HasPrefix(value, strings.ToLower(initial)) {
			continue
		}
		if final != "" && !strings.HasSuffix(value, strings.ToLower(final)) {
			continue
		}
		rest := strings.ToLower(strings.TrimPrefix(value, strings.ToLower(initial)))
		if final != "" {
			rest = strings.TrimSuffix(rest, strings.ToLower(final))
		}
		matched := true
		for _, segment := range any {
			index := strings.Index(rest, strings.ToLower(segment))
			if index < 0 {
				matched = false
				break
			}
			rest = rest[index+len(segment):]
		}
		if matched {
			return true
		}
	}
	return false
}

// attributeValues resolves an attribute by case-insensitive name.
func attributeValues(attributes map[string][]string, name string) []string {
	for key, values := range attributes {
		if strings.EqualFold(key, name) {
			return values
		}
	}
	return nil
}

func (s *Server) writePacket(conn net.Conn, messageID int64, protocolOp *ber.Packet) {
	message := ber.Encode(classUniversal, typeConstructed, tagSequence, nil, "LDAPMessage")
	message.AppendChild(ber.NewInteger(classUniversal, typePrimitive, tagInteger, messageID, "messageID"))
	message.AppendChild(protocolOp)
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, _ = conn.Write(message.Bytes())
}

func (s *Server) writeBindResponse(conn net.Conn, messageID int64, resultCode int64) {
	response := ber.Encode(classApplication, typeConstructed, applicationBind, nil, "BindResponse")
	response.AppendChild(ber.NewInteger(classUniversal, typePrimitive, tagEnumerated, resultCode, "resultCode"))
	response.AppendChild(ber.NewString(classUniversal, typePrimitive, tagOctetString, "", "matchedDN"))
	response.AppendChild(ber.NewString(classUniversal, typePrimitive, tagOctetString, "", "diagnosticMessage"))
	s.writePacket(conn, messageID, response)
}

func (s *Server) writeSearchEntry(conn net.Conn, messageID int64, entry *Entry, requested []string) {
	response := ber.Encode(classApplication, typeConstructed, applicationEntry, nil, "SearchResultEntry")
	response.AppendChild(ber.NewString(classUniversal, typePrimitive, tagOctetString, entry.DN, "objectName"))
	attributes := ber.NewSequence("attributes")
	names := make([]string, 0, len(entry.Attributes))
	for name := range entry.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !wantsAttribute(requested, name) {
			continue
		}
		partial := ber.NewSequence("partialAttribute")
		partial.AppendChild(ber.NewString(classUniversal, typePrimitive, tagOctetString, name, "type"))
		values := ber.Encode(classUniversal, typeConstructed, tagSet, nil, "vals")
		for _, value := range entry.Attributes[name] {
			values.AppendChild(ber.NewString(classUniversal, typePrimitive, tagOctetString, value, "value"))
		}
		partial.AppendChild(values)
		attributes.AppendChild(partial)
	}
	response.AppendChild(attributes)
	s.writePacket(conn, messageID, response)
}

// wantsAttribute resolves one requested attribute description, ignoring
// options such as ";binary".
func wantsAttribute(requested []string, name string) bool {
	if len(requested) == 0 {
		return true
	}
	for _, wanted := range requested {
		if strings.EqualFold(strings.SplitN(wanted, ";", 2)[0], name) {
			return true
		}
	}
	return false
}

func (s *Server) writeSearchDone(conn net.Conn, messageID int64, resultCode int64) {
	response := ber.Encode(classApplication, typeConstructed, applicationDone, nil, "SearchResultDone")
	response.AppendChild(ber.NewInteger(classUniversal, typePrimitive, tagEnumerated, resultCode, "resultCode"))
	response.AppendChild(ber.NewString(classUniversal, typePrimitive, tagOctetString, "", "matchedDN"))
	response.AppendChild(ber.NewString(classUniversal, typePrimitive, tagOctetString, "", "diagnosticMessage"))
	s.writePacket(conn, messageID, response)
}

func (s *Server) writeExtendedResponse(conn net.Conn, messageID int64, resultCode int64, responseName string) {
	response := ber.Encode(classApplication, typeConstructed, applicationExtResp, nil, "ExtendedResponse")
	response.AppendChild(ber.NewInteger(classUniversal, typePrimitive, tagEnumerated, resultCode, "resultCode"))
	response.AppendChild(ber.NewString(classUniversal, typePrimitive, tagOctetString, "", "matchedDN"))
	response.AppendChild(ber.NewString(classUniversal, typePrimitive, tagOctetString, "", "diagnosticMessage"))
	if responseName != "" {
		response.AppendChild(ber.NewString(classContext, typePrimitive, 10, responseName, "responseName"))
	}
	s.writePacket(conn, messageID, response)
}
