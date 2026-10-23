package credentialstore

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestReadCredentialInputPAT(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   io.Reader
		want    string
		wantErr string
	}{
		{name: "trimmed", input: strings.NewReader(" \t synthetic-token\r\n"), want: "synthetic-token"},
		{name: "size limit", input: strings.NewReader(strings.Repeat("a", maxCredentialSize)), want: strings.Repeat("a", maxCredentialSize)},
		{name: "missing", wantErr: "credential input is required"},
		{name: "empty", input: strings.NewReader(" \n"), wantErr: "credential must be nonempty"},
		{name: "oversized", input: strings.NewReader(strings.Repeat("a", maxCredentialSize+1)), wantErr: "no larger than 1 MiB"},
		{name: "whitespace", input: strings.NewReader("one two"), wantErr: "single nonempty ASCII token"},
		{name: "multiline", input: strings.NewReader("one\ntwo"), wantErr: "single nonempty ASCII token"},
		{name: "non-ASCII", input: strings.NewReader("token\xc3\xa9"), wantErr: "single nonempty ASCII token"},
		{name: "read failure", input: iotest.ErrReader(errors.New("sensitive diagnostic")), wantErr: "read credential input failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret, err := readCredentialInput(Options{Type: "pat", Stdin: true}, tc.input)
			defer clear(secret)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) ||
					strings.Contains(err.Error(), "sensitive diagnostic") || secret != nil {
					t.Fatal("invalid input returned a secret or incorrect diagnostic", err)
				}
				return
			}
			if err != nil || string(secret) != tc.want {
				t.Fatal("valid token was not preserved", err)
			}
		})
	}
}

func TestReadCredentialInputAppKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, key)})
	for _, tc := range []struct {
		name    string
		input   []byte
		wantErr bool
	}{
		{name: "PKCS1", input: pkcs1},
		{name: "PKCS8", input: pkcs8},
		{name: "not PEM", input: []byte("not a key"), wantErr: true},
		{name: "multiple keys", input: bytes.Repeat(pkcs1, 2), wantErr: true},
		{name: "invalid DER", input: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("invalid")}), wantErr: true},
		{name: "unsupported block", input: pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("invalid")}), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret, err := readCredentialInput(Options{Type: "github-app", Stdin: true}, bytes.NewReader(tc.input))
			defer clear(secret)
			if tc.wantErr {
				if err == nil || secret != nil {
					t.Fatal("invalid App key returned a secret", err)
				}
			} else if err != nil || !bytes.Equal(secret, bytes.TrimSpace(tc.input)) {
				t.Fatal("valid App key was not preserved", err)
			}
		})
	}
}
