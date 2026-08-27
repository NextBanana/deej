package deej

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/win"
	"go.uber.org/zap"
)

// win32 bits that lxn/win doesn't expose, bound lazily. user32 and gdi32 are
// already loaded into every GUI process, so this doesn't cost a real load
var (
	osdUser32 = syscall.NewLazyDLL("user32.dll")
	osdGdi32  = syscall.NewLazyDLL("gdi32.dll")

	procSetLayeredWindowAttributes = osdUser32.NewProc("SetLayeredWindowAttributes")
	procSetWindowRgn               = osdUser32.NewProc("SetWindowRgn")
	procFillRect                   = osdUser32.NewProc("FillRect")
	procCreateSolidBrush           = osdGdi32.NewProc("CreateSolidBrush")
	procCreateRoundRectRgn         = osdGdi32.NewProc("CreateRoundRectRgn")
)

const (
	osdClassName  = "DeejOSDWindow"
	osdWindowName = "deej overlay"

	// SPI_GETWORKAREA gives us the primary monitor's work area, which already
	// excludes the taskbar - no monitor enumeration needed
	spiGetWorkArea = 0x0030
	lwaAlpha       = 0x00000002

	// our own messages. WM_APP is the first value windows reserves for apps
	wmOSDUpdate = win.WM_APP + 1
	wmOSDQuit   = win.WM_APP + 2

	osdTimerID       = 1
	osdTimerInterval = 16 // ms, ~60fps while visible - the timer is killed when hidden

	// layout, in logical pixels at 96 dpi. everything gets scaled from here
	osdWidth      = 320
	osdPadding    = 14
	osdRowHeight  = 46
	osdRowGap     = 6
	osdBarHeight  = 6
	osdCornerRad  = 10
	osdLabelPtSz  = 10
	osdValuePtSz  = 9
	osdMinBarSize = 2
)

// theme. the dimmed variants are used for rows whose target isn't running
var (
	osdColorBackground = win.RGB(32, 32, 32)
	osdColorText       = win.RGB(255, 255, 255)
	osdColorTextDim    = win.RGB(138, 138, 138)
	osdColorBarTrack   = win.RGB(72, 72, 72)
	osdColorBarFill    = win.RGB(255, 255, 255)
	osdColorBarDim     = win.RGB(112, 112, 112)
)

// the window procedure is a package-level callback, so it needs a way back to
// the instance. deej only ever creates one overlay, which makes this safe
var activeOSD *windowsOSD

var osdWndProcCallback = syscall.NewCallback(osdWndProc)

type osdEntryState struct {
	entry     OSDEntry
	expiresAt time.Time
}

// WindowsOSD renders the overlay as a click-through layered window
type WindowsOSD struct {
	deej   *Deej
	logger *zap.SugaredLogger

	// guards entries, which is written from deej's goroutines and read by the ui thread
	mutex   sync.Mutex
	entries map[int]*osdEntryState

	// stored separately so ShowEntry can post to it without touching the ui thread
	hwnd atomic.Uintptr

	// everything below is only ever touched on the ui thread
	brushes    map[win.COLORREF]win.HBRUSH
	fontLabel  win.HFONT
	fontValue  win.HFONT
	fontScale  float64
	visible    bool
	fadingOut  bool
	alpha      float64
	lastRows   int
	classAtom  win.ATOM
	hInstance  win.HINSTANCE
	classNameW *uint16
}

// alias so the rest of the package can stay platform-neutral
type windowsOSD = WindowsOSD

// newOSD creates the Windows overlay implementation
func newOSD(deej *Deej, logger *zap.SugaredLogger) (OSD, error) {
	logger = logger.Named("osd")

	o := &WindowsOSD{
		deej:    deej,
		logger:  logger,
		entries: make(map[int]*osdEntryState),
		brushes: make(map[win.COLORREF]win.HBRUSH),
	}

	logger.Debug("Created Windows OSD instance")

	return o, nil
}

// Start creates the overlay window on a dedicated thread and pumps its messages
func (o *WindowsOSD) Start() error {
	ready := make(chan error, 1)

	go o.uiThread(ready)

	return <-ready
}

