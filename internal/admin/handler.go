package admin

import (
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/KumaTea/homir/internal/config"
	"github.com/KumaTea/homir/internal/store"
)

const sessionCookie = "homir_admin"

type Handler struct {
	auth       *Authenticator
	stats      func() (store.Stats, error)
	upstreams  map[string]config.Upstream
	configPath string
}

type dashboardData struct {
	Stats     store.Stats
	Upstreams []upstreamData
}

type upstreamData struct {
	Name     string
	Kind     string
	Primary  string
	Backups  int
	Security bool
}

func NewHandler(auth *Authenticator, stats func() (store.Stats, error), upstreams map[string]config.Upstream, configPath string) *Handler {
	return &Handler{auth: auth, stats: stats, upstreams: upstreams, configPath: configPath}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		http.Error(w, "admin UI is disabled; set HOMIR_ADMIN_PASSWORD or admin.password_hash", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path == "/admin/login" {
		h.login(w, r)
		return
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || !h.auth.Valid(cookie.Value) {
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return
	}
	if r.URL.Path == "/admin/config" {
		h.configEditor(w, r, cookie.Value)
		return
	}
	if r.URL.Path != "/admin/" {
		http.NotFound(w, r)
		return
	}
	stats, err := h.stats()
	if err != nil {
		http.Error(w, "load cache statistics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	names := make([]string, 0, len(h.upstreams))
	for name := range h.upstreams {
		names = append(names, name)
	}
	sort.Strings(names)
	upstreams := make([]upstreamData, 0, len(names))
	for _, name := range names {
		upstream := h.upstreams[name]
		upstreams = append(upstreams, upstreamData{Name: name, Kind: upstream.Kind, Primary: upstream.Primary, Backups: len(upstream.Backups), Security: upstream.Security})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardPage.Execute(w, dashboardData{Stats: stats, Upstreams: upstreams}); err != nil {
		return
	}
}

type configData struct {
	Content string
	CSRF    string
	Error   string
	Saved   bool
}

func (h *Handler) configEditor(w http.ResponseWriter, r *http.Request, token string) {
	if h.configPath == "" {
		http.Error(w, "configuration editing is unavailable", http.StatusNotFound)
		return
	}
	content, err := os.ReadFile(h.configPath)
	if err != nil {
		http.Error(w, "read configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}
	csrf, ok := h.auth.CSRF(token)
	if !ok {
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return
	}
	data := configData{Content: string(content), CSRF: csrf}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			data.Error = "Invalid form."
		} else if r.Form.Get("csrf") != csrf {
			data.Error = "Invalid CSRF token."
		} else {
			proposed := []byte(r.Form.Get("config"))
			if _, err := config.Parse(proposed); err != nil {
				data.Error = err.Error()
				data.Content = string(proposed)
			} else if err := writeAtomically(h.configPath, proposed); err != nil {
				data.Error = "Save configuration: " + err.Error()
				data.Content = string(proposed)
			} else {
				data.Content = string(proposed)
				data.Saved = true
			}
		}
	} else if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = configPage.Execute(w, data)
}

func writeAtomically(filename string, content []byte) error {
	info, err := os.Stat(filename)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(filename), ".homir-config-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, filename)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = loginPage.Execute(w, struct{ Error string }{})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	token, ok := h.auth.Login(r.Form.Get("username"), r.Form.Get("password"))
	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = loginPage.Execute(w, struct{ Error string }{Error: "Invalid username or password."})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/admin", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 12 * 60 * 60})
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

var loginPage = template.Must(template.New("login").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Homir login</title><style>body{font:16px system-ui,sans-serif;max-width:24rem;margin:12vh auto;padding:0 1rem}input{box-sizing:border-box;width:100%;padding:.55rem;margin:.25rem 0 1rem}button{padding:.55rem .9rem}.error{color:#a00}</style><h1>Homir</h1>{{with .Error}}<p class="error">{{.}}</p>{{end}}<form method="post"><label>Username<input name="username" autocomplete="username" required></label><label>Password<input name="password" type="password" autocomplete="current-password" required></label><button type="submit">Sign in</button></form></html>`))

var dashboardPage = template.Must(template.New("dashboard").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Homir admin</title><style>body{font:16px system-ui,sans-serif;max-width:60rem;margin:2rem auto;padding:0 1rem}table{border-collapse:collapse;width:100%}th,td{text-align:left;padding:.55rem;border-bottom:1px solid #ddd}code{word-break:break-all}.stats{display:flex;gap:2rem;flex-wrap:wrap}.stats p{margin:0}.muted{color:#666}</style><h1>Homir</h1><p><a href="/admin/config">Configuration</a></p><p class="muted">Read-only cache and upstream status.</p><div class="stats"><p><strong>{{.Stats.Entries}}</strong><br>cache entries</p><p><strong>{{.Stats.TrackedEntries}}</strong><br>tracked artifacts</p><p><strong>{{.Stats.SizeBytes}}</strong><br>bytes cached</p></div><h2>Upstreams</h2><table><thead><tr><th>Name</th><th>Type</th><th>Primary</th><th>Backups</th><th>Security</th></tr></thead><tbody>{{range .Upstreams}}<tr><td>{{.Name}}</td><td>{{.Kind}}</td><td><code>{{.Primary}}</code></td><td>{{.Backups}}</td><td>{{if .Security}}yes{{else}}no{{end}}</td></tr>{{end}}</tbody></table></html>`))

var configPage = template.Must(template.New("config").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Homir configuration</title><style>body{font:16px system-ui,sans-serif;max-width:60rem;margin:2rem auto;padding:0 1rem}textarea{box-sizing:border-box;width:100%;min-height:32rem;font:14px ui-monospace,monospace}.error{color:#a00}.ok{color:#070}</style><h1>Configuration</h1><p><a href="/admin/">Dashboard</a></p>{{with .Error}}<p class="error">{{.}}</p>{{end}}{{if .Saved}}<p class="ok">Configuration saved. Restart Homir to apply it safely.</p>{{end}}<p>YAML is validated before an atomic save. A restart is required to apply changes.</p><form method="post"><input type="hidden" name="csrf" value="{{.CSRF}}"><textarea name="config" spellcheck="false" required>{{.Content}}</textarea><p><button type="submit">Validate and save</button></p></form></html>`))
