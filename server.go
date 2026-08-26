package vov

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"time"
)

// A Server defines parameters for running an HTTP server.
// The zero value for Server is a valid configuration.
type Server struct {

	// DisableGeneralOptionsHandler, if true, passes "OPTIONS *" requests to the Handler,
	// otherwise responds with 200 OK and Content-Length: 0.
	DisableGeneralOptionsHandler bool

	// TLSConfig optionally provides a TLS configuration for use
	// by ServeTLS and ListenAndServeTLS. Note that this value is
	// cloned by ServeTLS and ListenAndServeTLS, so it's not
	// possible to modify the configuration with methods like
	// tls.Config.SetSessionTicketKeys. To use
	// SetSessionTicketKeys, use Server.Serve with a TLS Listener
	// instead.
	TLSConfig *tls.Config

	// default: 15 * time.Second
	// ReadTimeout is the maximum duration for reading the entire
	// request, including the body. A zero or negative value means
	// there will be no timeout.
	//
	// Because ReadTimeout does not let Handlers make per-request
	// decisions on each request body's acceptable deadline or
	// upload rate, most users will prefer to use
	// ReadHeaderTimeout. It is valid to use them both.
	ReadTimeout *time.Duration

	// default: 10 * time.Second
	// ReadHeaderTimeout is the amount of time allowed to read
	// request headers. The connection's read deadline is reset
	// after reading the headers and the Handler can decide what
	// is considered too slow for the body. If zero, the value of
	// ReadTimeout is used. If negative, or if zero and ReadTimeout
	// is zero or negative, there is no timeout.
	ReadHeaderTimeout *time.Duration

	// default: 15 * time.Second
	// WriteTimeout is the maximum duration before timing out
	// writes of the response. It is reset whenever a new
	// request's header is read. Like ReadTimeout, it does not
	// let Handlers make decisions on a per-request basis.
	// A zero or negative value means there will be no timeout.
	WriteTimeout *time.Duration

	// default: 60 * time.Second
	// IdleTimeout is the maximum amount of time to wait for the
	// next request when keep-alives are enabled. If zero, the value
	// of ReadTimeout is used. If negative, or if zero and ReadTimeout
	// is zero or negative, there is no timeout.
	IdleTimeout *time.Duration

	// MaxHeaderBytes controls the maximum number of bytes the
	// server will read parsing the request header's keys and
	// values, including the request line. It does not limit the
	// size of the request body.
	// If zero, DefaultMaxHeaderBytes is used.
	MaxHeaderBytes int

	// MaxHeaderValueCount controls the maximum number of header
	// values that the server is willing to parse from a request.
	// If zero, DefaultMaxHeaderValueCount is used.
	// Note that comma-separated values in a single header line are
	// counted once, while values sent as multiple header lines are
	// counted multiple times.
	MaxHeaderValueCount int

	// TLSNextProto optionally specifies a function to take over
	// ownership of the provided TLS connection when an ALPN
	// protocol upgrade has occurred. The map key is the protocol
	// name negotiated. The Handler argument should be used to
	// handle HTTP requests and will initialize the Request's TLS
	// and RemoteAddr if not already set. The connection is
	// automatically closed when the function returns.
	// If TLSNextProto is not nil, HTTP/2 support is not enabled
	// automatically.
	//
	// Historically, TLSNextProto was used to disable HTTP/2 support.
	// The Server.Protocols field now provides a simpler way to do this.
	TLSNextProto map[string]func(*http.Server, *tls.Conn, http.Handler)

	// ConnState specifies an optional callback function that is
	// called when a client connection changes state. See the
	// ConnState type and associated constants for details.
	ConnState func(net.Conn, http.ConnState)

	// ErrorLog specifies an optional logger for errors accepting
	// connections, unexpected behavior from handlers, and
	// underlying FileSystem errors.
	// If nil, logging is done via the log package's standard logger.
	ErrorLog *log.Logger

	// BaseContext optionally specifies a function that returns
	// the base context for incoming requests on this server.
	// The provided Listener is the specific Listener that's
	// about to start accepting requests.
	// If BaseContext is nil, the default is context.Background().
	// If non-nil, it must return a non-nil context.
	BaseContext func(net.Listener) context.Context

	// ConnContext optionally specifies a function that modifies
	// the context used for a new connection c. The provided ctx
	// is derived from the base context and has a ServerContextKey
	// value.
	ConnContext func(ctx context.Context, c net.Conn) context.Context

	// HTTP2 configures HTTP/2 connections.
	HTTP2 *http.HTTP2Config

	// Protocols is the set of protocols accepted by the server.
	//
	// If Protocols includes UnencryptedHTTP2, the server will accept
	// unencrypted HTTP/2 connections. The server can serve both
	// HTTP/1 and unencrypted HTTP/2 on the same address and port.
	//
	// If Protocols is nil, the default is usually HTTP/1 and HTTP/2.
	// If TLSNextProto is non-nil and does not contain an "h2" entry,
	// the default is HTTP/1 only.
	Protocols *http.Protocols

	// DisableClientPriority specifies whether client-specified priority, as
	// specified in RFC 9218, should be respected or not.
	//
	// This field only takes effect if using HTTP/2, and if no custom write
	// scheduler is defined for the HTTP/2 server. Otherwise, this field is a
	// no-op.
	//
	// If set to true, requests will be served in a round-robin manner, without
	// prioritization.
	DisableClientPriority bool
}

func (s *Server) ToNetHTTPServer(addr string, handler http.Handler) *http.Server {
	if s == nil {
		s = &Server{}
	}

	var thisReadHeaderTimeout time.Duration
	if s.ReadHeaderTimeout == nil {
		thisReadHeaderTimeout = 10 * time.Second
	} else {
		thisReadHeaderTimeout = *s.ReadHeaderTimeout
	}

	var thisReadTimeout time.Duration
	if s.ReadTimeout == nil {
		thisReadTimeout = 15 * time.Second
	} else {
		thisReadTimeout = *s.ReadTimeout
	}

	var thisWriteTimeout time.Duration
	if s.WriteTimeout == nil {
		thisWriteTimeout = 15 * time.Second
	} else {
		thisWriteTimeout = *s.WriteTimeout
	}

	var thisIdleTimeout time.Duration
	if s.IdleTimeout == nil {
		thisIdleTimeout = 60 * time.Second
	} else {
		thisIdleTimeout = *s.IdleTimeout
	}

	return &http.Server{
		Addr:                         addr,
		Handler:                      handler,
		DisableGeneralOptionsHandler: s.DisableGeneralOptionsHandler,
		TLSConfig:                    s.TLSConfig,
		ReadTimeout:                  thisReadTimeout,
		ReadHeaderTimeout:            thisReadHeaderTimeout,
		WriteTimeout:                 thisWriteTimeout,
		IdleTimeout:                  thisIdleTimeout,
		MaxHeaderBytes:               s.MaxHeaderBytes,
		MaxHeaderValueCount:          s.MaxHeaderValueCount,
		TLSNextProto:                 s.TLSNextProto,
		ConnState:                    s.ConnState,
		ErrorLog:                     s.ErrorLog,
		BaseContext:                  s.BaseContext,
		ConnContext:                  s.ConnContext,
		HTTP2:                        s.HTTP2,
		Protocols:                    s.Protocols,
		DisableClientPriority:        s.DisableClientPriority,
	}
}

// Ptr returns a pointer to v. It lets the pointer-typed fields of [Server] be set
// inline instead of through a temporary variable:
//
//	vov.Server{ReadTimeout: vov.Ptr(30 * time.Second)}
func Ptr[T any](v T) *T {
	return &v
}
