// Package views renders the HTML pages (the "view" part of MVC). Templates
// and static assets are embedded in the binary.
package views

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"itmon.nl/locutus/internal/i18n"
	"itmon.nl/locutus/internal/models"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Flash is a one-time message shown at the top of the next page.
type Flash struct {
	Kind    string // "success" or "error"
	Message string
}

// Page is the data passed to every template.
type Page struct {
	Lang string
	// Languages can be chosen in the menu; LangChoice is the language chosen
	// by the user, "" when the language of the browser is used.
	Languages  []i18n.Language
	LangChoice string
	Title      string
	User       *models.User
	CSRF       string
	Flash      *Flash
	Errors     []string
	Data       any
	Version    string
	Build      string
	Path       string
}

// Renderer executes the page templates.
type Renderer struct {
	pages   map[string]*template.Template
	tr      *i18n.Translator
	version string
	build   string
}

// New parses all templates. Every page template is combined with layout.html
// and the shared partials (files starting with an underscore).
func New(tr *i18n.Translator, version, build string) (*Renderer, error) {
	r := &Renderer{pages: map[string]*template.Template{}, tr: tr, version: version, build: build}
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		name := strings.TrimSuffix(path.Base(f), ".html")
		if name == "layout" || strings.HasPrefix(name, "_") {
			continue
		}
		t, err := template.New("layout.html").Funcs(r.funcs("")).ParseFS(templateFS, "templates/layout.html", "templates/_*.html", f)
		if err != nil {
			return nil, err
		}
		r.pages[name] = t
	}
	return r, nil
}

// FormatBytes formats a size with binary units.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (r *Renderer) funcs(lang string) template.FuncMap {
	return template.FuncMap{
		"t": func(key string, args ...any) string { return r.tr.T(lang, key, args...) },
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Local().Format("02-01-2006 15:04")
		},
		"datesec": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Local().Format("02-01-2006 15:04:05")
		},
		"weekday": func(t time.Time) string { return r.tr.T(lang, fmt.Sprintf("weekday.%d", t.Weekday())) },
		"hasID": func(ids []int64, id int64) bool {
			for _, x := range ids {
				if x == id {
					return true
				}
			}
			return false
		},
		"bytes":     FormatBytes,
		"join":      strings.Join,
		"hasPrefix": strings.HasPrefix,
		"add":       func(a, b int) int { return a + b },
		"list":      func(items ...string) []string { return items },
		// dict builds a map from key/value pairs, to pass several values to a sub-template.
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				k, _ := kv[i].(string)
				m[k] = kv[i+1]
			}
			return m
		},
	}
}

// Render writes the named page with the given HTTP status.
func (r *Renderer) Render(w http.ResponseWriter, status int, name string, p *Page) {
	base, ok := r.pages[name]
	if !ok {
		http.Error(w, "unknown template "+name, http.StatusInternalServerError)
		return
	}
	p.Version, p.Build = r.version, r.build
	t, err := base.Clone()
	if err == nil {
		t.Funcs(r.funcs(p.Lang))
		var buf bytes.Buffer
		if err = t.Execute(&buf, p); err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(status)
			buf.WriteTo(w)
			return
		}
	}
	log.Printf("render %s: %v", name, err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// Static serves the embedded CSS and JavaScript files.
func Static() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}
