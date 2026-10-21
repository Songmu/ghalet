package appsetup

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetupPage(t *testing.T) {
	for _, tc := range []struct {
		name string
		page setupPageData
		want string
	}{
		{"repository", setupPageData{Scope: "example/runners", Repository: true}, "Repository administration"},
		{"organization", setupPageData{Scope: "example"}, "Organization self-hosted runners"},
		{"installation", setupPageData{Scope: "example/runners", Repository: true, Slug: "ghalet-example", InstallationURL: installationURL("ghalet-example")}, "Only select repositories"},
		{"failure", setupPageData{Failed: true}, "App setup needs attention"},
		{"escaped", setupPageData{Scope: `<script>alert("scope")</script>`}, "&lt;script&gt;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			renderSetupPage(response, http.StatusOK, tc.page)
			body := response.Body.String()
			if response.Code != http.StatusOK || !strings.Contains(body, tc.want) ||
				strings.Contains(body, "<script>") {
				t.Fatal("incorrect or unsafe setup page")
			}
		})
	}
}
