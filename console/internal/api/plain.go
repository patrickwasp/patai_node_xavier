package api

import (
	"fmt"
	"html"
	"net"
	"net/http"

	"patai/console/internal/workloads"
)

// Plain is port 80. It explains and links, and there's nothing else here: no API and no login, because
// plain HTTP on a LAN can't protect one. With an app in front, it links to that first, and to the console's port.
func (s *Server) Plain() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !s.hostAllowed(host) {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		link := "https://" + html.EscapeString(host) + "/"
		admin := fmt.Sprintf("https://%s:%d/", html.EscapeString(host), AdminPort)
		where := fmt.Sprintf(`<p>This unit's console is at <a href="%[1]s">%[1]s</a>.</p>`, link)
		if name, ok := s.frontPage(r.Context()); ok {
			title := name
			if app, err := workloads.ReadApp(s.AppsPath, name); err == nil && app.Title != "" {
				title = app.Title
			}
			where = fmt.Sprintf(`<p>%[1]s is at <a href="%[2]s">%[2]s</a>.</p>
<p>The console, for its owner, is at <a href="%[3]s">%[3]s</a>.</p>`, html.EscapeString(title), link, admin)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>%[1]s</title>
<style>body{font:18px/1.5 system-ui,sans-serif;max-width:38em;margin:3em auto;padding:0 1em}code{word-break:break-all}</style>
<h1>%[1]s</h1>
%[2]s
<p>Your browser will warn that the connection isn't private, because the certificate was made by the unit itself and
not by a public authority. Before you continue, check that the fingerprint your browser shows matches the one on the
unit's screen or card:</p>
<p><code>%[3]s</code></p>`, html.EscapeString(s.Hostname), where, html.EscapeString(s.Fingerprint))
		if s.TLS == nil {
			return
		}
		if owner, _ := s.TLS.Owner(); owner != nil {
			fmt.Fprint(w, "\n<p>Its owner also gave it a certificate for these names, which browsers that trust its issuer won't warn about:</p>\n<ul>")
			for _, name := range owner.Names {
				fmt.Fprintf(w, `<li><a href="https://%[1]s/">https://%[1]s/</a></li>`, html.EscapeString(name))
			}
			fmt.Fprint(w, "</ul>\n<p>This page can't prove it's the unit's, so check the address you end up at.</p>\n")
		}
	})
}
