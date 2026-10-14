// Package nodemaven builds NodeMaven proxy credentials, checks a connection
// through the gateway, and wraps the dashboard account API.
//
// A Proxy is one gateway identity. New validates every parameter before
// anything is sent, because the gateway answers an unknown parameter name
// with 200 and drops it:
//
//	proxy, err := nodemaven.New(
//		nodemaven.WithLogin("your-login"),
//		nodemaven.WithPassword("your-password"),
//		nodemaven.WithParam("country", "us"),
//	)
//	client := &http.Client{Transport: &http.Transport{Proxy: proxy.HTTPProxy()}}
//
// Only Proxy.Check and the Client methods open connections. Errors are
// *Error values tested with errors.Is against ErrParam, ErrCredentials,
// ErrProvider, ErrCheck, ErrAPI, ErrAuth, ErrNotFound and ErrRateLimit.
//
// The gateway's dialect - separators, known names, value folding and what its
// statuses mean - is data in a TOML definition. The NodeMaven one is built
// in; LoadFile reads another.
package nodemaven
