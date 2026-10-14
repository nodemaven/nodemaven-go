package nodemaven

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultProvider is the id of the definition used when none is given.
const DefaultProvider = "nodemaven"

// ASCIIWhitespace is every character treated as whitespace in a parameter
// value. It is spelled out rather than taken from a standard library: Go's
// unicode.IsSpace also matches U+0085 and U+00A0, and the rule has to be the
// same in every NodeMaven SDK.
const ASCIIWhitespace = " \t\n\r\v\f"

//go:embed data/nodemaven.toml
var shippedNodemaven string

var shipped = map[string]string{"nodemaven": shippedNodemaven}

// Provider describes one gateway's username dialect: its separators, the
// parameter names it recognises, how values are folded, and what its CONNECT
// statuses mean. Build one with Load, LoadFile or LoadString, or with
// NewProvider for a definition written in code.
type Provider struct {
	id               string
	label            string
	knownParams      map[string]bool
	status           string
	prefix           string
	separator        string
	pairSeparator    string
	sessionParam     string
	host             string
	port             int
	aliases          map[string]string
	values           map[string][]string
	normalize        map[string]bool
	connectReactions map[string]string
	exitIPHeaders    []string
	source           string
	sourceRead       string
	notes            string
}

// ID is the definition's id, which names its environment variables.
func (p *Provider) ID() string { return p.id }

// Label is the gateway's display name.
func (p *Provider) Label() string { return p.label }

// KnownParams lists the recognised parameter names, sorted.
func (p *Provider) KnownParams() []string { return sortedKeys(p.knownParams) }

// Status is "measured" when the dialect was read off the wire and
// "documented" when it was transcribed and never exercised.
func (p *Provider) Status() string { return p.status }

// IsMeasured reports whether Status is "measured".
func (p *Provider) IsMeasured() bool { return p.status == "measured" }

// Separator joins the parameters in a username.
func (p *Provider) Separator() string { return p.separator }

// PairSeparator joins a parameter's name to its value.
func (p *Provider) PairSeparator() string { return p.pairSeparator }

// SessionParam names the sticky-session parameter, or "" when there is none.
func (p *Provider) SessionParam() string { return p.sessionParam }

// Host is the gateway host from the definition, or "".
func (p *Provider) Host() string { return p.host }

// Port is the gateway port from the definition, or 0.
func (p *Provider) Port() int { return p.port }

// ExitIPHeaders lists every response header the exit address may arrive
// on, in the order they are tried.
func (p *Provider) ExitIPHeaders() []string { return append([]string(nil), p.exitIPHeaders...) }

// Notes is the definition's free-text notes.
func (p *Provider) Notes() string { return p.notes }

// Spell is the name the gateway uses on the wire for a canonical parameter.
func (p *Provider) Spell(name string) string {
	if wire, ok := p.aliases[name]; ok {
		return wire
	}
	return name
}

// Reaction is what a CONNECT status means on this gateway, or "" when
// nobody has recorded one.
func (p *Provider) Reaction(status int) string {
	return p.connectReactions[strconv.Itoa(status)]
}

// Allowed lists the legal values of a parameter, or nil when nobody has
// established them and any value is passed through.
func (p *Provider) Allowed(name string) []string {
	if values, ok := p.values[name]; ok {
		return append([]string(nil), values...)
	}
	return nil
}

// Normalizes reports whether values of this parameter are folded.
func (p *Provider) Normalizes(name string) bool { return p.normalize[name] }

// Normalized is the wire form of a value: for a folded parameter, trimmed of
// ASCII whitespace, A-Z lowered to a-z and nothing else, and each space turned
// into an underscore. Other parameters are returned unchanged.
func (p *Provider) Normalized(name, value string) string {
	if !p.normalize[name] {
		return value
	}
	return strings.ReplaceAll(asciiLower(strings.Trim(value, ASCIIWhitespace)), " ", "_")
}

func (p *Provider) renderPrefix(login string) string {
	return strings.ReplaceAll(p.prefix, "{login}", login)
}

// asciiLower lowers A-Z only. strings.ToLower is Unicode-aware and folds
// characters such as U+0130 in a way other languages do not, which would make
// the same input build different usernames in different SDKs.
func asciiLower(text string) string {
	out := []byte(text)
	for i, b := range out {
		if b >= 'A' && b <= 'Z' {
			out[i] = b + ('a' - 'A')
		}
	}
	return string(out)
}

