// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build windows

package main

import (
	_ "embed"
	"encoding/binary"
	"log/slog"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
)

//go:embed tray_icon.ico
var trayIconBytes []byte

var (
	moduser32   = windows.NewLazySystemDLL("user32.dll")
	modshell32  = windows.NewLazySystemDLL("shell32.dll")
	modkernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW         = moduser32.NewProc("RegisterClassExW")
	procUnregisterClassW         = moduser32.NewProc("UnregisterClassW")
	procCreateWindowExW          = moduser32.NewProc("CreateWindowExW")
	procDefWindowProcW           = moduser32.NewProc("DefWindowProcW")
	procDestroyWindow            = moduser32.NewProc("DestroyWindow")
	procPostQuitMessage          = moduser32.NewProc("PostQuitMessage")
	procPostMessageW             = moduser32.NewProc("PostMessageW")
	procGetMessageW              = moduser32.NewProc("GetMessageW")
	procTranslateMessage         = moduser32.NewProc("TranslateMessage")
	procDispatchMessageW         = moduser32.NewProc("DispatchMessageW")
	procCreatePopupMenu          = moduser32.NewProc("CreatePopupMenu")
	procAppendMenuW              = moduser32.NewProc("AppendMenuW")
	procTrackPopupMenu           = moduser32.NewProc("TrackPopupMenu")
	procDestroyMenu              = moduser32.NewProc("DestroyMenu")
	procGetCursorPos             = moduser32.NewProc("GetCursorPos")
	procSetForegroundWindow      = moduser32.NewProc("SetForegroundWindow")
	procCreateIconFromResourceEx = moduser32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon              = moduser32.NewProc("DestroyIcon")
	procLoadIconW                = moduser32.NewProc("LoadIconW")
	procGetModuleHandleW         = modkernel32.NewProc("GetModuleHandleW")
	procShell_NotifyIconW        = modshell32.NewProc("Shell_NotifyIconW")
)

const (
	wmClose         = 0x0010
	wmDestroy       = 0x0002
	wmCommand       = 0x0111
	wmTrayCallback  = 0x8000 + 1 // WM_APP + 1
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmContextMenu   = 0x007B

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	mfString    = 0x00000000
	mfSeparator = 0x00000800

	tpmRightButton = 0x0002

	idConfig = 1001
	idExit   = 1002
)

type point struct {
	x, y int32
}

