package main

// Typing backend: turns the transcript into key events on the Wayland
// virtual keyboard from wayland.go.
//
// Chromium treats the physical meaning of a scancode as authoritative for
// control keys even when the compositor's keymap gives it a printable
// keysym, so literal text is only ever placed on the printable section of
// the keyboard (safeKeycodes). A keymap can hold at most that many distinct
// characters and Chromium reacts badly when the keymap is replaced on a live
// connection, so longer texts are split into segments of at most 48
// distinct characters, each typed over a fresh connection. The safe-keycode
// strategy mirrors the fix proposed upstream in wtype PR #77; the
// per-segment connection matches how separate wtype invocations are known to
// work in Chromium.

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// safeKeycodes are the evdev codes of the printable section of a PC
// keyboard: digits, letters, punctuation and space. Escape, Backspace, Tab,
// Enter, the modifiers, Caps Lock and the function keys are deliberately
// excluded.
var safeKeycodes = []uint32{
	2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13,
	16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27,
	30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41,
	43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53,
	57,
}

// keyPressGap is how long a virtual key stays pressed so that press and
// release do not share one millisecond timestamp.
const keyPressGap = time.Millisecond

// typeText types text into the focused Wayland surface.
func typeText(ctx context.Context, cfg *Config, text string) error {
	runes := sanitizeRunes(text)
	if len(runes) == 0 {
		return nil
	}
	delay := time.Duration(max(cfg.TypeDelayMS, 0)) * time.Millisecond
	for _, segment := range splitSegments(runes, len(safeKeycodes)) {
		if err := typeSegment(ctx, segment, delay); err != nil {
			return err
		}
	}
	return nil
}

// splitSegments splits runes into consecutive chunks that each contain at
// most limit distinct characters, so a single keymap can cover the chunk.
func splitSegments(runes []rune, limit int) [][]rune {
	var segments [][]rune
	seen := map[rune]bool{}
	start := 0
	for i, r := range runes {
		if !seen[r] {
			if len(seen) == limit {
				segments = append(segments, runes[start:i])
				seen = map[rune]bool{}
				start = i
			}
			seen[r] = true
		}
	}
	if start < len(runes) {
		segments = append(segments, runes[start:])
	}
	return segments
}

// typeSegment types one chunk over a dedicated virtual keyboard connection.
func typeSegment(ctx context.Context, runes []rune, delay time.Duration) error {
	kb, err := openVirtualKeyboard()
	if err != nil {
		return fmt.Errorf("wayland keyboard: %w", err)
	}
	defer kb.Close()

	// Assign each distinct character of the segment to one safe keycode.
	slot := map[rune]int{}
	order := make([]rune, 0, len(runes))
	for _, r := range runes {
		if _, ok := slot[r]; !ok {
			slot[r] = len(order)
			order = append(order, r)
		}
	}
	logger.Debug("typing segment",
		"chars", len(runes),
		"distinct", len(order),
		"delay_ms", delay.Milliseconds(),
	)

	if err := kb.uploadKeymap(keymapForGroup(order)); err != nil {
		return fmt.Errorf("upload keymap: %w", err)
	}
	if err := sleepCtx(ctx, delay); err != nil {
		return err
	}

	for _, r := range runes {
		code := safeKeycodes[slot[r]]
		if err := kb.keyPress(code); err != nil {
			return err
		}
		if err := sleepCtx(ctx, keyPressGap); err != nil {
			return err
		}
		if err := kb.keyRelease(code); err != nil {
			return err
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return err
		}
	}
	// Make sure every key event was processed before the connection goes away.
	return kb.roundtrip()
}

// keymapForGroup renders an XKB keymap that maps position i of the group to
// safeKeycodes[i]. The XKB keycode is the evdev code plus 8.
func keymapForGroup(group []rune) string {
	var b strings.Builder
	b.WriteString("xkb_keymap {\n")
	fmt.Fprintf(&b, "xkb_keycodes \"(unnamed)\" {\nminimum = 8;\nmaximum = %d;\n", safeKeycodes[len(group)-1]+9)
	for i := range group {
		fmt.Fprintf(&b, "<K%d> = %d;\n", i+1, safeKeycodes[i]+8)
	}
	b.WriteString("};\n")
	b.WriteString("xkb_types \"(unnamed)\" { include \"complete\" };\n")
	b.WriteString("xkb_compatibility \"(unnamed)\" { include \"complete\" };\n")
	b.WriteString("xkb_symbols \"(unnamed)\" {\n")
	for i, r := range group {
		fmt.Fprintf(&b, "key <K%d> {[%s]};\n", i+1, keysymName(r))
	}
	b.WriteString("};\n};\n")
	return b.String()
}

// keysymName renders a rune as an XKB keysym name: alphanumerics as the
// character itself, everything else in the UXXXX notation that xkbcommon
// maps to the matching Unicode keysym.
func keysymName(r rune) string {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return string(r)
	default:
		return fmt.Sprintf("U%04X", r)
	}
}

// sanitizeRunes turns control characters (newlines, tabs, ...) into spaces
// so a stray one cannot act as Enter/Tab in the target application.
func sanitizeRunes(text string) []rune {
	runes := make([]rune, 0, utf8.RuneCountInString(text))
	for _, r := range text {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		runes = append(runes, r)
	}
	return runes
}

// sleepCtx sleeps unless the context is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
