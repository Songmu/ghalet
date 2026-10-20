package appsetup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const projectURL = "https://github.com/Songmu/ghalet"

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type manifest struct {
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	RedirectURL string            `json:"redirect_url"`
	Public      bool              `json:"public"`
	Hook        hook              `json:"hook_attributes"`
	Permissions map[string]string `json:"default_permissions"`
	Events      []string          `json:"default_events"`
}

type hook struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

var startPage = template.Must(template.New("start").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><title>ghalet App setup</title>
<h1>Create a private GitHub App for {{.Scope}}</h1>
<p>Requested permission: {{.Permission}} (write). No webhooks or user OAuth authorization.</p>
<p>Review the App name on GitHub, then install it on the target account.
For a repository target, select only the target repository.</p>
<form action="{{.Action}}" method="post">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<button type="submit">Continue to GitHub</button>
</form></html>`))

func openBrowser(ctx context.Context, address string) error {
	command := "xdg-open"
	if runtime.GOOS == "darwin" {
		command = "open"
	}
	return exec.CommandContext(ctx, command, address).Run()
}

func (s service) create(ctx context.Context, options Options, scope, ownerType, permission string, output io.Writer) (app, error) {
	host, _, err := net.SplitHostPort(options.Listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return app{}, errors.New("--listen must use a loopback IP address and port")
	}
	listener, err := net.Listen("tcp", options.Listen)
	if err != nil {
		return app{}, err
	}
	defer listener.Close()
	origin := "http://" + listener.Addr().String()
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return app{}, err
	}
	state := hex.EncodeToString(random[:])
	name := options.ID
	if len(name) > 20 {
		name = name[:20]
	}
	value := manifest{
		Name: "ghalet-" + name + "-" + state[:6], URL: projectURL,
		RedirectURL: origin + "/callback", Hook: hook{URL: projectURL},
		Permissions: map[string]string{permission: "write"}, Events: []string{},
	}
	data, err := json.Marshal(value)
	if err != nil {
		return app{}, err
	}
	action := "https://github.com/settings/apps/new"
	if ownerType == "Organization" {
		action = "https://github.com/organizations/" + url.PathEscape(strings.Split(scope, "/")[0]) + "/settings/apps/new"
	}
	action += "?state=" + state
	code := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/start/"+state, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		if err := startPage.Execute(w, struct{ Scope, Permission, Action, Manifest string }{scope, permission, action, string(data)}); err != nil {
			http.Error(w, "Cannot render setup page", http.StatusInternalServerError)
			return
		}
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.Method != http.MethodGet || len(query["state"]) != 1 || len(query["code"]) != 1 ||
			subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 ||
			len(query.Get("code")) > 256 || !identifier.MatchString(query.Get("code")) {
			http.Error(w, "Invalid setup callback", http.StatusBadRequest)
			return
		}
		select {
		case code <- query.Get("code"):
			fmt.Fprintln(w, "App creation received. Return to the terminal to continue installation.")
		default:
			http.Error(w, "Setup callback already received", http.StatusConflict)
		}
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action https://github.com; frame-ancestors 'none'; base-uri 'none'")
		if r.Host != listener.Addr().String() {
			http.Error(w, "Invalid host", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second}
	serveDone := make(chan struct{})
	var serveError error
	go func() {
		serveError = server.Serve(listener)
		close(serveDone)
	}()
	defer func() {
		server.Close()
		<-serveDone
	}()
	address := origin + "/start/" + state
	if err := s.browse(ctx, options.NoBrowser, address, output); err != nil {
		return app{}, err
	}
	select {
	case temporary := <-code:
		var created app
		if err := s.api.request(ctx, http.MethodPost, "/app-manifests/"+temporary+"/conversions", "", &created); err != nil {
			return app{}, fmt.Errorf("exchange Manifest code: %w; if the App was created, inspect its settings before retrying", err)
		}
		return created, nil
	case <-ctx.Done():
		return app{}, fmt.Errorf("waiting for browser callback: %w", ctx.Err())
	case <-serveDone:
		return app{}, fmt.Errorf("local callback server stopped: %w", serveError)
	}
}

func (s service) browse(ctx context.Context, manual bool, address string, output io.Writer) error {
	if _, err := fmt.Fprintln(output, "Open in your browser:", address); err != nil {
		return err
	}
	if !manual {
		browserCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := s.open(browserCtx, address); err != nil {
			_, writeErr := fmt.Fprintf(output, "Browser could not be opened (%v); open the URL manually.\n", err)
			return writeErr
		}
	}
	return nil
}
