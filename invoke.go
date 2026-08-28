package vov

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// InvokeRequest describes an in-process call to a declared endpoint.
//
// It is a request in every sense that matters to the endpoint — path parameters
// resolve, middleware runs, the auth guard enforces the declaration — except
// that nothing is serialized and no socket is involved.
type InvokeRequest struct {
	// Method is the HTTP method to dispatch. Empty means GET.
	Method string

	// Path is the URL path, e.g. "/projects/42". It must begin with "/". Path
	// parameters are matched against the declared routes exactly as they are for
	// a network request, so r.PathValue works in the handler.
	Path string

	// Query is the URL query, if any.
	Query url.Values

	// Body is the request body. When it is non-empty and Header sets no
	// Content-Type, application/json is assumed — both known callers send JSON.
	Body []byte

	// Header carries any additional request headers.
	Header http.Header

	// User is the principal the call acts as, already resolved.
	//
	// Invoke does not authenticate: it takes this user's identity on trust,
	// because the caller is in-process and has established identity by whatever
	// means it owns — an OAuth access token for a tool call, a fixture for a
	// test. Supplying the wrong user here is an authorization bypass, so the
	// caller must be the thing that verified it.
	//
	// What Invoke does still enforce is everything the endpoint declares about
	// this user: RolesAnyOf, PermissionsAllOf and MinTier are checked by the same
	// guard a network request meets, with the same 401, 403 and 402 answers. A
	// nil User is anonymous and is refused by any endpoint requiring auth.
	User User
}

// InvokeResult is what the endpoint answered.
type InvokeResult struct {
	Status int
	Header http.Header
	Body   []byte
}

// Invoke dispatches a request to a declared endpoint in process and returns the
// response, without a network round trip.
//
// It exists because two things need the same primitive. A tool call — MCP or
// otherwise — that maps onto an existing endpoint should reach that endpoint
// rather than a second implementation of it, or the two drift. And a test runner
// that calls every declared endpoint as several principals, or creates a record
// as one user and reaches for it as another, is doing exactly this and nothing
// more.
//
// It runs the endpoint's full middleware stack, not just its handler. That is
// not an optimization to skip: an application whose stack republishes the
// resolved user, opens a transaction, or scopes a tenant would otherwise hand
// the handler a request missing all of it — and the failure is silent, because
// the handler still runs.
//
// It does not run [AppConfig.ServerWrappers]. Those exist for requests arriving
// over the network — CORS on an in-process call is meaningless, and a tool
// invocation is already inside whatever recovery and logging wrapped the request
// that triggered it. Dispatch goes through [App.Mux], which is the same
// distinction [App.Handler] draws.
//
// The dispatched request carries [RequestModeInvoke], so middleware can meter
// and audit the two channels apart. Handlers should read that, not branch on it
// — see [RequestMode].
//
// The returned error reports a request that could not be built — a malformed
// path, an invalid method. Anything the router or the endpoint decided, 404 and
// 405 included, comes back as an [InvokeResult] with that status, because those
// are answers rather than failures.
func (a *App) Invoke(ctx context.Context, req InvokeRequest) (InvokeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	if req.Path == "" || req.Path[0] != '/' {
		return InvokeResult{}, fmt.Errorf("vov: Invoke: path %q must begin with %q", req.Path, "/")
	}

	target := &url.URL{Path: req.Path}
	if len(req.Query) > 0 {
		target.RawQuery = req.Query.Encode()
	}

	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return InvokeResult{}, fmt.Errorf("vov: Invoke: %w", err)
	}

	// Shape it like a request the server received rather than one a client is
	// about to send: handlers and middleware that read RequestURI should see it.
	hr.RequestURI = target.RequestURI()
	if len(req.Body) > 0 {
		hr.ContentLength = int64(len(req.Body))
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	if len(req.Body) > 0 && hr.Header.Get("Content-Type") == "" {
		hr.Header.Set("Content-Type", "application/json")
	}

	// Vouch for the user, and tag the channel. Both keys are unexported, so
	// nothing outside this package can forge either — which is what keeps the
	// guard's trust in the first one safe, and the second one worth metering on.
	ictx := context.WithValue(hr.Context(), invokeUserKey{}, invokeUser{user: req.User})
	hr = hr.WithContext(withMode(ictx, RequestModeInvoke))

	rec := &captureWriter{header: make(http.Header)}
	a.mux.ServeHTTP(rec, hr)

	return InvokeResult{
		Status: rec.statusCode(),
		Header: rec.header,
		Body:   rec.body.Bytes(),
	}, nil
}

// captureWriter records a response instead of writing it to a connection.
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status // first write wins, as it does for a real response
	}
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(b)
}

// statusCode reports the status the endpoint set, defaulting to 200 for a
// handler that wrote nothing at all.
func (c *captureWriter) statusCode() int {
	if c.status == 0 {
		return http.StatusOK
	}
	return c.status
}
