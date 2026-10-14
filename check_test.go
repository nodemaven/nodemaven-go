package nodemaven

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeGateway serves one CONNECT on loopback, records the request head and
// answers with reply. Every assertion is about bytes that crossed a socket,
// and nothing leaves the machine.
func fakeGateway(t *testing.T, reply string) (server string, request <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	got := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)
		var head strings.Builder
		for {
			line, err := reader.ReadString('\n')
			head.WriteString(line)
			if err != nil || line == "\r\n" {
				break
			}
		}
		got <- head.String()
		_, _ = conn.Write([]byte(reply))
	}()
	return listener.Addr().String(), got
}

func gatewayProxy(t *testing.T, server string, opts ...Option) *Proxy {
	host, port, _ := net.SplitHostPort(server)
	number, _ := strconv.Atoi(port)
	return testProxy(t, append([]Option{WithHost(host), WithPort(number)}, opts...)...)
}

func TestCheckSendsTheBuiltUsername(t *testing.T) {
	server, request := fakeGateway(t, "HTTP/1.1 200 Connection established\r\nX-Exit-IP: 198.51.100.7\r\n\r\n")
	proxy := gatewayProxy(t, server, WithParam("country", "us"))
	check, err := proxy.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	head := <-request
	token := base64.StdEncoding.EncodeToString([]byte("acct-country-us:pw"))
	want := "CONNECT api.ipify.org:443 HTTP/1.1\r\nHost: api.ipify.org:443\r\n" +
		"Proxy-Authorization: Basic " + token + "\r\nProxy-Connection: close\r\n\r\n"
	if head != want {
		t.Fatalf("sent %q", head)
	}
	if !check.OK() || check.Reason != "Connection established" || check.ExitIP != "198.51.100.7" {
		t.Fatalf("%+v", check)
	}
	if strings.Contains(check.String(), token) {
		t.Fatal("the credential is in the result")
	}
}

func TestARefusalIsAValue(t *testing.T) {
	server, _ := fakeGateway(t, "HTTP/1.1 407 Proxy Authentication Required\r\nX-Proxy-Exit-IP: 1\r\nx-proxy-exit-ip: 2\r\n\r\n")
	check, err := gatewayProxy(t, server).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if check.OK() || check.Status != 407 || check.Meaning == "" {
		t.Fatalf("%+v", check)
	}
	if check.ExitIP != "2" {
		t.Fatalf("the last occurrence of the header wins, got %q", check.ExitIP)
	}
	if !strings.Contains(check.String(), check.Meaning) {
		t.Fatal(check.String())
	}
}

func TestExitHeadersAreTriedInOrder(t *testing.T) {
	server, _ := fakeGateway(t, "HTTP/1.1 200 OK\r\nX-Exit-IP: second\r\nX-Proxy-Exit-IP: first\r\n\r\n")
	check, err := gatewayProxy(t, server).Check(context.Background())
	if err != nil || check.ExitIP != "first" {
		t.Fatalf("%+v %v", check, err)
	}
}

func TestBareLineFeedsEndTheHead(t *testing.T) {
	server, _ := fakeGateway(t, "HTTP/1.0 200 OK\nX-Exit-IP: 192.0.2.1\n\nbody that is not read")
	check, err := gatewayProxy(t, server).Check(context.Background())
	if err != nil || check.ExitIP != "192.0.2.1" {
		t.Fatalf("%+v %v", check, err)
	}
}

func TestReasonIsLatin1(t *testing.T) {
	server, _ := fakeGateway(t, "HTTP/1.1 502 Bad \xe9\xff Gateway\r\n\r\n")
	check, err := gatewayProxy(t, server).Check(context.Background())
	if err != nil || check.Reason != "Bad éÿ Gateway" {
		t.Fatalf("%q %v", check.Reason, err)
	}
}

func TestNoAnswerIsAnError(t *testing.T) {
	for name, reply := range map[string]string{
		"closed without a word": "",
		"not a status line":     "SSH-2.0-OpenSSH_9.6\r\n\r\n",
		"superscript status":    "HTTP/1.1 2² OK\r\n\r\n",
		"plus in the status":    "HTTP/1.1 +20 OK\r\n\r\n",
		"cut mid-head":          "HTTP/1.1 200 OK\r\nX-Exit",
		"endless head":          "HTTP/1.1 200 OK\r\n" + strings.Repeat("X: y\r\n", 4000),
	} {
		server, _ := fakeGateway(t, reply)
		_, err := gatewayProxy(t, server).Check(context.Background())
		if !errors.Is(err, ErrCheck) {
			t.Errorf("%s: want ErrCheck, got %v", name, err)
		}
	}
}

func TestRefusedBeforeSending(t *testing.T) {
	connect := NewConnect("127.0.0.1:9", "u", "p")
	connect.Timeout = 0
	if _, err := connect.Send(context.Background()); !errors.Is(err, ErrCheck) || !strings.Contains(err.Error(), "Nothing was sent") {
		t.Fatalf("zero timeout: %v", err)
	}
	for _, server := range []string{"no-port", ":8080", "h:0", "h:+80", "h:65536"} {
		if _, err := NewConnect(server, "u", "p").Send(context.Background()); !errors.Is(err, ErrCheck) {
			t.Errorf("%q: want ErrCheck, got %v", server, err)
		}
	}
	connect = NewConnect("127.0.0.1:9", "u", "p")
	connect.Target = "a b"
	if _, err := connect.Send(context.Background()); !errors.Is(err, ErrCheck) {
		t.Fatalf("bad target: %v", err)
	}
}

func TestContextCancelsAWait(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer func() { _ = conn.Close() }()
			time.Sleep(5 * time.Second)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = NewConnect(listener.Addr().String(), "u", "p").Send(ctx)
	if !errors.Is(err, ErrCheck) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want ErrCheck wrapping the deadline, got %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("the context did not cut the wait short")
	}
}
