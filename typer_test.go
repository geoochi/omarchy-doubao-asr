package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestSafeKeycodesArePrintable(t *testing.T) {
	if len(safeKeycodes) != 48 {
		t.Fatalf("safeKeycodes has %d entries, want 48", len(safeKeycodes))
	}
	// Positions that must never carry literal text: Escape(1), Backspace(14),
	// Tab(15), Enter(28), Ctrl(29), Shift(42/54), KPAsterisk(55), Alt(56),
	// CapsLock(58) and everything above (function keys, media keys).
	excluded := map[uint32]bool{1: true, 14: true, 15: true, 28: true, 29: true, 42: true, 54: true, 55: true, 56: true, 58: true}
	for i, code := range safeKeycodes {
		if code < 2 || code > 57 {
			t.Errorf("safeKeycodes[%d] = %d is outside the printable range", i, code)
		}
		if excluded[code] {
			t.Errorf("safeKeycodes[%d] = %d is a control key position", i, code)
		}
		if i > 0 && code <= safeKeycodes[i-1] {
			t.Errorf("safeKeycodes is not strictly ascending at %d", i)
		}
	}
}

func TestSplitSegmentsDistinctLimit(t *testing.T) {
	runes := make([]rune, 0, 100)
	for i := range 100 {
		runes = append(runes, rune(0x4E00+i))
	}
	segs := splitSegments(runes, len(safeKeycodes))
	if len(segs) != 3 {
		t.Fatalf("segments = %d, want 3", len(segs))
	}
	for i, want := range []int{48, 48, 4} {
		if len(segs[i]) != want {
			t.Errorf("segment %d has %d runes, want %d", i, len(segs[i]), want)
		}
	}
	var got []rune
	for _, s := range segs {
		got = append(got, s...)
	}
	if !reflect.DeepEqual(got, runes) {
		t.Fatal("segments do not reconstruct the input")
	}
}

func TestSplitSegmentsDuplicatesStayTogether(t *testing.T) {
	runes := []rune(strings.Repeat("重复字符", 10) + "x")
	if segs := splitSegments(runes, len(safeKeycodes)); len(segs) != 1 {
		t.Fatalf("segments = %d, want 1 (only 5 distinct characters)", len(segs))
	}
}

func TestSplitSegmentsRepeatAfterBoundary(t *testing.T) {
	// 50 distinct runes followed by a repeat of the first one: the repeat
	// starts the next segment as a fresh distinct character.
	runes := make([]rune, 0, 51)
	for i := range 50 {
		runes = append(runes, rune(0x4E00+i))
	}
	runes = append(runes, rune(0x4E00))
	segs := splitSegments(runes, len(safeKeycodes))
	if len(segs) != 2 || len(segs[0]) != 48 || len(segs[1]) != 3 {
		t.Fatalf("got %d segments with sizes %v, want [48 3]", len(segs), segSizes(segs))
	}
	// The second segment holds the two remaining new characters plus the
	// repeat, which counts as a fresh distinct character again.
	if want := []rune{0x4E30, 0x4E31, 0x4E00}; !reflect.DeepEqual(segs[1], want) {
		t.Fatalf("second segment = %q, want %q", string(segs[1]), string(want))
	}
}

func segSizes(segs [][]rune) []int {
	sizes := make([]int, len(segs))
	for i, s := range segs {
		sizes[i] = len(s)
	}
	return sizes
}

func TestKeymapForGroup(t *testing.T) {
	km := keymapForGroup([]rune{'a', '中', ' '})
	for _, want := range []string{
		"xkb_keycodes \"(unnamed)\" {",
		"minimum = 8;",
		"maximum = 13;",
		"<K1> = 10;",
		"<K2> = 11;",
		"<K3> = 12;",
		"xkb_types \"(unnamed)\" { include \"complete\" };",
		"xkb_compatibility \"(unnamed)\" { include \"complete\" };",
		"key <K1> {[a]};",
		"key <K2> {[U4E2D]};",
		"key <K3> {[U0020]};",
	} {
		if !strings.Contains(km, want) {
			t.Errorf("keymap missing %q\n%s", want, km)
		}
	}
}

func TestKeymapForFullGroupUsesOnlySafeCodes(t *testing.T) {
	group := make([]rune, len(safeKeycodes))
	for i := range group {
		group[i] = rune(0x4E00 + i)
	}
	km := keymapForGroup(group)
	if got := strings.Count(km, "\n<K"); got != len(safeKeycodes) {
		t.Errorf("keycodes section has %d entries, want %d", got, len(safeKeycodes))
	}
	// Highest declared XKB keycode is evdev 57 + 8, and maximum sits above it.
	if !strings.Contains(km, "<K48> = 65;") {
		t.Error("last entry should map to XKB keycode 65")
	}
	if !strings.Contains(km, "maximum = 66;") {
		t.Error("maximum should be one above the last declared keycode")
	}
}

func TestKeysymName(t *testing.T) {
	cases := map[rune]string{
		'a': "a",
		'Z': "Z",
		'7': "7",
		'中': "U4E2D",
		' ': "U0020",
		',': "U002C",
		'😀': "U1F600",
	}
	for r, want := range cases {
		if got := keysymName(r); got != want {
			t.Errorf("keysymName(%q) = %q, want %q", r, got, want)
		}
	}
}

func TestSanitizeRunesReplacesControlCharacters(t *testing.T) {
	got := string(sanitizeRunes("你好\n世界\t!"))
	if want := "你好 世界 !"; got != want {
		t.Errorf("sanitizeRunes = %q, want %q", got, want)
	}
	if got := string(sanitizeRunes("a\x00b")); got != "a b" {
		t.Errorf("sanitizeRunes(NUL) = %q, want %q", got, "a b")
	}
}
