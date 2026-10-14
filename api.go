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
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the dashboard host the account API lives on.
const DefaultBaseURL = "https://dashboard.nodemaven.com"

// APIRoot is the path every account API route starts with.
const APIRoot = "/api/v2/base"

// DefaultPageSize is the largest limit the location endpoints accept. It is a
// ceiling on one answer, not a count of the rows behind it; Iterate walks the
// rest.
const DefaultPageSize = 1000

// DefaultAPITimeout bounds one request made by the built-in HTTP client.
const DefaultAPITimeout = 30 * time.Second

const userAgent = "nodemaven-go"

// Paging is how one endpoint pages: which query parameter sizes a page,
// which one moves the cursor, and how the cursor counts.
type Paging struct {
	SizeKey          string
	CursorKey        string
	CursorCountsRows bool
	// DefaultSize is sent when the caller sets no size; 0 sends none.
	DefaultSize int
	// FirstCursor is sent when the caller sets no cursor.
	FirstCursor int
	// EndsWithNotFound marks an endpoint that answers 404 past its last page.
	EndsWithNotFound bool
}

var (
	// ByOffset pages the location catalogue: limit and offset, from 0.
	ByOffset = Paging{SizeKey: "limit", CursorKey: "offset", CursorCountsRows: true, DefaultSize: DefaultPageSize}
	// ISPCitiesPaging is ByOffset at 100 rows, because that endpoint takes about
	// a minute to answer 1000.
	ISPCitiesPaging = Paging{SizeKey: "limit", CursorKey: "offset", CursorCountsRows: true, DefaultSize: 100}
	// ByPageNumber pages sub-users: per_page and page, from 1.
	ByPageNumber = Paging{SizeKey: "per_page", CursorKey: "page", FirstCursor: 1}
	// WhitelistPaging pages the IP whitelist: page_size and page, from 1, and a
	// 404 past the end.
	WhitelistPaging = Paging{SizeKey: "page_size", CursorKey: "page", DefaultSize: 100, FirstCursor: 1, EndsWithNotFound: true}
)

// Page is one answer from a list endpoint. Rows are decoded JSON, numbers as
// json.Number, objects as map[string]any.
type Page struct {
	Rows []any
	// Count, Next and Previous are the server's, and are empty on every
	// endpoint measured so far.
	Count    *int64
	Next     string
	Previous string

	rowsKey string
	path    string
	query   url.Values
	paging  *Paging
}

// Len is the number of rows on this page.
func (p *Page) Len() int { return len(p.Rows) }

// Client talks to the account API. Build one with NewClient.
type Client struct {
	key     string
	base    string
	timeout time.Duration
	builtin bool
	http    *http.Client
}

// ClientOption configures NewClient.
type ClientOption func(*clientSettings)

type clientSettings struct {
	key     string
	base    string
	timeout time.Duration
	http    *http.Client
}

// WithAPIKey sets the dashboard API key. Without it, NODEMAVEN_APIKEY is read.
// This is not the proxy password.
func WithAPIKey(key string) ClientOption { return func(s *clientSettings) { s.key = key } }

// WithBaseURL points the client at another host. Without it,
// NODEMAVEN_BASE_URL is read, then DefaultBaseURL.
func WithBaseURL(base string) ClientOption { return func(s *clientSettings) { s.base = base } }

// WithTimeout bounds each request made by the built-in HTTP client. It does
// not apply to a client passed with WithHTTPClient.
func WithTimeout(timeout time.Duration) ClientOption {
	return func(s *clientSettings) { s.timeout = timeout }
}

// WithHTTPClient sends requests through your own client. The built-in one
// follows no redirect and reads no proxy from the environment, because the
// API key travels in a header; a client of your own decides both for itself.
// A redirect it follows to the dashboard's not-found page still comes back as
// an error, because that page is not JSON.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(s *clientSettings) { s.http = client }
}

