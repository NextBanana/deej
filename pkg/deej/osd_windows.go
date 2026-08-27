package deej

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/win"
	"go.uber.org/zap"
)

// win32 bits that lxn/win doesn't expose, bound lazily. user32, gdi32 and shell32
// are already loaded into every GUI process, so this doesn't cost a real load
var (
	osdUser32  = syscall.NewLazyDLL("user32.dll")
	osdGdi32   = syscall.NewLazyDLL("gdi32.dll")
	osdShell32 = syscall.NewLazyDLL("shell32.dll")

	procSetLayeredWindowAttributes = osdUser32.NewProc("SetLayeredWindowAttributes")
	procSetWindowRgn               = osdUser32.NewProc("SetWindowRgn")
	procFillRect                   = osdUser32.NewProc("FillRect")
	procEnumDisplayMonitors        = osdUser32.NewProc("EnumDisplayMonitors")
	procCreateSolidBrush           = osdGdi32.NewProc("CreateSolidBrush")
	procCreateRoundRectRgn         = osdGdi32.NewProc("CreateRoundRectRgn")

	procSHQueryUserNotificationState = osdShell32.NewProc("SHQueryUserNotificationState")
)

const (
	osdClassName = "DeejOSDWindow"

	// SPI_GETWORKAREA gives the primary monitor's work area, taskbar excluded
	spiGetWorkArea = 0x0030
	lwaAlpha       = 0x00000002

	// our own messages. WM_APP is the first value windows reserves for apps
	wmOSDUpdate = win.WM_APP + 1
	wmOSDQuit   = win.WM_APP + 2

	osdTimerID       = 1
	osdTimerInterval = 16 // ms, ~60fps while visible - the timer is killed when hidden

	// a fullscreen foreground window can end up above topmost windows, so panels
	// re-assert their z-order every so often while they're on screen
	osdTopmostReassertTicks = 16

	// QUNS_RUNNING_D3D_FULL_SCREEN - a direct3d application has taken exclusive
	// control of a display. nothing drawn from outside that process reaches it
	qunsRunningD3DFullScreen = 3

	// layout, in logical pixels at 96 dpi. everything is scaled from here
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

// the window procedure is a package-level callback, so it needs a way back to the
// instance. deej only ever creates one overlay, which makes this safe
var activeOSD *WindowsOSD

var osdWndProcCallback = syscall.NewCallback(osdWndProc)

var osdEnumMonitorsCallback = syscall.NewCallback(osdEnumMonitorsProc)

// collected during EnumDisplayMonitors, which calls back synchronously. only ever
// touched from the ui thread, so it needs no locking
var osdEnumMonitorsResult []win.RECT

type osdEntryState struct {
	entry     OSDEntry
	expiresAt time.Time
}

// osdPanel is one monitor's copy of the overlay. Each has its own fonts because
// monitors can run at different dpi, and the app manifest opts into per-monitor
// dpi awareness - windows won't scale anything for us
type osdPanel struct {
	hwnd      win.HWND
	work      win.RECT
	scale     float64
	fontLabel win.HFONT
	fontValue win.HFONT
}

func (p *osdPanel) destroyFonts() {
	if p.fontLabel != 0 {
		win.DeleteObject(win.HGDIOBJ(p.fontLabel))
		p.fontLabel = 0
	}

	if p.fontValue != 0 {
		win.DeleteObject(win.HGDIOBJ(p.fontValue))
		p.fontValue = 0
	}
}

// WindowsOSD renders the overlay as click-through layered windows, one per monitor.
// A hidden controller window owns the timer and receives cross-thread posts, which
// keeps panels free to be destroyed and rebuilt when the monitor layout changes
type WindowsOSD struct {
	deej   *Deej
	logger *zap.SugaredLogger

	// guards entries, written from deej's goroutines and read by the ui thread
	mutex   sync.Mutex
	entries map[int]*osdEntryState

	// stable target for ShowEntry's posts, which come from other goroutines
	controller atomic.Uintptr

	// everything below is only ever touched on the ui thread
	panels     map[win.HWND]*osdPanel
	panelAreas []win.RECT
	brushes    map[win.COLORREF]win.HBRUSH

	visible      bool
	fadingOut    bool
	alpha        float64
	lastRows     int
	inFullscreen bool
	tickCount    uint64

	classAtom  win.ATOM
	hInstance  win.HINSTANCE
	classNameW *uint16
}

// newOSD creates the Windows overlay implementation
func newOSD(deej *Deej, logger *zap.SugaredLogger) (OSD, error) {
	logger = logger.Named("osd")

	o := &WindowsOSD{
		deej:    deej,
		logger:  logger,
		entries: make(map[int]*osdEntryState),
		panels:  make(map[win.HWND]*osdPanel),
		brushes: make(map[win.COLORREF]win.HBRUSH),
	}

	logger.Debug("Created Windows OSD instance")

	return o, nil
}

// Start creates the controller window on a dedicated thread and pumps its messages
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

	if controller := win.HWND(o.controller.Load()); controller != 0 {
		win.PostMessage(controller, wmOSDUpdate, 0, 0)
	}
}

