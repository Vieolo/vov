package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	smokePort     = 8080
	smokeBase     = "http://127.0.0.1:8080"
	shutdownGrace = 20 * time.Second // must exceed the example's ShutdownTimeout
)

// exampleEnv is what the example reads at start-up. TASKS_TOKEN is declared
// required, which the first check below relies on.
var exampleEnv = []string{
	"TASKS_TOKEN=t-ramtin",
	"TASKS_GREETING=env-ok",
}

// The demo principals, all suffixes of the configured token.
const (
	tokMember    = "t-ramtin"           // member + tasks.write
	tokAdmin     = "t-ramtin-admin"     // also role admin
	tokOwner     = "t-ramtin-owner"     // role owner + tasks.write
	tokPro       = "t-ramtin-pro"       // member, paid tier 2
	tokHalfAdmin = "t-ramtin-halfadmin" // role admin, no permission
	tokReader    = "t-ramtin-reader"    // member only
	tokRevoked   = "t-ramtin-revoked"   // a dead credential
	tokBoom      = "t-boom"             // makes the authenticator fail
)

// smoke builds the example, runs it, and exercises it over real HTTP.
func smoke(root string) int {
	var r report
	example := filepath.Join(root, "examples", "tasks")

	if listening(smokePort) {
		fmt.Printf("port %d is already in use\n", smokePort)
		r.fail("port %d is in use", smokePort)
		return r.done("SMOKE")
	}

	tmp, err := os.MkdirTemp("", "vov-smoke-")
	if err != nil {
		r.fail("temp dir: %v", err)
		return r.done("SMOKE")
	}
	defer os.RemoveAll(tmp)

	bin := filepath.Join(tmp, "tasks")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir, build.Stdout, build.Stderr = example, os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		r.fail("the example did not build: %v", err)
		return r.done("SMOKE")
	}

	// A required environment variable that is missing must stop the server from
	// starting, and the error must name the variable without echoing a value.
	bare := exec.Command(bin)
	bare.Env = append(os.Environ(), "TASKS_GREETING=env-ok")
	bareOut, _ := bare.CombinedOutput()
	named := strings.Contains(string(bareOut), "TASKS_TOKEN")
	if !r.okf(bare.ProcessState.ExitCode() != 0 && named,
		"missing required env aborts boot: exit=%d names_var=%v",
		bare.ProcessState.ExitCode(), named) {
		r.fail("a missing required env var did not abort start-up")
	}

	srv := exec.Command(bin)
	srv.Env = append(os.Environ(), exampleEnv...)
	var logs bytes.Buffer
	srv.Stdout, srv.Stderr = &logs, &logs
	if err := srv.Start(); err != nil {
		r.fail("starting the example: %v", err)
		return r.done("SMOKE")
	}
	defer func() {
		if srv.ProcessState == nil {
			_ = srv.Process.Kill()
			_, _ = srv.Process.Wait()
		}
	}()

	if !waitForBind(srv) {
		fmt.Println(logs.String())
		r.fail("the server did not start")
		return r.done("SMOKE")
	}

	checkEndpoints(&r)
	checkMCP(&r)
	checkServerSeam(&r)

	// Graceful shutdown: SIGTERM should drain, run the hook, and exit 0.
	_ = srv.Process.Signal(syscall.SIGTERM)
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	select {
	case <-exited:
	case <-time.After(shutdownGrace):
		_ = srv.Process.Kill()
		r.fail("the server did not shut down within %s", shutdownGrace)
	}

	log := logs.String()
	// The dependency holder reached a handler: createTask used both the logger
	// and the S3 client it was handed.
	if !r.okf(strings.Contains(log, "archived") && strings.Contains(log, "s3://"),
		"handler used the shared globals") {
		r.fail("a handler did not reach its globals")
	}
	// The audit trail tells the two channels apart.
	if !r.okf(strings.Contains(log, "path=/reports") && strings.Contains(log, "mode=api"),
		"API call audited as api") {
		r.fail("an API request was not audited with mode=api")
	}
	if !r.okf(strings.Contains(log, "path=/reports") && strings.Contains(log, "mode=mcp"),
		"tool call audited as mcp") {
		r.fail("an MCP request was not audited with mode=mcp")
	}
	code := srv.ProcessState.ExitCode()
	hookRan := strings.Contains(log, "tasks_in_memory")
	if !r.okf(code == 0 && hookRan, "SIGTERM -> exit %d, hook_ran=%v", code, hookRan) {
		r.fail("graceful shutdown: exit %d, hook ran %v", code, hookRan)
	}

	return r.done("SMOKE")
}

