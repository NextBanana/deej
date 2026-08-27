package deej

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

// demo values for the tray's overlay preview - one dimmed row is included
// on purpose, so the inactive styling can be checked without closing an app
var osdPreviewEntries = []OSDEntry{
	{SliderID: 0, Label: "Master", Percent: 0.72, Active: true},
	{SliderID: 1, Label: "Browser", Percent: 0.45, Active: true},
	{SliderID: 2, Label: "Spotify", Percent: 0.30, Active: false},
}

// showOSDPreview displays a sample overlay, using the user's own slider labels
// where they've configured them. This exists to make tuning position, scale and
// timing possible without touching the hardware
func (d *Deej) showOSDPreview() {
	for _, entry := range osdPreviewEntries {
		entry.Label = d.osdLabel(entry.SliderID, entry.Label)
		d.osd.ShowEntry(entry)
	}
}

// osdLabel returns the configured label for a slider, falling back to the
// provided default when the user hasn't named it
func (d *Deej) osdLabel(sliderID int, fallback string) string {
	if label, ok := d.config.SliderLabels[sliderID]; ok && label != "" {
		return label
	}

	return fallback
}
