package nodemaven

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func testProxy(t *testing.T, opts ...Option) *Proxy {
	t.Helper()
	clearGatewayEnv(t)
	base := []Option{WithLogin("acct"), WithPassword("pw"), WithHost("gate.example.com"), WithPort(8080)}
	proxy, err := New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return proxy
}

func TestShippedDefinitionLoads(t *testing.T) {
	provider, err := Load(DefaultProvider)
	if err != nil {
		t.Fatal(err)
	}
	if !provider.IsMeasured() || provider.SessionParam() != "sid" || provider.Port() == 0 || provider.Host() == "" {
		t.Fatalf("unexpected shipped definition: %+v", provider)
	}
	if got := provider.ExitIPHeaders(); len(got) != 2 {
		t.Fatalf("want two exit headers, got %q", got)
	}
	if provider.Reaction(407) == "" {
		t.Fatal("the shipped definition explains no 407")
	}
	if _, err := Load("nope"); !errors.Is(err, ErrProvider) {
		t.Fatalf("want ErrProvider, got %v", err)
	}
}

// Go's unicode.IsSpace is true for U+0085 and U+00A0; the contract's
// whitespace is six ASCII bytes and neither of those.
func TestUnicodeSpaceIsNotWhitespace(t *testing.T) {
	for _, value := range []string{"a\u0085b", "a b"} {
		proxy := testProxy(t, WithParam("sid", value))
		if !strings.HasSuffix(proxy.Username(), "-sid-"+value) {
			t.Fatalf("%q was changed: %s", value, proxy.Username())
		}
	}
	for _, value := range []string{"a b", "a\tb", "a\vb", "a\fb", "a\rb", "a\nb"} {
		clearGatewayEnv(t)
		_, err := New(WithLogin("a"), WithPassword("b"), WithParam("sid", value))
		if !errors.Is(err, ErrParam) {
			t.Fatalf("%q: want ErrParam, got %v", value, err)
		}
	}
}

// strconv.Atoi accepts all of these; none of them is a port.
func TestPortTextIsDigitsOnly(t *testing.T) {
	for _, text := range []string{"+8080", "-1", "08080x", "080800", "65536", " 8080", "8080 ", "0x50", "８０"} {
		clearGatewayEnv(t)
		t.Setenv("NODEMAVEN_PORT", text)
		_, err := New(WithLogin("a"), WithPassword("b"))
		if !errors.Is(err, ErrCredentials) {
			t.Fatalf("%q: want ErrCredentials, got %v", text, err)
		}
	}
	clearGatewayEnv(t)
	t.Setenv("NODEMAVEN_PORT", "08080")
	proxy, err := New(WithLogin("a"), WithPassword("b"))
	if err != nil || !strings.HasSuffix(proxy.Server(), ":8080") {
		t.Fatalf("08080 is five ASCII digits and a port: %v %v", proxy, err)
	}
}

func TestPortOptionIsChecked(t *testing.T) {
	for _, port := range []int{0, -1, 65536} {
		clearGatewayEnv(t)
		if _, err := New(WithLogin("a"), WithPassword("b"), WithPort(port)); !errors.Is(err, ErrCredentials) {
			t.Fatalf("%d: want ErrCredentials, got %v", port, err)
		}
	}
}

func TestEnvironmentFillsCredentials(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("NODEMAVEN_LOGIN", "envlogin")
	t.Setenv("NODEMAVEN_PASSWORD", "envpass")
	proxy, err := New(WithParam("country", "us"))
	if err != nil {
		t.Fatal(err)
	}
	if proxy.Username() != "envlogin-country-us" {
		t.Fatal(proxy.Username())
	}
	clearGatewayEnv(t)
	if _, err := New(); !errors.Is(err, ErrCredentials) || !strings.Contains(err.Error(), "no login and no password") {
		t.Fatalf("want a credentials error naming both, got %v", err)
	}
}

