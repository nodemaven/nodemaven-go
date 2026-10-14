# nodemaven-go

Go client for [NodeMaven](https://nodemaven.com) proxies. It builds the proxy
username the gateway expects, checks the parameters before anything is sent,
and hands the result to the HTTP client you already use. It also wraps the
dashboard account API.

```
go get github.com/nodemaven/nodemaven-go
```

Requires Go 1.22 or later. One dependency, `github.com/BurntSushi/toml`, to
read gateway definitions.

## Quick start

```go
proxy, err := nodemaven.New(
	nodemaven.WithLogin("your-login"),       // or NODEMAVEN_LOGIN
	nodemaven.WithPassword("your-password"), // or NODEMAVEN_PASSWORD
	nodemaven.WithParam("country", "us"),
	nodemaven.WithParam("sid", "order4417"),
)
if err != nil {
	log.Fatal(err)
}

client := &http.Client{Transport: &http.Transport{Proxy: proxy.HTTPProxy()}}
resp, err := client.Get("https://example.com")
```

Other forms of the same proxy:

| call | gives |
|---|---|
| `proxy.Username()` | `your-login-country-us-sid-order4417` |
| `proxy.URL()` | `http://user:pass@host:port`, both credentials percent-encoded |
| `proxy.URLWithScheme("socks5")` | the same with another scheme |
| `proxy.ProxyURL()` | a `*url.URL` |
| `proxy.HTTPProxy()` | a value for `http.Transport.Proxy` |
| `proxy.Browser()` | server, username and password separately, for Playwright, chromedp or Rod |

Printing a `Proxy`, a `Connect` or a `BrowserProxy` with any verb, `%#v`
included, shows the password as `***`.

## Parameters

| parameter | what it selects | values measured to work |
|---|---|---|
| `country` | country code, or `any` | `us`, `de`, ... |
| `region` | area inside the country | a name |
| `city` | city inside the country - send it with its `region` | a name |
| `isp` | the exit's ISP | a name |
| `type` | mobile or residential exits, which are different pools | `mobile`, `residential` |
| `sid` | the sticky session - see below | any string with no `-` |
| `ttl` | how long that session is held | `1m`, `10m`, `10h`, `24h` |
| `filter` | IP quality | `low`, `medium`, `high` |
| `speed` | claims a connection speed class | `fast`, `slow` |
| `ipv4` | claims to force IPv4 | `true` |

`ipv4` and `speed` are accepted by the gateway; their effects have not been
independently verified.

**Names are checked; values are passed through** unless a gateway definition
lists the legal ones. An unknown name fails in `New`, because the gateway
answers it with 200 and drops the setting:

```text
NodeMaven does not know the parameter "contry": it is answered with 200 and
dropped, so the connection would succeed and your setting would NOT be applied.
Known: ["city" "country" "filter" "ipv4" "isp" "region" "sid" "speed" "ttl" "type"]
```

`country`, `region`, `city`, `isp` and `type` are folded to the wire form:
surrounding ASCII whitespace trimmed, `A`-`Z` lowered and nothing else, each
remaining space turned into `_`. `District of Columbia` is sent as
`district_of_columbia`. `sid`, `filter`, `ttl` and `speed` are sent as given,
so pass those in lower case. A value that is not folded and contains
whitespace is refused.

Errors are values you test with `errors.Is`:

| error | when |
|---|---|
| `ErrParam` | an unknown name, an empty value, a value with whitespace or the separator in it, a value outside a declared list |
| `ErrCredentials` | no login or password, no host or port, a port that is not 1 to 65535, no API key |
| `ErrProvider` | a gateway definition that does not describe a gateway |
| `ErrCheck` | a CONNECT that produced no answer at all |
| `ErrAPI` | the account API answered with an error or with something that is not JSON |
| `ErrAuth`, `ErrNotFound`, `ErrRateLimit` | 401/403, 404, 429 - each also matches `ErrAPI` |

Every error is a `*nodemaven.Error` carrying the kind and, for the account API,
the HTTP status. None carries a response body.

## Sticky sessions

```go
held, err := proxy.Session("order4417")
germany, err := proxy.With("country", "de") // a new identity, a new exit
plain, err := proxy.Without("filter")       // also a new identity
```

A `Proxy` never changes; these return new ones. **The session key is the whole
parameter set**, so adding or removing any parameter moves you to a different
exit. The order parameters are given in does not.

A session id cannot contain `-`: the gateway cuts every value at its
separator. For a pool, draw the ids:

```go
pool, err := proxy.Sessions(50) // 50 proxies, each its own exit
```

Ids are 12 hexadecimal characters from `crypto/rand`, distinct within one
call; `SessionsOfLength(n, bytes)` sets the length.

## Checking a connection

```go
check, err := proxy.Check(ctx)
if err != nil {
	log.Fatal(err) // DNS, refused, timed out, or not an HTTP answer
}
fmt.Println(check)
// 407 Proxy Authentication Required via gate.nodemaven.com:8080 in 0.41s
// <what 407 means on this gateway>
```

`Check` sends one CONNECT to `api.ipify.org:443` and reads the reply head. A
refusal is a `*Check` with its `Status`, `Reason`, `Headers` and `Meaning`,
not an error. `ExitIP` is empty when the gateway did not send the exit address,
which is normal. `OK()` means the tunnel opened, not that every parameter was
applied. To change the target or the timeout:

```go
connect := proxy.Connect()
connect.Target = "example.com:443"
connect.Timeout = 30 * time.Second
check, err := connect.Send(ctx)
```

`Check` is the only call on a `Proxy` that opens a socket.

## Account API

```go
client, err := nodemaven.NewClient() // NODEMAVEN_APIKEY: the dashboard key, not the proxy password
me, err := client.Me(ctx)
page, err := client.Cities(ctx, url.Values{"country__code": {"us"}})

rows := client.Iterate(ctx, page)
for rows.Next() {
	city := rows.Row().(map[string]any)
	_ = city["code"]
}
if err := rows.Err(); err != nil { ... }
```

| call | returns |
|---|---|
| `Me` | the account |
| `Countries`, `Regions`, `Cities`, `ISPs`, `ISPRegions`, `ISPCities`, `ZipCodes`, `ZipCodeRegions`, `ZipCodeCities` | a `*Page` |
| `StatisticsData`, `StatisticsRequests` | an object |
| `DomainStatistics` | a `*Page` that does not page |
| `SubUsers`, `WhitelistIPs` | a `*Page` |
| `WhitelistIP` | an object |
| `CreateSubUser`, `UpdateSubUser`, `DeleteSubUser`, `ResetSubUserUsage` | the server's payload |
| `UpsertWhitelistIP`, `DeleteWhitelistIP` | the server's answer |
| `Iterate`, `All` | every row from a page onwards |
| `Validate` | problems with a proxy's `country` and `city`, checked against the catalogue |

Objects are `map[string]any` with numbers as `json.Number`; rows are `[]any`.

- **`Me` and `SubUsers` return live proxy passwords.** Do not log their
  results.
- Filters go through in the server's own spelling. The server ignores a name
  it does not know, so a misspelt filter is dropped without an error.
- One location page holds at most 1000 rows; `Cities` for the US has more.
  Use `Iterate`. It stops with an error instead of looping, and refuses a next
  link to another host, since that request would carry the key.
- Statistics dates are `dd-mm-yyyy`. Give `start` and `end`, or `period`
  (`today`, `hours24`); without either the server answers 500.
- The built-in HTTP client follows no redirect and reads no proxy from the
  environment, because the key travels in a header. Pass your own with
  `WithHTTPClient`; a JSON-less 2xx is still an error.

Nothing here retries.

## Other gateways

The dialect is data, not code: separators, known names, folds and status
meanings live in a TOML definition. `LoadFile("mygateway.toml")` reads one
and `WithProvider` uses it; `NewProvider` builds one in code. Definitions are
checked when loaded.

## License

MIT