// checkEndpoints covers auth, the middleware phases, routing, and the paywall.
func checkEndpoints(r *report) {
	// --- auth: required by default, escaped explicitly -----------------------
	res := do("GET", "/tasks")
	r.check("GET  /tasks (no credentials)", res.Status, 401)
	// The guard runs inside the middleware chain, so a 401 is still stamped.
	r.check("     401 still has request id", res.has("X-Request-Id"), true)

	// A revoked credential is cleared as it is refused, so the browser stops
	// re-sending a dead cookie for the rest of its 30-day life.
	res = do("GET", "/tasks", auth(tokRevoked))
	r.check("GET  /tasks (revoked credential)", res.Status, 401)
	r.check("     401 clears the cookie", strings.Contains(res.Header.Get("Set-Cookie"), "Max-Age=0"), true)
	res = do("GET", "/tasks", auth(tokMember))
	r.check("     valid request sets no cookie", res.has("Set-Cookie"), false)

	// A failing authenticator is a broken dependency, not a bad password.
	r.check("GET  /tasks (authenticator fails)", do("GET", "/tasks", auth(tokBoom)).Status, 500)

	res = do("GET", "/tasks", auth(tokMember))
	r.check("GET  /tasks (authenticated)", res.Status, 200)
	r.check("     /tasks inherits defaults", res.has("X-Request-Id"), true)

	// --- the after-auth phase ------------------------------------------------
	r.check("     after-auth sees the user", res.Header.Get("X-Audit-User"), "ramtin")
	r.check("     401 skips after-auth", do("GET", "/tasks").has("X-Audit-User"), false)

	// --- routing and handler behaviour --------------------------------------
	res = do("POST", "/tasks", auth(tokMember), jsonBody(`{"title":"write the tests"}`))
	r.check("POST /tasks", res.Status, 201)
	r.check("     /tasks POST keeps defaults", res.has("X-Request-Id"), true)
	r.check("     task owner from context", res.field("owner"), "ramtin")

	r.check("POST /tasks (no title)", do("POST", "/tasks", auth(tokMember), jsonBody(`{}`)).Status, 400)
	// The extra middleware layer this endpoint added on top of the defaults.
	r.check("POST /tasks (not JSON)",
		do("POST", "/tasks", auth(tokMember), body("nope", "text/plain")).Status, 415)

	r.check("GET  /tasks/1", do("GET", "/tasks/1", auth(tokMember)).Status, 200)
	r.check("GET  /tasks/99", do("GET", "/tasks/99", auth(tokMember)).Status, 404)

	// --- roles and permissions ----------------------------------------------
	r.check("POST /tasks (no permission)",
		do("POST", "/tasks", auth(tokReader), jsonBody(`{"title":"nope"}`)).Status, 403)
	// DELETE needs a role AND a permission. Has the permission, lacks the role:
	r.check("DEL  /tasks/1 (no role)", do("DELETE", "/tasks/1", auth(tokMember)).Status, 403)
	// Has the role, lacks the permission — the two are AND:
	r.check("DEL  /tasks/1 (role but no permission)", do("DELETE", "/tasks/1", auth(tokHalfAdmin)).Status, 403)
	// ...but GET on the same URL needs neither.
	r.check("GET  /tasks/1 (same URL, no role needed)", do("GET", "/tasks/1", auth(tokMember)).Status, 200)

	// --- the paywall, and the order refusals are decided in ------------------
	// No credentials is 401, never 402: no price is quoted to a stranger.
	r.check("GET  /reports (no credentials)", do("GET", "/reports").Status, 401)
	// Lacks the role, so paying would not help: 403.
	r.check("GET  /reports (wrong role, unpaid)", do("GET", "/reports", auth(tokOwner)).Status, 403)
	// Clears everything else and is merely unsubscribed: 402.
	r.check("GET  /reports (right role, unpaid)", do("GET", "/reports", auth(tokReader)).Status, 402)
	r.check("GET  /reports (paid tier 2)", do("GET", "/reports", auth(tokPro)).Status, 200)

	// In-process dispatch is exercised by the MCP section below, which is the
	// only channel that reaches App.Invoke. The mode a dispatch carries is a unit
	// test's job — see invoke_test.go — since nothing about it needs a subprocess.
	r.check("GET  /reports audited as api", do("GET", "/reports", auth(tokPro)).Header.Get("X-Audit-Mode"), "api")

	// --- one URL, several methods -------------------------------------------
	r.check("DEL  /tasks/1 (admin)", do("DELETE", "/tasks/1", auth(tokAdmin)).Status, 204)
	// "owner" is the second of the any-of roles: also allowed.
	r.check("POST /tasks (owner)",
		do("POST", "/tasks", auth(tokOwner), jsonBody(`{"title":"by owner"}`)).Status, 201)
	r.check("DEL  /tasks/2 (owner, any-of role)", do("DELETE", "/tasks/2", auth(tokOwner)).Status, 204)
	r.check("GET  /tasks/1 (after delete)", do("GET", "/tasks/1", auth(tokMember)).Status, 404)
	// Unauthenticated is 401, not 403: vov does not know who you are.
	r.check("DEL  /tasks/2 (no credentials)", do("DELETE", "/tasks/2").Status, 401)

	// A method the Route does not declare: net/http derives 405 and Allow from
	// the methods it does. HEAD comes along with GET.
	res = do("PUT", "/tasks/1", auth(tokMember), jsonBody(`{}`))
	r.check("PUT  /tasks/1 (undeclared)", res.Status, 405)
	allow := strings.Split(strings.ReplaceAll(res.Header.Get("Allow"), " ", ""), ",")
	sort.Strings(allow)
	r.check("     405 lists the declared methods", allow, []string{"DELETE", "GET", "HEAD"})

	// --- opted-out routes ----------------------------------------------------
	res = do("GET", "/healthz")
	r.check("GET  /healthz (no credentials)", res.Status, 200)
	// TASKS_GREETING overrode the built-in default and reached a handler.
	r.check("     env value reached the handler", res.field("status"), "env-ok")
	r.check("     /healthz is bare", res.has("X-Request-Id"), false)
	r.check("     /healthz skips after-auth", res.has("X-Audit-User"), false)

	// The "webhook" stack: its own signature check in Pre, and it shares the
	// request id and logging the default stack also uses.
	res = do("POST", "/webhook", jsonBody(`{}`))
	r.check("POST /webhook (no signature)", res.Status, 401)
	r.check("     webhook stack runs its Pre", res.has("X-Request-Id"), true)
	// NoAuth, so the stack's Post half never runs.
	r.check("     webhook stack skips Post", res.has("X-Audit-User"), false)
	r.check("POST /webhook (signed, no credentials)",
		do("POST", "/webhook", jsonBody(`{}`), header("X-Signature", "sig")).Status, 200)

	res = do("GET", "/version")
	r.check("GET  /version (escape hatch)", res.Status, 200)
	r.check("     /version has no middleware", res.has("X-Request-Id"), false)

	r.check("POST /healthz (wrong method)", do("POST", "/healthz").Status, 405)
}

