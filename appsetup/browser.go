package appsetup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
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

func openBrowser(ctx context.Context, address string) error {
	command := "xdg-open"
	if runtime.GOOS == "darwin" {
		command = "open"
	}
	return exec.CommandContext(ctx, command, address).Run()
}

func (s service) create(ctx context.Context, options Options, scope, ownerType, permission string, output io.Writer, save func(app) error) error {
	host, _, err := net.SplitHostPort(options.Listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("--listen must use a loopback IP address and port")
	}
	listener, err := net.Listen("tcp", options.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	origin := "http://" + listener.Addr().String()
	var random [64]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	state := hex.EncodeToString(random[:32])
	nonce := hex.EncodeToString(random[32:])
	name := options.credentialName
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
		return err
	}
	action := "https://github.com/settings/apps/new"
	if ownerType == "Organization" {
		action = "https://github.com/organizations/" + url.PathEscape(strings.Split(scope, "/")[0]) + "/settings/apps/new"
	}
	action += "?state=" + state
	done := make(chan error, 1)
	callbackDone := make(chan struct{})
	const (
		waiting = iota
		processing
		stopped
	)
	var callback atomic.Int32
	page := setupPageData{Scope: scope, Nonce: nonce, Repository: strings.Contains(scope, "/")}
	mux := http.NewServeMux()
	mux.HandleFunc("/start/"+state, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		start := page
		start.Action, start.Manifest = action, string(data)
		renderSetupPage(w, http.StatusOK, start)
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.Method != http.MethodGet || len(query["state"]) != 1 || len(query["code"]) != 1 ||
			subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 ||
			len(query.Get("code")) > 256 || !identifier.MatchString(query.Get("code")) {
			http.Error(w, "Invalid setup callback", http.StatusBadRequest)
			return
		}
		if !callback.CompareAndSwap(waiting, processing) {
			http.Error(w, "Setup callback already received", http.StatusConflict)
			return
		}
		defer close(callbackDone)
		// Persist converted credentials even if the browser disconnects. Saving
		// can outlast the normal response deadline while the store is locked.
		controller := http.NewResponseController(w)
		controller.SetWriteDeadline(time.Time{})
		var created app
		err := s.api.request(ctx, http.MethodPost, "/app-manifests/"+query.Get("code")+"/conversions", "", &created)
		if err != nil {
			err = fmt.Errorf("exchange Manifest code: %w; if the App was created, inspect its settings before retrying", err)
		} else {
			err = save(created)
		}
		created.PEM = ""
		finished := page
		status := http.StatusOK
		if err != nil || ctx.Err() != nil {
			finished.Failed = true
			status = http.StatusInternalServerError
		} else {
			finished.Slug = created.Slug
			finished.InstallationURL = installationURL(created.Slug)
		}
		controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		renderSetupPage(w, status, finished)
		controller.Flush()
		done <- err
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'nonce-"+nonce+"'; form-action https://github.com; frame-ancestors 'none'; base-uri 'none'")
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
		received := !callback.CompareAndSwap(waiting, stopped)
		server.Close()
		<-serveDone
		if received {
			// Closing the browser connection must not abandon credentials
			// returned by the one-time Manifest conversion.
			<-callbackDone
		}
	}()
	address := origin + "/start/" + state
	if err := s.browse(ctx, options.NoBrowser, address, output); err != nil {
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("waiting for browser callback: %w", ctx.Err())
	case <-serveDone:
		return fmt.Errorf("local callback server stopped: %w", serveError)
	}
}

func installationURL(slug string) string {
	return "https://github.com/apps/" + slug + "/installations/new"
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
