//go:build darwin && cgo

package bmaexecutor

/*
#cgo LDFLAGS: -framework ApplicationServices
#include <ApplicationServices/ApplicationServices.h>
static int bwTrusted(void) { return AXIsProcessTrusted(); }
static void bwSize(double *x,double *y,double *w,double *h) {
 CGRect r=CGDisplayBounds(CGMainDisplayID()); *x=r.origin.x; *y=r.origin.y; *w=r.size.width; *h=r.size.height;
}
static int bwMouse(int type,int button,double x,double y,unsigned long long flags) {
 CGEventRef e=CGEventCreateMouseEvent(NULL,(CGEventType)type,CGPointMake(x,y),(CGMouseButton)button);
 if(!e)return 0; CGEventSetFlags(e,(CGEventFlags)flags); CGEventPost(kCGHIDEventTap,e);CFRelease(e);return 1;
}
static int bwKey(int key,int down,unsigned long long flags) {
 CGEventRef e=CGEventCreateKeyboardEvent(NULL,(CGKeyCode)key,down);if(!e)return 0;
 CGEventSetFlags(e,(CGEventFlags)flags);CGEventPost(kCGHIDEventTap,e);CFRelease(e);return 1;
}
static int bwWheel(int dy,unsigned long long flags){
 CGEventRef e=CGEventCreateScrollWheelEvent(NULL,kCGScrollEventUnitLine,1,dy);if(!e)return 0;
 CGEventSetFlags(e,(CGEventFlags)flags);CGEventPost(kCGHIDEventTap,e);CFRelease(e);return 1;
}
*/
import "C"
import (
	"context"
	"errors"
	"sync"
)

type macDesktopInput struct {
	mu      sync.Mutex
	keys    map[uint32]uint16
	buttons uint8
	x, y    float64
}

func newNativeDesktopInput() (NativeDesktopInput, error) {
	if C.bwTrusted() == 0 {
		return nil, errors.New("accessibility_permission_required")
	}
	return &macDesktopInput{keys: map[uint32]uint16{}}, nil
}
func (n *macDesktopInput) flags() uint64 {
	var f uint64
	for k := range n.keys {
		switch k {
		case 0xffe1, 0xffe2:
			f |= 1 << 17
		case 0xffe3, 0xffe4:
			f |= 1 << 18
		case 0xffe9, 0xffea:
			f |= 1 << 19
		case 0xffeb, 0xffec:
			f |= 1 << 20
		}
	}
	return f
}

