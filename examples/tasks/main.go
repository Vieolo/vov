// Command tasks is a small example service built with the vov framework. It
// exists to exercise vov's ergonomics from the outside: environment loaded into
// a struct before anything is built, one Route per URL with its methods grouped
// beside their handlers, named middleware stacks split by the auth seam,
// authentication that applies unless an endpoint opts out, the mux escape hatch,
// a cleanup hook, and a lifecycle-managed Run. It keeps its data in memory and
// depends only on the standard library plus vov.
//
// This file wires the service together; tasks.go holds the task resource.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vieolo/vov"
)

// config is both the declaration of what this service reads from the
// environment and the way it reads it, so the two cannot drift apart. Required
// variables stop the server from starting; the rest fall back to a default or a
// zero value.
type config struct {
	Addr     string        `env:"TASKS_ADDR" envDefault:":8080"`
	Token    string        `env:"TASKS_TOKEN,required"`
	Greeting string        `env:"TASKS_GREETING" envDefault:"ok"`
	MaxTasks int           `env:"TASKS_MAX" envDefault:"100"`
	Debug    bool          `env:"TASKS_DEBUG"`
	Idle     time.Duration `env:"TASKS_IDLE_TIMEOUT" envDefault:"90s"`
	Bucket   string        `env:"TASKS_BUCKET" envDefault:"tasks-archive"`
	Origin   string        `env:"TASKS_ORIGIN" envDefault:"https://app.example.com"`
}