type msg struct {
	hwnd    windows.HWND
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type notifyIconData struct {
	cbSize            uint32
	_                 uint32
	hWnd              windows.HWND
	uID               uint32
	uFlags            uint32
	uCallbackMessage  uint32
	_                 uint32
	hIcon             windows.Handle
	szTip             [128]uint16
	dwState           uint32
	dwStateMask       uint32
	szInfo            [256]uint16
	uTimeoutOrVersion uint32
	szInfoTitle       [64]uint16
	dwInfoFlags       uint32
	guidItem          windows.GUID
	hBalloonIcon      windows.Handle
}

var (
	trayAppMu sync.Mutex
	trayApp   *syncthing.App
)

func loadTrayIcon() windows.Handle {
	if len(trayIconBytes) >= 22 {
		size := binary.LittleEndian.Uint32(trayIconBytes[14:18])
		offset := binary.LittleEndian.Uint32(trayIconBytes[18:22])
		if int(offset+size) <= len(trayIconBytes) {
			h, _, _ := procCreateIconFromResourceEx.Call(
				uintptr(unsafe.Pointer(&trayIconBytes[offset])),
				uintptr(size),
				1,          // TRUE (fIcon)
				0x00030000, // dwVersion
				0, 0,       // cxDesired, cyDesired
				0x00000040, // LR_DEFAULTSIZE
			)
			if h != 0 {
				return windows.Handle(h)
			}
		}
	}
	// Fallback to standard application icon
	h, _, _ := procLoadIconW.Call(0, 32512) // IDI_APPLICATION
	return windows.Handle(h)
}

func trayWndProc(hWnd windows.HWND, uMsg uint32, wParam uintptr, lParam uintptr) uintptr {
	switch uMsg {
	case wmTrayCallback:
		switch lParam {
		case wmLButtonUp, wmLButtonDblClk:
			openBrowserConfig()
			return 0
		case wmRButtonUp, wmContextMenu:
			showContextMenu(hWnd)
			return 0
		}

	case wmCommand:
		switch loword(wParam) {
		case idConfig:
			openBrowserConfig()
			return 0
		case idExit:
			trayAppMu.Lock()
			app := trayApp
			trayAppMu.Unlock()
			if app != nil {
				go app.Stop(svcutil.ExitSuccess)
			}
			return 0
		}

	case wmClose:
		procDestroyWindow.Call(uintptr(hWnd))
		return 0

	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(uintptr(hWnd), uintptr(uMsg), wParam, lParam)
	return ret
}

func loword(l uintptr) uint32 {
	return uint32(l & 0xFFFF)
}

func openBrowserConfig() {
	go func() {
		cmd := browserCmd{Verify: false}
		if err := cmd.Run(); err != nil {
			slog.Error("Tray: Failed to open browser", slogutil.Error(err))
		}
	}()
}

func showContextMenu(hWnd windows.HWND) {
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	configText, _ := windows.UTF16PtrFromString("Configurar")
	exitText, _ := windows.UTF16PtrFromString("Saír")

	procAppendMenuW.Call(hMenu, mfString, idConfig, uintptr(unsafe.Pointer(configText)))
	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	procAppendMenuW.Call(hMenu, mfString, idExit, uintptr(unsafe.Pointer(exitText)))

	procSetForegroundWindow.Call(uintptr(hWnd))
	procTrackPopupMenu.Call(hMenu, tpmRightButton, uintptr(pt.x), uintptr(pt.y), 0, uintptr(hWnd), 0)
}

func startTray(app *syncthing.App, _ config.Wrapper) func() {
	trayAppMu.Lock()
	trayApp = app
	trayAppMu.Unlock()

	className, _ := windows.UTF16PtrFromString("SyncthingTrayClass")
	windowName, _ := windows.UTF16PtrFromString("Syncthing Tray")

	ready := make(chan windows.HWND)
	stopped := make(chan struct{})

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(stopped)

		hInst, _, _ := procGetModuleHandleW.Call(0)
		hIcon := loadTrayIcon()
		defer procDestroyIcon.Call(uintptr(hIcon))

		wc := wndClassEx{
			cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
			lpfnWndProc:   windows.NewCallback(trayWndProc),
			hInstance:     windows.Handle(hInst),
			hIcon:         hIcon,
			lpszClassName: className,
		}

		atom, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		if atom == 0 {
			slog.Debug("Tray: Failed to register window class")
			close(ready)
			return
		}
		defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(className)), hInst)

		hWnd, _, _ := procCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(className)),
			uintptr(unsafe.Pointer(windowName)),
			0, // WS_OVERLAPPED
			0, 0, 0, 0,
			0, 0, hInst, 0,
		)

		if hWnd == 0 {
			slog.Debug("Tray: Failed to create tray window")
			close(ready)
			return
		}

		nid := notifyIconData{
			cbSize:           uint32(unsafe.Sizeof(notifyIconData{})),
			hWnd:             windows.HWND(hWnd),
			uID:              1,
			uFlags:           nifMessage | nifIcon | nifTip,
			uCallbackMessage: wmTrayCallback,
			hIcon:            hIcon,
		}
		copy(nid.szTip[:], windows.StringToUTF16("Syncthing"))

		res, _, _ := procShell_NotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
		if res == 0 {
			slog.Debug("Tray: Failed to add notification icon (headless or non-interactive session)")
			procDestroyWindow.Call(hWnd)
			close(ready)
			return
		}
		defer procShell_NotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))

		ready <- windows.HWND(hWnd)

		var m msg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if r == 0 || int32(r) == -1 {
				break
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()

	hWnd, ok := <-ready
	if !ok || hWnd == 0 {
		return func() {}
	}

	return func() {
		procPostMessageW.Call(uintptr(hWnd), wmClose, 0, 0)
		<-stopped
	}
}