// Keysyms map to physical ANSI keys for shortcuts. Non-ANSI text is deliberately
// unsupported until a layout-aware text path is connected; it is never guessed.
func macKey(k uint32) (uint16, bool) {
	codes := map[uint32]uint16{0xff08: 51, 0xff09: 48, 0xff0d: 36, 0xff1b: 53, 0xffff: 117, 0xff50: 115, 0xff57: 119, 0xff55: 116, 0xff56: 121, 0xff51: 123, 0xff52: 126, 0xff53: 124, 0xff54: 125, 0xffe1: 56, 0xffe2: 60, 0xffe3: 59, 0xffe4: 62, 0xffe9: 58, 0xffea: 61, 0xffeb: 55, 0xffec: 54, ' ': 49}
	if c, ok := codes[k]; ok {
		return c, true
	}
	letters := map[uint32]uint16{'a': 0, 's': 1, 'd': 2, 'f': 3, 'h': 4, 'g': 5, 'z': 6, 'x': 7, 'c': 8, 'v': 9, 'b': 11, 'q': 12, 'w': 13, 'e': 14, 'r': 15, 'y': 16, 't': 17, 'u': 32, 'i': 34, 'o': 31, 'p': 35, 'l': 37, 'j': 38, 'k': 40, 'n': 45, 'm': 46}
	if k >= 'A' && k <= 'Z' {
		k += 32
	}
	if c, ok := letters[k]; ok {
		return c, true
	}
	shifted := map[uint32]uint32{'!': '1', '@': '2', '#': '3', '$': '4', '%': '5', '^': '6', '&': '7', '*': '8', '(': '9', ')': '0', '_': '-', '+': '=', '{': '[', '}': ']', '|': '\\', ':': ';', '"': '\'', '<': ',', '>': '.', '?': '/', '~': '`'}
	if plain, ok := shifted[k]; ok {
		k = plain
	}
	pairs := map[uint32]uint16{'1': 18, '2': 19, '3': 20, '4': 21, '5': 23, '6': 22, '7': 26, '8': 28, '9': 25, '0': 29, '-': 27, '=': 24, '[': 33, ']': 30, '\\': 42, ';': 41, '\'': 39, ',': 43, '.': 47, '/': 44, '`': 50}
	c, ok := pairs[k]
	return c, ok
}
func (n *macDesktopInput) Apply(ctx context.Context, e DesktopInputEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateDesktopInput(e); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	switch e.Type {
	case "key":
		code, ok := macKey(e.Keysym)
		if !ok {
			return errors.New("unsupported_desktop_key")
		}
		if e.Down {
			n.keys[e.Keysym] = code
		} else {
			delete(n.keys, e.Keysym)
		}
		down := 0
		if e.Down {
			down = 1
		}
		if C.bwKey(C.int(code), C.int(down), C.ulonglong(n.flags())) == 0 {
			n.keys[e.Keysym] = code
			return ErrDesktopInputUnknown
		}
	case "pointer", "wheel":
		var x, y, w, h C.double
		C.bwSize(&x, &y, &w, &h)
		n.x = float64(x) + e.X/e.W*float64(w)
		n.y = float64(y) + e.Y/e.H*float64(h)
		if e.Type == "wheel" {
			if C.bwWheel(C.int(-e.DY), C.ulonglong(n.flags())) == 0 {
				return ErrDesktopInputUnknown
			}
			return nil
		}
		for _, b := range []struct {
			mask             uint8
			button, down, up int
		}{{1, 0, 1, 2}, {2, 2, 25, 26}, {4, 1, 3, 4}} {
			if (n.buttons & b.mask) != (e.Buttons & b.mask) {
				kind := b.up
				if e.Buttons&b.mask != 0 {
					kind = b.down
				}
				if C.bwMouse(C.int(kind), C.int(b.button), C.double(n.x), C.double(n.y), C.ulonglong(n.flags())) == 0 {
					return ErrDesktopInputUnknown
				}
				n.buttons = (n.buttons &^ b.mask) | (e.Buttons & b.mask)
			}
		}
		kind, button := 5, 0
		if e.Buttons&1 != 0 {
			kind = 6
		} else if e.Buttons&4 != 0 {
			kind = 7
			button = 1
		} else if e.Buttons&2 != 0 {
			kind = 27
			button = 2
		}
		if C.bwMouse(C.int(kind), C.int(button), C.double(n.x), C.double(n.y), C.ulonglong(n.flags())) == 0 {
			return ErrDesktopInputUnknown
		}
	}
	return nil
}
func (n *macDesktopInput) ReleaseAll(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	var failed bool
	for k, c := range n.keys {
		delete(n.keys, k)
		if C.bwKey(C.int(c), 0, C.ulonglong(n.flags())) == 0 {
			n.keys[k] = c
			failed = true
		}
	}
	for _, b := range []struct {
		mask       uint8
		button, up int
	}{{1, 0, 2}, {2, 2, 26}, {4, 1, 4}} {
		if n.buttons&b.mask != 0 {
			if C.bwMouse(C.int(b.up), C.int(b.button), C.double(n.x), C.double(n.y), 0) == 0 {
				failed = true
			} else {
				n.buttons &^= b.mask
			}
		}
	}
	if failed {
		return ErrDesktopInputUnknown
	}
	return nil
}
