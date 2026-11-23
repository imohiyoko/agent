package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/imohiyoko/agent/internal/audit"
	"github.com/imohiyoko/agent/internal/auth"
	"github.com/imohiyoko/agent/internal/spec"
)

type ctxKey struct{}

func principalOf(r *http.Request) string {
	p, _ := r.Context().Value(ctxKey{}).(string)
	return p
}

// Handler returns the API handler (ADR 0003).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/whoami", s.whoami)
	mux.HandleFunc("GET /v1/namespaces", s.listNamespaces)
	mux.HandleFunc("GET /v1/namespaces/{ns}", s.ns(auth.ActNamespaceRead, s.getNamespace))
	mux.HandleFunc("GET /v1/namespaces/{ns}/runs", s.ns(auth.ActRunRead, s.listRuns))
	mux.HandleFunc("POST /v1/namespaces/{ns}/runs", s.ns(auth.ActRunCreate, s.createRun))
	mux.HandleFunc("GET /v1/namespaces/{ns}/runs/{id}", s.ns(auth.ActRunRead, s.getRun))
	mux.HandleFunc("GET /v1/namespaces/{ns}/runs/{id}/logs", s.ns(auth.ActRunRead, s.runLogs))
	mux.HandleFunc("POST /v1/namespaces/{ns}/runs/{id}/stop", s.ns(auth.ActRunStop, s.stopRun))
	mux.HandleFunc("GET /v1/namespaces/{ns}/audit", s.ns(auth.ActAuditRead, s.readAudit))
	return s.authenticate(mux)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		principal := ""
		if ok {
			var err error
			principal, err = s.tokens.Authenticate(strings.TrimSpace(tok))
			if err != nil {
				apiError(w, http.StatusInternalServerError, "cannot read tokens")
				return
			}
		}
		if principal == "" {
			s.log.Write(audit.Event{Kind: "api", Action: r.Method + " " + r.URL.Path, Allowed: false, Reason: "unauthenticated"})
			w.Header().Set("WWW-Authenticate", `Bearer realm="nsctl"`)
			apiError(w, http.StatusUnauthorized, "a valid token is required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, principal)))
	})
}

type nsHandler func(w http.ResponseWriter, r *http.Request, ns *spec.Namespace)

// ns looks up the namespace and checks the action. A namespace the caller may
// not read looks the same as one that does not exist.
func (s *Server) ns(action string, h nsHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r)
		name := r.PathValue("ns")
		ns := s.nsSet().Namespaces[name]
		allowed := ns != nil && s.authz.Allowed(p, ns, action)
		s.log.Write(audit.Event{Namespace: nsName(ns), Kind: "api", Principal: p, Action: action, Target: r.URL.Path, Allowed: allowed})
		if !allowed {
			if ns != nil && s.authz.Allowed(p, ns, auth.ActNamespaceRead) {
				apiError(w, http.StatusForbidden, "your role in this namespace does not allow "+action)
				return
			}
			apiError(w, http.StatusNotFound, "not found")
			return
		}
		h(w, r, ns)
	}
}

func nsName(ns *spec.Namespace) string {
	if ns == nil {
		return ""
	}
	return ns.Name
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	roles := map[string]string{}
	set := s.nsSet()
	for _, name := range set.Names() {
		if role := s.authz.Role(p, set.Namespaces[name]); role != "" {
			roles[name] = role
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"principal": p, "roles": roles})
}

// namespaceView is what the API shows of a declaration: names and rules, never
// secret names' values (it has none) and never the members of other roles.
type namespaceView struct {
	Name        string       `json:"name"`
	Role        string       `json:"role"`
	Agents      []string     `json:"agents"`
	MCPServers  []mcpView    `json:"mcp_servers"`
	Network     spec.Network `json:"network"`
	Credentials []string     `json:"credential_routes"`
}

type mcpView struct {
	Name  string   `json:"name"`
	Tools []string `json:"tools"`
}

func (s *Server) view(p string, ns *spec.Namespace) namespaceView {
	v := namespaceView{Name: ns.Name, Role: s.authz.Role(p, ns), Network: ns.Network, Agents: []string{}, MCPServers: []mcpView{}, Credentials: []string{}}
	for _, a := range ns.Agents {
		v.Agents = append(v.Agents, a.Name)
	}
	for _, m := range ns.MCP.Servers {
		v.MCPServers = append(v.MCPServers, mcpView{Name: m.Name, Tools: m.Tools})
	}
	for _, c := range ns.Credentials {
		v.Credentials = append(v.Credentials, c.Name)
	}
	return v
}

func (s *Server) listNamespaces(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	out := []namespaceView{}
	set := s.nsSet()
	for _, name := range set.Names() {
		ns := set.Namespaces[name]
		if s.authz.Allowed(p, ns, auth.ActNamespaceRead) {
			out = append(out, s.view(p, ns))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getNamespace(w http.ResponseWriter, r *http.Request, ns *spec.Namespace) {
	writeJSON(w, http.StatusOK, s.view(principalOf(r), ns))
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request, ns *spec.Namespace) {
	writeJSON(w, http.StatusOK, s.runs.list(ns.Name))
}

type createRunRequest struct {
	Agent  string `json:"agent"`
	Prompt string `json:"prompt"`
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request, ns *spec.Namespace) {
	var req createRunRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "body must be {\"agent\": ..., \"prompt\": ...}")
		return
	}
	if ns.Agent(req.Agent) == nil {
		apiError(w, http.StatusBadRequest, "no agent "+req.Agent+" in this namespace")
		return
	}
	if strings.ContainsRune(req.Prompt, 0) {
		apiError(w, http.StatusBadRequest, "prompt must not contain NUL")
		return
	}
	run, err := s.runs.start(ns, req.Agent, req.Prompt, principalOf(r))
	switch {
	case errors.Is(err, ErrLimit):
		apiError(w, http.StatusTooManyRequests, err.Error())
		return
	case err != nil:
		s.log.Write(audit.Event{Namespace: ns.Name, Kind: "run", Principal: principalOf(r), Action: "start", Target: req.Agent, Allowed: false, Reason: err.Error()})
		apiError(w, http.StatusInternalServerError, "cannot start the run: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.runs.get(ns.Name, run.ID))
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request, ns *spec.Namespace) {
	run := s.runs.get(ns.Name, r.PathValue("id"))
	if run == nil {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) runLogs(w http.ResponseWriter, r *http.Request, ns *spec.Namespace) {
	id := r.PathValue("id")
	p := s.runs.logPath(ns.Name, id)
	if p == "" {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		apiError(w, http.StatusNotFound, "no output yet")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("follow") != "1" {
		_, _ = io.Copy(w, f)
		return
	}
	// follow=1: stream the output until the run ends or the client leaves.
	fl, _ := w.(http.Flusher)
	done := s.runs.doneCh(ns.Name, id)
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := io.Copy(w, f); err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-done:
			_, _ = io.Copy(w, f)
			return
		case <-tick.C:
		}
	}
}

func (s *Server) stopRun(w http.ResponseWriter, r *http.Request, ns *spec.Namespace) {
	if err := s.runs.stop(ns.Name, r.PathValue("id")); err != nil {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, s.runs.get(ns.Name, r.PathValue("id")))
}

func (s *Server) readAudit(w http.ResponseWriter, r *http.Request, ns *spec.Namespace) {
	ev, err := s.log.Read(ns.Name, 500)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "cannot read the audit log")
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) serveAPI(ctx context.Context, l net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Tokens exposes the token store (for the CLI).
func (s *Server) Tokens() *auth.Store { return s.tokens }
