package vov

import (
	"context"
	"encoding/json"
	"time"
)

// ToolOutcome says how a tool call ended.
//
// The distinction that matters is whether an endpoint was reached at all: a call
// an endpoint refused and a call that never got that far are different events,
// and only the first has a status worth recording.
type ToolOutcome string

const (
	// ToolOutcomeOK is a call an endpoint answered successfully.
	ToolOutcomeOK ToolOutcome = "ok"

	// ToolOutcomeRefused is a call an endpoint answered with a non-2xx status —
	// the declared policy said no, or the handler did.
	ToolOutcomeRefused ToolOutcome = "refused"

	// ToolOutcomeRejected is a call that never reached an endpoint because its
	// arguments could not be mapped onto one: a missing path parameter, a value
	// that is not a scalar.
	//
	// This is the outcome nothing else can see. No endpoint ran, so no
	// middleware ran either, and an assistant looping on an argument it keeps
	// getting wrong is invisible everywhere but here.
	ToolOutcomeRejected ToolOutcome = "rejected"

	// ToolOutcomeFailed is a call that could not be dispatched: the caller could
	// not be resolved, or dispatch itself errored. It is reported to the client
	// as a protocol error rather than a tool error.
	ToolOutcomeFailed ToolOutcome = "failed"
)

// ToolCall is the record of one tool call, handed to [MCPConfig.OnToolCall].
type ToolCall struct {
	// Tool is the name the assistant called.
	Tool string

	// Arguments are the arguments it sent, undecoded, and are empty when the
	// call failed before they were read.
	//
	// They are raw on purpose. vov does not know which of an application's
	// fields hold a person's name, a note body, or anything else worth being
	// careful with, so it passes what it has rather than inventing a summary
	// that would be wrong in a way nobody notices. Deciding what is safe to keep
	// is the application's — see [MCPConfig.OnToolCall].
	Arguments map[string]json.RawMessage

	// Outcome says how the call ended; Status is the endpoint's, and is zero
	// unless the outcome is [ToolOutcomeOK] or [ToolOutcomeRefused].
	Outcome ToolOutcome
	Status  int

	// User is the principal the call acted as, and is nil when the call failed
	// before one was resolved.
	User User

	// Scopes are the scopes the calling credential carried.
	Scopes []string

	// Err is set for [ToolOutcomeFailed] and explains what could not be done.
	Err error

	// Duration is how long the call took, including authentication.
	Duration time.Duration
}

// observeToolCall hands the record to the application's sink, if it has one.
//
// It cannot fail the call, and it cannot take it down either: a sink that panics
// is recovered here. Both follow from when this runs — the endpoint has already
// committed whatever it was going to commit, so there is nothing left to undo and
// nothing useful to do with a complaint. Turning a written row into a reported
// failure would be a lie about what happened.
func observeToolCall(ctx context.Context, cfg *MCPConfig, call ToolCall) {
	if cfg.OnToolCall == nil {
		return
	}
	defer func() { _ = recover() }()
	// Values without cancellation. A sink wants the trace id and the request
	// scope the call carried; it must not lose the row because the assistant
	// hung up, and the row describes something that already happened, so there
	// is nothing left for a deadline to save.
	cfg.OnToolCall(context.WithoutCancel(ctx), call)
}

// outcomeOf classifies a completed dispatch.
func outcomeOf(status int) ToolOutcome {
	if status >= 200 && status < 300 {
		return ToolOutcomeOK
	}
	return ToolOutcomeRefused
}
