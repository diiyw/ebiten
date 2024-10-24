// Copyright 2015 Hajime Hoshi
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build wechat

package ui

import (
	"errors"
	"math"
	"sync"
	"syscall/js"
	"time"

	"github.com/hajimehoshi/ebiten/v2/internal/gamepad"
	"github.com/hajimehoshi/ebiten/v2/internal/graphicsdriver"
	"github.com/hajimehoshi/ebiten/v2/internal/graphicsdriver/opengl"
	"github.com/hajimehoshi/ebiten/v2/internal/hook"
)

type graphicsDriverCreatorImpl struct {
	canvas     js.Value
	colorSpace graphicsdriver.ColorSpace
}

func (g *graphicsDriverCreatorImpl) newAuto() (graphicsdriver.Graphics, GraphicsLibrary, error) {
	graphics, err := g.newOpenGL()
	return graphics, GraphicsLibraryOpenGL, err
}

func (g *graphicsDriverCreatorImpl) newOpenGL() (graphicsdriver.Graphics, error) {
	return opengl.NewGraphics(g.canvas, g.colorSpace)
}

func (*graphicsDriverCreatorImpl) newDirectX() (graphicsdriver.Graphics, error) {
	return nil, errors.New("ui: DirectX is not supported in this environment")
}

func (*graphicsDriverCreatorImpl) newMetal() (graphicsdriver.Graphics, error) {
	return nil, errors.New("ui: Metal is not supported in this environment")
}

func (*graphicsDriverCreatorImpl) newPlayStation5() (graphicsdriver.Graphics, error) {
	return nil, errors.New("ui: PlayStation 5 is not supported in this environment")
}

type userInterfaceImpl struct {
	graphicsDriver graphicsdriver.Graphics

	runnableOnUnfocused bool
	fpsMode             FPSModeType
	renderingScheduled  bool
	cursorMode          CursorMode
	cursorPrevMode      CursorMode
	captureCursorLater  bool
	cursorShape         CursorShape
	onceUpdateCalled    bool
	lastCaptureExitTime time.Time
	hiDPIEnabled        bool

	context                   *context
	inputState                InputState
	keyDurationsByKeyProperty map[Key]int
	cursorXInClient           float64
	cursorYInClient           float64
	origCursorXInClient       float64
	origCursorYInClient       float64
	touchesInClient           []touchInClient

	savedCursorX              float64
	savedCursorY              float64
	savedOutsideWidth         float64
	savedOutsideHeight        float64
	outsideSizeUnchangedCount int

	keyboardLayoutMap js.Value

	m         sync.Mutex
	dropFileM sync.Mutex
}

var (
	wx                    = js.Global().Get("wx")
	systemInfoSync        = wx.Call("getSystemInfoSync")
	canvas                js.Value
	requestAnimationFrame = js.Global().Get("requestAnimationFrame")
	setTimeout            = js.Global().Get("setTimeout")
)

func (u *UserInterface) SetFullscreen(fullscreen bool) {
	// Default is fullscreen.
	return
}

func (u *UserInterface) IsFullscreen() bool {
	return true
}

func (u *UserInterface) IsFocused() bool {
	return u.isFocused()
}

func (u *UserInterface) SetRunnableOnUnfocused(runnableOnUnfocused bool) {
	u.runnableOnUnfocused = runnableOnUnfocused
}

func (u *UserInterface) IsRunnableOnUnfocused() bool {
	return u.runnableOnUnfocused
}

func (u *UserInterface) FPSMode() FPSModeType {
	return u.fpsMode
}

func (u *UserInterface) SetFPSMode(mode FPSModeType) {
	u.fpsMode = mode
}

func (u *UserInterface) ScheduleFrame() {
	u.renderingScheduled = true
}

func (u *UserInterface) CursorMode() CursorMode {
	return u.cursorMode
}

func (u *UserInterface) SetCursorMode(mode CursorMode) {
	return
}

func (u *UserInterface) setCursorMode(mode CursorMode) {
	return
}

func (u *UserInterface) recoverCursorMode() {
	return
}

func (u *UserInterface) CursorShape() CursorShape {
	return u.cursorShape
}

func (u *UserInterface) SetCursorShape(shape CursorShape) {
	return
}

func (u *UserInterface) outsideSize() (float64, float64) {
	cWidth := systemInfoSync.Get("windowWidth").Float()
	cHeight := systemInfoSync.Get("windowHeight").Float()
	scale := theMonitor.DeviceScaleFactor()
	width := dipFromNativePixels(cWidth, scale)
	height := dipFromNativePixels(cHeight, scale)
	return width, height
}

func (u *UserInterface) suspended() bool {
	if u.runnableOnUnfocused {
		return false
	}
	return !u.isFocused()
}

func (u *UserInterface) isFocused() bool {
	return true
}

