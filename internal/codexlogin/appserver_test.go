//go:build !windows

package codexlogin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppServerRunnerDeviceCodeFlow(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codex-home")
	fake := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
set -eu
[ "$CODEX_HOME" = "$EXPECTED_CODEX_HOME" ]
[ -z "${OPENAI_API_KEY:-}" ]
[ -z "${CODEX_API_KEY:-}" ]
[ -z "${CODEX_ACCESS_TOKEN:-}" ]
IFS= read -r initialize
printf '%s\n' '{"id":1,"result":{"serverInfo":{"name":"codex","version":"test"}}}'
IFS= read -r initialized
IFS= read -r login
printf '%s\n' '{"id":2,"result":{"type":"chatgptDeviceCode","loginId":"login-1","verificationUrl":"https://auth.openai.com/codex/device","userCode":"ABCD-1234"}}'
printf '%s\n' '{"method":"account/login/completed","params":{"loginId":"login-1","success":true,"error":null}}'
while IFS= read -r line; do :; done
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXPECTED_CODEX_HOME", home)
	t.Setenv("OPENAI_API_KEY", "must-be-removed")
	t.Setenv("CODEX_API_KEY", "must-be-removed")
	t.Setenv("CODEX_ACCESS_TOKEN", "must-be-removed")

	runner := &AppServerRunner{CodexBin: fake, ClientVersion: "1.2.3"}
	var got Challenge
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runner.Login(ctx, home, func(challenge Challenge) error { got = challenge; return nil }); err != nil {
		t.Fatal(err)
	}
	if got.LoginID != "login-1" || got.VerificationURL != "https://auth.openai.com/codex/device" || got.UserCode != "ABCD-1234" {
		t.Fatalf("challenge=%+v", got)
	}
}

func TestAppServerRunnerClassifiesDisabledDeviceAuth(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
set -eu
IFS= read -r initialize
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r initialized
IFS= read -r login
printf '%s\n' '{"id":2,"error":{"code":-32600,"message":"Device code login is not enabled"}}'
while IFS= read -r line; do :; done
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &AppServerRunner{CodexBin: fake, ClientVersion: "1.2.3"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := runner.Login(ctx, filepath.Join(t.TempDir(), "home"), func(Challenge) error { return nil })
	if !errors.Is(err, ErrDeviceAuthUnavailable) {
		t.Fatalf("err=%v", err)
	}
}