func main() {
	// -manifest prints the route manifest and exits, so CI can diff it against
	// the checked-in routes.txt without starting a server.
	printManifest := flag.Bool("manifest", false, "print the route manifest and exit")
	flag.Parse()

	// Load the environment first: everything below is built from it, which is
	// why this is a plain function and not a field of vov.AppConfig.
	var cfg config
	if err := vov.LoadEnv(&cfg); err != nil {
		log.Fatalf("tasks: %v", err)
	}
	if cfg.Debug {
		log.Printf("tasks: config %+v", cfg.redacted())
	}

	// Built once, then published to the handlers through the holder in deps.go.
	deps.Set(newDeps(cfg))

	app, err := vov.NewApp(vov.AppConfig{
		// vov checks the global was populated and refuses to build if not.
		RequireGlobals: []vov.Readiness{deps},
		Address:        cfg.Addr,
		// Tune the underlying server with a vov.Server: the http.Server knobs
		// minus Addr and Handler (vov owns those). Defaulted timeouts are
		// pointers — leave one nil to take vov's default, or set it inline with
		// vov.Ptr (even to 0, meaning "no timeout"). Other fields pass through.
		Server: &vov.Server{
			ReadHeaderTimeout: vov.Ptr(5 * time.Second),
			IdleTimeout:       vov.Ptr(cfg.Idle),
		},
		// Wraps the whole handler, so it also covers the requests the mux
		// refuses on its own — a path no route declares (404), and a method the
		// path does not declare (405), which is what a CORS preflight is.
		// Recovery is outermost so it catches panics in everything inside it,
		// including CORS; CORS sets its headers on the way in, so they survive
		// on a recovered 500 too.
		// Everything that belongs to the HTTP channel and not to the tool one.
		// The POST that carries a tool call is wrapped by these — it is an HTTP
		// request like any other — but the tool call inside it is not.
		API: vov.APIConfig{
			// How this app resolves the user of an HTTP request. Endpoints
			// require one unless they declare vov.AuthModeNone. The valid token
			// comes from the environment, which is why it is built from cfg.
			Authenticator: makeAuthenticator(cfg.Token),
			ServerWrappers: []vov.Middleware{
				recoverPanic(deps.Get().Log),
				cors(cfg.Origin),
			},
		},
		// The middleware combinations this service uses, named once here. Pre
		// runs outside the auth guard (so it covers rejected requests too); Post
		// runs inside it, where the user is known.
		MiddlewareStacks: map[string]vov.MiddlewareStack{
			// requestID and logging sit here so each response says which stack
			// ran, which is what the smoke suite asserts on. A production app
			// would hoist both into API.ServerWrappers instead, so that unrouted
			// requests get an id and a log line too.
			vov.DefaultStackName: {
				Pre:  []vov.Middleware{requestID, logging},
				Post: []vov.Middleware{auditLog},
			},
			// The default plus a body-type check, for endpoints that take JSON.
			"json": {
				Pre:  []vov.Middleware{requestID, logging},
				Post: []vov.Middleware{auditLog, requireJSON},
			},
			// Authenticated by signature rather than by user, so the check goes
			// in Pre: this stack's endpoints declare NoAuth, and Post never runs
			// for them.
			"webhook": {
				Pre: []vov.Middleware{requestID, logging, verifySignature},
			},
			// Deliberately nothing. A health check should not depend on any of it.
			"bare": {},
		},
		// Scopes govern the MCP channel and nothing else: this app's tokens are
		// issued by its OAuth server for assistants, and a browser session has
		// no scope at all, so governing the API would refuse every browser call.
		//
		// Keying by method is what keeps it from being a thing to remember: a
		// new mutating endpoint is governed the moment it exists, because its
		// method was already spoken for. There is no per-route line to forget.
		Scopes: &vov.ScopePolicy{
			// No API map: a browser session carries no scopes, so that channel
			// has no scope model and is not governed at all. The absence is the
			// declaration — and an app that did want to gate, say, deletion from
			// the browser would add an API map with that one method in it.
			MCP: map[string][]string{
				http.MethodGet:    {"tasks:read"},
				http.MethodHead:   {"tasks:read"},
				http.MethodPost:   {"tasks:write"},
				http.MethodPut:    {"tasks:write"},
				http.MethodPatch:  {"tasks:write"},
				http.MethodDelete: {"tasks:write"},
			},
		},
		// The tool server. It names itself and says how a tool caller is
		// identified — and nothing else, because which endpoints are exposed is
		// declared on the endpoints themselves.
		MCP: &vov.MCPConfig{
			Name:    "tasks",
			Title:   "Tasks",
			Version: "0.1.0",
			// Read once, at connection, and worth more than it looks: it is how
			// the assistant learns the order to do things in.
			Instructions: "A shared task list. Call list_tasks before creating or " +
				"deleting anything, so you act on ids that exist. Reports are only " +
				"available to accounts on a paid plan.",
			// The same function vov itself authenticates with. An app whose tool
			// endpoint honoured OAuth access tokens would pass a different one.
			Authenticate: makeAuthenticator(cfg.Token),
			// Every tool call, including the ones no endpoint ever saw. The
			// argument names are recorded and the values are not: vov hands over
			// what the assistant sent, and deciding what is safe to keep is the
			// application's job, not the framework's.
			OnToolCall: auditToolCall,
			// Serve it here. vov builds the handler and mounts it; there is
			// nothing to do after NewApp.
			Path: "/mcp",
		},

		// Which URL each endpoint group is mounted on. The groups themselves
		// live beside their handlers — see tasks.go — so this list stays a map
		// of the service's URLs rather than a pile of handler references.
		Routes: []vov.Route{
			{Path: "/healthz", Endpoints: healthEndpoints(cfg.Greeting)},
			{Path: "/tasks", Endpoints: collectionEndpoints()},
			{Path: "/tasks/{id}", Endpoints: itemEndpoints()},
			{Path: "/reports", Endpoints: reportEndpoints()},
			{Path: "/webhook", Endpoints: webhookEndpoints()},
		},
	})
	if err != nil {
		log.Fatalf("tasks: %v", err)
	}

	application.Set(app)

	if *printManifest {
		fmt.Print(app.Manifest())
		os.Exit(0)
	}

	// Escape hatch: register straight on the underlying mux, bypassing vov.
	// Useful for anything vov does not model yet. These routes are entirely
	// yours — the framework adds no middleware and no auth to them.
	app.Mux().HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": "0.1.0"})
	})

	// Deliberately panics, to show that ServerWrappers cover escape-hatch
	// routes too: nothing vov knows about is involved in serving this.
	app.Mux().HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) {
		panic("demonstrating panic recovery")
	})

	// Cleanup hook, run during graceful shutdown.
	app.OnShutdown(func(ctx context.Context) error {
		d := deps.Get()
		d.Log.Info("shutting down", "tasks_in_memory", d.Store.len())
		return nil
	})

	deps.Get().Log.Info("listening", "addr", cfg.Addr)
	if err := app.Run(); err != nil {
		log.Fatalf("tasks: %v", err)
	}
}