// NewClient builds an account API client.
func NewClient(opts ...ClientOption) (*Client, error) {
	var s clientSettings
	for _, opt := range opts {
		opt(&s)
	}
	key := strings.TrimSpace(firstNonEmpty(s.key, os.Getenv("NODEMAVEN_APIKEY")))
	if key == "" {
		return nil, newError(ErrCredentials,
			"no API key: pass WithAPIKey or set NODEMAVEN_APIKEY in the environment. This is "+
				"the dashboard API key and not the proxy password - they are different secrets.")
	}
	base := strings.TrimRight(firstNonEmpty(s.base, os.Getenv("NODEMAVEN_BASE_URL"), DefaultBaseURL), "/")
	timeout := s.timeout
	if timeout <= 0 {
		timeout = DefaultAPITimeout
	}
	client := &Client{key: key, base: base, timeout: timeout, http: s.http}
	if client.http == nil {
		client.builtin = true
		client.http = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:               nil,
				DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
				TLSHandshakeTimeout: timeout,
				ForceAttemptHTTP2:   true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return client, nil
}

// String does not show the key.
func (c *Client) String() string { return fmt.Sprintf("Client{base: %q, key: %q}", c.base, redacted) }

// GoString does not show the key.
func (c *Client) GoString() string { return c.String() }

// Me is the account. Its fields are the server's own; it carries a live proxy
// password, so do not log it.
func (c *Client) Me(ctx context.Context) (map[string]any, error) {
	return c.object(ctx, "GET", APIRoot+"/users/me", nil, nil)
}

// Countries lists the countries. filters are passed through in the server's
// own spelling, such as country__code; the server ignores a name it does not
// know, so a misspelt filter is dropped without a word.
func (c *Client) Countries(ctx context.Context, filters url.Values) (*Page, error) {
	return c.list(ctx, "locations/countries/", filters, "results", &ByOffset)
}

// Regions lists the regions.
func (c *Client) Regions(ctx context.Context, filters url.Values) (*Page, error) {
	return c.list(ctx, "locations/regions/", filters, "results", &ByOffset)
}

// Cities lists the cities. One page holds at most 1000; use Iterate.
func (c *Client) Cities(ctx context.Context, filters url.Values) (*Page, error) {
	return c.list(ctx, "locations/cities/", filters, "results", &ByOffset)
}

// ISPs lists the ISPs.
func (c *Client) ISPs(ctx context.Context, filters url.Values) (*Page, error) {
	return c.list(ctx, "locations/isps/", filters, "isps", &ByOffset)
}

// ISPRegions lists the regions that have ISPs.
func (c *Client) ISPRegions(ctx context.Context, filters url.Values) (*Page, error) {
	return c.list(ctx, "locations/isps/regions/", filters, "regions", &ByOffset)
}

// ISPCities lists the cities that have ISPs, 100 at a time.
func (c *Client) ISPCities(ctx context.Context, filters url.Values) (*Page, error) {
	return c.list(ctx, "locations/isps/cities/", filters, "cities", &ISPCitiesPaging)
}

// ZipCodes lists the zip codes.
func (c *Client) ZipCodes(ctx context.Context, filters url.Values) (*Page, error) {
	return c.list(ctx, "locations/zipcodes/", filters, "results", &ByOffset)
}

// ZipCodeRegions lists the regions that have zip codes.
func (c *Client) ZipCodeRegions(ctx context.Context, filters url.Values) (*Page, error) {
	return c.list(ctx, "locations/zipcodes/regions/", filters, "regions", &ByOffset)
}

// ZipCodeCities lists the cities that have zip codes.
func (c *Client) ZipCodeCities(ctx context.Context, filters url.Values) (*Page, error) {
	return c.list(ctx, "locations/zipcodes/cities/", filters, "cities", &ByOffset)
}

// StatisticsData is traffic over time for one proxy username. Dates are
// dd-mm-yyyy; give start and end, or period, or the server answers 500.
func (c *Client) StatisticsData(ctx context.Context, proxyUsername string, filters url.Values) (map[string]any, error) {
	return c.object(ctx, "GET", APIRoot+"/statistics/data/", with(filters, "proxy_username", proxyUsername), nil)
}

// StatisticsRequests is request counts over time for one proxy username.
func (c *Client) StatisticsRequests(ctx context.Context, proxyUsername string, filters url.Values) (map[string]any, error) {
	return c.object(ctx, "GET", APIRoot+"/statistics/requests/", with(filters, "proxy_username", proxyUsername), nil)
}

// DomainStatistics is traffic by domain for one proxy username. It does not
// page; the whole answer is one page.
func (c *Client) DomainStatistics(ctx context.Context, proxyUsername string, filters url.Values) (*Page, error) {
	query := with(filters, "proxy_username", proxyUsername)
	path := APIRoot + "/statistics/domains/"
	body, err := c.request(ctx, "GET", path, query, nil)
	if err != nil {
		return nil, err
	}
	page, err := pageFrom(body, "data")
	if err != nil {
		return nil, err
	}
	page.path, page.query = path, query
	return page, nil
}

// SubUsers lists the sub-users. Each row carries a live proxy password.
func (c *Client) SubUsers(ctx context.Context, filters url.Values) (*Page, error) {
	return c.list(ctx, "sub-users/", filters, "payload", &ByPageNumber)
}

// CreateSubUser creates a sub-user. extra adds fields such as traffic_limit.
func (c *Client) CreateSubUser(ctx context.Context, proxyUsername, proxyPassword string, extra map[string]any) (any, error) {
	body := map[string]any{"proxy_username": proxyUsername, "proxy_password": proxyPassword}
	for name, value := range extra {
		body[name] = value
	}
	return c.envelope(ctx, "POST", APIRoot+"/sub-users/", nil, body)
}

// UpdateSubUser changes the given fields of one sub-user.
func (c *Client) UpdateSubUser(ctx context.Context, id any, changes map[string]any) (any, error) {
	if len(changes) == 0 {
		return nil, apiError(0, fmt.Sprintf(
			"UpdateSubUser(%s) was given nothing to change. A body carrying only an id can be "+
				"answered 200, so this would look like it worked and do nothing.", text(id)))
	}
	body := map[string]any{}
	for name, value := range changes {
		body[name] = value
	}
	body["id"] = id
	return c.envelope(ctx, "PUT", APIRoot+"/sub-users/", nil, body)
}

// DeleteSubUser deletes one sub-user.
func (c *Client) DeleteSubUser(ctx context.Context, id any) (any, error) {
	return c.envelope(ctx, "DELETE", APIRoot+"/sub-users/", url.Values{"id": {text(id)}}, nil)
}

// ResetSubUserUsage resets the usage counters of the given sub-users.
func (c *Client) ResetSubUserUsage(ctx context.Context, ids []any) (any, error) {
	if ids == nil {
		ids = []any{}
	}
	return c.envelope(ctx, "POST", APIRoot+"/sub-users/reset/usage", nil, map[string]any{"ids": ids})
}

// WhitelistIPs lists the whitelisted addresses.
func (c *Client) WhitelistIPs(ctx context.Context, filters url.Values) (*Page, error) {
	return c.list(ctx, "whitelist/ips", filters, "results", &WhitelistPaging)
}

// WhitelistIP is one whitelisted address.
func (c *Client) WhitelistIP(ctx context.Context, ipID any) (map[string]any, error) {
	return c.object(ctx, "GET", APIRoot+"/whitelist/ip/"+percentEncode(text(ipID)), nil, nil)
}

// UpsertWhitelistIP creates or replaces a whitelisted address. protocol is
// always sent, as HTTP unless extra says otherwise, because the server does
// not apply the default its own document declares.
func (c *Client) UpsertWhitelistIP(ctx context.Context, ip string, portsCount int, extra map[string]any) (map[string]any, error) {
	body := map[string]any{"ip": ip, "ports_count": portsCount, "protocol": "HTTP"}
	for name, value := range extra {
		body[name] = value
	}
	return c.object(ctx, "POST", APIRoot+"/whitelist/ip/upsert", nil, body)
}

// DeleteWhitelistIP removes a whitelisted address.
func (c *Client) DeleteWhitelistIP(ctx context.Context, ipID any) (any, error) {
	return c.request(ctx, "DELETE", APIRoot+"/whitelist/ip/"+percentEncode(text(ipID)), nil, nil)
}

// Validate checks a proxy's parameters against the live catalogue and returns
// a sentence per problem, or none. It costs a request per catalogue page.
func (c *Client) Validate(ctx context.Context, proxy *Proxy) ([]string, error) {
	var problems []string
	city, hasCity := proxy.Param("city")
	if _, hasRegion := proxy.Param("region"); hasCity && !hasRegion {
		problems = append(problems, fmt.Sprintf(
			"city=%q was sent without a region. The gateway answers that with 500 Internal "+
				"Server Error, which reads as a fault on their side and is not one - the same "+
				"city with its own region answers 200.", city))
	}
	wanted, ok := proxy.Param("country")
	if !ok {
		return problems, nil
	}
	connectionType, ok := proxy.Param("type")
	if !ok {
		connectionType = "residential"
	}
	page, err := c.Countries(ctx, url.Values{"connection_type": {connectionType}})
	if err != nil {
		return nil, err
	}
	codes := map[string]bool{}
	rows := c.Iterate(ctx, page)
	for rows.Next() {
		if row, ok := rows.Row().(map[string]any); ok {
			if code, ok := row["code"].(string); ok {
				codes[asciiLower(code)] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch {
	case len(codes) == 0:
		problems = append(problems,
			"the country catalogue came back with no readable codes, so nothing was checked. "+
				"This is a bug here rather than a problem with your parameters - do not treat "+
				"it as a pass.")
	case wanted != "any" && !codes[asciiLower(wanted)]:
		problems = append(problems, fmt.Sprintf(
			"country=%q is not in the catalogue for connection_type=%q. The gateway answers "+
				"this with 407 Proxy Authentication Required, which reads as a credentials "+
				"problem and is not one.", wanted, connectionType))
	}
	return problems, nil
}

// Rows walks a collection row by row from the page given, asking for the
// following pages as it goes:
//
//	rows := client.Iterate(ctx, page)
//	for rows.Next() {
//		use(rows.Row())
//	}
//	if err := rows.Err(); err != nil { ... }
//
// It stops with an error rather than loop: past MaxPages pages, on a next
// link it has already followed, on a page identical to the one before, and on
// a next link to another host, because that request would carry the API key.
type Rows struct {
	// MaxPages bounds the walk. Set it before the first call to Next.
	MaxPages int

	ctx     context.Context
	client  *Client
	current *Page
	index   int
	pages   int
	seen    map[string]bool
	row     any
	err     error
	done    bool
}

// Iterate starts a walk from page.
func (c *Client) Iterate(ctx context.Context, page *Page) *Rows {
	return &Rows{MaxPages: 100, ctx: ctx, client: c, current: page, seen: map[string]bool{}}
}

// All walks the whole collection and returns every row.
func (c *Client) All(ctx context.Context, page *Page) ([]any, error) {
	var out []any
	rows := c.Iterate(ctx, page)
	for rows.Next() {
		out = append(out, rows.Row())
	}
	return out, rows.Err()
}

// Row is the current row.
func (r *Rows) Row() any { return r.row }

// Err is what stopped the walk, or nil when it reached the end.
func (r *Rows) Err() error { return r.err }

// Next advances to the next row, fetching a page when needed.
func (r *Rows) Next() bool {
	for !r.done && r.current != nil {
		current := r.current
		if r.index < len(current.Rows) {
			r.row = current.Rows[r.index]
			r.index++
			return true
		}
		next := current.Next
		var path string
		var asked url.Values
		if next == "" {
			var ok bool
			if path, asked, ok = nextStep(current); !ok {
				r.done = true
				break
			}
		}
		r.pages++
		if r.pages >= r.MaxPages {
			return r.fail(apiError(0, fmt.Sprintf(
				"stopped after %d pages, which is the bound in MaxPages. Raise it deliberately "+
					"if the collection really is this large.", r.MaxPages)))
		}
		if next != "" {
			if r.seen[next] {
				return r.fail(apiError(0, fmt.Sprintf(
					"the API returned a next page url it had already returned (%q), so "+
						"following it is a loop. Stopped after %d pages.", next, r.pages)))
			}
			r.seen[next] = true
			body, err := r.client.request(r.ctx, "GET", next, nil, nil)
			if err != nil {
				return r.fail(err)
			}
			page, err := pageFrom(body, current.rowsKey)
			if err != nil {
				return r.fail(err)
			}
			r.current, r.index = page, 0
			continue
		}
		paging := current.paging
		body, err := r.client.request(r.ctx, "GET", path, asked, nil)
		if err != nil {
			if paging.EndsWithNotFound && errors.Is(err, ErrNotFound) {
				r.done = true
				break
			}
			return r.fail(err)
		}
		following, err := pageFrom(body, current.rowsKey)
		if err != nil {
			return r.fail(err)
		}
		if len(following.Rows) > 0 && reflect.DeepEqual(following.Rows, current.Rows) {
			cursor := paging.CursorKey
			return r.fail(apiError(0, fmt.Sprintf(
				"asking %s for %s=%s returned the same %d rows as %s=%s, so the server is "+
					"accepting `%s` and ignoring it. Every further page would repeat these rows.",
				path, cursor, asked.Get(cursor), len(following.Rows), cursor,
				current.query.Get(cursor), cursor)))
		}
		following.path, following.query, following.paging = path, asked, paging
		r.current, r.index = following, 0
	}
	r.row = nil
	return false
}

func (r *Rows) fail(err error) bool {
	r.err, r.done, r.row = err, true, nil
	return false
}

func nextStep(page *Page) (string, url.Values, bool) {
	if page.path == "" || page.paging == nil || len(page.query) == 0 || len(page.Rows) == 0 {
		return "", nil, false
	}
	paging := page.paging
	cursor, err := strconv.ParseInt(page.query.Get(paging.CursorKey), 10, 64)
	if err != nil {
		return "", nil, false
	}
	advanced := cursor + 1
	if paging.CursorCountsRows {
		size, err := strconv.ParseInt(page.query.Get(paging.SizeKey), 10, 64)
		if err != nil || size == 0 {
			return "", nil, false
		}
		advanced = cursor + int64(len(page.Rows))
	}
	following := cloneValues(page.query)
	following.Set(paging.CursorKey, strconv.FormatInt(advanced, 10))
	return page.path, following, true
}

func (c *Client) list(ctx context.Context, path string, filters url.Values, rowsKey string, paging *Paging) (*Page, error) {
	path = APIRoot + "/" + path
	query := cloneValues(filters)
	if paging.DefaultSize != 0 && !query.Has(paging.SizeKey) {
		query.Set(paging.SizeKey, strconv.Itoa(paging.DefaultSize))
	}
	if !query.Has(paging.CursorKey) {
		query.Set(paging.CursorKey, strconv.Itoa(paging.FirstCursor))
	}
	body, err := c.request(ctx, "GET", path, query, nil)
	if err != nil {
		return nil, err
	}
	page, err := pageFrom(body, rowsKey)
	if err != nil {
		return nil, err
	}
	page.path, page.query, page.paging = path, query, paging
	return page, nil
}

func (c *Client) object(ctx context.Context, method, path string, query url.Values, body map[string]any) (map[string]any, error) {
	answer, err := c.request(ctx, method, path, query, body)
	if err != nil {
		return nil, err
	}
	object, ok := answer.(map[string]any)
	if !ok {
		return nil, apiError(0, fmt.Sprintf(
			"%s %s answered with %s where an object was expected. The API's shape has changed "+
				"or something other than the API answered.", method, path, kind(answer)))
	}
	return object, nil
}

func (c *Client) envelope(ctx context.Context, method, path string, query url.Values, body map[string]any) (any, error) {
	answer, err := c.request(ctx, method, path, query, body)
	if err != nil {
		return nil, err
	}
	object, ok := answer.(map[string]any)
	if !ok {
		return answer, nil
	}
	if success, ok := object["success"].(bool); ok && !success {
		reason := detail(object)
		if reason == "" {
			reason = "no reason given"
		}
		return nil, apiError(0, fmt.Sprintf(
			"%s %s answered 2xx with success=false: %s. A success flag inside a 2xx is the "+
				"server disagreeing with its own status line.", method, path, reason))
	}
	if payload, ok := object["payload"]; ok {
		return payload, nil
	}
	return object, nil
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, body map[string]any) (any, error) {
	target := c.base + path
	if strings.HasPrefix(path, "http") {
		if !sameOrigin(c.base, path) {
			return nil, apiError(0, fmt.Sprintf(
				"refusing to send the API key to %q, which is not %s. This url came back from "+
					"the API as a paging link, and the request that would follow it carries your "+
					"key in a header.", path, c.base))
		}
		target = path
	}
	if encoded := encodeQuery(query); encoded != "" {
		if strings.Contains(target, "?") {
			target += "&" + encoded
		} else {
			target += "?" + encoded
		}
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, &Error{Kind: ErrAPI, cause: err, Message: fmt.Sprintf(
				"%s %s: the request body cannot be written as JSON: %v. Nothing was sent.", method, path, err)}
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return nil, &Error{Kind: ErrAPI, cause: err, Message: fmt.Sprintf(
			"%s %s cannot be sent: %v. Nothing was sent.", method, target, err)}
	}
	req.Header.Set("Authorization", "x-api-key "+c.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, &Error{Kind: ErrAPI, cause: err, Message: fmt.Sprintf(
			"%s %s: %s. No status came back, so this says nothing about the API key or the request.",
			method, target, c.failure(ctx, err))}
	}
	return interpret(resp.StatusCode, raw, method, target)
}

func (c *Client) failure(ctx context.Context, err error) string {
	var timeout interface{ Timeout() bool }
	switch {
	case ctx.Err() != nil:
		return "the context ended first: " + ctx.Err().Error()
	case errors.As(err, &timeout) && timeout.Timeout():
		if c.builtin {
			return fmt.Sprintf("no complete answer before the timeout of %g s", c.timeout.Seconds())
		}
		return "no complete answer before the timeout"
	default:
		return fmt.Sprintf("the connection failed mid-request: %v", err)
	}
}

func interpret(status int, raw []byte, method, target string) (any, error) {
	trimmed := strings.TrimSpace(string(raw))
	var parsed any
	isJSON := false
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		decoder := json.NewDecoder(strings.NewReader(trimmed))
		decoder.UseNumber()
		isJSON = decoder.Decode(&parsed) == nil
	}
	where := method + " " + target
	if status >= 200 && status < 300 {
		if trimmed == "" {
			if method == "DELETE" {
				return map[string]any{}, nil
			}
			return nil, apiError(status, fmt.Sprintf(
				"%s answered %d with an empty body where JSON was expected, so there is nothing "+
					"to read the answer from.", where, status))
		}
		if !isJSON {
			what := "text"
			if strings.HasPrefix(trimmed, "<") {
				what = "HTML"
			}
			return nil, apiError(status, fmt.Sprintf(
				"%s answered %d with %d bytes of %s where JSON was expected. This host "+
					"redirects a path it does not serve to a not-found page, and a client that "+
					"follows the redirect gets that page with 200, so the status says nothing "+
					"about whether the call did anything.", where, status, len(raw), what))
		}
		return parsed, nil
	}
	reason := trimmed
	if isJSON {
		reason = detail(parsed)
	}
	if reason == "" {
		reason = "HTTP " + strconv.Itoa(status)
	}
	switch {
	case status == 401 || status == 403:
		return nil, &Error{Kind: ErrAuth, Status: status, Message: fmt.Sprintf(
			"%s was refused: %s. This is the dashboard API key, not the proxy password - "+
				"check NODEMAVEN_APIKEY.", where, reason)}
	case status == 404:
		return nil, &Error{Kind: ErrNotFound, Status: status, Message: fmt.Sprintf(
			"%s found nothing: %s.", where, reason)}
	case status == 429:
		return nil, &Error{Kind: ErrRateLimit, Status: status, Message: fmt.Sprintf(
			"%s was rate limited: %s. Nothing here retries; back off on your own schedule.", where, reason)}
	case status >= 500:
		return nil, apiError(status, fmt.Sprintf(
			"%s failed on the server: %s. A 5xx that repeats is worth reporting rather than "+
				"hammering.", where, reason))
	default:
		return nil, apiError(status, fmt.Sprintf("%s was refused: %s.", where, reason))
	}
}

func pageFrom(body any, rowsKey string) (*Page, error) {
	switch v := body.(type) {
	case []any:
		return &Page{Rows: v, rowsKey: rowsKey}, nil
	case map[string]any:
		if rows, ok := v[rowsKey].([]any); ok {
			page := &Page{Rows: rows, rowsKey: rowsKey}
			if number, ok := v["count"].(json.Number); ok {
				if count, err := number.Int64(); err == nil && count >= 0 {
					page.Count = &count
				}
			}
			page.Next, _ = v["next"].(string)
			page.Previous, _ = v["previous"].(string)
			return page, nil
		}
		if len(v) == 0 {
			return &Page{Rows: []any{}, rowsKey: rowsKey}, nil
		}
		return &Page{Rows: []any{v}, rowsKey: rowsKey}, nil
	default:
		return nil, apiError(0, fmt.Sprintf(
			"a list endpoint answered with %s, which is neither a page nor a list. Either the "+
				"API changed shape or something other than the API answered.", kind(body)))
	}
}

// detail reads the reason out of an error body, in any of the shapes this
// API uses. It never returns the body itself.
func detail(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		return flatten(v)
	case map[string]any:
		for _, key := range []string{"detail", "message", "error", "description"} {
			if text, ok := v[key].(string); ok && text != "" {
				return text
			}
		}
		if nested, ok := v["error"].(map[string]any); ok {
			if message, ok := nested["message"].(string); ok {
				if code, ok := nested["code"].(string); ok && code != "" {
					return message + " (" + code + ")"
				}
				return message
			}
		}
		switch errs := v["errors"].(type) {
		case string:
			if errs != "" {
				return errs
			}
		case map[string]any:
			if len(errs) > 0 {
				return flatten(errs)
			}
		case []any:
			if len(errs) > 0 {
				return flatten(errs)
			}
		}
		return flatten(v)
	default:
		return ""
	}
}

func flatten(value any) string {
	switch v := value.(type) {
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = flatten(item)
		}
		return strings.Join(parts, "; ")
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = key + ": " + flatten(v[key])
		}
		return strings.Join(parts, ", ")
	default:
		return text(v)
	}
}

