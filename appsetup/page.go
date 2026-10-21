package appsetup

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
	"strconv"
)

//go:embed page.html
var pageHTML string

var setupPage = template.Must(template.New("setup").Parse(pageHTML))

type setupPageData struct {
	Scope, Nonce, Action, Manifest, Slug, InstallationURL string
	Repository, Failed                                    bool
}

func renderSetupPage(w http.ResponseWriter, status int, page setupPageData) {
	var body bytes.Buffer
	if err := setupPage.Execute(&body, page); err != nil {
		http.Error(w, "Cannot render setup page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	w.WriteHeader(status)
	w.Write(body.Bytes())
}