// redacted returns a copy safe to log: the token is a credential and must never
// reach a log line, the way vov itself never puts a value in an EnvError.
func (c config) redacted() config {
	if c.Token != "" {
		c.Token = "[redacted]"
	}
	return c
}

// --- auth -------------------------------------------------------------------

// user is this app's own model. It satisfies vov.User by answering the three
// questions vov asks; everything else about it stays the app's business.
//
// Roles and permissions are resolved once, when the user is built, and answered
// from memory — so an endpoint requiring two permissions does not become two
// lookups. In a real service this is where the session query would put them.
type user struct {
	name  string
	roles []string
	perms []string
	tier  int // 0 = free; higher numbers are paid subscriptions
}

func (u *user) IsAuthenticated() bool { return u != nil && u.name != "" }

func (u *user) Roles() []string {
	if u == nil {
		return nil
	}
	return u.roles
}

func (u *user) Permissions() []string {
	if u == nil {
		return nil
	}
	return u.perms
}

func (u *user) Tier() int {
	if u == nil {
		return 0
	}
	return u.tier
}

// makeAuthenticator builds the app's vov.Authenticator around the token from the
// environment. It owns the credential lookup, which in a real service would hit
// a session table or verify a signature.
func makeAuthenticator(valid string) vov.Authenticator {
	return func(resp vov.AuthResponse, r *http.Request) (vov.User, error) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case token == "":
			return nil, nil // no credentials presented -> 401
		case token == "t-boom":
			return nil, errors.New("session store unavailable") // -> 500, not 401
		case token == valid+"-revoked":
			// The account is gone, but the credential is a signed cookie that
			// stays valid for 30 days. Refusing is not enough: clear it here, or
			// the browser keeps presenting a dead credential until it expires.
			resp.SetCookie(&http.Cookie{
				Name: "session", Path: "/", MaxAge: -1,
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			return nil, nil // -> 401, with the cookie cleared on the way out
		case token == valid:
			// An ordinary member: may write tasks, but is not an admin. A full
			// grant, so the scope rules below never bite for this credential.
			resp.SetScopes([]string{"tasks:read", "tasks:write"})
			return &user{name: "ramtin", roles: []string{"member"}, perms: []string{"tasks.write"}}, nil
		case token == valid+"-admin":
			resp.SetScopes([]string{"tasks:read", "tasks:write"})
			return &user{name: "admin", roles: []string{"member", "admin"}, perms: []string{"tasks.write"}}, nil
		case token == valid+"-owner":
			resp.SetScopes([]string{"tasks:read", "tasks:write"})
			// The second of DELETE's any-of roles: also allowed.
			return &user{name: "owner", roles: []string{"owner"}, perms: []string{"tasks.write"}}, nil
		case token == valid+"-halfadmin":
			resp.SetScopes([]string{"tasks:read", "tasks:write"})
			// Holds the role but not the permission. DELETE needs both, so this
			// is the case a roles-or-permissions design could not express.
			return &user{name: "halfadmin", roles: []string{"admin"}}, nil
		case token == valid+"-pro":
			resp.SetScopes([]string{"tasks:read", "tasks:write"})
			// A paying member: the only one who gets past /reports.
			return &user{name: "pro", roles: []string{"member"}, perms: []string{"tasks.write"}, tier: 2}, nil
		case token == valid+"-readonly":
			// The scope axis, in one case. Same person and same permissions as
			// the ordinary member above — a *different key*, cut for reading
			// only. No permission model can express this: the principal is
			// identical, and it is the credential that is narrower.
			resp.SetScopes([]string{"tasks:read"})
			return &user{name: "ramtin", roles: []string{"member"}, perms: []string{"tasks.write"}}, nil
		case token == valid+"-reader":
			// Authenticated, but holds no write permission: 403, never 401. A
			// full grant, so what refuses this one is the permission model and
			// not the credential — the two axes stay separable in the tests.
			resp.SetScopes([]string{"tasks:read", "tasks:write"})
			return &user{name: "reader", roles: []string{"member"}}, nil
		default:
			return nil, nil
		}
	}
}

