package mcpgw

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/imohiyoko/agent/internal/audit"
	"github.com/imohiyoko/agent/internal/spec"
)

// TestMain lets the test binary act as a fake stdio MCP server.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_MCP") == "1" {
		fakeServer()
		return
	}
	os.Exit(m.Run())
}

func fakeServer() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		id, ok := m["id"]
		if !ok {
			continue
		}
		var method string
		_ = json.Unmarshal(m["method"], &method)
		var result string
		switch method {
		case "initialize":
			result = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"0"}}`
		case "tools/list":
			result = `{"tools":[{"name":"read_file","inputSchema":{}},{"name":"delete_file","inputSchema":{}}]}`
		case "tools/call":
			result = `{"content":[{"type":"text","text":"called"}]}`
		default:
			result = `{}`
		}
		fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", id, result)
	}
}

func setup(t *testing.T) *httptest.Server {
	ns := &spec.Namespace{Name: "t", MCP: spec.MCP{Servers: []spec.MCPServer{{Name: "fake", Command: []string{"x"}, Tools: []string{"read_file"}}}}}
	g := New(ns, func(s *spec.MCPServer) (*exec.Cmd, error) {
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "FAKE_MCP=1")
		return c, nil
	}, audit.New(t.TempDir()))
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, session, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(b), &m)
	return resp, m
}

func TestGatewayFiltersTools(t *testing.T) {
	srv := setup(t)
	url := srv.URL + "/mcp/fake"
	resp, m := post(t, url, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" || m["result"] == nil {
		t.Fatalf("initialize: %v %v", resp.Status, m)
	}
	if r, _ := post(t, url, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); r.StatusCode != 202 {
		t.Fatalf("notification: %s", r.Status)
	}

	_, m = post(t, url, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := m["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "read_file" {
		t.Fatalf("tools/list not filtered: %v", tools)
	}

	_, m = post(t, url, sid, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_file"}}`)
	if m["result"] == nil {
		t.Fatalf("allowed call failed: %v", m)
	}
	_, m = post(t, url, sid, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"delete_file"}}`)
	if m["error"] == nil {
		t.Fatalf("denied tool was called: %v", m)
	}
	// Keys that differ only in case must not let a checked method or tool
	// name differ from the one the server reads.
	for _, b := range []string{
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","Method":"ping","params":{"name":"delete_file"}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"delete_file","Name":"read_file"}}`,
	} {
		if _, m = post(t, url, sid, b); m["error"] == nil {
			t.Fatalf("ambiguous message was accepted: %s -> %v", b, m)
		}
	}
	_, m = post(t, url, sid, `{"jsonrpc":"2.0","id":5,"method":"resources/list"}`)
	if m["error"] == nil {
		t.Fatalf("resources must be denied by default: %v", m)
	}
}

func TestGatewayRejectsUnknownServerAndSession(t *testing.T) {
	srv := setup(t)
	if r, _ := post(t, srv.URL+"/mcp/other", "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`); r.StatusCode != 404 {
		t.Errorf("unknown server: %s", r.Status)
	}
	if r, _ := post(t, srv.URL+"/mcp/fake", "made-up", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); r.StatusCode != 404 {
		t.Errorf("unknown session: %s", r.Status)
	}
	if r, _ := post(t, srv.URL+"/mcp/fake", "", `[{"jsonrpc":"2.0","id":1,"method":"initialize"}]`); r.StatusCode != 200 {
		t.Errorf("batch: %s", r.Status)
	}
}

func TestConfigJSON(t *testing.T) {
	ns := &spec.Namespace{MCP: spec.MCP{Servers: []spec.MCPServer{{Name: "files"}}}}
	b, _ := ConfigJSON(ns, "127.0.0.1:3129")
	if !strings.Contains(string(b), `"url": "http://127.0.0.1:3129/mcp/files"`) {
		t.Fatal(string(b))
	}
}

func TestDecodeStrictAndSameID(t *testing.T) {
	for _, b := range []string{`{"a":1,"a":2}`, `{"x":{"k":1,"K":2}}`, `[{"a":1,"A":1}]`, `{"a":1} {}`} {
		var m message
		if decodeStrict([]byte(b), &m) == nil {
			t.Errorf("%s must be rejected", b)
		}
	}
	var m message
	if err := decodeStrict([]byte(`{"jsonrpc":"2.0","id":1,"method":"x","params":{"a":[{"b":1},{"b":2}]}}`), &m); err != nil {
		t.Errorf("valid message rejected: %v", err)
	}
	if !sameID(json.RawMessage(`"a"`), json.RawMessage(`"\u0061"`)) || !sameID(json.RawMessage(`1`), json.RawMessage(`1.0`)) {
		t.Error("equal ids must match")
	}
	if sameID(json.RawMessage(`1`), json.RawMessage(`"1"`)) {
		t.Error("number and string ids must differ")
	}
}

func TestRelaySSEMultiLineAndCap(t *testing.T) {
	g := New(&spec.Namespace{Name: "t"}, nil, audit.New(t.TempDir()))
	srv := &spec.MCPServer{Name: "r", Tools: []string{"read_file"}}
	in := "id: 1\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1,\"result\":{}}\n\n"
	rec := httptest.NewRecorder()
	g.relaySSE(rec, strings.NewReader(in), srv, json.RawMessage(`1`), false)
	want := "id: 1\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1,\"result\":{}}\n\n"
	if rec.Body.String() != want {
		t.Fatalf("got %q, want %q", rec.Body.String(), want)
	}

	big := strings.Repeat("data: "+strings.Repeat("x", 1<<20)+"\n", 20) + "\n"
	rec = httptest.NewRecorder()
	g.relaySSE(rec, strings.NewReader(big), srv, nil, false)
	if rec.Body.Len() != 0 {
		t.Fatalf("an event over the cap must not be relayed (%d bytes out)", rec.Body.Len())
	}
}
