package audit

import (
	"io"
	"os"
)

// stderrSink is overridable in tests; in production it points to os.Stderr.
var stderrSink io.Writer = os.Stderr