// canCaptureCursor reports whether a cursor can be captured or not now.
// Just after escaping from a capture, a browser might not be able to capture a cursor (#2693).
// If it is too early to capture a cursor, Ebitengine tries to delay it.
//
// See also https://w3c.github.io/pointerlock/#extensions-to-the-element-interface
//
// > Pointer lock is a transient activation-gated API, therefore a requestPointerLock() call
// > MUST fail if the relevant global object of this does not have transient activation.
// > This prevents locking upon initial navigation or re-acquiring lock without user's attention.
func (u *UserInterface) canCaptureCursor() bool {
	// 1.5 [sec] seems enough in the real world.
	return time.Now().Sub(u.lastCaptureExitTime) >= 1500*time.Millisecond
}

func (u *UserInterface) update() error {
	if u.captureCursorLater && u.canCaptureCursor() {
		u.setCursorMode(CursorModeCaptured)
	}

	if u.suspended() {
		return hook.SuspendAudio()
	}
	if err := hook.ResumeAudio(); err != nil {
		return err
	}
	return u.updateImpl(false)
}

func (u *UserInterface) updateImpl(force bool) error {
	// Guard updateImpl as this function cannot be invoked until this finishes (#2339).
	u.m.Lock()
	defer u.m.Unlock()

	// context can be nil when an event is fired but the loop doesn't start yet (#1928).
	if u.context == nil {
		return nil
	}

	if err := gamepad.Update(); err != nil {
		return err
	}

	// TODO: If DeviceScaleFactor changes, call updateScreenSize.
	// Now there is not a good way to detect the change.
	// See also https://crbug.com/123694.

	w, h := u.outsideSize()
	if force {
		if err := u.context.forceUpdateFrame(u.graphicsDriver, w, h, theMonitor.DeviceScaleFactor(), u); err != nil {
			return err
		}
	} else {
		if err := u.context.updateFrame(u.graphicsDriver, w, h, theMonitor.DeviceScaleFactor(), u); err != nil {
			return err
		}
	}
	return nil
}

func (u *UserInterface) needsUpdate() bool {
	if u.fpsMode != FPSModeVsyncOffMinimum {
		return true
	}
	if !u.onceUpdateCalled {
		return true
	}
	if u.renderingScheduled {
		return true
	}
	// TODO: Watch the gamepad state?
	return false
}

func (u *UserInterface) loopGame() error {
	// Initialize the screen size first (#3034).
	// If ebiten.SetRunnableOnUnfocused(false) and the canvas is not focused,
	// suspended() returns true and the update routine cannot start.
	u.updateScreenSize()

	errCh := make(chan error, 1)
	reqStopAudioCh := make(chan struct{})
	resStopAudioCh := make(chan struct{})

	var cf js.Func
	f := func() {
		if err := u.error(); err != nil {
			errCh <- err
			return
		}
		if u.needsUpdate() {
			defer func() {
				u.onceUpdateCalled = true
			}()
			u.renderingScheduled = false
			if err := u.update(); err != nil {
				close(reqStopAudioCh)
				<-resStopAudioCh

				errCh <- err
				return
			}
		}
		switch u.fpsMode {
		case FPSModeVsyncOn:
			requestAnimationFrame.Invoke(cf)
		case FPSModeVsyncOffMaximum:
			setTimeout.Invoke(cf, 0)
		case FPSModeVsyncOffMinimum:
			requestAnimationFrame.Invoke(cf)
		}
	}

	// TODO: Should cf be released after the game ends?
	cf = js.FuncOf(func(this js.Value, args []js.Value) any {
		// f can be blocked but callbacks must not be blocked. Create a goroutine (#1161).
		go f()
		return nil
	})

	// Call f asyncly since ch is used in f.
	go f()

	// Run another loop to watch suspended() as the above update function is never called when the tab is hidden.
	// To check the document's visibility, visibilitychange event should usually be used. However, this event is
	// not reliable and sometimes it is not fired (#961). Then, watch the state regularly instead.
	go func() {
		defer close(resStopAudioCh)

		const interval = 100 * time.Millisecond
		t := time.NewTicker(interval)
		defer func() {
			t.Stop()

			// This is a dirty hack. (*time.Ticker).Stop() just marks the timer 'deleted' [1] and
			// something might run even after Stop. On Wasm, this causes an issue to execute Go program
			// even after finishing (#1027). Sleep for the interval time duration to ensure that
			// everything related to the timer is finished.
			//
			// [1] runtime.deltimer
			time.Sleep(interval)
		}()

		for {
			select {
			case <-t.C:
				if u.suspended() {
					if err := hook.SuspendAudio(); err != nil {
						errCh <- err
						return
					}
				} else {
					if err := hook.ResumeAudio(); err != nil {
						errCh <- err
						return
					}
				}
			case <-reqStopAudioCh:
				return
			}
		}
	}()

	return <-errCh
}

func (u *UserInterface) init() error {
	u.userInterfaceImpl = userInterfaceImpl{
		runnableOnUnfocused: true,
		savedCursorX:        math.NaN(),
		savedCursorY:        math.NaN(),
		hiDPIEnabled:        true,
	}
	canvas = wx.Call("createCanvas")
	u.setCanvasEventHandlers(canvas)
	return nil
}