func TestPasswordNeverPrinted(t *testing.T) {
	proxy := testProxy(t, WithPassword("s3cret-pw"), WithParam("country", "us"))
	for _, shown := range []string{
		fmt.Sprint(proxy), fmt.Sprintf("%v", proxy), fmt.Sprintf("%+v", proxy), fmt.Sprintf("%#v", proxy),
		fmt.Sprint(proxy.Browser()), fmt.Sprintf("%#v", proxy.Browser()),
		fmt.Sprint(proxy.Connect()), fmt.Sprintf("%#v", proxy.Connect()),
	} {
		if strings.Contains(shown, "s3cret") {
			t.Fatalf("password printed: %s", shown)
		}
	}
	client, err := NewClient(WithAPIKey("k3y-value"))
	if err != nil {
		t.Fatal(err)
	}
	if shown := fmt.Sprintf("%v %#v", client, client); strings.Contains(shown, "k3y") {
		t.Fatalf("api key printed: %s", shown)
	}
}

func TestWithAndWithoutLeaveTheOriginal(t *testing.T) {
	proxy := testProxy(t, WithParam("country", "us"), WithParam("sid", "a"))
	changed, err := proxy.With("country", "de")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := proxy.Without("sid")
	if err != nil {
		t.Fatal(err)
	}
	if proxy.Username() != "acct-country-us-sid-a" || changed.Username() != "acct-country-de-sid-a" ||
		removed.Username() != "acct-country-us" {
		t.Fatal(proxy.Username(), changed.Username(), removed.Username())
	}
	if _, err := proxy.With("contry", "de"); !errors.Is(err, ErrParam) {
		t.Fatalf("want ErrParam, got %v", err)
	}
}

func TestSetTwiceKeepsThePosition(t *testing.T) {
	proxy := testProxy(t, WithParam("country", "us"), WithParam("sid", "a"), WithParam("country", "de"))
	if proxy.Username() != "acct-country-de-sid-a" {
		t.Fatal(proxy.Username())
	}
}