// checkServerSeam covers the requests the mux answers on its own.
func checkServerSeam(r *report) {
	const origin = "https://app.example.com"
	allowed := func(res *resp) string { return res.Header.Get("Access-Control-Allow-Origin") }

	// A preflight is OPTIONS against a path registered for other methods.
	// Without a seam outside the mux this is a 405 and the browser blocks the
	// real request.
	res := do("OPTIONS", "/tasks/9",
		header("Origin", origin), header("Access-Control-Request-Method", "DELETE"))
	r.check("OPT  /tasks/9 (CORS preflight)", res.Status, 204)
	r.check("     preflight is allowed", allowed(res), origin)
	r.check("     preflight allows credentials", res.Header.Get("Access-Control-Allow-Credentials"), "true")

	// 404 and 405 must carry CORS headers, or the browser reports a CORS error
	// instead of the wrong-path bug it actually is.
	res = do("GET", "/nope", header("Origin", origin))
	r.check("GET  /nope (unrouted)", res.Status, 404)
	r.check("     404 carries CORS", allowed(res), origin)

	res = do("PUT", "/tasks/9", auth(tokMember), jsonBody(`{}`), header("Origin", origin))
	r.check("     405 carries CORS", allowed(res), origin)

	// An unknown origin gets no allow header, but still gets Vary.
	res = do("GET", "/nope", header("Origin", "https://evil.example"))
	r.check("     unknown origin refused", res.has("Access-Control-Allow-Origin"), false)
	r.check("     Vary: Origin set for caches", strings.Contains(res.Header.Get("Vary"), "Origin"), true)

	// The seam covers escape-hatch routes: /boom is on the raw mux and panics.
	res = do("GET", "/boom", header("Origin", origin))
	r.check("GET  /boom (panic on a mux route)", res.Status, 500)
	r.check("     recovered 500 carries CORS", allowed(res), origin)
}

