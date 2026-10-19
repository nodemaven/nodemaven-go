package nodemaven

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuiltinClientSendsTheKeyHeader(t *testing.T) {
	clearGatewayEnv(t)
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"email": "x"}`))
	}))
	defer server.Close()
	client, err := NewClient(WithAPIKey(" k "), WithBaseURL(server.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	me, err := client.Me(context.Background())
	if err != nil || me["email"] != "x" {
		t.Fatal(me, err)
	}
	if got.Get("Authorization") != "x-api-key k" || got.Get("Accept") != "application/json" || got.Get("User-Agent") != "nodemaven-go" {
		t.Fatal(got)
	}
}

// The API key travels in a header, so a redirect must not take it anywhere.
func TestBuiltinClientFollowsNoRedirect(t *testing.T) {
	clearGatewayEnv(t)
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/not-found", http.StatusFound)
	}))
	defer server.Close()
	client, err := NewClient(WithAPIKey("k"), WithBaseURL(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Me(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Kind != ErrAPI || apiErr.Status != 302 {
		t.Fatalf("want an api error carrying 302, got %v", err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

// Go never proxies a loopback address, so the target here is a name that does
// not resolve: through a proxy from the environment it would reach the fake
// proxy, and without one it fails at DNS. net/http reads the proxy variables
// once per process, so the request is made in a child process whose
// environment is set before anything reads it.
func TestBuiltinClientReadsNoProxyFromTheEnvironment(t *testing.T) {
	if os.Getenv("NODEMAVEN_GO_PROXY_CHILD") == "1" {
		client, err := NewClient(WithAPIKey("k"), WithBaseURL("http://api.nodemaven.invalid"), WithTimeout(5*time.Second))
		if err == nil {
			_, err = client.Me(context.Background())
		}
		fmt.Println("child:", errorName(err))
		return
	}
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer proxy.Close()
	env := []string{"NODEMAVEN_GO_PROXY_CHILD=1", "NODEMAVEN_APIKEY=", "NODEMAVEN_BASE_URL=", "NO_PROXY=", "no_proxy="}
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		env = append(env, name+"="+proxy.URL)
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "NODEMAVEN_APIKEY", "NODEMAVEN_BASE_URL":
			continue
		}
		env = append(env, entry)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBuiltinClientReadsNoProxyFromTheEnvironment$", "-test.count=1")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	if proxied.Load() != 0 {
		t.Fatalf("the request went through the proxy named in the environment\n%s", out)
	}
	if !strings.Contains(string(out), "child: api") {
		t.Fatalf("the child did not report an api error\n%s", out)
	}
}

func TestTimeoutIsAnAPIError(t *testing.T) {
	clearGatewayEnv(t)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)
	client, err := NewClient(WithAPIKey("k"), WithBaseURL(server.URL), WithTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Me(context.Background())
	if !errors.Is(err, ErrAPI) || !strings.Contains(err.Error(), "timeout of 0.2 s") {
		t.Fatalf("want ErrAPI naming the timeout, got %v", err)
	}
}

func TestErrorDialects(t *testing.T) {
	clearGatewayEnv(t)
	for body, want := range map[string]string{
		`{"error": {"code": "authentication_required", "message": "No key.", "retryable": false}}`: "No key. (authentication_required)",
		`{"detail": "Invalid page."}`:                "Invalid page.",
		`{"errors": {"ip": "bad address"}}`:          "ip: bad address",
		`{"error": "Please enter a valid protocol"}`: "Please enter a valid protocol",
		`not json`: "not json",
	} {
		transport := &cannedTransport{t: t, responses: []any{map[string]any{"status": jsonNumber(400), "body": body}}}
		client, _ := NewClient(WithAPIKey("k"), WithHTTPClient(&http.Client{Transport: transport}))
		_, err := client.Me(context.Background())
		if !errors.Is(err, ErrAPI) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q in the message, got %v", body, want, err)
		}
	}
}

// The sub-user payload carries the live proxy password, and an answer with no
// message of its own once printed it into the error text.
func TestAnAnswerWithoutAMessageNeverPrintsItsValues(t *testing.T) {
	clearGatewayEnv(t)
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{200, `{"success": false, "description": "", "errors": "", "payload": {"proxy_username": "u", "proxy_password": "LIVE-PASSWORD"}}`,
			"keys: description, errors, payload, success"},
		{400, `{"proxy_username": "u", "proxy_password": "LIVE-PASSWORD"}`, "keys: proxy_password, proxy_username"},
		{500, `[{"proxy_password": "LIVE-PASSWORD"}]`, "a list of 1"},
		{400, `{"expiry_date": ["Enter a valid date."]}`, "Enter a valid date."},
		{400, `{"errors": {"payload": {"proxy_password": "LIVE-PASSWORD"}}}`, "keys: payload"},
		{400, `["LIVE-PASSWORD", {"proxy_password": "x"}]`, "a list of 2"},
	} {
		transport := &cannedTransport{t: t, responses: []any{map[string]any{"status": jsonNumber(c.status), "body": c.body}}}
		client, _ := NewClient(WithAPIKey("k"), WithHTTPClient(&http.Client{Transport: transport}))
		_, err := client.DeleteSubUser(context.Background(), 7)
		if err == nil || strings.Contains(err.Error(), "LIVE-PASSWORD") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q and no password in the message, got %v", c.body, c.want, err)
		}
	}
}

func TestErrorKindsAreAPIKinds(t *testing.T) {
	for _, kind := range []error{ErrAuth, ErrNotFound, ErrRateLimit} {
		err := &Error{Kind: kind}
		if !errors.Is(err, ErrAPI) || !errors.Is(err, kind) || errors.Is(err, ErrParam) {
			t.Fatalf("%v", kind)
		}
	}
	if errors.Is(&Error{Kind: ErrParam}, ErrAPI) {
		t.Fatal("a param error is not an api error")
	}
}

func TestNoKeyIsACredentialsError(t *testing.T) {
	clearGatewayEnv(t)
	if _, err := NewClient(); !errors.Is(err, ErrCredentials) {
		t.Fatalf("want ErrCredentials, got %v", err)
	}
	t.Setenv("NODEMAVEN_APIKEY", "  ")
	if _, err := NewClient(); !errors.Is(err, ErrCredentials) {
		t.Fatalf("a blank key: want ErrCredentials, got %v", err)
	}
}

func TestQueryIsSortedAndEncoded(t *testing.T) {
	got := encodeQuery(url.Values{"z": {"a b"}, "a": {"x/y", "1"}})
	if got != "a=x%2Fy&a=1&z=a%20b" {
		t.Fatal(got)
	}
}

func TestOrigin(t *testing.T) {
	for _, pair := range [][2]string{
		{"https://dashboard.nodemaven.com", "https://DASHBOARD.nodemaven.com:443/api?x=1"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/a"},
	} {
		if !sameOrigin(pair[0], pair[1]) {
			t.Errorf("%q and %q are one origin", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]string{
		{"https://dashboard.nodemaven.com", "http://dashboard.nodemaven.com/"},
		{"https://dashboard.nodemaven.com", "https://dashboard.nodemaven.com.evil.example/"},
		{"https://dashboard.nodemaven.com", "https://user@dashboard.nodemaven.com/"},
		{"https://dashboard.nodemaven.com", "https://dashboard.nodemaven.com:8443/"},
	} {
		if sameOrigin(pair[0], pair[1]) {
			t.Errorf("%q and %q are different origins", pair[0], pair[1])
		}
	}
}

func TestIdenticalPagesStopTheWalk(t *testing.T) {
	page := map[string]any{"status": jsonNumber(200), "body": `{"results": [{"code": "us"}]}`}
	transport := &cannedTransport{t: t, responses: []any{page, page}}
	client, _ := NewClient(WithAPIKey("k"), WithHTTPClient(&http.Client{Transport: transport}))
	first, err := client.Countries(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.All(context.Background(), first); !errors.Is(err, ErrAPI) || !strings.Contains(err.Error(), "ignoring it") {
		t.Fatalf("want the repeated page refused, got %v", err)
	}
}

func TestMaxPagesBoundsTheWalk(t *testing.T) {
	var responses []any
	for i := 0; i < 5; i++ {
		responses = append(responses, map[string]any{"status": jsonNumber(200), "body": `{"results": [{"n": ` + string(rune('0'+i)) + `}]}`})
	}
	transport := &cannedTransport{t: t, responses: responses}
	client, _ := NewClient(WithAPIKey("k"), WithHTTPClient(&http.Client{Transport: transport}))
	first, _ := client.Countries(context.Background(), nil)
	rows := client.Iterate(context.Background(), first)
	rows.MaxPages = 3
	count := 0
	for rows.Next() {
		count++
	}
	if !errors.Is(rows.Err(), ErrAPI) || count != 3 {
		t.Fatalf("want 3 rows and a bound, got %d and %v", count, rows.Err())
	}
}

func TestValidate(t *testing.T) {
	transport := &cannedTransport{t: t, responses: []any{
		map[string]any{"status": jsonNumber(200), "body": `{"results": [{"code": "US"}, {"code": "de"}]}`},
		map[string]any{"status": jsonNumber(200), "body": `{"results": []}`},
	}}
	client, _ := NewClient(WithAPIKey("k"), WithHTTPClient(&http.Client{Transport: transport}))
	proxy := testProxy(t, WithParam("country", "fr"), WithParam("city", "paris"))
	problems, err := client.Validate(context.Background(), proxy)
	if err != nil || len(problems) != 2 {
		t.Fatalf("want two problems, got %q %v", problems, err)
	}
	if !containsPair(transport.sent[0].query, "connection_type", "residential") {
		t.Fatal(transport.sent[0].query)
	}
}