func TestSessions(t *testing.T) {
	proxy := testProxy(t, WithParam("country", "us"))
	batch, err := proxy.Sessions(50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, pinned := range batch {
		sid, _ := pinned.Param("sid")
		if len(sid) != 12 || seen[sid] || strings.Trim(sid, "0123456789abcdef") != "" {
			t.Fatalf("bad or repeated id %q", sid)
		}
		seen[sid] = true
	}
	if _, err := proxy.Sessions(0); !errors.Is(err, ErrParam) {
		t.Fatalf("want ErrParam, got %v", err)
	}
	if _, err := proxy.SessionsOfLength(256, 1); !errors.Is(err, ErrParam) {
		t.Fatalf("256 ids from 256 possible must be refused, got %v", err)
	}
	if strings.Contains(fmt.Sprint(proxy.SessionsOfLength(2, 0)), "  ") {
		t.Fatal("double space in a message")
	}
}

func TestSessionNeedsASessionParameter(t *testing.T) {
	provider, err := LoadString("label = \"P\"\nknown_params = [\"country\"]\nhost = \"h\"\nport = 1\n", "p")
	if err != nil {
		t.Fatal(err)
	}
	proxy := testProxy(t, WithProvider(provider))
	if _, err := proxy.Session("x"); !errors.Is(err, ErrParam) {
		t.Fatalf("want ErrParam, got %v", err)
	}
}

func TestURLForms(t *testing.T) {
	proxy := testProxy(t, WithPassword("p@ss:w/rd"), WithParam("country", "us"))
	if proxy.URL() != "http://acct-country-us:p%40ss%3Aw%2Frd@gate.example.com:8080" {
		t.Fatal(proxy.URL())
	}
	if proxy.URLWithScheme("socks5") != "socks5://acct-country-us:p%40ss%3Aw%2Frd@gate.example.com:8080" {
		t.Fatal(proxy.URLWithScheme("socks5"))
	}
	u := proxy.ProxyURL()
	password, _ := u.User.Password()
	if u.User.Username() != "acct-country-us" || password != "p@ss:w/rd" || u.Host != "gate.example.com:8080" {
		t.Fatal(u)
	}
	got, err := proxy.HTTPProxy()(&http.Request{})
	if err != nil || got.String() != u.String() {
		t.Fatal(got, err)
	}
	browser := proxy.Browser()
	if browser.Server != "http://gate.example.com:8080" || browser.Username != "acct-country-us" || browser.Password != "p@ss:w/rd" {
		t.Fatalf("%#v", browser)
	}
}

func TestUnknownParameterNamesTheKnownOnes(t *testing.T) {
	clearGatewayEnv(t)
	_, err := New(WithLogin("a"), WithPassword("b"), WithParam("contry", "us"))
	if !errors.Is(err, ErrParam) || !strings.Contains(err.Error(), `"country"`) {
		t.Fatalf("want ErrParam listing country, got %v", err)
	}
}

func TestDefinitionChecks(t *testing.T) {
	for name, text := range map[string]string{
		"missing label":             "known_params = [\"a\"]\n",
		"missing known_params":      "label = \"P\"\n",
		"alias of unknown":          "label = \"P\"\nknown_params = [\"a\"]\n[aliases]\nb = \"c\"\n",
		"session of unknown":        "label = \"P\"\nknown_params = [\"a\"]\nsession_param = \"sid\"\n",
		"values of unknown":         "label = \"P\"\nknown_params = [\"a\"]\nvalues = { b = [\"x\"] }\n",
		"empty values":              "label = \"P\"\nknown_params = [\"a\"]\nvalues = { a = [] }\n",
		"normalize unknown":         "label = \"P\"\nknown_params = [\"a\"]\nnormalize = [\"b\"]\n",
		"empty exit header":         "label = \"P\"\nknown_params = [\"a\"]\nexit_ip_header = \"\"\n",
		"empty exit header list":    "label = \"P\"\nknown_params = [\"a\"]\nexit_ip_header = []\n",
		"empty reaction":            "label = \"P\"\nknown_params = [\"a\"]\n[connect_reactions]\n407 = \"\"\n",
		"port as text":              "label = \"P\"\nknown_params = [\"a\"]\nport = \"8080\"\n",
		"not toml":                  "label = \n",
		"underscore pair with fold": "label = \"P\"\nknown_params = [\"a\"]\npair_separator = \"_\"\nnormalize = [\"a\"]\n",
	} {
		if _, err := LoadString(text, "p"); !errors.Is(err, ErrProvider) {
			t.Errorf("%s: want ErrProvider, got %v", name, err)
		}
	}
	provider, err := LoadString("label = \"P\"\nknown_params = [\"a\"]\nexit_ip_header = [\"X-One\", \"X-Two\"]\n", "p")
	if err != nil {
		t.Fatal(err)
	}
	if got := provider.ExitIPHeaders(); len(got) != 2 || got[1] != "X-Two" {
		t.Fatal(got)
	}
}

func TestNewProviderRunsTheSameChecks(t *testing.T) {
	if _, err := LoadString("label = \"P\"\nknown_params = [\"a\"]\n", ""); !errors.Is(err, ErrProvider) {
		t.Fatalf("an empty id: want ErrProvider, got %v", err)
	}
	if _, err := NewProvider(ProviderSpec{ID: "p", Label: "P"}); !errors.Is(err, ErrProvider) {
		t.Fatalf("no known params: want ErrProvider, got %v", err)
	}
	if _, err := NewProvider(ProviderSpec{ID: "p", Label: "P", KnownParams: []string{"a"}, SessionParam: "sid"}); !errors.Is(err, ErrProvider) {
		t.Fatalf("want ErrProvider, got %v", err)
	}
	provider, err := NewProvider(ProviderSpec{
		ID: "p", Label: "P", KnownParams: []string{"region"}, Normalize: []string{"region"},
		Values: map[string][]string{"region": {"District of Columbia"}}, Host: "h", Port: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy := testProxy(t, WithProvider(provider), WithParam("region", "DISTRICT of columbia"))
	if proxy.Username() != "acct-region-district_of_columbia" {
		t.Fatal(proxy.Username())
	}
}
