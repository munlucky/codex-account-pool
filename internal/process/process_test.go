package process

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildEnvironmentUnsetsCredentialOverrides(t *testing.T) {
	base := []string{
		"PATH=/bin",
		"OPENAI_API_KEY=do-not-pass",
		"CODEX_ACCESS_TOKEN=do-not-pass",
		"CODEX_API_KEY=do-not-pass",
		"Keep=ok",
	}
	env := BuildEnvironment(base, map[string]string{"CODEX_HOME": "/tmp/profile"}, []string{
		"OPENAI_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_API_KEY",
	})
	got := map[string]string{}
	for _, entry := range env {
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				got[entry[:i]] = entry[i+1:]
				break
			}
		}
	}
	if got["OPENAI_API_KEY"] != "" || got["CODEX_ACCESS_TOKEN"] != "" || got["CODEX_API_KEY"] != "" {
		t.Fatalf("credential override leaked into child environment: %#v", got)
	}
	if got["CODEX_HOME"] != "/tmp/profile" || got["Keep"] != "ok" {
		t.Fatalf("expected environment values missing: %#v", got)
	}
}

func TestOSExecutorRunsFakeExecutableWithSanitizedEnvironment(t *testing.T) {
	if os.Getenv("GPT_CODEX_ROUTER_HELPER_PROCESS") == "1" {
		output := os.Getenv("GPT_CODEX_ROUTER_HELPER_OUTPUT")
		values := []string{
			"CODEX_HOME=" + os.Getenv("CODEX_HOME"),
			"OPENAI_API_KEY=" + os.Getenv("OPENAI_API_KEY"),
			"CODEX_ACCESS_TOKEN=" + os.Getenv("CODEX_ACCESS_TOKEN"),
		}
		if err := os.WriteFile(output, []byte(strings.Join(values, "\n")), 0o600); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}

	output := t.TempDir() + string(os.PathSeparator) + "env.txt"
	t.Setenv("OPENAI_API_KEY", "dummy-must-not-pass")
	t.Setenv("CODEX_ACCESS_TOKEN", "dummy-must-not-pass")
	executor := OSExecutor{}
	err := executor.Run(context.Background(), Command{
		Executable: os.Args[0],
		Args:       []string{"-test.run=TestOSExecutorRunsFakeExecutableWithSanitizedEnvironment"},
		SetEnv: map[string]string{
			"GPT_CODEX_ROUTER_HELPER_PROCESS": "1",
			"GPT_CODEX_ROUTER_HELPER_OUTPUT":  output,
			"CODEX_HOME":                      "/fake/codex-home",
		},
		UnsetEnv: []string{"OPENAI_API_KEY", "CODEX_ACCESS_TOKEN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "CODEX_HOME=/fake/codex-home") {
		t.Fatalf("CODEX_HOME not passed to fake executable: %s", text)
	}
	if strings.Contains(text, "dummy-must-not-pass") {
		t.Fatalf("credential override leaked to fake executable: %s", text)
	}
}

func TestNativeCodexFromNPMShim(t *testing.T) {
	root := t.TempDir()
	shim := root + string(os.PathSeparator) + "codex.cmd"
	if err := os.WriteFile(shim, []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	native := root + string(os.PathSeparator) + "node_modules" + string(os.PathSeparator) + "@openai" + string(os.PathSeparator) + "codex" + string(os.PathSeparator) + "node_modules" + string(os.PathSeparator) + "@openai" + string(os.PathSeparator) + "codex-win32-x64" + string(os.PathSeparator) + "vendor" + string(os.PathSeparator) + "x86_64-pc-windows-msvc" + string(os.PathSeparator) + "bin" + string(os.PathSeparator) + "codex.exe"
	if err := os.MkdirAll(filepath.Dir(native), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, ok := NativeCodexFromNPMShim(shim)
	if !ok || got != native {
		t.Fatalf("native=%q ok=%v want=%q", got, ok, native)
	}
}

func TestNativeCodexFromNPMShimRejectsUnrelatedBatchFile(t *testing.T) {
	if got, ok := NativeCodexFromNPMShim(filepath.Join(t.TempDir(), "other.cmd")); ok || got != "" {
		t.Fatalf("unexpected resolution: %q %v", got, ok)
	}
}