// ShowEntry records a row and wakes the ui thread to draw it
func (o *WindowsOSD) ShowEntry(entry OSDEntry) {
	if !o.deej.config.OSD.Enabled {
		return
	}

	timeout := time.Duration(o.deej.config.OSD.RowTimeoutMS) * time.Millisecond

	o.mutex.Lock()
	o.entries[entry.SliderID] = &osdEntryState{
		entry:     entry,
		expiresAt: time.Now().Add(timeout),
	}
	o.mutex.Unlock()

	if hwnd := win.HWND(o.hwnd.Load()); hwnd != 0 {
		win.PostMessage(hwnd, wmOSDUpdate, 0, 0)
	}
}

// Stop asks the ui thread to destroy the window and exit its message loop
func (o *WindowsOSD) Stop() {
	if hwnd := win.HWND(o.hwnd.Load()); hwnd != 0 {
		win.PostMessage(hwnd, wmOSDQuit, 0, 0)
	}
}

// uiThread owns the window for its entire lifetime. win32 windows belong to the
// thread that created them, so this goroutine must stay pinned to one os thread
func (o *WindowsOSD) uiThread(ready chan error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := o.createWindow(); err != nil {
		o.logger.Errorw("Failed to create overlay window", "error", err)
		ready <- err

		return
	}

	ready <- nil
	o.logger.Debug("Overlay window ready")

	var msg win.MSG
	for win.GetMessage(&msg, 0, 0, 0) > 0 {
		win.TranslateMessage(&msg)
		win.DispatchMessage(&msg)
	}

	o.releaseResources()
	o.logger.Debug("Overlay message loop ended")
}

func (o *WindowsOSD) createWindow() error {
	activeOSD = o

	className, err := syscall.UTF16PtrFromString(osdClassName)
	if err != nil {
		return fmt.Errorf("encode class name: %w", err)
	}

	windowName, err := syscall.UTF16PtrFromString(osdWindowName)
	if err != nil {
		return fmt.Errorf("encode window name: %w", err)
	}

	o.classNameW = className
	o.hInstance = win.GetModuleHandle(nil)

	wndClass := win.WNDCLASSEX{
		LpfnWndProc:   osdWndProcCallback,
		HInstance:     o.hInstance,
		LpszClassName: className,
	}
	wndClass.CbSize = uint32(unsafe.Sizeof(wndClass))

	o.classAtom = win.RegisterClassEx(&wndClass)
	if o.classAtom == 0 {
		return fmt.Errorf("register window class")
	}

	// WS_EX_TRANSPARENT makes it click-through, WS_EX_NOACTIVATE keeps it from ever
	// stealing focus, WS_EX_TOOLWINDOW keeps it out of alt-tab and the taskbar
	hwnd := win.CreateWindowEx(
		win.WS_EX_LAYERED|win.WS_EX_TRANSPARENT|win.WS_EX_TOOLWINDOW|
			win.WS_EX_NOACTIVATE|win.WS_EX_TOPMOST,
		className,
		windowName,
		win.WS_POPUP,
		0, 0, 1, 1,
		0, 0, o.hInstance, nil)

	if hwnd == 0 {
		return fmt.Errorf("create overlay window")
	}

	o.hwnd.Store(uintptr(hwnd))
	setLayeredWindowAttributes(hwnd, 0, 0, lwaAlpha)

	return nil
}

func osdWndProc(hwnd win.HWND, msg uint32, wParam uintptr, lParam uintptr) uintptr {
	o := activeOSD
	if o == nil {
		return win.DefWindowProc(hwnd, msg, wParam, lParam)
	}

	switch msg {
	case win.WM_PAINT:
		o.paint(hwnd)
		return 0

	case wmOSDUpdate:
		o.onUpdate(hwnd)
		return 0

	case win.WM_TIMER:
		o.onTick(hwnd)
		return 0

	case wmOSDQuit:
		win.DestroyWindow(hwnd)
		return 0

	case win.WM_DESTROY:
		win.PostQuitMessage(0)
		return 0
	}

	return win.DefWindowProc(hwnd, msg, wParam, lParam)
}

