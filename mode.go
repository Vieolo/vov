package vov

import "context"

// RequestMode says which of an application's channels a request arrived on: its
// HTTP API, or its MCP tool interface.
//
// It names the channel rather than the transport that carried it. Every API
// request happens to arrive over the network and every MCP call happens to be
// dispatched in process, but those are implementation, and a reader of an audit
// row or a `switch` in a handler wants the fact they can act on. It also stays
// true when the correspondence breaks: a runner exercising the API dispatches in
// process like any tool call, and is still exercising the API.
//
// It is a legitimate input to how an application handles a request, not merely a
// label to record. Metering and audit rows are the obvious uses; rendering a
// response differently for a model than for a browser, or applying a different
// body limit, are equally proper.
//
// # Vary the wiring, not the effect
//
// The reason [App.invoke] dispatches to the declared endpoint at all is that a
// tool call and an API call should be the same code path. Varying how a request
// is carried, rendered, limited or metered keeps that. Varying what the operation
// *does* — which records it touches, what it writes, whether it is permitted —
// gives it up, and puts a second implementation behind one URL that no test, no
// manifest and no reviewer is looking at.
//
// The line that holds in practice: shape the request and the response by mode,
// decide the outcome by the declaration.
//
// # It cannot be set from outside
//
// There is no wire representation and no exported setter. That is a security
// property rather than housekeeping, and more so the more an application decides
// with it: were the mode a header, whoever held a credential could choose it —
// billing browser traffic to a tool's allowance, or marking records as
// agent-reviewed that no agent saw. The only ways into a mode are to arrive over
// the network, or to be dispatched by [App.invoke], whose caller is in process
// and already trusted with [invokeRequest.User].
type RequestMode string

const (
	// RequestModeAPI is a call to the application's HTTP API. It is what
	// [ModeFrom] reports for any request not tagged otherwise, which is both safe
	// and true: a request nothing dispatched arrived over the network, and
	// defaulting the other way would bill browser traffic to a tool's allowance.
	RequestModeAPI RequestMode = "api"

	// RequestModeMCP is a call arriving as a Model Context Protocol tool, which
	// reaches its endpoint through [App.invoke].
	RequestModeMCP RequestMode = "mcp"
)

// valid reports whether m is a mode vov knows. RequestMode is a string type, so
// a typo compiles; [App.invoke] rejects one rather than dispatching a request
// whose channel nothing downstream will recognize. The zero value is not a mode:
// [invokeRequest.Mode] requires one, and an untagged request is answered by
// [ModeFrom] rather than carrying an empty mode of its own.
func (m RequestMode) valid() bool {
	switch m {
	case RequestModeAPI, RequestModeMCP:
		return true
	default:
		return false
	}
}

// requestModeKey types the context slot carrying the mode.
//
// It is unexported, and there is deliberately no exported way to set it. The
// mode has no wire representation at all: were it a header, whoever holds a
// credential could set it, and the abuse is not hypothetical — an account could
// bill its browser usage to a tool allowance, or mark records as agent-reviewed
// that no agent ever saw. Naming a mode on an [invokeRequest] is the only way to
// be in one other than by arriving over the network.
type requestModeKey struct{}

// ModeFrom reports which channel the request carrying ctx arrived on. It answers
// [RequestModeAPI] for anything vov did not dispatch itself, which is what an
// untagged request is.
//
// vov names the two channels it serves. An application with more of them — a
// queue consumer, a CLI, a second protocol — can put its own value on the
// context it passes to [App.invoke], where it reaches the handler alongside this
// one.
func ModeFrom(ctx context.Context) RequestMode {
	if m, ok := ctx.Value(requestModeKey{}).(RequestMode); ok {
		return m
	}
	return RequestModeAPI
}

// withMode tags ctx with the mode. Unexported on purpose — see [requestModeKey].
func withMode(ctx context.Context, m RequestMode) context.Context {
	return context.WithValue(ctx, requestModeKey{}, m)
}
