package opus

import "errors"

// ErrNoCgo: this build has no libopus (built with CGO_ENABLED=0).
var ErrNoCgo = errors.New("opus: built without cgo (no libopus)")
