package main

// Minimal Wayland client for the virtual-keyboard-unstable-v1 protocol.
//
// The tool used to shell out to wtype 0.4, which assigns each distinct
// character the next physical keycode starting at Escape. Text with more than
// a dozen distinct characters therefore lands characters on Backspace/Tab/
// Control positions, and Chromium (unlike most toolkits) falls back to the
// physical meaning of those scancodes, deleting or dropping the output. This
// client instead puts characters only on the printable section of the
// keyboard (see typer.go). The wire handling below is deliberately limited to
// the handful of messages that need.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Object ids, request opcodes and event opcodes of the two protocols used
// (wayland.xml and virtual-keyboard-unstable-v1.xml).
const (
	wlDisplayObjectID = 1

	displayOpSync        = 0
	displayOpGetRegistry = 1

	displayEvError    = 0
	displayEvDeleteID = 1

	registryOpBind         = 0
	registryEvGlobal       = 0
	registryEvGlobalRemove = 1

	callbackEvDone = 0

	vkManagerOpCreate = 0

	vkOpKeymap  = 0
	vkOpKey     = 1
	vkOpDestroy = 3

	vkKeymapFormatXKBV1 = 1
	vkKeyStateReleased  = 0
	vkKeyStatePressed   = 1
)

const (
	ifaceSeat      = "wl_seat"
	ifaceVKManager = "zwp_virtual_keyboard_manager_v1"
)

// wlIOTimeout bounds every single socket operation; the overall session
// timeout is governed by the caller's context instead.
const wlIOTimeout = 10 * time.Second

var errShortMessage = errors.New("truncated wayland event")

// virtualKeyboard is a short-lived Wayland connection used to inject key
// events into the compositor as if they came from a physical keyboard.
type virtualKeyboard struct {
	conn   *net.UnixConn
	buf    []byte
	nextID uint32
	start  time.Time

	registryID uint32

	seatGlobalName uint32
	seatGlobalVer  uint32
	mgrGlobalName  uint32

	seatID uint32
	mgrID  uint32
	vkID   uint32
}

// waylandSocketPath resolves $WAYLAND_DISPLAY against $XDG_RUNTIME_DIR, the
// same way libwayland does. An absolute $WAYLAND_DISPLAY is used as-is, and
// an unset one falls back to "wayland-0".
func waylandSocketPath() (string, error) {
	name := os.Getenv("WAYLAND_DISPLAY")
	if name == "" {
		name = "wayland-0"
	}
	if strings.HasPrefix(name, "/") {
		return name, nil
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", errors.New("XDG_RUNTIME_DIR is not set")
	}
	return filepath.Join(dir, name), nil
}

// openVirtualKeyboard connects to the compositor and binds everything needed
// to type: the registry, a seat and a fresh zwp_virtual_keyboard_v1 object.
func openVirtualKeyboard() (*virtualKeyboard, error) {
	path, err := waylandSocketPath()
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", path, err)
	}
	k := &virtualKeyboard{conn: conn, nextID: 2, start: time.Now()}

	k.registryID = k.allocID()
	if err := k.send(wlDisplayObjectID, displayOpGetRegistry, le32(k.registryID)); err != nil {
		conn.Close()
		return nil, err
	}
	if err := k.roundtrip(); err != nil {
		conn.Close()
		return nil, err
	}

	if k.mgrGlobalName == 0 {
		conn.Close()
		return nil, errors.New("compositor does not support the virtual keyboard protocol")
	}
	if k.seatGlobalName == 0 {
		conn.Close()
		return nil, errors.New("no wl_seat found")
	}

	k.seatID = k.allocID()
	if err := k.bind(k.seatGlobalName, ifaceSeat, minU32(k.seatGlobalVer, 7), k.seatID); err != nil {
		conn.Close()
		return nil, err
	}
	k.mgrID = k.allocID()
	if err := k.bind(k.mgrGlobalName, ifaceVKManager, 1, k.mgrID); err != nil {
		conn.Close()
		return nil, err
	}

	k.vkID = k.allocID()
	if err := k.send(k.mgrID, vkManagerOpCreate, le32(k.seatID, k.vkID)); err != nil {
		conn.Close()
		return nil, err
	}
	return k, nil
}

