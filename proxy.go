package nodemaven

import (
	"context"
	"crypto/rand"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Param is one gateway parameter, as it will be sent.
type Param struct {
	Name  string
	Value string
}

// Proxy is one gateway identity: credentials, address and parameters. It is
// immutable; every method that changes something returns a new Proxy. Nothing
// here opens a connection except Check.
type Proxy struct {
	provider *Provider
	login    string
	password string
	host     string
	port     int
	params   []Param
}

// Option configures New.
type Option func(*settings)

type settings struct {
	provider *Provider
	login    string
	password string
	host     string
	port     int
	portSet  bool
	params   []Param
}

// WithProvider uses a definition other than the shipped NodeMaven one.
func WithProvider(p *Provider) Option { return func(s *settings) { s.provider = p } }

// WithLogin sets the proxy login. Without it, <ID>_LOGIN is read.
func WithLogin(login string) Option { return func(s *settings) { s.login = login } }

// WithPassword sets the proxy password. Without it, <ID>_PASSWORD is read.
func WithPassword(password string) Option { return func(s *settings) { s.password = password } }

// WithHost sets the gateway host. Without it, <ID>_HOST is read, then the
// definition's own host.
func WithHost(host string) Option { return func(s *settings) { s.host = host } }

// WithPort sets the gateway port. Without it, <ID>_PORT is read, then the
// definition's own port.
func WithPort(port int) Option {
	return func(s *settings) { s.port, s.portSet = port, true }
}

// WithParam sets one gateway parameter. Parameters go into the username in
// the order they are first set; setting a name again replaces its value in
// place.
func WithParam(name, value string) Option {
	return func(s *settings) { s.params = setParam(s.params, name, value) }
}

// New builds a proxy and validates every parameter against the definition.
// Credentials have to be present, never valid: nothing is sent.
func New(opts ...Option) (*Proxy, error) {
	var s settings
	for _, opt := range opts {
		opt(&s)
	}
	provider := s.provider
	if provider == nil {
		var err error
		if provider, err = Load(DefaultProvider); err != nil {
			return nil, err
		}
	}
	env := strings.ReplaceAll(strings.ToUpper(provider.id), "-", "_")
	login := firstNonEmpty(s.login, os.Getenv(env+"_LOGIN"))
	password := firstNonEmpty(s.password, os.Getenv(env+"_PASSWORD"))
	host := firstNonEmpty(s.host, os.Getenv(env+"_HOST"), provider.host)

	port := provider.port
	switch {
	case s.portSet:
		if s.port < 1 || s.port > 65535 {
			own := "not in this definition"
			if provider.port != 0 {
				own = strconv.Itoa(provider.port)
			}
			return nil, newError(ErrCredentials, fmt.Sprintf(
				"port %d is not a TCP port: it has to be a whole number from 1 to 65535. "+
					"Nothing was built. The gateway's own port is %s, and 0 is not a port at "+
					"all - it means \"any free port\" when binding and is meaningless when "+
					"connecting.", s.port, own))
		}
		port = s.port
	case os.Getenv(env+"_PORT") != "":
		text := os.Getenv(env + "_PORT")
		number, ok := portNumber(text)
		if !ok {
			return nil, newError(ErrCredentials, fmt.Sprintf(
				"%s_PORT is %q, which is not a TCP port: it has to be a whole number from 1 "+
					"to 65535, so nothing can connect. Unset it or pass WithPort.", env, text))
		}
		port = number
	}

	if login == "" || password == "" {
		var missing []string
		if login == "" {
			missing = append(missing, "login")
		}
		if password == "" {
			missing = append(missing, "password")
		}
		return nil, newError(ErrCredentials, fmt.Sprintf(
			"no %s for %s: pass WithLogin and WithPassword or set %s_LOGIN and %s_PASSWORD "+
				"in the environment. Nothing was built, so nothing will connect.",
			strings.Join(missing, " and no "), provider.label, env, env))
	}
	if host == "" || port == 0 {
		return nil, newError(ErrCredentials, fmt.Sprintf(
			"no gateway address for %s: pass WithHost and WithPort or set %s_HOST and %s_PORT.",
			provider.label, env, env))
	}
	params, err := validate(provider, s.params)
	if err != nil {
		return nil, err
	}
	return &Proxy{provider: provider, login: login, password: password, host: host, port: port, params: params}, nil
}

// Provider is the definition behind this proxy.
func (p *Proxy) Provider() *Provider { return p.provider }

// Params are the parameters as they will be sent, already folded. A copy.
func (p *Proxy) Params() []Param { return append([]Param(nil), p.params...) }

// Param returns the value of one parameter as it will be sent.
func (p *Proxy) Param(name string) (string, bool) {
	for _, param := range p.params {
		if param.Name == name {
			return param.Value, true
		}
	}
	return "", false
}

// Server is host:port, with no credentials in it.
func (p *Proxy) Server() string { return p.host + ":" + strconv.Itoa(p.port) }

// Username is the username in the gateway's dialect.
func (p *Proxy) Username() string {
	parts := make([]string, 0, len(p.params)+1)
	parts = append(parts, p.provider.renderPrefix(p.login))
	for _, param := range p.params {
		parts = append(parts, p.provider.Spell(param.Name)+p.provider.pairSeparator+param.Value)
	}
	return strings.Join(parts, p.provider.separator)
}

// URL is http://user:pass@host:port with both credentials percent-encoded.
func (p *Proxy) URL() string { return p.URLWithScheme("http") }

// URLWithScheme is URL with another scheme, such as socks5.
func (p *Proxy) URLWithScheme(scheme string) string {
	return scheme + "://" + percentEncode(p.Username()) + ":" + percentEncode(p.password) + "@" + p.Server()
}

// ProxyURL is the proxy as a *url.URL, for anything that takes one.
func (p *Proxy) ProxyURL() *url.URL {
	return &url.URL{Scheme: "http", User: url.UserPassword(p.Username(), p.password), Host: p.Server()}
}

// HTTPProxy is a value for http.Transport.Proxy that sends every request
// through this proxy:
//
//	client := &http.Client{Transport: &http.Transport{Proxy: proxy.HTTPProxy()}}
func (p *Proxy) HTTPProxy() func(*http.Request) (*url.URL, error) {
	return http.ProxyURL(p.ProxyURL())
}

// BrowserProxy is the shape browser automation takes a proxy in: the server
// without credentials, and the credentials separately and not encoded.
type BrowserProxy struct {
	Server   string
	Username string
	Password string
}

// String redacts the password.
func (b BrowserProxy) String() string {
	return fmt.Sprintf("BrowserProxy{Server: %q, Username: %q, Password: %q}", b.Server, b.Username, redacted)
}

// GoString redacts the password.
func (b BrowserProxy) GoString() string { return b.String() }

// Browser is the proxy for Playwright, chromedp, Rod and the like.
func (p *Proxy) Browser() BrowserProxy {
	return BrowserProxy{Server: "http://" + p.Server(), Username: p.Username(), Password: p.password}
}

// Connect is the CONNECT a Check would send, for changing its target or
// timeout before sending it.
func (p *Proxy) Connect() Connect {
	connect := NewConnect(p.Server(), p.Username(), p.password)
	connect.ExitIPHeaders = p.provider.ExitIPHeaders()
	connect.Reactions = make(map[string]string, len(p.provider.connectReactions))
	for status, meaning := range p.provider.connectReactions {
		connect.Reactions[status] = meaning
	}
	return connect
}

// Check opens one CONNECT through the gateway to DefaultTarget and returns
// its answer. It is the only call on a Proxy that opens a socket.
func (p *Proxy) Check(ctx context.Context) (*Check, error) {
	return p.Connect().Send(ctx)
}

// With returns a copy with one parameter set, validated.
func (p *Proxy) With(name, value string) (*Proxy, error) {
	return p.rebuilt(setParam(p.Params(), name, value))
}

// Without returns a copy with one parameter removed.
func (p *Proxy) Without(name string) (*Proxy, error) {
	params := make([]Param, 0, len(p.params))
	for _, param := range p.params {
		if param.Name != name {
			params = append(params, param)
		}
	}
	return p.rebuilt(params)
}

// Session returns a copy pinned to one sticky session.
func (p *Proxy) Session(id string) (*Proxy, error) {
	if p.provider.sessionParam == "" {
		return nil, newError(ErrParam, fmt.Sprintf(
			"%s declares no session parameter, so %q cannot be sent and every request will "+
				"leave from whichever exit the gateway picks.", p.provider.label, id))
	}
	return p.With(p.provider.sessionParam, id)
}

// Sessions returns count copies, each pinned to its own random session of
// 12 hexadecimal characters.
func (p *Proxy) Sessions(count int) ([]*Proxy, error) { return p.SessionsOfLength(count, 6) }

// SessionsOfLength is Sessions with ids of 2*length characters drawn from
// the operating system's random source. Ids are distinct within one call.
func (p *Proxy) SessionsOfLength(count, length int) ([]*Proxy, error) {
	if count < 1 {
		return nil, newError(ErrParam, fmt.Sprintf(
			"Sessions(%d) asks for no identities. Nothing would be returned and the call is "+
				"a mistake somewhere upstream.", count))
	}
	if length < 1 {
		return nil, newError(ErrParam, fmt.Sprintf("length=%d would produce an empty session id.", length))
	}
	separators := []string{p.provider.separator, p.provider.pairSeparator}
	var alphabet []byte
	for _, c := range []byte("0123456789abcdef") {
		if separators[0] != string(c) && separators[1] != string(c) {
			alphabet = append(alphabet, c)
		}
	}
	chars := saturatingMul(length, 2)
	space := 1
	for i := 0; i < chars && space <= count; i++ {
		space = saturatingMul(space, len(alphabet))
	}
	if space <= count {
		return nil, newError(ErrParam, fmt.Sprintf(
			"Sessions(%d, length=%d) asks for at least the whole space: %d characters from %d "+
				"symbols make at most %d distinct ids, and drawing without repeating is what "+
				"this does. Raise the length rather than the count, and note that ids are only "+
				"unique within one call - the space has to be large enough for every process "+
				"that draws from it.", count, length, chars, len(alphabet), space))
	}
	var longSeparators []string
	for _, separator := range separators {
		if len(separator) > 1 {
			longSeparators = append(longSeparators, separator)
		}
	}
	seen := make(map[string]bool, count)
	out := make([]*Proxy, 0, count)
	limit := saturatingMul(count, 1000)
	if limit < math.MaxInt-1000 {
		limit += 1000
	}
	for draws := 0; len(out) < count; {
		draws++
		if draws > limit {
			return nil, newError(ErrParam, fmt.Sprintf(
				"drew %d session ids of %d characters and found only %d that avoid the "+
					"separators %q. Raise the length.", limit, chars, len(out), separators))
		}
		id, err := draw(alphabet, chars)
		if err != nil {
			return nil, err
		}
		if containsAny(id, longSeparators) || seen[id] {
			continue
		}
		seen[id] = true
		pinned, err := p.Session(id)
		if err != nil {
			return nil, err
		}
		out = append(out, pinned)
	}
	return out, nil
}

// String redacts the password.
func (p *Proxy) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Proxy{provider: %q, server: %q, login: %q, password: %q",
		p.provider.id, p.Server(), p.login, redacted)
	for _, param := range p.params {
		fmt.Fprintf(&b, ", %s: %q", param.Name, param.Value)
	}
	b.WriteString("}")
	return b.String()
}

