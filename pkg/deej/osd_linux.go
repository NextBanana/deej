package deej

import "go.uber.org/zap"

// noopOSD stands in on platforms without an overlay implementation, so the
// rest of the package can call into it unconditionally
type noopOSD struct{}

// newOSD returns a do-nothing overlay
func newOSD(deej *Deej, logger *zap.SugaredLogger) (OSD, error) {
	logger.Named("osd").Debug("Overlay is not implemented on this platform, using no-op")

	return &noopOSD{}, nil
}

// Start does nothing
func (o *noopOSD) Start() error { return nil }

// ShowEntry does nothing
func (o *noopOSD) ShowEntry(entry OSDEntry) {}

// Stop does nothing
func (o *noopOSD) Stop() {}