// onUpdate runs when a new or refreshed row arrives
func (o *WindowsOSD) onUpdate(hwnd win.HWND) {
	rows := o.rowCount()
	if rows == 0 {
		return
	}

	o.fadingOut = false
	o.relayout(hwnd, rows)

	if !o.visible {
		o.visible = true
		o.alpha = 0
		win.ShowWindow(hwnd, win.SW_SHOWNOACTIVATE)
		win.SetTimer(hwnd, osdTimerID, osdTimerInterval, 0)
	}

	o.applyAlpha(hwnd)
	win.InvalidateRect(hwnd, nil, false)
}

// onTick expires rows one by one and drives the fade
func (o *WindowsOSD) onTick(hwnd win.HWND) {
	rows := o.expireEntries()

	if rows == 0 {
		o.fadingOut = true
	} else if rows != o.lastRows {

		// a row dropped off while others remain - the panel has to shrink
		o.relayout(hwnd, rows)
		win.InvalidateRect(hwnd, nil, false)
	}

	fadeMS := o.deej.config.OSD.FadeMS
	step := 255.0
	if fadeMS > 0 {
		step = 255.0 * float64(osdTimerInterval) / float64(fadeMS)
	}

	if o.fadingOut {
		o.alpha -= step
	} else {
		o.alpha += step
	}

	if o.alpha > 255 {
		o.alpha = 255
	}

	if o.alpha <= 0 {
		o.alpha = 0

		if o.fadingOut {
			win.KillTimer(hwnd, osdTimerID)
			win.ShowWindow(hwnd, win.SW_HIDE)
			o.visible = false
			o.lastRows = 0

			return
		}
	}

	o.applyAlpha(hwnd)
}

// relayout resizes and repositions the panel for the given number of rows.
// the panel is anchored to its configured edge, so with a bottom position it
// grows upwards and the bottom row stays put
func (o *WindowsOSD) relayout(hwnd win.HWND, rows int) {
	scale := o.scale(hwnd)
	o.ensureFonts(scale)

	sc := func(v int) int32 { return int32(math.Round(float64(v) * scale)) }

	width := sc(osdWidth)
	height := sc(osdPadding)*2 +
		int32(rows)*sc(osdRowHeight) +
		int32(rows-1)*sc(osdRowGap)

	work := primaryWorkArea()
	offset := sc(o.deej.config.OSD.Offset)

	var x, y int32

	switch o.deej.config.OSD.Position {
	case "top-left":
		x, y = work.Left+offset, work.Top+offset
	case "top-right":
		x, y = work.Right-width-offset, work.Top+offset
	case "top-center":
		x, y = work.Left+(work.Right-work.Left-width)/2, work.Top+offset
	case "bottom-left":
		x, y = work.Left+offset, work.Bottom-height-offset
	case "bottom-right":
		x, y = work.Right-width-offset, work.Bottom-height-offset
	default: // bottom-center
		x, y = work.Left+(work.Right-work.Left-width)/2, work.Bottom-height-offset
	}

	// with many rows at a large scale the panel can outgrow the work area, and a
	// bottom anchor would then push its top edge off screen. keep it on the monitor
	if area := work.Bottom - work.Top; height > area {
		height = area
	}

	if y < work.Top {
		y = work.Top
	}

	if y+height > work.Bottom {
		y = work.Bottom - height
	}

	win.SetWindowPos(hwnd, win.HWND_TOPMOST, x, y, width, height, win.SWP_NOACTIVATE)

	// rounded corners. once the region is handed over, windows owns it -
	// deleting it here would be a use-after-free
	radius := sc(osdCornerRad) * 2
	if rgn := createRoundRectRgn(0, 0, width+1, height+1, radius, radius); rgn != 0 {
		setWindowRgn(hwnd, rgn, false)
	}

	o.lastRows = rows
}

