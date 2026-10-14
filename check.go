package nodemaven

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// DefaultTarget is the host a check asks the gateway to tunnel to. Whatever
// is named sees a real TCP connection from the exit address.
const DefaultTarget = "api.ipify.org:443"

// DefaultCheckTimeout bounds each step of a check. One of the gateway's
// measured reactions is no reply for about 20 seconds, so a shorter default
// would report a parameter mistake as a network problem.
const DefaultCheckTimeout = 15 * time.Second

// maxHead bounds the response head. A head is a few hundred bytes; anything
// longer without a blank line is a stream, not a reply.
const maxHead = 16384

// Header is one response header. Name is lower-cased ASCII.
type Header struct {
	Name  string
	Value string
}

// Check is the gateway's answer to one CONNECT. A refusal is a Check with a
// status other than 200, not an error: the status is the answer.
type Check struct {
	// Status is the CONNECT status.
	Status int
	// Reason is the reason phrase, verbatim.
	Reason string
	// Server is the host:port that was asked.
	Server string
	// Elapsed includes DNS and the TCP handshake.
	Elapsed time.Duration
	// Headers holds every response header in the order received.
	Headers []Header
	// ExitIP is the exit address, or "" when the gateway did not send one.
	// That is normal on this gateway, not an error.
	ExitIP string
	// Meaning is what this status means on this gateway, or "" when nobody
	// has recorded it.
	Meaning string
}

// OK reports whether the tunnel opened. It does not mean every parameter was
// applied: an unknown name is answered 200 and dropped, which is why New
// refuses unknown names before anything is sent.
func (c *Check) OK() bool { return c.Status == 200 }

// Header returns the last value sent under name, compared without case.
func (c *Check) Header(name string) (string, bool) {
	wanted := asciiLower(name)
	for i := len(c.Headers) - 1; i >= 0; i-- {
		if c.Headers[i].Name == wanted {
			return c.Headers[i].Value, true
		}
	}
	return "", false
}

func (c *Check) String() string {
	out := fmt.Sprintf("%d %s via %s in %.2fs", c.Status, c.Reason, c.Server, c.Elapsed.Seconds())
	if c.ExitIP != "" {
		out += ", exit " + c.ExitIP
	}
	if c.Meaning != "" && !c.OK() {
		out += "\n" + c.Meaning
	}
	return out
}

// Connect is one CONNECT request, before it is sent. Proxy.Connect returns
// one filled in from the proxy and its definition; NewConnect fills in the
// defaults for a gateway described by hand.
type Connect struct {
	Server   string
	Username string
	Password string
	// Target is the host:port to tunnel to.
	Target string
	// Timeout bounds each step: resolving, connecting, writing, and each read.
	// Zero is refused rather than read as "no limit".
	Timeout time.Duration
	// ExitIPHeaders are tried in order; the first one present wins.
	ExitIPHeaders []string
	// Reactions maps a status, as text, to what it means on this gateway.
	Reactions map[string]string
}

// NewConnect describes a CONNECT to server with the default target and
// timeout.
func NewConnect(server, username, password string) Connect {
	return Connect{
		Server: server, Username: username, Password: password,
		Target: DefaultTarget, Timeout: DefaultCheckTimeout,
	}
}

// String redacts the password.
func (c Connect) String() string {
	return fmt.Sprintf("Connect{Server: %q, Username: %q, Password: %q, Target: %q, Timeout: %s}",
		c.Server, c.Username, redacted, c.Target, c.Timeout)
}

// GoString redacts the password.
func (c Connect) GoString() string { return c.String() }

// Send opens one connection to the gateway, writes the CONNECT and reads the
// response head. It returns an error only when nothing usable came back.
func (c Connect) Send(ctx context.Context) (*Check, error) {
	if c.Timeout <= 0 {
		return nil, newError(ErrCheck, fmt.Sprintf(
			"the timeout for %s is %s, so there is no time in which to answer. Nothing was sent.",
			c.Server, c.Timeout))
	}
	host, port, err := splitAddress(c.Server)
	if err != nil {
		return nil, err
	}
	if !isRequestTarget(c.Target) {
		return nil, newError(ErrCheck, fmt.Sprintf(
			"%q cannot go in a request line: a target is visible ASCII with no spaces, so "+
				"that it lands in the CONNECT as one token. Nothing was sent.", c.Target))
	}
	token := base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))
	request := "CONNECT " + c.Target + " HTTP/1.1\r\n" +
		"Host: " + c.Target + "\r\n" +
		"Proxy-Authorization: Basic " + token + "\r\n" +
		"Proxy-Connection: close\r\n" +
		"\r\n"

	started := time.Now()
	dialer := net.Dialer{Timeout: c.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, noAnswer(c.Server, ctx, err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if err := conn.SetWriteDeadline(time.Now().Add(c.Timeout)); err != nil {
		return nil, noAnswer(c.Server, ctx, err)
	}
	if _, err := io.WriteString(conn, request); err != nil {
		return nil, noAnswer(c.Server, ctx, err)
	}
	head, err := readHead(ctx, conn, c.Server, c.Timeout)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(started)
	status, reason, headers, err := parseHead(head, c.Server)
	if err != nil {
		return nil, err
	}
	check := &Check{
		Status: status, Reason: reason, Server: c.Server, Elapsed: elapsed,
		Headers: headers, Meaning: c.Reactions[strconv.Itoa(status)],
	}
	for _, name := range c.ExitIPHeaders {
		if value, ok := check.Header(name); ok {
			check.ExitIP = value
			break
		}
	}
	return check, nil
}

func readHead(ctx context.Context, conn net.Conn, server string, timeout time.Duration) ([]byte, error) {
	var head []byte
	chunk := make([]byte, 4096)
	for {
		if end := headEnd(head); end >= 0 {
			return head[:end], nil
		}
		if len(head) > maxHead {
			return nil, newError(ErrCheck, fmt.Sprintf(
				"%s sent %d bytes with no end to the response head. A head is a few hundred "+
					"bytes, so this is a stream and not a reply, and reading further is how a "+
					"client runs out of memory.", server, len(head)))
		}
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, noAnswer(server, ctx, err)
		}
		n, err := conn.Read(chunk)
		head = append(head, chunk[:n]...)
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			if end := headEnd(head); end >= 0 {
				return head[:end], nil
			}
			if len(head) == 0 {
				return head, nil
			}
			return nil, newError(ErrCheck, fmt.Sprintf(
				"%s closed the connection after %d bytes, part-way through the response "+
					"head. Whatever came before the cut is not an answer - a status line "+
					"without its blank line may be missing headers that had not arrived yet.",
				server, len(head)))
		}
		return nil, noAnswer(server, ctx, err)
	}
}

