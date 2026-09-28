package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type Command struct {
	Executable string
	Args       []string
	Dir        string
	SetEnv     map[string]string
	UnsetEnv   []string
	Detached   bool
}

type Executor interface {
	Run(context.Context, Command) error
}

type OSExecutor struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

func (e OSExecutor) Run(ctx context.Context, spec Command) error {
	if strings.TrimSpace(spec.Executable) == "" {
		return fmt.Errorf("executable is empty")
	}
	executable, err := ResolveExecutable(spec.Executable)
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	if spec.Detached {
		if err := ctx.Err(); err != nil {
			return err
		}
		cmd = exec.Command(executable, spec.Args...)
	} else {
		cmd = exec.CommandContext(ctx, executable, spec.Args...)
	}
	cmd.Env = BuildEnvironment(os.Environ(), spec.SetEnv, spec.UnsetEnv)
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	if !spec.Detached {
		cmd.Stdin = e.Stdin
		cmd.Stdout = e.Stdout
		cmd.Stderr = e.Stderr
	}
	if spec.Detached {
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start %s: %w", spec.Executable, err)
		}
		if err := cmd.Process.Release(); err != nil {
			return fmt.Errorf("release %s: %w", spec.Executable, err)
		}
		return nil
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %s: %w", spec.Executable, err)
	}
	return nil
}

func ResolveExecutable(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("find executable %s: %w", name, err)
	}
	if runtime.GOOS != "windows" {
		return path, nil
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".exe" || ext == ".com" || ext == "" {
		return path, nil
	}
	if ext == ".cmd" || ext == ".bat" {
		if native, ok := NativeCodexFromNPMShim(path); ok {
			return native, nil
		}
		return "", fmt.Errorf("refusing to execute Windows batch shim %q through cmd.exe; configure a native executable override", path)
	}
	return path, nil
}

func NativeCodexFromNPMShim(shim string) (string, bool) {
	base := strings.ToLower(filepath.Base(shim))
	if base != "codex.cmd" && base != "codex.bat" {
		return "", false
	}
	npmDir := filepath.Dir(shim)
	patterns := []string{
		filepath.Join(npmDir, "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-win32-*", "vendor", "*", "bin", "codex.exe"),
		filepath.Join(npmDir, "node_modules", "@openai", "codex-win32-*", "vendor", "*", "bin", "codex.exe"),
	}
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, match := range matches {
			if info, err := os.Stat(match); err == nil && !info.IsDir() {
				return match, true
			}
		}
	}
	return "", false
}

func BuildEnvironment(base []string, set map[string]string, unset []string) []string {
	blocked := map[string]bool{}
	for _, key := range unset {
		blocked[strings.ToUpper(key)] = true
	}
	values := map[string]string{}
	casing := map[string]string{}
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		upper := strings.ToUpper(key)
		if blocked[upper] {
			continue
		}
		values[upper] = value
		casing[upper] = key
	}
	for key, value := range set {
		upper := strings.ToUpper(key)
		values[upper] = value
		casing[upper] = key
		delete(blocked, upper)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, upper := range keys {
		out = append(out, casing[upper]+"="+values[upper])
	}
	return out
}