func (o *WindowsOSD) paint(hwnd win.HWND) {
	var ps win.PAINTSTRUCT

	hdc := win.BeginPaint(hwnd, &ps)
	defer win.EndPaint(hwnd, &ps)

	var client win.RECT
	win.GetClientRect(hwnd, &client)

	width := client.Right - client.Left
	height := client.Bottom - client.Top

	if width <= 0 || height <= 0 {
		return
	}

	// double-buffered, otherwise the panel flickers on every level change
	memDC := win.CreateCompatibleDC(hdc)
	bitmap := win.CreateCompatibleBitmap(hdc, width, height)
	previous := win.SelectObject(memDC, win.HGDIOBJ(bitmap))

	defer func() {
		win.SelectObject(memDC, previous)
		win.DeleteObject(win.HGDIOBJ(bitmap))
		win.DeleteDC(memDC)
	}()

	fillRect(memDC, &client, o.brush(osdColorBackground))
	win.SetBkMode(memDC, win.TRANSPARENT)

	scale := o.scale(hwnd)
	sc := func(v int) int32 { return int32(math.Round(float64(v) * scale)) }

	// windows can send WM_PAINT before we've ever laid the panel out, so don't
	// assume the fonts already exist. this is a no-op once they do
	o.ensureFonts(scale)

	entries := o.snapshot()
	rowHeight := sc(osdRowHeight)
	rowGap := sc(osdRowGap)
	padding := sc(osdPadding)
	barHeight := sc(osdBarHeight)

	for idx, entry := range entries {
		top := padding + int32(idx)*(rowHeight+rowGap)
		left := padding
		right := width - padding

		textColor := osdColorText
		barColor := osdColorBarFill

		if !entry.Active && o.deej.config.OSD.DimInactive {
			textColor = osdColorTextDim
			barColor = osdColorBarDim
		}

		win.SetTextColor(memDC, textColor)

		// label and percentage share one line above the bar
		textRect := win.RECT{
			Left:   left,
			Top:    top,
			Right:  right,
			Bottom: top + rowHeight - barHeight - sc(6),
		}

		if o.deej.config.OSD.ShowPercentage {
			value := fmt.Sprintf("%d%%", int(math.Round(float64(entry.Percent)*100)))

			win.SelectObject(memDC, win.HGDIOBJ(o.fontValue))
			drawText(memDC, value, &textRect, win.DT_RIGHT|win.DT_SINGLELINE|win.DT_VCENTER)
		}

		win.SelectObject(memDC, win.HGDIOBJ(o.fontLabel))
		drawText(memDC, entry.Label, &textRect,
			win.DT_LEFT|win.DT_SINGLELINE|win.DT_VCENTER|win.DT_END_ELLIPSIS)

		// track, then the filled portion on top of it
		barRect := win.RECT{
			Left:   left,
			Top:    top + rowHeight - barHeight,
			Right:  right,
			Bottom: top + rowHeight,
		}
		fillRect(memDC, &barRect, o.brush(osdColorBarTrack))

		filled := int32(math.Round(float64(entry.Percent) * float64(barRect.Right-barRect.Left)))
		if filled > 0 {
			if filled < osdMinBarSize {
				filled = osdMinBarSize
			}

			fillRect(memDC, &win.RECT{
				Left:   barRect.Left,
				Top:    barRect.Top,
				Right:  barRect.Left + filled,
				Bottom: barRect.Bottom,
			}, o.brush(barColor))
		}
	}

	win.BitBlt(hdc, 0, 0, width, height, memDC, 0, 0, win.SRCCOPY)
}

// snapshot returns the current rows sorted by slider id. sorting by id rather
// than by recency keeps a given slider in a fixed place, so the panel stays readable
func (o *WindowsOSD) snapshot() []OSDEntry {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	entries := make([]OSDEntry, 0, len(o.entries))
	for _, state := range o.entries {
		entries = append(entries, state.entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].SliderID < entries[j].SliderID
	})

	return entries
}

func (o *WindowsOSD) rowCount() int {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	return len(o.entries)
}

// expireEntries drops rows whose individual timeout has passed
func (o *WindowsOSD) expireEntries() int {
	now := time.Now()

	o.mutex.Lock()
	defer o.mutex.Unlock()

	for sliderID, state := range o.entries {
		if state.expiresAt.Before(now) {
			delete(o.entries, sliderID)
		}
	}

	return len(o.entries)
}

func (o *WindowsOSD) applyAlpha(hwnd win.HWND) {
	setLayeredWindowAttributes(hwnd, 0, byte(o.alpha), lwaAlpha)
}

// scale combines the monitor's dpi with the user's configured scale. the app
// manifest declares per-monitor dpi awareness, so windows won't scale us itself
func (o *WindowsOSD) scale(hwnd win.HWND) float64 {
	dpi := win.GetDpiForWindow(hwnd)
	if dpi == 0 {
		dpi = 96
	}

	configured := o.deej.config.OSD.Scale
	if configured <= 0 {
		configured = 1
	}

	return float64(dpi) / 96.0 * configured
}

