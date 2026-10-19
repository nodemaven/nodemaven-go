package nodemaven

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The shared specification, vendored under testdata/spec. Every case is
// keyed on the wire; this file only translates it into Go calls.

func readSpec(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/spec/cases/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func errorName(err error) string {
	for _, pair := range []struct {
		kind error
		name string
	}{
		{ErrAuth, "auth"}, {ErrNotFound, "not_found"}, {ErrRateLimit, "rate_limit"},
		{ErrAPI, "api"}, {ErrParam, "param"}, {ErrCredentials, "credentials"},
		{ErrProvider, "provider"}, {ErrCheck, "check"},
	} {
		if errors.Is(err, pair.kind) {
			return pair.name
		}
	}
	return fmt.Sprintf("%T", err)
}

// clearGatewayEnv makes sure no test reads a real login or port.
func clearGatewayEnv(t *testing.T) {
	for _, suffix := range []string{"LOGIN", "PASSWORD", "HOST", "PORT", "APIKEY", "BASE_URL"} {
		t.Setenv("NODEMAVEN_"+suffix, "")
	}
}

func asInt(t *testing.T, value any) int {
	t.Helper()
	n, err := value.(json.Number).Int64()
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func TestSpecProxyCases(t *testing.T) {
	spec := readSpec(t, "proxy.json")
	defaults := spec["defaults"].(map[string]any)["credentials"].(map[string]any)
	cases := spec["cases"].([]any)
	if len(cases) < 20 {
		t.Fatalf("read only %d proxy cases", len(cases))
	}
	for _, raw := range cases {
		c := raw.(map[string]any)
		t.Run(c["id"].(string), func(t *testing.T) {
			clearGatewayEnv(t)
			expect := c["expect"].(map[string]any)
			got := runProxyCase(t, c, defaults)
			if want, ok := expect["error"]; ok {
				if got["error"] != want {
					t.Fatalf("want error %v, got %v", want, got)
				}
				return
			}
			if _, ok := got["error"]; ok {
				t.Fatalf("want %v, got %v", expect, got)
			}
			if excluded, ok := expect["sid_excludes"].(string); ok {
				sids := got["sids"].([]string)
				if len(sids) != asInt(t, expect["sessions"]) {
					t.Fatalf("want %v sessions, got %d", expect["sessions"], len(sids))
				}
				seen := map[string]bool{}
				for _, sid := range sids {
					if seen[sid] || strings.Contains(sid, excluded) {
						t.Fatalf("sid %q is repeated or contains %q", sid, excluded)
					}
					seen[sid] = true
				}
				return
			}
			for key, want := range expect {
				if got[key] != want {
					t.Errorf("%s: want %q, got %q", key, want, got[key])
				}
			}
		})
	}
}

func runProxyCase(t *testing.T, c, defaults map[string]any) map[string]any {
	failed := func(err error) map[string]any { return map[string]any{"error": errorName(err)} }
	if load, ok := c["load"].(map[string]any); ok {
		if _, err := LoadString(load["toml"].(string), "p"); err != nil {
			return failed(err)
		}
		return map[string]any{"loaded": true}
	}
	creds := map[string]any{}
	for k, v := range defaults {
		creds[k] = v
	}
	if override, ok := c["credentials"].(map[string]any); ok {
		for k, v := range override {
			creds[k] = v
		}
	}
	opts := []Option{
		WithLogin(creds["login"].(string)),
		WithPassword(creds["password"].(string)),
		WithHost(creds["host"].(string)),
	}
	if text, ok := c["port_text"].(string); ok {
		// The one text path every SDK has: the environment variable.
		t.Setenv("NODEMAVEN_PORT", text)
	} else {
		opts = append(opts, WithPort(asInt(t, creds["port"])))
	}
	if definition, ok := c["provider"].(map[string]any); ok {
		provider, err := LoadString(definition["toml"].(string), "p")
		if err != nil {
			return failed(err)
		}
		opts = append(opts, WithProvider(provider))
	}
	if params, ok := c["params"].([]any); ok {
		for _, pair := range params {
			p := pair.([]any)
			opts = append(opts, WithParam(p[0].(string), p[1].(string)))
		}
	}
	proxy, err := New(opts...)
	if err != nil {
		return failed(err)
	}
	if sessions, ok := c["sessions"].(map[string]any); ok {
		batch, err := proxy.SessionsOfLength(asInt(t, sessions["count"]), asInt(t, sessions["length"]))
		if err != nil {
			return failed(err)
		}
		sids := make([]string, len(batch))
		for i, pinned := range batch {
			sids[i], _ = pinned.Param("sid")
		}
		return map[string]any{"sids": sids}
	}
	return map[string]any{"username": proxy.Username(), "url": proxy.URL()}
}

// cannedTransport answers each request with the next canned response and
// records what was sent.
type cannedTransport struct {
	t         *testing.T
	responses []any
	html      string
	sent      []sentRequest
}

type sentRequest struct {
	method string
	path   string
	query  [][2]string
	body   map[string]any
}

func (c *cannedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	sent := sentRequest{method: req.Method, path: req.URL.Path}
	if req.URL.RawQuery != "" {
		for _, part := range strings.Split(req.URL.RawQuery, "&") {
			name, value, _ := strings.Cut(part, "=")
			name, _ = url.QueryUnescape(name)
			value, _ = url.QueryUnescape(value)
			sent.query = append(sent.query, [2]string{name, value})
		}
	}
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &sent.body); err != nil {
				c.t.Fatalf("request body is not JSON: %v", err)
			}
		}
	}
	c.sent = append(c.sent, sent)
	if len(c.responses) == 0 {
		c.t.Fatal("no canned response left")
	}
	r := c.responses[0].(map[string]any)
	c.responses = c.responses[1:]
	switch r["raise"] {
	case "timeout":
		return nil, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	case "connection_reset":
		return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	}
	var body string
	if rows, ok := r["rows"].(map[string]any); ok {
		list := []map[string]any{}
		for i := 0; i < asInt(c.t, rows["count"]); i++ {
			list = append(list, map[string]any{"n": i})
		}
		var shaped any = map[string]any{rows["key"].(string): list}
		if rows["envelope"] == true {
			shaped = map[string]any{"success": true, "description": "", "errors": []any{}, "payload": list}
		}
		raw, _ := json.Marshal(shaped)
		body = string(raw)
	} else if r["body"] == "$html_page" {
		body = c.html
	} else {
		body = r["body"].(string)
	}
	return &http.Response{
		StatusCode: asInt(c.t, r["status"]),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func filtersOf(args map[string]any) url.Values {
	out := url.Values{}
	for name, value := range args {
		out.Set(name, value.(string))
	}
	return out
}

func invoke(ctx context.Context, client *Client, call map[string]any) (any, error) {
	args, _ := call["args"].(map[string]any)
	switch op := call["op"].(string); op {
	case "me":
		return client.Me(ctx)
	case "delete_sub_user":
		return client.DeleteSubUser(ctx, args["id"])
	case "create_sub_user":
		return client.CreateSubUser(ctx, args["proxy_username"].(string), args["proxy_password"].(string), nil)
	case "update_sub_user":
		return client.UpdateSubUser(ctx, args["id"], args["changes"].(map[string]any))
	case "reset_sub_user_usage":
		return client.ResetSubUserUsage(ctx, args["ids"].([]any))
	case "delete_whitelist_ip":
		return client.DeleteWhitelistIP(ctx, args["ip_id"])
	case "upsert_whitelist_ip":
		n, _ := args["ports_count"].(json.Number).Int64()
		return client.UpsertWhitelistIP(ctx, args["ip"].(string), int(n), nil)
	case "countries":
		return client.Countries(ctx, filtersOf(args))
	case "cities":
		return client.Cities(ctx, filtersOf(args))
	case "isp_cities":
		return client.ISPCities(ctx, filtersOf(args))
	case "sub_users":
		return client.SubUsers(ctx, filtersOf(args))
	case "whitelist_ips":
		return client.WhitelistIPs(ctx, filtersOf(args))
	default:
		return nil, fmt.Errorf("the spec runner has no mapping for op %q", op)
	}
}

// normal reads a value back through JSON, so 1 and json.Number("1") compare
// equal.
func normal(value any) any {
	raw, _ := json.Marshal(value)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestSpecAPICases(t *testing.T) {
	spec := readSpec(t, "api.json")
	cases := spec["cases"].([]any)
	if len(cases) < 20 {
		t.Fatalf("read only %d api cases", len(cases))
	}
	for _, raw := range cases {
		c := raw.(map[string]any)
		t.Run(c["id"].(string), func(t *testing.T) {
			clearGatewayEnv(t)
			transport := &cannedTransport{t: t, responses: c["responses"].([]any), html: spec["html_page"].(string)}
			client, err := NewClient(WithAPIKey("k"), WithHTTPClient(&http.Client{
				Transport:     transport,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			}))
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			call := c["call"].(map[string]any)
			got := map[string]any{}
			if call["op"] == "iterate" {
				var page any
				if page, err = invoke(ctx, client, call["of"].(map[string]any)); err == nil {
					var rows []any
					rows, err = client.All(ctx, page.(*Page))
					got["rows"] = len(rows)
				}
			} else {
				_, err = invoke(ctx, client, call)
				got["ok"] = true
			}
			if err != nil {
				got = map[string]any{"error": errorName(err), "message": err.Error()}
			}
			got["requests_sent"] = len(transport.sent)
			for key, want := range c["expect"].(map[string]any) {
				if key == "message_excludes" {
					if message, _ := got["message"].(string); strings.Contains(message, want.(string)) {
						t.Errorf("error text carries %q: %s", want, message)
					}
					continue
				}
				if !reflect.DeepEqual(normal(got[key]), normal(want)) {
					t.Errorf("%s: want %v, got %v (all: %v)", key, want, got[key], got)
				}
			}
			expected, _ := c["expect_requests"].([]any)
			for i, raw := range expected {
				want := raw.(map[string]any)
				if i >= len(transport.sent) {
					t.Fatalf("request %d was never sent", i)
				}
				sent := transport.sent[i]
				if sent.method != want["method"] || sent.path != want["path"] {
					t.Errorf("request %d: want %v %v, got %s %s", i, want["method"], want["path"], sent.method, sent.path)
				}
				if query, ok := want["query"]; ok && !reflect.DeepEqual(normal(sent.query), normal(query)) {
					t.Errorf("request %d: want query %v, got %v", i, query, sent.query)
				}
				if has, ok := want["query_has"].([]any); ok {
					for _, pair := range has {
						p := pair.([]any)
						if !containsPair(sent.query, p[0].(string), p[1].(string)) {
							t.Errorf("request %d: query %v lacks %v", i, sent.query, p)
						}
					}
				}
				if has, ok := want["json_has"].(map[string]any); ok {
					for key, value := range has {
						if !reflect.DeepEqual(normal(sent.body[key]), normal(value)) {
							t.Errorf("request %d: body %s is %v, want %v", i, key, sent.body[key], value)
						}
					}
				}
			}
		})
	}
}

func containsPair(query [][2]string, name, value string) bool {
	for _, pair := range query {
		if pair[0] == name && pair[1] == value {
			return true
		}
	}
	return false
}

func jsonNumber(n int) json.Number { return json.Number(strconv.Itoa(n)) }
