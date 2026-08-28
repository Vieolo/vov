package vov

import "context"

// RequestMode says how a request reached an endpoint: over the network, or
// in process through [App.Invoke].
//
// It exists so that an application can meter and audit the two channels
// separately — a tool call spending a different quota than a browser call, an
// audit row recording which one wrote a record. It does not exist so that
// handlers can behave differently.
//
// # Read it, do not branch on it
//
// The reason [App.Invoke] dispatches to the declared endpoint at all is that a
// tool and an API call should be the same code path. A handler that changes what
// it *does* based on the mode puts two implementations behind one URL, which is
// the exact thing the in-process dispatch exists to prevent — and the second
// implementation is the one no test, no manifest, and no reviewer is looking at.
//
// The line that holds in practice: recording the mode is fine, deciding with it
// is not. Metering, audit trails, and log fields are recording. An `if mode ==
// …` around business logic is deciding.
type RequestMode string

const (
	// RequestModeNetwork is a request that arrived over HTTP. It is what
	// [ModeFrom] reports for any request not tagged otherwise, which is the safe
	// default: an untagged request really is a network request, and defaulting
	// the other way would bill browser traffic to a tool's allowance.
	RequestModeNetwork RequestMode = "network"

	// RequestModeInvoke is a request dispatched in process by [App.Invoke] —
	// a tool call, or a test runner exercising an endpoint.
	RequestModeInvoke RequestMode = "invoke"
)

// requestModeKey types the context slot carrying the mode.
//
// It is unexported, and there is deliberately no exported way to set it. The
// mode has no wire representation at all: were it a header, whoever holds a
// credential could set it, and the abuse is not hypothetical — an account could
// bill its browser usage to a tool allowance, or mark records as agent-reviewed
// that no agent ever saw. Being [App.Invoke] is the only way to be in invoke
// mode.
type requestModeKey struct{}

// ModeFrom reports how the request carrying ctx arrived. It answers
// [RequestModeNetwork] for anything vov did not dispatch itself.
//
// vov distinguishes exactly two channels, because those are the two it can tell
// apart. An application that needs a finer label — which *kind* of in-process
// caller, say an MCP tool as against a test runner — can put its own value on
// the context it passes to [App.Invoke], where it will reach the handler
// alongside this one.
func ModeFrom(ctx context.Context) RequestMode {
	if m, ok := ctx.Value(requestModeKey{}).(RequestMode); ok {
		return m
	}
	return RequestModeNetwork
}

// withMode tags ctx with the mode. Unexported on purpose — see [requestModeKey].
func withMode(ctx context.Context, m RequestMode) context.Context {
	return context.WithValue(ctx, requestModeKey{}, m)
}
