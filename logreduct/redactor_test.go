package logreduct

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
)

func TestOverlappingSecretRedaction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		secrets []string
		input   string
		want    string
	}{
		{"prefix", []string{"shared-secret", "shared-secret-private-suffix"}, "before shared-secret-private-suffix after", "before *** after"},
		{"partial overlap", []string{"abc", "bcd"}, "abcd", "***"},
		{"overlap chain", []string{"abc", "cde", "efg"}, "abcdefgh", "***h"},
		{"nested", []string{"inner", "outer-inner-end"}, "outer-inner-end", "***"},
		{"self overlap", []string{"aaa"}, "aaaaa", "***"},
		{"separated", []string{"abc", "bcd"}, "abcd / abcd", "*** / ***"},
		{"adjacent", []string{"abc", "def"}, "abcdef", "******"},
		{"multiline component", []string{"header\nshared-secret", "shared-secret-private-suffix"}, "shared-secret-private-suffix", "***"},
		{"replacement marker", []string{"sensitive", "*"}, "sensitive", "***"},
		{"no match", []string{"absent"}, "unchanged", "unchanged"},
		{"empty input", []string{"secret"}, "", ""},
		{"empty secret", []string{""}, "unchanged", "unchanged"},
	} {
		for _, reverse := range []bool{false, true} {
			order := "forward"
			if reverse {
				order = "reverse"
			}
			t.Run(tc.name+"/"+order, func(t *testing.T) {
				secret := &Redactor{}
				for i := range tc.secrets {
					index := i
					if reverse {
						index = len(tc.secrets) - 1 - i
					}
					secret.Add(tc.secrets[index])
				}
				if got := secret.Redact(tc.input); got != tc.want {
					t.Fatalf("redaction: got %q, want %q", got, tc.want)
				}
				var output bytes.Buffer
				logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{ReplaceAttr: secret.ReplaceAttr}))
				logger.Info(tc.input, "value", tc.input, "error", errors.New(tc.input))
				var record struct {
					Message string `json:"msg"`
					Value   string `json:"value"`
					Error   string `json:"error"`
				}
				if err := json.Unmarshal(output.Bytes(), &record); err != nil {
					t.Fatal(err)
				}
				if record.Message != tc.want || record.Value != tc.want || record.Error != tc.want {
					t.Fatal("structured log redaction differs from direct masking", record)
				}
			})
		}
	}
}

func TestMultilineSecretReferenceCounts(t *testing.T) {
	secret := &Redactor{}
	first := "shared-line\r\nfirst-line\r\nshared-line\r\n"
	second := "shared-line\nsecond-line"
	secret.Add(first)
	secret.Add(first)
	secret.Add(second)
	secret.Add("first-line")
	secret.Release([]string{first})
	if got := secret.Redact("shared-line first-line second-line"); got != "*** *** ***" {
		t.Fatal("releasing one reference exposed shared components", got)
	}
	secret.Release([]string{first})
	if got := secret.Redact("shared-line first-line second-line"); got != "*** *** ***" {
		t.Fatal("releasing a multiline secret exposed independently registered components", got)
	}
	secret.Release([]string{second})
	if got := secret.Redact("shared-line first-line second-line"); got != "shared-line *** second-line" {
		t.Fatal("multiline components were not released independently", got)
	}
	secret.Release([]string{"first-line"})
	if len(secret.values) != 0 || len(secret.refs) != 0 {
		t.Fatal("released multiline secrets retained registered components")
	}

	raw, err := json.Marshal(map[string]string{"credential": base64.StdEncoding.EncodeToString([]byte(first))})
	if err != nil {
		t.Fatal(err)
	}
	values := secret.AddJIT(base64.StdEncoding.EncodeToString(raw))
	if got := secret.Redact("shared-line first-line"); got != "*** ***" {
		t.Fatal("decoded multiline JIT secret was not registered", got)
	}
	secret.Release(values)
	if len(secret.values) != 0 || len(secret.refs) != 0 {
		t.Fatal("JIT release retained multiline components")
	}
}

func TestZeroValueAndConcurrentUse(t *testing.T) {
	var secret Redactor
	if got := secret.Redact("unchanged"); got != "unchanged" {
		t.Fatal("zero value changed unregistered text", got)
	}
	secret.Release([]string{"unregistered"})
	secret.Add("shared-secret")
	var workers sync.WaitGroup
	for range 10 {
		workers.Go(func() {
			for range 100 {
				secret.Add("shared-secret")
				if got := secret.Redact("shared-secret"); got != "***" {
					t.Error("registered secret was exposed", got)
				}
				secret.Release([]string{"shared-secret"})
			}
		})
	}
	workers.Wait()
	secret.Release([]string{"shared-secret"})
	if got := secret.Redact("shared-secret"); got != "shared-secret" {
		t.Fatal("released secret remained registered", got)
	}
}
