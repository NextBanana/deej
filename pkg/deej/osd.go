package deej

import (
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"time"
	"unicode"
)

// OSDEntry is a single row of the on-screen overlay, describing one slider's
// current state at the moment it was moved
type OSDEntry struct {

	// SliderID determines the row's position - rows are always sorted by it,
	// so a given slider keeps its place instead of jumping around
	SliderID int

	// Label is the human-readable name shown on the left of the row
	Label string

	// Percent is the slider's level, between 0.0 and 1.0
	Percent float32

	// Active is false when the slider's target isn't currently running,
	// which renders the row dimmed instead of hiding it
	Active bool
}

// OSD renders deej's on-screen volume overlay. Implementations are
// platform-specific; non-Windows builds get a no-op
type OSD interface {

	// Start brings the overlay up in the background. It doesn't display
	// anything on its own - that only happens once entries come in
	Start() error

	// ShowEntry displays a row, or refreshes it if it's already on screen,
	// restarting its individual timeout either way. Safe to call from any goroutine
	ShowEntry(entry OSDEntry)

	// Stop tears the overlay down
	Stop()
}

// levels used for the preview's bars, cycled across however many sliders exist.
// they're deliberately uneven so the bar rendering can be judged at a glance
var osdPreviewLevels = []float32{0.72, 0.45, 0.30, 0.88, 0.15, 0.60}

// showOSDPreview displays a sample overlay built from the user's own configuration,
// so the preview has the same number of rows - and therefore the same size - as the
// panel they'll see in practice. This is what makes tuning position, scale and timing
// possible without touching the hardware
func (d *Deej) showOSDPreview() {
	sliderIDs := d.configuredSliderIDs()

	// nothing configured yet - still show something, or the menu item looks broken
	if len(sliderIDs) == 0 {
		sliderIDs = []int{0}
	}

	for idx, sliderID := range sliderIDs {
		d.osd.ShowEntry(OSDEntry{
			SliderID: sliderID,
			Label:    d.osdLabel(sliderID, d.osdFallbackLabel(sliderID)),
			Percent:  osdPreviewLevels[idx%len(osdPreviewLevels)],

			// every third row is previewed as inactive, so the dimmed styling can be
			// judged without having to close an application first
			Active: idx%3 != 2,
		})
	}
}

// configuredSliderIDs returns every slider the user has either mapped to a target
// or given a label, sorted ascending. Sliders left empty in the config are skipped
func (d *Deej) configuredSliderIDs() []int {
	unique := make(map[int]bool)

	d.config.SliderMapping.iterate(func(sliderIdx int, targets []string) {
		if len(targets) > 0 {
			unique[sliderIdx] = true
		}
	})

	for sliderIdx := range d.config.SliderLabels {
		unique[sliderIdx] = true
	}

	sliderIDs := make([]int, 0, len(unique))
	for sliderIdx := range unique {
		sliderIDs = append(sliderIDs, sliderIdx)
	}

	sort.Ints(sliderIDs)

	return sliderIDs
}

// osdLabel returns the configured label for a slider, falling back to the
// provided default when the user hasn't named it
func (d *Deej) osdLabel(sliderID int, fallback string) string {
	if label, ok := d.config.SliderLabels[sliderID]; ok && label != "" {
		return label
	}

	return fallback
}

// showSliderOSD is the path real slider movements take to the overlay. It applies
// the startup suppression that the manual preview deliberately bypasses
func (d *Deej) showSliderOSD(entry OSDEntry) {
	if d.osdSuppressed() {
		return
	}

	// the overlay must never be able to take deej down with it - volume control is
	// the job that actually matters, and it runs on this same goroutine
	defer func() {
		if recovered := recover(); recovered != nil {
			d.logger.Errorw("Recovered from a panic while showing the overlay",
				"panic", recovered, "stack", string(debug.Stack()))
		}
	}()

	d.osd.ShowEntry(entry)
}

// setupOSDSuppression silences the overlay for a moment after startup and after
// every config reload. Both of those make serial.go forget its last known slider
// values, which makes the next line from the board emit a move event for every
// single slider at once - without this, the full panel would pop up unprompted
func (d *Deej) setupOSDSuppression() {
	d.suppressOSD()

	configReloadedChannel := d.config.SubscribeToChanges()

	go func() {
		for {
			<-configReloadedChannel
			d.suppressOSD()
		}
	}()
}

func (d *Deej) suppressOSD() {
	window := time.Duration(d.config.OSD.SuppressStartupMS) * time.Millisecond

	if window <= 0 {
		d.osdSuppressUntil.Store(0)

		return
	}

	d.osdSuppressUntil.Store(time.Now().Add(window).UnixNano())
}

func (d *Deej) osdSuppressed() bool {
	until := d.osdSuppressUntil.Load()

	return until != 0 && time.Now().UnixNano() < until
}

// osdLabelFromTargets derives a display name from a slider's configured targets,
// for sliders the user hasn't named in slider_labels
func osdLabelFromTargets(targets []string) string {
	if len(targets) == 0 {
		return ""
	}

	label := osdFriendlyTargetName(targets[0])

	// groups get a counter rather than a long list, which wouldn't fit anyway
	if len(targets) > 1 {
		label = fmt.Sprintf("%s +%d", label, len(targets)-1)
	}

	return label
}

func osdFriendlyTargetName(target string) string {
	target = strings.TrimSpace(target)
	normalized := strings.ToLower(target)

	switch normalized {
	case masterSessionName:
		return "Master"
	case systemSessionName:
		return "System"
	case inputSessionName:
		return "Microphone"
	case specialTargetTransformPrefix + specialTargetCurrentWindow:
		return "Current app"
	case specialTargetTransformPrefix + specialTargetAllUnmapped:
		return "Unmapped apps"
	}

	// device targets look like "Speakers (Realtek High Definition Audio)" - the
	// friendly part in front is enough to tell devices apart, and it actually fits
	if deviceSessionKeyPattern.MatchString(target) {
		if idx := strings.Index(target, " ("); idx > 0 {
			return target[:idx]
		}

		return target
	}

	// plain process names: drop the extension and give it a capital letter
	return osdCapitalize(strings.TrimSuffix(normalized, ".exe"))
}

func osdCapitalize(value string) string {
	if value == "" {
		return value
	}

	runes := []rune(value)

	return string(unicode.ToUpper(runes[0])) + string(runes[1:])
}

// osdFallbackLabel derives a slider's name from its configured targets, falling
// back to its index when it has none
func (d *Deej) osdFallbackLabel(sliderID int) string {
	if targets, ok := d.config.SliderMapping.get(sliderID); ok {
		if label := osdLabelFromTargets(targets); label != "" {
			return label
		}
	}

	return fmt.Sprintf("Slider %d", sliderID)
}