// Available lists the ids of the shipped definitions, sorted.
func Available() []string {
	ids := make([]string, 0, len(shipped))
	for id := range shipped {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Load returns a shipped definition.
func Load(id string) (*Provider, error) {
	text, ok := shipped[id]
	if !ok {
		return nil, newError(ErrProvider, fmt.Sprintf(
			"no provider definition %q is shipped here, so no username can be built for it. "+
				"Shipped: %q. To use a gateway that is not in that list, write a .toml for it "+
				"and pass it to LoadFile().", id, Available()))
	}
	return parse(text, id, "the shipped "+id+" definition")
}

// LoadFile reads a definition from a TOML file. Its id is the file name
// without the extension.
func LoadFile(path string) (*Provider, error) {
	id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return LoadFileAs(path, id)
}

// LoadFileAs reads a definition from a TOML file under the given id.
func LoadFileAs(path, id string) (*Provider, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, newError(ErrProvider, fmt.Sprintf("cannot read the provider definition %s: %v", path, err))
	}
	return parse(string(data), id, path)
}

// LoadString reads a definition from TOML text.
func LoadString(text, id string) (*Provider, error) {
	return parse(text, id, "the definition passed in")
}

// ProviderSpec describes a definition written in code. Zero values take the
// same defaults as a file that leaves the key out.
type ProviderSpec struct {
	ID               string
	Label            string
	KnownParams      []string
	Status           string
	Prefix           string
	Separator        string
	PairSeparator    string
	SessionParam     string
	Host             string
	Port             int
	Aliases          map[string]string
	Values           map[string][]string
	Normalize        []string
	ConnectReactions map[int]string
	ExitIPHeaders    []string
	Notes            string
}

// NewProvider builds a definition in code and runs the same consistency
// checks a definition read from a file goes through.
func NewProvider(spec ProviderSpec) (*Provider, error) {
	if spec.ID == "" || spec.Label == "" || len(spec.KnownParams) == 0 {
		return nil, newError(ErrProvider,
			"this definition needs an ID, a Label and KnownParams. Without KnownParams nothing "+
				"can be validated, and an unknown parameter name is the one mistake a gateway "+
				"does not report.")
	}
	p := blank(spec.ID, spec.Label)
	for _, name := range spec.KnownParams {
		p.knownParams[name] = true
	}
	setIf(&p.status, spec.Status)
	setIf(&p.prefix, spec.Prefix)
	setIf(&p.separator, spec.Separator)
	setIf(&p.pairSeparator, spec.PairSeparator)
	p.sessionParam = spec.SessionParam
	p.host = spec.Host
	if spec.Port != 0 {
		if spec.Port < 1 || spec.Port > 65535 {
			return nil, newError(ErrProvider, fmt.Sprintf(
				"this definition gives port as %d. It has to be a whole number from 1 to 65535.", spec.Port))
		}
		p.port = spec.Port
	}
	for k, v := range spec.Aliases {
		p.aliases[k] = v
	}
	for k, v := range spec.Values {
		if len(v) == 0 {
			return nil, newError(ErrProvider, fmt.Sprintf(
				"this definition gives the legal values of %q as an empty list; leave the entry "+
					"out to mean nobody has established them.", k))
		}
		p.values[k] = append([]string(nil), v...)
	}
	for _, name := range spec.Normalize {
		p.normalize[name] = true
	}
	for status, meaning := range spec.ConnectReactions {
		if meaning == "" {
			return nil, newError(ErrProvider, fmt.Sprintf(
				"this definition describes CONNECT status %d with an empty sentence.", status))
		}
		p.connectReactions[strconv.Itoa(status)] = meaning
	}
	p.exitIPHeaders = append([]string(nil), spec.ExitIPHeaders...)
	p.notes = spec.Notes
	foldValues(p)
	if err := check(p, "this definition"); err != nil {
		return nil, err
	}
	return p, nil
}

func blank(id, label string) *Provider {
	return &Provider{
		id: id, label: label,
		knownParams: map[string]bool{}, status: "documented", prefix: "{login}",
		separator: "-", pairSeparator: "-",
		aliases: map[string]string{}, values: map[string][]string{},
		normalize: map[string]bool{}, connectReactions: map[string]string{},
	}
}

func setIf(field *string, value string) {
	if value != "" {
		*field = value
	}
}

func parse(text, id, origin string) (*Provider, error) {
	if id == "" {
		return nil, newError(ErrProvider, fmt.Sprintf(
			"%s has no id, and the id is what names its environment variables. Pass one "+
				"to LoadFileAs or LoadString.", origin))
	}
	raw := map[string]any{}
	if _, err := toml.Decode(text, &raw); err != nil {
		return nil, newError(ErrProvider, fmt.Sprintf("%s is not valid TOML: %v", origin, err))
	}
	var missing []string
	for _, key := range []string{"label", "known_params"} {
		if _, ok := raw[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, newError(ErrProvider, fmt.Sprintf(
			"%s is missing %q, so it does not describe a gateway. Without known_params nothing "+
				"can be validated, and an unknown parameter name is the one mistake a gateway "+
				"does not report.", origin, missing))
	}
	label, err := tomlString(raw, "label", origin)
	if err != nil {
		return nil, err
	}
	p := blank(id, label)
	known, err := tomlStrings(raw, "known_params", origin)
	if err != nil {
		return nil, err
	}
	for _, name := range known {
		p.knownParams[name] = true
	}
	for key, field := range map[string]*string{
		"status": &p.status, "prefix": &p.prefix, "separator": &p.separator,
		"pair_separator": &p.pairSeparator, "source": &p.source,
		"source_read": &p.sourceRead, "notes": &p.notes,
		"session_param": &p.sessionParam, "host": &p.host,
	} {
		if _, ok := raw[key]; ok {
			value, err := tomlString(raw, key, origin)
			if err != nil {
				return nil, err
			}
			*field = value
		}
	}
	if value, ok := raw["exit_ip_header"]; ok {
		refuse := newError(ErrProvider, fmt.Sprintf(
			"%s gives exit_ip_header as %v. It has to be a header name or a non-empty list "+
				"of header names.", origin, value))
		var names []string
		switch v := value.(type) {
		case string:
			names = []string{v}
		case []any:
			for _, item := range v {
				name, ok := item.(string)
				if !ok {
					return nil, refuse
				}
				names = append(names, name)
			}
		default:
			return nil, refuse
		}
		if len(names) == 0 {
			return nil, refuse
		}
		for _, name := range names {
			if name == "" {
				return nil, refuse
			}
		}
		p.exitIPHeaders = names
	}
	if value, ok := raw["port"]; ok {
		number, ok := value.(int64)
		if !ok {
			return nil, newError(ErrProvider, fmt.Sprintf("%s gives port as %v, which is not a number.", origin, value))
		}
		port, ok := portNumber(strconv.FormatInt(number, 10))
		if !ok {
			return nil, newError(ErrProvider, fmt.Sprintf(
				"%s gives port as %d. It has to be a whole number from 1 to 65535; 0 means "+
					"\"any free port\" when binding and is meaningless when connecting.", origin, number))
		}
		p.port = port
	}
	if value, ok := raw["aliases"]; ok {
		table, ok := value.(map[string]any)
		if !ok {
			return nil, newError(ErrProvider, fmt.Sprintf("%s gives aliases as %v, which is not a table.", origin, value))
		}
		for canonical, wire := range table {
			spelled, ok := wire.(string)
			if !ok {
				return nil, newError(ErrProvider, fmt.Sprintf(
					"%s aliases %q to %v, which is not a string.", origin, canonical, wire))
			}
			p.aliases[canonical] = spelled
		}
	}
	if _, ok := raw["normalize"]; ok {
		names, err := tomlStrings(raw, "normalize", origin)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			p.normalize[name] = true
		}
	}
	if value, ok := raw["values"]; ok {
		table, ok := value.(map[string]any)
		if !ok {
			return nil, newError(ErrProvider, fmt.Sprintf("%s gives values as %v, which is not a table.", origin, value))
		}
		for name, allowed := range table {
			list, ok := allowed.([]any)
			if !ok || len(list) == 0 {
				return nil, newError(ErrProvider, fmt.Sprintf(
					"%s gives the legal values of %q as %v. It has to be a non-empty list of "+
						"strings. Leave the entry out entirely to mean 'nobody has established "+
						"these' - an empty list would mean 'every value is refused'.", origin, name, allowed))
			}
			legal := make([]string, 0, len(list))
			for _, item := range list {
				text, ok := item.(string)
				if !ok {
					return nil, newError(ErrProvider, fmt.Sprintf(
						"%s lists %v as a legal value of %q, and it is not a string.", origin, item, name))
				}
				legal = append(legal, text)
			}
			p.values[name] = legal
		}
	}
	foldValues(p)
	if value, ok := raw["connect_reactions"]; ok {
		table, ok := value.(map[string]any)
		if !ok {
			return nil, newError(ErrProvider, fmt.Sprintf(
				"%s gives connect_reactions as %v, which is not a table.", origin, value))
		}
		for status, meaning := range table {
			text, ok := meaning.(string)
			if !ok || text == "" {
				return nil, newError(ErrProvider, fmt.Sprintf(
					"%s describes CONNECT status %q as %v. It has to be a non-empty string: this "+
						"text is shown to a caller as the reason their connection was refused.",
					origin, status, meaning))
			}
			p.connectReactions[status] = text
		}
	}
	if err := check(p, origin); err != nil {
		return nil, err
	}
	return p, nil
}

// foldValues folds the legal-value list of every folded parameter, because
// the caller's value is folded before it is compared.
func foldValues(p *Provider) {
	for name, legal := range p.values {
		if !p.normalize[name] {
			continue
		}
		folded := make([]string, len(legal))
		for i, value := range legal {
			folded[i] = p.Normalized(name, value)
		}
		p.values[name] = folded
	}
}

// check refuses a definition that reads like a working setting and can never
// fire.
func check(p *Provider, origin string) error {
	var unknownAlias []string
	for name := range p.aliases {
		if !p.knownParams[name] {
			unknownAlias = append(unknownAlias, name)
		}
	}
	if len(unknownAlias) > 0 {
		sort.Strings(unknownAlias)
		return newError(ErrProvider, fmt.Sprintf(
			"%s aliases %q which are not in known_params, so those parameters would be refused "+
				"before the alias was ever used.", origin, unknownAlias))
	}
	if p.sessionParam != "" && !p.knownParams[p.sessionParam] {
		return newError(ErrProvider, fmt.Sprintf(
			"%s declares session_param %q which is not in known_params, so every sticky session "+
				"would be refused.", origin, p.sessionParam))
	}
	for _, name := range sortedKeys(boolKeys(p.values)) {
		if !p.knownParams[name] {
			return newError(ErrProvider, fmt.Sprintf(
				"%s lists legal values for %q, which is not in known_params, so that parameter is "+
					"refused by name before its value is ever looked at.", origin, name))
		}
	}
	var unknownNormalize []string
	for name := range p.normalize {
		if !p.knownParams[name] {
			unknownNormalize = append(unknownNormalize, name)
		}
	}
	if len(unknownNormalize) > 0 {
		sort.Strings(unknownNormalize)
		return newError(ErrProvider, fmt.Sprintf(
			"%s normalizes %q which are not in known_params, so those parameters are refused by "+
				"name and the fold can never run.", origin, unknownNormalize))
	}
	if len(p.normalize) > 0 {
		for _, separator := range []string{p.separator, p.pairSeparator} {
			if separator == "_" {
				return newError(ErrProvider, fmt.Sprintf(
					"%s separates parameters with %q and also normalizes %q, and the fold turns a "+
						"space into %q. A value with a space in it would be cut at the separator "+
						"the fold had just inserted. Pick one.",
					origin, separator, sortedKeys(p.normalize), separator))
			}
		}
	}
	for _, name := range p.exitIPHeaders {
		if name == "" {
			return newError(ErrProvider, fmt.Sprintf(
				"%s names an empty exit_ip_header. It has to be a header name, and an empty one "+
					"can never match.", origin))
		}
	}
	return nil
}

func tomlString(raw map[string]any, key, origin string) (string, error) {
	text, ok := raw[key].(string)
	if !ok {
		return "", newError(ErrProvider, fmt.Sprintf("%s gives %s as %v, which is not a string.", origin, key, raw[key]))
	}
	return text, nil
}

func tomlStrings(raw map[string]any, key, origin string) ([]string, error) {
	items, ok := raw[key].([]any)
	if !ok {
		return nil, newError(ErrProvider, fmt.Sprintf("%s gives %s as %v, which is not a list.", origin, key, raw[key]))
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, newError(ErrProvider, fmt.Sprintf(
				"%s has %v in %s, and a parameter name has to be a string.", origin, item, key))
		}
		out = append(out, text)
	}
	return out, nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func boolKeys[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