// Close destroys the virtual keyboard and drops the connection.
func (k *virtualKeyboard) Close() {
	if k.vkID != 0 {
		_ = k.send(k.vkID, vkOpDestroy, nil)
	}
	_ = k.conn.Close()
}

func (k *virtualKeyboard) allocID() uint32 {
	id := k.nextID
	k.nextID++
	return id
}

func (k *virtualKeyboard) bind(name uint32, iface string, version, id uint32) error {
	payload := append(le32(name), wlStr(iface)...)
	payload = append(payload, le32(version, id)...)
	return k.send(k.registryID, registryOpBind, payload)
}

// uploadKeymap hands an XKB keymap string to the compositor and waits until
// it has been processed, so that key events sent afterwards resolve against
// the new map.
func (k *virtualKeyboard) uploadKeymap(keymap string) error {
	f, err := os.CreateTemp("", "doubao-keymap-*")
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()
	// The compositor mmaps the file and parses it as a NUL-terminated string.
	if _, err := f.WriteString(keymap + "\x00"); err != nil {
		return err
	}
	// Wire order is format, fd, size; the fd travels in ancillary data.
	payload := le32(vkKeymapFormatXKBV1, uint32(len(keymap)+1))
	if err := k.sendWithFD(k.vkID, vkOpKeymap, payload, int(f.Fd())); err != nil {
		return err
	}
	return k.roundtrip()
}

// keyPress/keyRelease emit key events; key codes are evdev scancodes.
func (k *virtualKeyboard) keyPress(code uint32) error {
	return k.send(k.vkID, vkOpKey, le32(k.timestamp(), code, vkKeyStatePressed))
}

func (k *virtualKeyboard) keyRelease(code uint32) error {
	return k.send(k.vkID, vkOpKey, le32(k.timestamp(), code, vkKeyStateReleased))
}

func (k *virtualKeyboard) timestamp() uint32 {
	return uint32(time.Since(k.start).Milliseconds())
}

// roundtrip sends wl_display.sync and reads events until the compositor has
// confirmed it processed every request sent so far.
func (k *virtualKeyboard) roundtrip() error {
	cb := k.allocID()
	if err := k.send(wlDisplayObjectID, displayOpSync, le32(cb)); err != nil {
		return err
	}
	for {
		obj, op, payload, err := k.recvMessage()
		if err != nil {
			return err
		}
		switch {
		case obj == wlDisplayObjectID && op == displayEvError:
			_, off, err := readU32(payload, 0) // offending object id
			if err != nil {
				return err
			}
			code, off, err := readU32(payload, off)
			if err != nil {
				return err
			}
			msg, _, err := readString(payload, off)
			if err != nil {
				return err
			}
			return fmt.Errorf("compositor error %d: %s", code, msg)
		case obj == wlDisplayObjectID && op == displayEvDeleteID:
			continue
		case obj == k.registryID && op == registryEvGlobal:
			name, off, err := readU32(payload, 0)
			if err != nil {
				return err
			}
			iface, off, err := readString(payload, off)
			if err != nil {
				return err
			}
			version, _, err := readU32(payload, off)
			if err != nil {
				return err
			}
			switch iface {
			case ifaceSeat:
				if k.seatGlobalName == 0 {
					k.seatGlobalName, k.seatGlobalVer = name, version
				}
			case ifaceVKManager:
				if k.mgrGlobalName == 0 {
					k.mgrGlobalName = name
				}
			}
		case obj == cb && op == callbackEvDone:
			return nil
		default:
			// wl_seat capabilities, global_remove, ... are irrelevant here.
		}
	}
}

// send writes one protocol message. The compositor must be reachable; a
// per-operation deadline keeps a wedged server from hanging the daemon.
func (k *virtualKeyboard) send(obj uint32, op uint16, payload []byte) error {
	return k.writeMsg(obj, op, payload, -1)
}

func (k *virtualKeyboard) sendWithFD(obj uint32, op uint16, payload []byte, fd int) error {
	return k.writeMsg(obj, op, payload, fd)
}

