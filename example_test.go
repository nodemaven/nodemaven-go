package nodemaven_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"

	"github.com/nodemaven/nodemaven-go"
)

func Example() {
	proxy, err := nodemaven.New(
		nodemaven.WithLogin("your-login"),
		nodemaven.WithPassword("your-password"),
		nodemaven.WithParam("country", "us"),
		nodemaven.WithParam("sid", "order4417"),
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(proxy.Username())

	client := &http.Client{Transport: &http.Transport{Proxy: proxy.HTTPProxy()}}
	_ = client // client.Get("https://example.com") leaves from one sticky exit
	// Output: your-login-country-us-sid-order4417
}

func ExampleNew_refusal() {
	_, err := nodemaven.New(
		nodemaven.WithLogin("your-login"),
		nodemaven.WithPassword("your-password"),
		nodemaven.WithParam("contry", "us"),
	)
	fmt.Println(errors.Is(err, nodemaven.ErrParam))
	// Output: true
}

func ExampleProxy_Sessions() {
	proxy, err := nodemaven.New(
		nodemaven.WithLogin("your-login"),
		nodemaven.WithPassword("your-password"),
		nodemaven.WithParam("country", "de"),
	)
	if err != nil {
		log.Fatal(err)
	}
	pool, err := proxy.Sessions(10)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(pool))
	// Output: 10
}

func ExampleProxy_Check() {
	proxy, err := nodemaven.New(nodemaven.WithParam("country", "us"))
	if err != nil {
		log.Fatal(err)
	}
	check, err := proxy.Check(context.Background())
	if err != nil {
		log.Fatal(err) // nothing came back at all
	}
	fmt.Println(check) // a refusal is a status, with what it means on this gateway
}

func ExampleClient_Iterate() {
	client, err := nodemaven.NewClient() // reads NODEMAVEN_APIKEY
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	page, err := client.Cities(ctx, url.Values{"country__code": {"us"}})
	if err != nil {
		log.Fatal(err)
	}
	rows := client.Iterate(ctx, page)
	for rows.Next() {
		city := rows.Row().(map[string]any)
		fmt.Println(city["code"])
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
}