// currentUser is the typed accessor the app defines over vov's interface. vov
// hands back a vov.User; the concrete type reappears here, in one place.
func currentUser(r *http.Request) *user {
	u, ok := vov.UserFrom(r.Context())
	if !ok {
		return nil
	}
	return u.(*user)
}

// healthEndpoints serves /healthz: open and unwrapped, since a health check
// should depend on neither auth nor middleware. It reports the greeting from
// the environment, the shortest way to watch a value travel from an env var to
// a response body.
func healthEndpoints(greeting string) vov.Endpoints {
	return vov.Endpoints{
		GET: vov.Endpoint{
			MiddlewareStack: "bare",
			AuthMode:        vov.AuthModeNone,
			Handler: func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, map[string]string{"status": greeting})
			},
		},
	}
}

// reportEndpoints serves /reports: behind the paywall.
//
// It also demands a role, which is what makes the refusal order visible: a user
// without the "member" role is refused 403 even if they have not paid, because
// paying would not get them in. Only someone who clears every other requirement
// and is merely unsubscribed sees 402.
func reportEndpoints() vov.Endpoints {
	return vov.Endpoints{
		GET: vov.Endpoint{
			RolesAnyOf: []string{"member"},
			MinTier:    2,
			MCPTool: &vov.MCPTool{
				Name: "get_reports",
				Description: "Aggregate figures across the whole list. Requires a " +
					"paid plan; on a free account this returns a message saying so.",
			},
			Handler: func(w http.ResponseWriter, r *http.Request) {
				d := deps.Get()
				d.Log.Info("report served", "to", currentUser(r).name)
				writeJSON(w, http.StatusOK, map[string]any{"tasks": d.Store.len()})
			},
		},
	}
}

// webhookEndpoints serves /webhook: authenticated by signature rather than by
// user, so it opts out of auth and takes the stack that checks the signature.
func webhookEndpoints() vov.Endpoints {
	return vov.Endpoints{
		POST: vov.Endpoint{
			MiddlewareStack: "webhook",
			AuthMode:        vov.AuthModeNone,
			Handler: func(w http.ResponseWriter, r *http.Request) {
				deps.Get().Log.Info("webhook received")
				writeJSON(w, http.StatusOK, map[string]string{"received": "ok"})
			},
		},
	}
}

// --- server wrappers --------------------------------------------------------