// headEnd is the index just past the blank line ending the head, or -1. A
// bare LF is accepted as a line ending, as the other SDKs accept it.
func headEnd(head []byte) int {
	end := -1
	for _, terminator := range [][]byte{[]byte("\n\n"), []byte("\n\r\n")} {
		if found := bytes.Index(head, terminator); found >= 0 {
			candidate := found + len(terminator)
			if end < 0 || candidate < end {
				end = candidate
			}
		}
	}
	return end
}

func parseHead(head []byte, server string) (int, string, []Header, error) {
	if len(head) == 0 {
		return 0, "", nil, newError(ErrCheck, fmt.Sprintf(
			"%s accepted the connection and then closed it without answering. That is not "+
				"one of the reactions this gateway is known to have, so it is worth reporting "+
				"with the parameters that produced it.", server))
	}
	// latin-1: a header value is bytes, and a gateway may put any of them in a
	// reason phrase. Each byte becomes the code point of the same value.
	runes := make([]rune, len(head))
	for i, b := range head {
		runes[i] = rune(b)
	}
	lines := strings.Split(string(runes), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	first := lines[0]
	parts := strings.SplitN(first, " ", 3)
	version, code := parts[0], ""
	if len(parts) > 1 {
		code = parts[1]
	}
	if !isHTTPVersion(version) || !isStatusCode(code) {
		return 0, "", nil, newError(ErrCheck, fmt.Sprintf(
			"%s answered %q, which is not an HTTP status line. Either something other than "+
				"a proxy is listening on that port, or a middlebox answered instead of the "+
				"gateway.", server, first))
	}
	status, _ := strconv.Atoi(code)
	reason := ""
	if len(parts) > 2 {
		reason = parts[2]
	}
	var headers []Header
	for _, line := range lines[1:] {
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok {
			headers = append(headers, Header{
				Name:  asciiLower(strings.TrimSpace(name)),
				Value: strings.TrimSpace(value),
			})
		}
	}
	return status, reason, headers, nil
}

func isASCIIDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

func isStatusCode(text string) bool { return len(text) == 3 && isASCIIDigits(text) }

func isHTTPVersion(text string) bool {
	return len(text) == 8 && strings.HasPrefix(text, "HTTP/") &&
		isASCIIDigits(text[5:6]) && text[6] == '.' && isASCIIDigits(text[7:8])
}

func isRequestTarget(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] <= ' ' || text[i] >= 0x7f {
			return false
		}
	}
	return true
}

func splitAddress(server string) (string, int, error) {
	refused := newError(ErrCheck, fmt.Sprintf(
		"%q is not a gateway address: it has to be host:port, with a port from 1 to 65535. "+
			"Nothing was sent.", server))
	at := strings.LastIndexByte(server, ':')
	if at <= 0 {
		return "", 0, refused
	}
	port, ok := portNumber(server[at+1:])
	if !ok {
		return "", 0, refused
	}
	host := server[:at]
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if host == "" {
		return "", 0, refused
	}
	return host, port, nil
}

// portNumber is the one definition of a port in this package: one to five
// ASCII digits, 1 to 65535. strconv.Atoi accepts "+8080", which is not a port.
func portNumber(text string) (int, bool) {
	if !isASCIIDigits(text) || len(text) > 5 {
		return 0, false
	}
	port, err := strconv.Atoi(text)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

func noAnswer(server string, ctx context.Context, err error) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return &Error{Kind: ErrCheck, cause: err, Message: fmt.Sprintf(
		"no answer from %s: %v. Nothing here can tell you whether the credentials are "+
			"right, because the gateway was never reached.", server, err)}
}