func (u *UserInterface) setWindowEventHandlers(v js.Value) {

}

func (u *UserInterface) setCanvasEventHandlers(v js.Value) {
	// Keyboard
	wx.Call("onKeyDown", js.FuncOf(func(this js.Value, args []js.Value) any {
		e := args[0]
		if err := u.updateInputFromEvent(e); err != nil {
			u.setError(err)
			return nil
		}
		return nil
	}))
	wx.Call("onKeyUp", js.FuncOf(func(this js.Value, args []js.Value) any {
		e := args[0]
		if err := u.updateInputFromEvent(e); err != nil {
			u.setError(err)
			return nil
		}
		return nil
	}))

	// Mouse
	wx.Call("onMouseDown", js.FuncOf(func(this js.Value, args []js.Value) any {
		e := args[0]
		if err := u.updateInputFromEvent(e); err != nil {
			u.setError(err)
			return nil
		}
		return nil
	}))
	wx.Call("onMouseUp", js.FuncOf(func(this js.Value, args []js.Value) any {
		e := args[0]
		if err := u.updateInputFromEvent(e); err != nil {
			u.setError(err)
			return nil
		}
		return nil
	}))
	wx.Call("onMouseMove", js.FuncOf(func(this js.Value, args []js.Value) any {
		e := args[0]
		if err := u.updateInputFromEvent(e); err != nil {
			u.setError(err)
			return nil
		}
		return nil
	}))
	wx.Call("onWheel", js.FuncOf(func(this js.Value, args []js.Value) any {
		e := args[0]
		if err := u.updateInputFromEvent(e); err != nil {
			u.setError(err)
			return nil
		}
		return nil
	}))

	// Touch
	wx.Call("onTouchStart", js.FuncOf(func(this js.Value, args []js.Value) any {
		e := args[0]
		if err := u.updateInputFromEvent(e); err != nil {
			u.setError(err)
			return nil
		}
		return nil
	}))
	wx.Call("onTouchEnd", js.FuncOf(func(this js.Value, args []js.Value) any {
		e := args[0]
		if err := u.updateInputFromEvent(e); err != nil {
			u.setError(err)
			return nil
		}
		return nil
	}))
	wx.Call("onTouchMove", js.FuncOf(func(this js.Value, args []js.Value) any {
		e := args[0]
		if err := u.updateInputFromEvent(e); err != nil {
			u.setError(err)
			return nil
		}
		return nil
	}))
}

func (u *UserInterface) appendDroppedFiles(data js.Value) {
	return
}

func (u *UserInterface) forceUpdateOnMinimumFPSMode() {
	if u.fpsMode != FPSModeVsyncOffMinimum {
		return
	}

	// updateImpl can block. Use goroutine.
	// See https://pkg.go.dev/syscall/js#FuncOf.
	go func() {
		if err := u.updateImpl(true); err != nil {
			u.setError(err)
		}
	}()
}

func (u *UserInterface) shouldFocusFirst(options *RunOptions) bool {
	return true
}

func (u *UserInterface) initOnMainThread(options *RunOptions) error {
	u.setRunning(true)

	u.hiDPIEnabled = !options.DisableHiDPI

	g, lib, err := newGraphicsDriver(&graphicsDriverCreatorImpl{
		canvas:     canvas,
		colorSpace: options.ColorSpace,
	}, options.GraphicsLibrary)
	if err != nil {
		return err
	}
	u.graphicsDriver = g
	u.setGraphicsLibrary(lib)

	return nil
}

func (u *UserInterface) updateScreenSize() {
	return
}

func (u *UserInterface) readInputState(inputState *InputState) {
	u.inputState.copyAndReset(inputState)
	u.keyboardLayoutMap = js.Value{}
}

func (u *UserInterface) Window() Window {
	return &nullWindow{}
}

type Monitor struct {
	deviceScaleFactor float64
}

var theMonitor = &Monitor{}

func (m *Monitor) Name() string {
	return "WeChat"
}

func (m *Monitor) DeviceScaleFactor() float64 {
	if m.deviceScaleFactor != 0 {
		return m.deviceScaleFactor
	}
	m.deviceScaleFactor = systemInfoSync.Get("pixelRatio").Float()
	return m.deviceScaleFactor
}

func (m *Monitor) Size() (int, int) {
	return systemInfoSync.Get("windowWidth").Int(), systemInfoSync.Get("windowHeight").Int()
}

func (u *UserInterface) AppendMonitors(mons []*Monitor) []*Monitor {
	return append(mons, theMonitor)
}

func (u *UserInterface) Monitor() *Monitor {
	return theMonitor
}

func (u *UserInterface) updateIconIfNeeded() error {
	return nil
}

func IsScreenTransparentAvailable() bool {
	return true
}

func dipToNativePixels(x float64, scale float64) float64 {
	return x * scale
}

func dipFromNativePixels(x float64, scale float64) float64 {
	return x / scale
}