func (k *virtualKeyboard) writeMsg(obj uint32, op uint16, payload []byte, fd int) error {
	msg := marshalMessage(obj, op, payload)
	if err := k.conn.SetWriteDeadline(time.Now().Add(wlIOTimeout)); err != nil {
		return err
	}
	if fd >= 0 {
		oob := syscall.UnixRights(fd)
		n, oobn, err := k.conn.WriteMsgUnix(msg, oob, nil)
		if err != nil {
			return err
		}
		if oobn != len(oob) {
			return errors.New("wayland: control message truncated")
		}
		msg = msg[n:]
	}
	for len(msg) > 0 {
		n, err := k.conn.Write(msg)
		if err != nil {
			return err
		}
		msg = msg[n:]
	}
	return nil
}

// recvMessage returns the next complete protocol message, reading from the
// socket as needed. File descriptors that arrive unrequested are closed.
func (k *virtualKeyboard) recvMessage() (uint32, uint16, []byte, error) {
	for {
		if len(k.buf) >= 8 {
			// The header is the object id followed by an opcode (u16) and
			// the total message size (u16), both little-endian.
			size := int(binary.LittleEndian.Uint32(k.buf[4:]) >> 16)
			if size < 8 {
				return 0, 0, nil, fmt.Errorf("wayland: malformed message (size %d)", size)
			}
			if size <= len(k.buf) {
				obj := binary.LittleEndian.Uint32(k.buf[0:4])
				op := binary.LittleEndian.Uint16(k.buf[4:6])
				payload := k.buf[8:size]
				k.buf = k.buf[size:]
				return obj, op, payload, nil
			}
		}
		if err := k.conn.SetReadDeadline(time.Now().Add(wlIOTimeout)); err != nil {
			return 0, 0, nil, err
		}
		chunk := make([]byte, 4096)
		oob := make([]byte, 256)
		n, oobn, _, _, err := k.conn.ReadMsgUnix(chunk, oob)
		if err != nil {
			return 0, 0, nil, err
		}
		if oobn > 0 {
			closeReceivedFDs(oob[:oobn])
		}
		k.buf = append(k.buf, chunk[:n]...)
	}
}

// closeReceivedFDs closes any file descriptors that arrived in ancillary
// data; this client does not expect any.
func closeReceivedFDs(oob []byte) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return
	}
	for _, m := range msgs {
		fds, err := syscall.ParseUnixRights(&m)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			syscall.Close(fd)
		}
	}
}

// marshalMessage encodes the wayland wire format: a little-endian u32 object
// id followed by a u32 that packs the message size into its upper 16 bits
// and the opcode into its lower 16 bits.
func marshalMessage(obj uint32, op uint16, payload []byte) []byte {
	size := 8 + len(payload)
	msg := make([]byte, size)
	binary.LittleEndian.PutUint32(msg[0:4], obj)
	binary.LittleEndian.PutUint32(msg[4:8], uint32(size)<<16|uint32(op))
	copy(msg[8:], payload)
	return msg
}

// le32 encodes u32 arguments in wire order.
func le32(vals ...uint32) []byte {
	out := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint32(out[4*i:], v)
	}
	return out
}

// wlStr encodes a wayland string: a u32 byte length including the trailing
// NUL, the bytes themselves and padding to a 4-byte boundary.
func wlStr(s string) []byte {
	n := len(s) + 1
	out := make([]byte, 4+((n+3)&^3))
	binary.LittleEndian.PutUint32(out, uint32(n))
	copy(out[4:], s)
	return out
}

func readU32(p []byte, off int) (uint32, int, error) {
	if off+4 > len(p) {
		return 0, 0, errShortMessage
	}
	return binary.LittleEndian.Uint32(p[off:]), off + 4, nil
}

func readString(p []byte, off int) (string, int, error) {
	if off+4 > len(p) {
		return "", 0, errShortMessage
	}
	n := int(binary.LittleEndian.Uint32(p[off:]))
	off += 4
	if n < 1 || off+n > len(p) {
		return "", 0, errShortMessage
	}
	s := string(p[off : off+n-1])
	return s, off + (n+3)&^3, nil
}

func minU32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}