func (o *WindowsOSD) ensureFonts(scale float64) {
	if o.fontLabel != 0 && math.Abs(o.fontScale-scale) < 0.001 {
		return
	}

	o.destroyFonts()

	o.fontLabel = createOSDFont(osdLabelPtSz, scale, int32(win.FW_SEMIBOLD))
	o.fontValue = createOSDFont(osdValuePtSz, scale, int32(win.FW_NORMAL))
	o.fontScale = scale
}

func (o *WindowsOSD) destroyFonts() {
	if o.fontLabel != 0 {
		win.DeleteObject(win.HGDIOBJ(o.fontLabel))
		o.fontLabel = 0
	}

	if o.fontValue != 0 {
		win.DeleteObject(win.HGDIOBJ(o.fontValue))
		o.fontValue = 0
	}
}

// brush caches solid brushes by color so painting doesn't churn gdi objects
func (o *WindowsOSD) brush(color win.COLORREF) win.HBRUSH {
	if existing, ok := o.brushes[color]; ok {
		return existing
	}

	created := createSolidBrush(color)
	o.brushes[color] = created

	return created
}

func (o *WindowsOSD) releaseResources() {
	o.destroyFonts()

	for color, brush := range o.brushes {
		win.DeleteObject(win.HGDIOBJ(brush))
		delete(o.brushes, color)
	}

	if o.classAtom != 0 && o.classNameW != nil {
		win.UnregisterClass(o.classNameW)
		o.classAtom = 0
	}

	o.hwnd.Store(0)
	activeOSD = nil
}

func createOSDFont(points int, scale float64, weight int32) win.HFONT {
	logFont := win.LOGFONT{
		LfHeight:  int32(-math.Round(float64(points) * scale * 96.0 / 72.0)),
		LfWeight:  weight,
		LfCharSet: byte(win.DEFAULT_CHARSET),
		LfQuality: byte(win.CLEARTYPE_QUALITY),
	}

	if name, err := syscall.UTF16FromString("Segoe UI"); err == nil {
		copy(logFont.LfFaceName[:], name)
	}

	return win.CreateFontIndirect(&logFont)
}

func drawText(hdc win.HDC, text string, rect *win.RECT, format uint32) {
	encoded, err := syscall.UTF16FromString(text)
	if err != nil {
		return
	}

	win.DrawTextEx(hdc, &encoded[0], int32(len(encoded)-1), rect, format, nil)
}

// primaryWorkArea returns the primary monitor's usable area, taskbar excluded
func primaryWorkArea() win.RECT {
	var rect win.RECT

	if !win.SystemParametersInfo(spiGetWorkArea, 0, unsafe.Pointer(&rect), 0) {
		return win.RECT{Left: 0, Top: 0, Right: 1920, Bottom: 1080}
	}

	return rect
}

func setLayeredWindowAttributes(hwnd win.HWND, key win.COLORREF, alpha byte, flags uint32) {
	syscall.SyscallN(procSetLayeredWindowAttributes.Addr(),
		uintptr(hwnd), uintptr(key), uintptr(alpha), uintptr(flags))
}

func setWindowRgn(hwnd win.HWND, rgn win.HRGN, redraw bool) {
	var redrawFlag uintptr
	if redraw {
		redrawFlag = 1
	}

	syscall.SyscallN(procSetWindowRgn.Addr(), uintptr(hwnd), uintptr(rgn), redrawFlag)
}

func createRoundRectRgn(left, top, right, bottom, width, height int32) win.HRGN {
	ret, _, _ := syscall.SyscallN(procCreateRoundRectRgn.Addr(),
		uintptr(left), uintptr(top), uintptr(right), uintptr(bottom),
		uintptr(width), uintptr(height))

	return win.HRGN(ret)
}

func createSolidBrush(color win.COLORREF) win.HBRUSH {
	ret, _, _ := syscall.SyscallN(procCreateSolidBrush.Addr(), uintptr(color))

	return win.HBRUSH(ret)
}

func fillRect(hdc win.HDC, rect *win.RECT, brush win.HBRUSH) {
	syscall.SyscallN(procFillRect.Addr(),
		uintptr(hdc), uintptr(unsafe.Pointer(rect)), uintptr(brush))
}