// checkMCP drives the tool server over real JSON-RPC.
func checkMCP(r *report) {
	init := rpc("initialize", map[string]any{
		"protocolVersion": "2026-07-28",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "smoke", "version": "1"},
	}, tokMember)
	r.check("MCP  initialize", dig(init, "result", "serverInfo", "name"), "tasks")

	listed := rpc("tools/list", map[string]any{}, tokMember)
	raw, _ := dig(listed, "result", "tools").([]any)
	tools := map[string]map[string]any{}
	var names []string
	for _, t := range raw {
		m, _ := t.(map[string]any)
		name, _ := m["name"].(string)
		tools[name] = m
		names = append(names, name)
	}
	sort.Strings(names)
	r.check("MCP  tools/list", names,
		[]string{"create_task", "delete_task", "get_reports", "get_task", "list_tasks"})

	// The schema is derived from the endpoint's declared Body, not hand-written.
	props, _ := dig(tools["create_task"], "inputSchema", "properties").(map[string]any)
	var fields []string
	for k := range props {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	r.check("     create_task schema from Body", fields, []string{"notes", "tags", "title"})
	r.check("     required derives from the vov tag",
		dig(tools["create_task"], "inputSchema", "required"), []any{"title"})
	// A path parameter becomes a required argument.
	r.check("     get_task takes its path param",
		dig(tools["get_task"], "inputSchema", "required"), []any{"id"})
	// ReadOnly is derived from the HTTP method.
	r.check("     GET tool is read-only", dig(tools["list_tasks"], "annotations", "readOnlyHint"), true)
	r.check("     POST tool is not", dig(tools["create_task"], "annotations", "readOnlyHint"), false)

	isErr, text := callTool("create_task", map[string]any{"title": "from an assistant"}, tokMember)
	r.check("MCP  create_task", isErr, false)
	var created map[string]any
	_ = json.Unmarshal([]byte(text), &created)
	r.check("     handler ran, owner from the token", created["owner"], "ramtin")

	isErr, _ = callTool("list_tasks", map[string]any{}, tokMember)
	r.check("MCP  list_tasks", isErr, false)

	// The tool meets the same policy the endpoint declares.
	isErr, text = callTool("delete_task", map[string]any{"id": "1"}, tokMember)
	r.check("MCP  delete_task (no role)", isErr && strings.Contains(text, "may not perform"), true)

	// And the paywall answers with something a model can act on.
	isErr, text = callTool("get_reports", map[string]any{}, tokReader)
	r.check("MCP  get_reports (unpaid)", isErr && strings.Contains(text, "paid subscription"), true)
	isErr, _ = callTool("get_reports", map[string]any{}, tokPro)
	r.check("MCP  get_reports (paid)", isErr, false)

	// A missing required argument never reaches the endpoint.
	isErr, text = callTool("get_task", map[string]any{}, tokMember)
	r.check("MCP  get_task (no id)", isErr && strings.Contains(text, "missing required argument"), true)
}

// --- HTTP plumbing ----------------------------------------------------------

type resp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r *resp) has(header string) bool { return r.Header.Get(header) != "" }

// field decodes the body as a JSON object and returns one member.
func (r *resp) field(name string) any {
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		return fmt.Sprintf("<not JSON: %s>", strings.TrimSpace(string(r.Body)))
	}
	return m[name]
}

type reqOpt func(*http.Request)

func auth(token string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}
func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func body(content, contentType string) reqOpt {
	return func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(content))
		r.ContentLength = int64(len(content))
		r.Header.Set("Content-Type", contentType)
	}
}
func jsonBody(content string) reqOpt { return body(content, "application/json") }

// do performs a request against the running example. A transport failure is
// reported as a synthetic status so that one broken call cannot abort the run.
func do(method, path string, opts ...reqOpt) *resp {
	req, err := http.NewRequest(method, smokeBase+path, nil)
	if err != nil {
		return &resp{Status: -1, Header: http.Header{}}
	}
	for _, o := range opts {
		o(req)
	}
	res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return &resp{Status: -1, Header: http.Header{}, Body: []byte(err.Error())}
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return &resp{Status: res.StatusCode, Header: res.Header, Body: b}
}

// rpc sends one JSON-RPC request to the tool server.
func rpc(method string, params any, token string) map[string]any {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	res := do("POST", "/mcp", auth(token), jsonBody(string(payload)),
		header("Accept", "application/json, text/event-stream"))
	var out map[string]any
	_ = json.Unmarshal(res.Body, &out)
	return out
}

// callTool invokes one tool and returns whether it reported an error, with its
// text content.
func callTool(name string, args map[string]any, token string) (bool, string) {
	out := rpc("tools/call", map[string]any{"name": name, "arguments": args}, token)
	isErr, _ := dig(out, "result", "isError").(bool)
	content, _ := dig(out, "result", "content").([]any)
	var text strings.Builder
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			s, _ := m["text"].(string)
			text.WriteString(s)
		}
	}
	return isErr, strings.TrimSpace(text.String())
}

// dig walks nested JSON maps, returning nil when any step is missing.
func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// --- process plumbing -------------------------------------------------------

// listening reports whether something already holds the port.
//
// It connects rather than binding: a bind without SO_REUSEADDR fails while
// sockets from a previous run sit in TIME_WAIT, which would call the port busy
// even though the server — which does set SO_REUSEADDR — could take it.
func listening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// waitForBind blocks until the example answers, or gives up.
func waitForBind(srv *exec.Cmd) bool {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if srv.ProcessState != nil {
			return false // it exited before binding
		}
		if do("GET", "/healthz").Status == 200 {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