func sameOrigin(base, target string) bool {
	here, ok1 := origin(base)
	there, ok2 := origin(target)
	return ok1 && ok2 && here == there
}

func origin(raw string) (string, bool) {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return "", false
	}
	scheme = asciiLower(scheme)
	authority := rest
	if at := strings.IndexAny(rest, "/?#"); at >= 0 {
		authority = rest[:at]
	}
	if strings.Contains(authority, "@") {
		return "", false
	}
	host := authority
	var port int
	if at := strings.LastIndexByte(authority, ':'); at >= 0 && !strings.Contains(authority[at:], "]") {
		number, ok := portNumber(authority[at+1:])
		if !ok {
			return "", false
		}
		host, port = authority[:at], number
	} else {
		switch scheme {
		case "https":
			port = 443
		case "http":
			port = 80
		default:
			return "", false
		}
	}
	if host == "" {
		return "", false
	}
	return scheme + "://" + asciiLower(host) + ":" + strconv.Itoa(port), true
}

func kind(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "a bool"
	case json.Number, float64:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// text renders an id the way it goes into a path or a query: a string as it
// is, anything else as JSON.
func text(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(raw)
}

// encodeQuery percent-encodes the same way as the other NodeMaven SDKs, with
// names sorted so the same query is the same bytes every time.
func encodeQuery(query url.Values) string {
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, name)
	}
	sort.Strings(names)
	var parts []string
	for _, name := range names {
		for _, value := range query[name] {
			parts = append(parts, percentEncode(name)+"="+percentEncode(value))
		}
	}
	return strings.Join(parts, "&")
}

func cloneValues(values url.Values) url.Values {
	out := make(url.Values, len(values))
	for name, list := range values {
		out[name] = append([]string(nil), list...)
	}
	return out
}

func with(filters url.Values, name, value string) url.Values {
	query := cloneValues(filters)
	query.Set(name, value)
	return query
}
