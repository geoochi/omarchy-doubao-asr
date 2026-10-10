package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// outputText delivers the transcript to the focused window: by default it is
// typed through the built-in Wayland virtual keyboard (typer.go), with
// "clipboard" mode putting it on the clipboard via wl-copy instead.
func outputText(ctx context.Context, cfg *Config, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if cfg.Mode == "clipboard" {
		return runWithStdin(ctx, text, "wl-copy")
	}
	return typeText(ctx, cfg, text)
}

func runWithStdin(ctx context.Context, input string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
