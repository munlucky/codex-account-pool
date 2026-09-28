package codexlogin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/munlucky/codex-account-pool/internal/process"
)

var (
	ErrDeviceAuthUnavailable = errors.New("codex device authentication is unavailable")
	ErrLoginFailed           = errors.New("codex device authentication failed")
	ErrProtocol              = errors.New("codex app-server protocol failure")
)

type Challenge struct {
	LoginID         string
	VerificationURL string
	UserCode        string
}

type Runner interface {
	Login(context.Context, string, func(Challenge) error) error
}

type AppServerRunner struct {
	CodexBin      string
	ClientVersion string
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

func (r *AppServerRunner) Login(ctx context.Context, codexHome string, onChallenge func(Challenge) error) error {
	if r == nil || strings.TrimSpace(codexHome) == "" || onChallenge == nil {
		return fmt.Errorf("%w: login runtime is not configured", ErrProtocol)
	}
	codexBin := strings.TrimSpace(r.CodexBin)
	if codexBin == "" {
		codexBin = "codex"
	}
	executable, err := process.ResolveExecutable(codexBin)
	if err != nil {
		return fmt.Errorf("%w: codex executable not found", ErrProtocol)
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return fmt.Errorf("%w: prepare CODEX_HOME", ErrProtocol)
	}

	cmd := exec.Command(executable, "-c", `cli_auth_credentials_store="file"`, "app-server", "--listen", "stdio://")
	cmd.Env = process.BuildEnvironment(os.Environ(), map[string]string{
		"CODEX_HOME": codexHome,
	}, []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"})
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("%w: open app-server stdin", ErrProtocol)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%w: open app-server stdout", ErrProtocol)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: start app-server", ErrProtocol)
	}

	messages := make(chan rpcEnvelope, 16)
	readErr := make(chan error, 1)
	go readJSONL(stdout, messages, readErr)
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	var writeMu sync.Mutex
	write := func(value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		_, err = stdin.Write(data)
		return err
	}

	closed := false
	shutdown := func() {
		if closed {
			return
		}
		closed = true
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-waitErr:
		case <-time.After(time.Second):
		}
	}
	defer shutdown()

	version := strings.TrimSpace(r.ClientVersion)
	if version == "" {
		version = "docker"
	}
	if err := write(map[string]any{
		"id":     1,
		"method": "initialize",
		"params": map[string]any{
			"clientInfo":   map[string]string{"name": "gpt_codex_router", "title": "GPT Codex Router", "version": version},
			"capabilities": map[string]bool{"experimentalApi": false},
		},
	}); err != nil {
		return fmt.Errorf("%w: send initialize", ErrProtocol)
	}
	if _, err := waitResponse(ctx, 1, messages, readErr, waitErr); err != nil {
		return err
	}
	if err := write(map[string]any{"method": "initialized"}); err != nil {
		return fmt.Errorf("%w: send initialized", ErrProtocol)
	}
	if err := write(map[string]any{
		"id":     2,
		"method": "account/login/start",
		"params": map[string]string{"type": "chatgptDeviceCode"},
	}); err != nil {
		return fmt.Errorf("%w: start device login", ErrProtocol)
	}
	response, err := waitResponse(ctx, 2, messages, readErr, waitErr)
	if err != nil {
		if errors.Is(err, ErrDeviceAuthUnavailable) {
			return err
		}
		return err
	}
	var challengeResult struct {
		Type            string `json:"type"`
		LoginID         string `json:"loginId"`
		VerificationURL string `json:"verificationUrl"`
		UserCode        string `json:"userCode"`
	}
	if err := json.Unmarshal(response.Result, &challengeResult); err != nil ||
		challengeResult.Type != "chatgptDeviceCode" ||
		strings.TrimSpace(challengeResult.LoginID) == "" ||
		strings.TrimSpace(challengeResult.VerificationURL) == "" ||
		strings.TrimSpace(challengeResult.UserCode) == "" {
		return fmt.Errorf("%w: invalid device login response", ErrProtocol)
	}
	challenge := Challenge{
		LoginID:         challengeResult.LoginID,
		VerificationURL: challengeResult.VerificationURL,
		UserCode:        challengeResult.UserCode,
	}
	if err := onChallenge(challenge); err != nil {
		_ = sendCancel(write, challenge.LoginID)
		return err
	}

	for {
		select {
		case <-ctx.Done():
			_ = sendCancel(write, challenge.LoginID)
			return ctx.Err()
		case err := <-readErr:
			if err == nil {
				err = io.EOF
			}
			return fmt.Errorf("%w: app-server output closed", ErrProtocol)
		case err := <-waitErr:
			if err == nil {
				err = io.EOF
			}
			return fmt.Errorf("%w: app-server exited", ErrProtocol)
		case msg := <-messages:
			if msg.Method != "account/login/completed" {
				continue
			}
			var completed struct {
				LoginID string  `json:"loginId"`
				Success bool    `json:"success"`
				Error   *string `json:"error"`
			}
			if err := json.Unmarshal(msg.Params, &completed); err != nil || completed.LoginID != challenge.LoginID {
				continue
			}
			if !completed.Success {
				return ErrLoginFailed
			}
			return nil
		}
	}
}

func sendCancel(write func(any) error, loginID string) error {
	return write(map[string]any{
		"id":     3,
		"method": "account/login/cancel",
		"params": map[string]string{"loginId": loginID},
	})
}

func waitResponse(ctx context.Context, id int, messages <-chan rpcEnvelope, readErr <-chan error, waitErr <-chan error) (rpcEnvelope, error) {
	for {
		select {
		case <-ctx.Done():
			return rpcEnvelope{}, ctx.Err()
		case <-readErr:
			return rpcEnvelope{}, fmt.Errorf("%w: app-server output closed", ErrProtocol)
		case <-waitErr:
			return rpcEnvelope{}, fmt.Errorf("%w: app-server exited", ErrProtocol)
		case msg := <-messages:
			var got int
			if len(msg.ID) == 0 || json.Unmarshal(msg.ID, &got) != nil || got != id {
				continue
			}
			if msg.Error != nil {
				if strings.Contains(strings.ToLower(msg.Error.Message), "device code login is not enabled") ||
					strings.Contains(strings.ToLower(msg.Error.Message), "device auth") {
					return rpcEnvelope{}, ErrDeviceAuthUnavailable
				}
				return rpcEnvelope{}, ErrLoginFailed
			}
			return msg, nil
		}
	}
}

func readJSONL(reader io.Reader, messages chan<- rpcEnvelope, readErr chan<- error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var message rpcEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		messages <- message
	}
	readErr <- scanner.Err()
}