// GoString redacts the password, so %#v does not print it either.
func (p *Proxy) GoString() string { return p.String() }

func (p *Proxy) rebuilt(params []Param) (*Proxy, error) {
	checked, err := validate(p.provider, params)
	if err != nil {
		return nil, err
	}
	copied := *p
	copied.params = checked
	return &copied, nil
}

func validate(provider *Provider, params []Param) ([]Param, error) {
	var separators []string
	for _, separator := range []string{provider.separator, provider.pairSeparator} {
		if separator != "" && !contains(separators, separator) {
			separators = append(separators, separator)
		}
	}
	sort.Strings(separators)
	checked := make([]Param, 0, len(params))
	for _, param := range params {
		name := param.Name
		if !provider.knownParams[name] {
			return nil, newError(ErrParam, fmt.Sprintf(
				"%s does not know the parameter %q: it is answered with 200 and dropped, so "+
					"the connection would succeed and your setting would NOT be applied. "+
					"Known: %q", provider.label, name, provider.KnownParams()))
		}
		value := provider.Normalized(name, param.Value)
		if value == "" {
			return nil, newError(ErrParam, fmt.Sprintf(
				"empty value for %q: the gateway does not reply to this, the connection hangs "+
					"for about 20 s and then fails. Drop the parameter instead of passing an "+
					"empty value.", name))
		}
		if at := strings.IndexAny(value, ASCIIWhitespace); at >= 0 {
			var folded []string
			for _, known := range provider.KnownParams() {
				if provider.normalize[known] {
					folded = append(folded, known)
				}
			}
			return nil, newError(ErrParam, fmt.Sprintf(
				"the value of %q is %q and contains whitespace (%q), which cannot be sent: a "+
					"proxy username is a single token, so the value would be malformed or cut "+
					"short. %s folds a space to an underscore for %q and for nothing else, so "+
					"pass %q without whitespace.",
				name, value, value[at:at+1], provider.label, folded, name))
		}
		var bad []string
		for _, separator := range separators {
			if strings.Contains(value, separator) {
				bad = append(bad, separator)
			}
		}
		if len(bad) > 0 {
			return nil, newError(ErrParam, fmt.Sprintf(
				"the value of %q is %q and contains %q, which %s uses to separate parameters. "+
					"The username would be split into different settings than you asked for. "+
					"Session ids in particular cannot carry a %q - use %q or another character.",
				name, value, bad, provider.label, bad[0], strings.ReplaceAll(value, bad[0], "")))
		}
		if allowed, ok := provider.values[name]; ok && !contains(allowed, value) {
			return nil, newError(ErrParam, fmt.Sprintf(
				"%q is not a value %s accepts for %q. Allowed: %q. This one is worth catching "+
					"here because the gateway answers a bad value for some parameters with 407 "+
					"Proxy Authentication Required, which reads as a credentials problem and is "+
					"not one.", value, provider.label, name, allowed))
		}
		checked = append(checked, Param{Name: name, Value: value})
	}
	return checked, nil
}

