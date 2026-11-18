//go:build darwin && cgo

package bmaexecutor

import (
	"slices"
	"unicode/utf16"
	"testing"
)

func TestMacTextTypesWhatTheLinuxWorkerTypes(t *testing.T) {
	for _, tc := range []struct {
		keysym uint32
		want   []uint16
	}{
		{0x01004f60, []uint16{0x4f60}},         // 你
		{0x0100ff0c, []uint16{0xff0c}},         // ，
		{0x000000e9, []uint16{0x00e9}},         // é as a Latin-1 keysym
		{0x010000e9, []uint16{0x00e9}},         // é as a Unicode keysym
		{0x0101f600, []uint16{0xd83d, 0xde00}}, // 😀 needs a surrogate pair
	} {
		got, ok := macText(tc.keysym)
		if !ok || !slices.Equal(got, tc.want) {
			t.Errorf("macText(%#x) = %x, %v; want %x", tc.keysym, got, ok, tc.want)
		}
	}
}

func TestMacTextRejectsNonCharacters(t *testing.T) {
	for _, keysym := range []uint32{
		0xff0d,     // Return is a key, not text
		0xffe1,     // Shift_L
		0x9f,       // C1 control
		0x01000000, // no code point
		0x0100000a, // control character
		0x0100d800, // surrogate
		0x01110000, // beyond Unicode
	} {
		if got, ok := macText(keysym); ok {
			t.Errorf("macText(%#x) = %x, want rejected", keysym, got)
		}
	}
}

func TestMacKeyStillOwnsShortcutKeys(t *testing.T) {
	for _, keysym := range []uint32{'a', 'A', '1', '!', ' ', 0xff0d, 0xffe3} {
		if _, ok := macKey(keysym); !ok {
			t.Errorf("macKey(%#x) not mapped to a physical key", keysym)
		}
	}
	if _, ok := macKey(0x01004f60); ok {
		t.Error("macKey mapped a CJK keysym to a physical key")
	}
}

func TestMacTextChunksKeepSurrogatePairsWhole(t *testing.T) {
	text := "中文输入测试abcdefghijklmn😀z"
	chunks := macTextChunks(text)
	var all []uint16
	for _, c := range chunks {
		if len(c) == 0 || len(c) > 20 {
			t.Fatalf("chunk of %d units", len(c))
		}
		if last := c[len(c)-1]; last >= 0xd800 && last < 0xdc00 {
			t.Fatal("chunk ends inside a surrogate pair")
		}
		all = append(all, c...)
	}
	if string(utf16.Decode(all)) != text {
		t.Fatalf("chunks lose text: %q", string(utf16.Decode(all)))
	}
}