// Stop asks the ui thread to tear everything down and exit its message loop
func (o *WindowsOSD) Stop() {
	if controller := win.HWND(o.controller.Load()); controller != 0 {
		win.PostMessage(controller, wmOSDQuit, 0, 0)
	}
}

// uiThread owns every window for its entire lifetime. win32 windows belong to the
// thread that created them, so this goroutine must stay pinned to one os thread
func (o *WindowsOSD) uiThread(ready chan error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := o.createController(); err != nil {
		o.logger.Errorw("Failed to create overlay controller window", "error", err)
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

func (o *WindowsOSD) createController() error {
	activeOSD = o

	className, err := syscall.UTF16PtrFromString(osdClassName)
	if err != nil {
		return fmt.Errorf("encode class name: %w", err)
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

	// never shown - it exists purely to own the timer and receive posted messages
	controller := win.CreateWindowEx(
		0, className, className, win.WS_POPUP,
		0, 0, 0, 0, 0, 0, o.hInstance, nil)

	if controller == 0 {
		return fmt.Errorf("create overlay controller window")
	}

	o.controller.Store(uintptr(controller))

	return nil
}

func osdWndProc(hwnd win.HWND, msg uint32, wParam uintptr, lParam uintptr) uintptr {
	o := activeOSD
	if o == nil {
		return win.DefWindowProc(hwnd, msg, wParam, lParam)
	}

	if hwnd == win.HWND(o.controller.Load()) {
		switch msg {
		case wmOSDUpdate:
			o.onUpdate()
			return 0

		case win.WM_TIMER:
			o.onTick()
			return 0

		case wmOSDQuit:
			o.destroyPanels()
			win.DestroyWindow(hwnd)
			return 0

		case win.WM_DESTROY:
			win.PostQuitMessage(0)
			return 0
		}

		return win.DefWindowProc(hwnd, msg, wParam, lParam)
	}

	if msg == win.WM_PAINT {
		if panel, ok := o.panels[hwnd]; ok {
			o.paintPanel(panel)

			return 0
		}
	}

	return win.DefWindowProc(hwnd, msg, wParam, lParam)
}

// onUpdate runs when a new or refreshed row arrives
func (o *WindowsOSD) onUpdate() {
	rows := o.rowCount()
	if rows == 0 {
		return
	}

	if o.checkExclusiveFullscreen() && o.deej.config.OSD.SkipInExclusiveFullscreen {

		// drop the pending rows rather than keeping them - the panels stay hidden,
		// so their timer never runs and nothing would ever expire them
		o.mutex.Lock()
		o.entries = make(map[int]*osdEntryState)
		o.mutex.Unlock()

		return
	}

	o.ensurePanels()

	if len(o.panels) == 0 {
		return
	}

	o.fadingOut = false
	o.relayoutPanels(rows)

	if !o.visible {
		o.visible = true
		o.alpha = 0

		for _, panel := range o.panels {
			win.ShowWindow(panel.hwnd, win.SW_SHOWNOACTIVATE)
		}

		win.SetTimer(win.HWND(o.controller.Load()), osdTimerID, osdTimerInterval, 0)
	}

	o.applyAlpha()
	o.invalidatePanels()
}

// onTick expires rows one by one and drives the fade
func (o *WindowsOSD) onTick() {
	o.tickCount++

	// applications going fullscreen can push themselves above the topmost band.
	// claiming it back periodically keeps panels visible over borderless fullscreen
	if o.visible && o.tickCount%osdTopmostReassertTicks == 0 {
		for _, panel := range o.panels {
			win.SetWindowPos(panel.hwnd, win.HWND_TOPMOST, 0, 0, 0, 0,
				win.SWP_NOACTIVATE|win.SWP_NOMOVE|win.SWP_NOSIZE)
		}
	}

	rows := o.expireEntries()

	if rows == 0 {
		o.fadingOut = true
	} else if rows != o.lastRows {

		// a row dropped off while others remain - the panels have to shrink
		o.relayoutPanels(rows)
		o.invalidatePanels()
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
			win.KillTimer(win.HWND(o.controller.Load()), osdTimerID)

			for _, panel := range o.panels {
				win.ShowWindow(panel.hwnd, win.SW_HIDE)
			}

			o.visible = false
			o.lastRows = 0

			return
		}
	}

	o.applyAlpha()
}

// ensurePanels syncs the set of panels with the monitors currently attached. It's
// called on every update, which is cheap and means display changes, resolution
// switches and monitors being turned off need no separate handling
func (o *WindowsOSD) ensurePanels() {
	areas := o.targetWorkAreas()

	if len(o.panels) > 0 && sameWorkAreas(areas, o.panelAreas) {
		return
	}

	o.destroyPanels()

	for _, area := range areas {
		panel := o.createPanel(area)
		if panel == nil {
			continue
		}

		o.panels[panel.hwnd] = panel
	}

	o.panelAreas = areas

	// the fresh panels start hidden, so the show path has to run again
	o.visible = false

	o.logger.Debugw("Rebuilt overlay panels", "count", len(o.panels))
}

func (o *WindowsOSD) createPanel(area win.RECT) *osdPanel {

	// created at the monitor's own origin so GetDpiForWindow resolves against it
	hwnd := win.CreateWindowEx(
		win.WS_EX_LAYERED|win.WS_EX_TRANSPARENT|win.WS_EX_TOOLWINDOW|
			win.WS_EX_NOACTIVATE|win.WS_EX_TOPMOST,
		o.classNameW,
		o.classNameW,
		win.WS_POPUP,
		area.Left, area.Top, 1, 1,
		0, 0, o.hInstance, nil)

	if hwnd == 0 {
		o.logger.Warnw("Failed to create overlay panel", "area", area)

		return nil
	}

	setLayeredWindowAttributes(hwnd, 0, 0, lwaAlpha)

	return &osdPanel{hwnd: hwnd, work: area}
}

func (o *WindowsOSD) destroyPanels() {
	for hwnd, panel := range o.panels {
		panel.destroyFonts()
		win.DestroyWindow(hwnd)
		delete(o.panels, hwnd)
	}

	o.panelAreas = nil
}

// targetWorkAreas returns the work area of every monitor the overlay should appear
// on, honouring the osd.monitors setting
func (o *WindowsOSD) targetWorkAreas() []win.RECT {
	if strings.EqualFold(o.deej.config.OSD.Monitors, "primary") {
		return []win.RECT{primaryWorkArea()}
	}

	areas := enumerateWorkAreas()
	if len(areas) == 0 {
		return []win.RECT{primaryWorkArea()}
	}

	return areas
}

func (o *WindowsOSD) relayoutPanels(rows int) {
	for _, panel := range o.panels {
		o.relayoutPanel(panel, rows)
	}

	o.lastRows = rows
}

// relayoutPanel resizes and repositions one panel. The panel is anchored to its
// configured edge and grows away from it, so with a bottom position the bottom row
// stays put no matter how many sliders are shown
func (o *WindowsOSD) relayoutPanel(panel *osdPanel, rows int) {
	o.ensurePanelMetrics(panel)

	sc := func(v int) int32 { return int32(math.Round(float64(v) * panel.scale)) }

	width := sc(osdWidth)
	height := sc(osdPadding)*2 +
		int32(rows)*sc(osdRowHeight) +
		int32(rows-1)*sc(osdRowGap)

	work := panel.work
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

	win.SetWindowPos(panel.hwnd, win.HWND_TOPMOST, x, y, width, height, win.SWP_NOACTIVATE)

	// rounded corners. once the region is handed over, windows owns it - deleting
	// it here would be a use-after-free
	radius := sc(osdCornerRad) * 2
	if rgn := createRoundRectRgn(0, 0, width+1, height+1, radius, radius); rgn != 0 {
		setWindowRgn(panel.hwnd, rgn, false)
	}
}

// ensurePanelMetrics recreates the panel's fonts when its effective scale changed
func (o *WindowsOSD) ensurePanelMetrics(panel *osdPanel) {
	dpi := win.GetDpiForWindow(panel.hwnd)
	if dpi == 0 {
		dpi = 96
	}

	configured := o.deej.config.OSD.Scale
	if configured <= 0 {
		configured = 1
	}

	scale := float64(dpi) / 96.0 * configured

	if panel.fontLabel != 0 && math.Abs(panel.scale-scale) < 0.001 {
		return
	}

	panel.destroyFonts()
	panel.fontLabel = createOSDFont(osdLabelPtSz, scale, int32(win.FW_SEMIBOLD))
	panel.fontValue = createOSDFont(osdValuePtSz, scale, int32(win.FW_NORMAL))
	panel.scale = scale
}

func (o *WindowsOSD) paintPanel(panel *osdPanel) {
	var ps win.PAINTSTRUCT

	hdc := win.BeginPaint(panel.hwnd, &ps)
	defer win.EndPaint(panel.hwnd, &ps)

	var client win.RECT
	win.GetClientRect(panel.hwnd, &client)

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

	// windows can send WM_PAINT before we've ever laid the panel out, so don't
	// assume the fonts already exist. this is a no-op once they do
	o.ensurePanelMetrics(panel)

	sc := func(v int) int32 { return int32(math.Round(float64(v) * panel.scale)) }

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

			win.SelectObject(memDC, win.HGDIOBJ(panel.fontValue))
			drawText(memDC, value, &textRect, win.DT_RIGHT|win.DT_SINGLELINE|win.DT_VCENTER)
		}

		win.SelectObject(memDC, win.HGDIOBJ(panel.fontLabel))
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

func (o *WindowsOSD) applyAlpha() {
	alpha := byte(o.alpha)

	for _, panel := range o.panels {
		setLayeredWindowAttributes(panel.hwnd, 0, alpha, lwaAlpha)
	}
}

func (o *WindowsOSD) invalidatePanels() {
	for _, panel := range o.panels {
		win.InvalidateRect(panel.hwnd, nil, false)
	}
}

// snapshot returns the current rows sorted by slider id. Sorting by id rather than
// by recency keeps a given slider in a fixed place, so the panel stays readable
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

// checkExclusiveFullscreen reports whether a direct3d application currently owns a
// display outright. It logs the transition once rather than on every move, so the
// log says plainly why the overlay stayed away
func (o *WindowsOSD) checkExclusiveFullscreen() bool {
	var state int32

	// S_OK is 0. on any failure we assume there's no exclusive fullscreen, which
	// keeps the overlay's behaviour unchanged if the call ever goes wrong
	ret, _, _ := syscall.SyscallN(procSHQueryUserNotificationState.Addr(),
		uintptr(unsafe.Pointer(&state)))

	if ret != 0 {
		return false
	}

	fullscreen := state == qunsRunningD3DFullScreen

	if fullscreen != o.inFullscreen {
		o.inFullscreen = fullscreen

		if fullscreen {
			o.logger.Infow("A display is under exclusive fullscreen - no external window "+
				"can be drawn over that one. Panels on the other monitors are unaffected",
				"skipping", o.deej.config.OSD.SkipInExclusiveFullscreen)
		} else {
			o.logger.Debug("Exclusive fullscreen ended")
		}
	}

	return fullscreen
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
	o.destroyPanels()

	for color, brush := range o.brushes {
		win.DeleteObject(win.HGDIOBJ(brush))
		delete(o.brushes, color)
	}

	if o.classAtom != 0 && o.classNameW != nil {
		win.UnregisterClass(o.classNameW)
		o.classAtom = 0
	}

	o.controller.Store(0)
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

// enumerateWorkAreas returns the usable area of every attached monitor, taskbar
// excluded, ordered left to right so the set stays comparable between calls
func enumerateWorkAreas() []win.RECT {
	osdEnumMonitorsResult = nil

	syscall.SyscallN(procEnumDisplayMonitors.Addr(), 0, 0, osdEnumMonitorsCallback, 0)

	areas := osdEnumMonitorsResult
	osdEnumMonitorsResult = nil

	sort.Slice(areas, func(i, j int) bool {
		if areas[i].Left != areas[j].Left {
			return areas[i].Left < areas[j].Left
		}

		return areas[i].Top < areas[j].Top
	})

	return areas
}

func osdEnumMonitorsProc(monitor win.HMONITOR, hdc win.HDC, clip *win.RECT, data uintptr) uintptr {
	var info win.MONITORINFO
	info.CbSize = uint32(unsafe.Sizeof(info))

	if win.GetMonitorInfo(monitor, &info) {
		osdEnumMonitorsResult = append(osdEnumMonitorsResult, info.RcWork)
	}

	// a non-zero return continues the enumeration
	return 1
}

func sameWorkAreas(a []win.RECT, b []win.RECT) bool {
	if len(a) != len(b) {
		return false
	}

	for idx := range a {
		if a[idx] != b[idx] {
			return false
		}
	}

	return true
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