// percentEncode keeps A-Z, a-z, 0-9 and -_.~ and encodes every other byte.
func percentEncode(text string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// draw takes chars symbols from alphabet, uniformly, by rejection sampling
// over the operating system's random source.
func draw(alphabet []byte, chars int) (string, error) {
	size := len(alphabet)
	limit := 256 - 256%size
	out := make([]byte, 0, chars)
	buffer := make([]byte, 64)
	for len(out) < chars {
		if _, err := rand.Read(buffer); err != nil {
			return "", &Error{Kind: ErrParam, cause: err, Message: fmt.Sprintf(
				"the operating system's random source failed (%v), so no session id could "+
					"be drawn safely.", err)}
		}
		for _, b := range buffer {
			if len(out) == chars {
				break
			}
			if int(b) < limit {
				out = append(out, alphabet[int(b)%size])
			}
		}
	}
	return string(out), nil
}

func setParam(params []Param, name, value string) []Param {
	for i := range params {
		if params[i].Name == name {
			params[i].Value = value
			return params
		}
	}
	return append(params, Param{Name: name, Value: value})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func contains(list []string, item string) bool {
	for _, x := range list {
		if x == item {
			return true
		}
	}
	return false
}

func containsAny(text string, parts []string) bool {
	for _, part := range parts {
		if strings.Contains(text, part) {
			return true
		}
	}
	return false
}

func saturatingMul(a, b int) int {
	if a != 0 && b > math.MaxInt/a {
		return math.MaxInt
	}
	return a * b
}
