package credentialstore

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"syscall"
)

const maxCredentialSize = 1024 * 1024

func readCredentialInput(options Options, input io.Reader) ([]byte, error) {
	if options.File != "" {
		info, err := os.Lstat(options.File)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("--file requires a regular file, not a symlink or pipe")
		}
		file, err := os.OpenFile(options.File, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		info, err = file.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("input file must be accessible only to its owner (normally chmod 600)")
		}
		input = file
	}
	if input == nil {
		return nil, errors.New("credential input is required")
	}
	data, err := io.ReadAll(io.LimitReader(input, maxCredentialSize+1))
	if err != nil {
		clear(data)
		return nil, errors.New("read credential input failed")
	}
	secret := bytes.TrimSpace(data)
	if len(data) > maxCredentialSize || len(secret) == 0 {
		clear(data)
		return nil, errors.New("credential must be nonempty and no larger than 1 MiB")
	}
	if options.Type == "pat" {
		if bytes.IndexFunc(secret, func(r rune) bool { return r <= ' ' || r >= 127 }) >= 0 {
			clear(data)
			return nil, errors.New("PAT must be a single nonempty ASCII token without whitespace")
		}
	} else {
		block, rest := pem.Decode(secret)
		if block == nil || len(bytes.TrimSpace(rest)) != 0 {
			clear(data)
			return nil, errors.New("GitHub App input must contain one unencrypted RSA PEM private key")
		}
		var key any
		switch block.Type {
		case "RSA PRIVATE KEY":
			key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		case "PRIVATE KEY":
			key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		default:
			err = errors.New("unsupported PEM block")
		}
		_, rsaKey := key.(*rsa.PrivateKey)
		if err != nil || !rsaKey {
			clear(data)
			return nil, errors.New("GitHub App input must contain an unencrypted RSA PEM private key")
		}
	}
	return secret, nil
}
