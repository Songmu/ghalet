ghalet
=======

[![Test Status](https://github.com/Songmu/ghalet/actions/workflows/test.yaml/badge.svg?branch=main)][actions]
[![Coverage Status](https://codecov.io/gh/Songmu/ghalet/branch/main/graph/badge.svg)][codecov]
[![MIT License](https://img.shields.io/github/license/Songmu/ghalet)][license]
[![PkgGoDev](https://pkg.go.dev/badge/github.com/Songmu/ghalet)][PkgGoDev]

[actions]: https://github.com/Songmu/ghalet/actions?workflow=test
[codecov]: https://codecov.io/gh/Songmu/ghalet
[license]: https://github.com/Songmu/ghalet/blob/main/LICENSE
[PkgGoDev]: https://pkg.go.dev/github.com/Songmu/ghalet

ghalet short description

## Synopsis

```go
// simple usage here
```

## Description

## Installation

```console
# Install the latest version. (Install it into ./bin/ by default).
% curl -sfL https://raw.githubusercontent.com/Songmu/ghalet/main/install.sh | sh -s

# Specify installation directory ($(go env GOPATH)/bin/) and version.
% curl -sfL https://raw.githubusercontent.com/Songmu/ghalet/main/install.sh | sh -s -- -b $(go env GOPATH)/bin [vX.Y.Z]

# In alpine linux (as it does not come with curl by default)
% wget -O - -q https://raw.githubusercontent.com/Songmu/ghalet/main/install.sh | sh -s [vX.Y.Z]

# go install
% go install github.com/Songmu/ghalet/cmd/ghalet@latest
```

## Development

After changing GitHub Actions dependencies, run `gh actions-lock` and commit
`.github/workflows/actions.lock` with the workflow changes. Install the tool
with `gh extension install github/gh-actions-lock`.

## Author

[Songmu](https://github.com/Songmu)