// cors makes the API usable from a browser on another origin.
//
// It answers preflights itself, which it must: a preflight is OPTIONS against a
// path registered for other methods, so http.ServeMux answers it 405 before any
// endpoint middleware runs. Nothing inside the mux can fix that.
func cors(origin string) vov.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The response varies by Origin, so a shared cache must not hand one
			// origin's response to another.
			w.Header().Add("Vary", "Origin")

			// Echo one configured origin, never "*": the browser rejects a
			// wildcard alongside credentials, and echoing whatever arrives would
			// let any site read authenticated responses.
			if o := r.Header.Get("Origin"); o != "" && o == origin {
				w.Header().Set("Access-Control-Allow-Origin", o)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}

			// Only a real preflight carries Access-Control-Request-Method, so an
			// endpoint that genuinely declares OPTIONS still gets its request.
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Signature")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// recoverPanic turns a panic into a 500 and a log line instead of a dropped
// connection. It belongs at the server layer: a panic on an unrouted path is
// exactly the one an operator wants a record of.
func recoverPanic(log *slog.Logger) vov.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic recovered", "method", r.Method, "path", r.URL.Path, "panic", rec)
					http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// --- endpoint middleware ----------------------------------------------------

// auditToolCall records every tool call, dispatched or not.
//
// The rejected ones are why this exists: they reach no endpoint, so no middleware
// runs for them, and an assistant looping on an argument it keeps getting wrong
// is visible here and nowhere else.
//
// Only the argument *names* are recorded. Their values are a founder's note body
// or an investor's name as often as not, and a sink that writes them verbatim is
// one careless log line from putting private text where it does not belong.
func auditToolCall(ctx context.Context, c vov.ToolCall) {
	// The call's values, without its cancellation — so a row still gets written
	// when an assistant hangs up mid-call, which is exactly when one matters.
	_ = ctx
	names := make([]string, 0, len(c.Arguments))
	for k := range c.Arguments {
		names = append(names, k)
	}
	sort.Strings(names)
	who := "unresolved"
	if u, ok := c.User.(*user); ok {
		who = u.name
	}
	deps.Get().Log.Info("tool_call",
		"tool", c.Tool, "outcome", string(c.Outcome), "status", c.Status,
		"by", who, "args", strings.Join(names, ","), "err", c.Err)
}

// requestID stamps every response with an id. Part of the default stack, so its
// presence in a response is evidence that stack ran.
func requestID(next http.Handler) http.Handler {
	var n atomicCounter
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An inbound trace id wins, so a caller can correlate its own request
		// with this server's log. This is also the whole of what an MCP call
		// needs to be enriched: a tool call arrives carrying the transport's
		// headers, so the middleware an app already writes reaches them, and
		// there is no second MCP-only hook to learn.
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = strconv.FormatUint(n.next(), 10)
		}
		w.Header().Set("X-Request-Id", id)
		deps.Get().Log.Info("request", "id", id, "path", r.URL.Path, "mode", string(vov.ModeFrom(r.Context())))
		next.ServeHTTP(w, r)
	})
}

// logging records method, path, and latency. Part of the default stack. Because
// the auth guard runs inside the middleware chain, rejected requests are logged
// here too.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start))
	})
}

// auditLog records who performed the request, and over which channel. It is the
// reason the after-auth phase exists: it needs the user, so it cannot run in the
// outer stack, which executes before anyone has been authenticated.
//
// Recording the mode is what this row is for: it is how a tool call and a
// browser call get told apart afterwards. An application may also shape a
// response by it — see vov.RequestMode for where that stops.
func auditLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		mode := vov.ModeFrom(r.Context())
		w.Header().Set("X-Audit-User", u.name)
		w.Header().Set("X-Audit-Mode", string(mode))
		deps.Get().Log.Info("audit", "method", r.Method, "path", r.URL.Path,
			"by", u.name, "mode", string(mode))
		next.ServeHTTP(w, r)
	})
}

// requireJSON rejects a body that is not declared as JSON. Added to one endpoint
// on top of the default stack.
func requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "" && ct != "application/json" {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "expected application/json"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// verifySignature stands in for a real webhook signature check. It replaces the
// default stack rather than adding to it.
func verifySignature(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Signature") == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing signature"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- plumbing ---------------------------------------------------------------

type atomicCounter struct {
	mu sync.Mutex
	n  uint64
}

func (c *atomicCounter) next() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
